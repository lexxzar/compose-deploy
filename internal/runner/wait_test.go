package runner

import (
	"context"
	"errors"
	"testing"
	"time"
)

// testWaitOpts returns options with a small grace/poll and a generous timeout
// so timeout doesn't interfere with the rule under test.
func testWaitOpts() WaitOptions {
	return WaitOptions{
		Timeout: time.Hour,
		Grace:   10 * time.Second,
		Poll:    2 * time.Second,
	}
}

// feedPolls threads a sequence of status snapshots through EvaluateWait,
// advancing elapsed by opts.Poll between polls, stopping early when done.
// Returns the final state, whether it finished, and the elapsed at the last poll.
func feedPolls(services []string, opts WaitOptions, polls []map[string]ServiceStatus) (WaitState, bool, time.Duration) {
	st := NewWaitState(services)
	var elapsed time.Duration
	done := false
	for _, p := range polls {
		st, done = EvaluateWait(st, p, opts, elapsed)
		if done {
			return st, done, elapsed
		}
		elapsed += opts.Poll
	}
	return st, done, elapsed
}

func TestEvaluateWait_AllHealthy(t *testing.T) {
	opts := testWaitOpts()
	polls := []map[string]ServiceStatus{
		{"web": {Running: true, Health: "starting"}, "db": {Running: true, Health: "starting"}},
		{"web": {Running: true, Health: "healthy"}, "db": {Running: true, Health: "healthy"}},
	}
	st, done, _ := feedPolls([]string{"web", "db"}, opts, polls)
	if !done {
		t.Fatalf("expected done after both healthy")
	}
	if st.Verdicts["web"] != VerdictHealthy || st.Verdicts["db"] != VerdictHealthy {
		t.Fatalf("verdicts = %v, want both healthy", st.Verdicts)
	}
	rep := st.Report(0)
	if !rep.OK {
		t.Errorf("report OK = false, want true")
	}
}

func TestEvaluateWait_GracePass(t *testing.T) {
	opts := testWaitOpts() // Grace 10s, Poll 2s
	// No healthcheck, running the whole time. Passes once continuously running >= Grace.
	running := map[string]ServiceStatus{"web": {Running: true}}
	polls := []map[string]ServiceStatus{
		running, // elapsed 0  -> firstRunningAt=0
		running, // 2
		running, // 4
		running, // 6
		running, // 8
		running, // 10 -> 10-0 >= 10 -> pass
	}
	st, done, elapsed := feedPolls([]string{"web"}, opts, polls)
	if !done {
		t.Fatalf("expected done once grace elapsed")
	}
	if elapsed != 10*time.Second {
		t.Errorf("passed at elapsed %v, want 10s", elapsed)
	}
	if st.Verdicts["web"] != VerdictRunningNoHC {
		t.Errorf("verdict = %q, want %q", st.Verdicts["web"], VerdictRunningNoHC)
	}
}

func TestEvaluateWait_GracePendingBeforeThreshold(t *testing.T) {
	opts := testWaitOpts()
	running := map[string]ServiceStatus{"web": {Running: true}}
	// Only up to elapsed 8s (< grace 10s): must still be pending.
	polls := []map[string]ServiceStatus{running, running, running, running, running}
	st, done, _ := feedPolls([]string{"web"}, opts, polls)
	if done {
		t.Fatalf("expected NOT done before grace elapsed")
	}
	if st.Verdicts["web"] != VerdictPending {
		t.Errorf("verdict = %q, want pending", st.Verdicts["web"])
	}
}

func TestEvaluateWait_GraceResetAfterRestartFlap(t *testing.T) {
	opts := testWaitOpts() // Grace 10s, Poll 2s
	running := map[string]ServiceStatus{"web": {Running: true}}
	restarting := map[string]ServiceStatus{"web": {Running: false, Uptime: "restarting"}}
	// Running for a bit, one restart flap resets the grace anchor, then must
	// run a full grace window from the flap onward before passing.
	polls := []map[string]ServiceStatus{
		running,    // 0  firstRunningAt=0
		running,    // 2
		restarting, // 4  -> counter=1, firstRunningAt reset
		running,    // 6  firstRunningAt=6
		running,    // 8   6..8  = 2
		running,    // 10  = 4
		running,    // 12  = 6
		running,    // 14  = 8
		running,    // 16  = 10 -> pass
	}
	st, done, elapsed := feedPolls([]string{"web"}, opts, polls)
	if !done {
		t.Fatalf("expected done after grace from flap")
	}
	if elapsed != 16*time.Second {
		t.Errorf("passed at %v, want 16s (grace restarted at 6s)", elapsed)
	}
	if st.Verdicts["web"] != VerdictRunningNoHC {
		t.Errorf("verdict = %q, want %q", st.Verdicts["web"], VerdictRunningNoHC)
	}
}

func TestEvaluateWait_UnhealthyFailFast(t *testing.T) {
	opts := testWaitOpts()
	polls := []map[string]ServiceStatus{
		{"web": {Running: true, Health: "unhealthy"}},
	}
	st, done, _ := feedPolls([]string{"web"}, opts, polls)
	if !done {
		t.Fatalf("expected done (fail fast) on unhealthy")
	}
	if st.Verdicts["web"] != VerdictUnhealthy {
		t.Errorf("verdict = %q, want %q", st.Verdicts["web"], VerdictUnhealthy)
	}
	if st.Report(0).OK {
		t.Errorf("report OK = true, want false")
	}
}

func TestEvaluateWait_ExitedAfterRunningFailFast(t *testing.T) {
	opts := testWaitOpts()
	polls := []map[string]ServiceStatus{
		{"web": {Running: true, Health: "starting"}}, // observed running
		{"web": {Running: false}},                    // then exited
	}
	st, done, _ := feedPolls([]string{"web"}, opts, polls)
	if !done {
		t.Fatalf("expected done (fail fast) on exit-after-running")
	}
	if st.Verdicts["web"] != VerdictExited {
		t.Errorf("verdict = %q, want %q", st.Verdicts["web"], VerdictExited)
	}
}

func TestEvaluateWait_RestartingFailFastAtThree(t *testing.T) {
	opts := testWaitOpts()
	restarting := map[string]ServiceStatus{"web": {Running: false, Uptime: "restarting"}}

	// 2 restarting polls MUST NOT fail and MUST NOT be misread as exited.
	st := NewWaitState([]string{"web"})
	var elapsed time.Duration
	st, done := EvaluateWait(st, restarting, opts, elapsed)
	if done || st.Verdicts["web"] != VerdictPending {
		t.Fatalf("after 1 restarting: done=%v verdict=%q, want pending", done, st.Verdicts["web"])
	}
	elapsed += opts.Poll
	st, done = EvaluateWait(st, restarting, opts, elapsed)
	if done || st.Verdicts["web"] != VerdictPending {
		t.Fatalf("after 2 restarting: done=%v verdict=%q, want pending (not exited)", done, st.Verdicts["web"])
	}
	// 3rd consecutive restarting trips the verdict.
	elapsed += opts.Poll
	st, done = EvaluateWait(st, restarting, opts, elapsed)
	if !done {
		t.Fatalf("after 3 restarting: expected done")
	}
	if st.Verdicts["web"] != VerdictRestarting {
		t.Errorf("verdict = %q, want %q", st.Verdicts["web"], VerdictRestarting)
	}
}

func TestEvaluateWait_RestartingCounterResets(t *testing.T) {
	opts := testWaitOpts()
	restarting := map[string]ServiceStatus{"web": {Running: false, Uptime: "restarting"}}
	healthy := map[string]ServiceStatus{"web": {Running: true, Health: "healthy"}}
	// Two restarts, then a healthy observation resolves it — the counter never
	// reaches 3 because it resets on the non-restarting poll.
	polls := []map[string]ServiceStatus{restarting, restarting, healthy}
	st, done, _ := feedPolls([]string{"web"}, opts, polls)
	if !done {
		t.Fatalf("expected done once healthy")
	}
	if st.Verdicts["web"] != VerdictHealthy {
		t.Errorf("verdict = %q, want healthy", st.Verdicts["web"])
	}
}

func TestEvaluateWait_NeverRunningDebounce(t *testing.T) {
	opts := testWaitOpts()
	notRunning := map[string]ServiceStatus{"web": {Running: false}}

	// 1 not-running poll (never observed running) MUST NOT fail.
	st := NewWaitState([]string{"web"})
	st, done := EvaluateWait(st, notRunning, opts, 0)
	if done || st.Verdicts["web"] != VerdictPending {
		t.Fatalf("after 1 not-running: done=%v verdict=%q, want pending", done, st.Verdicts["web"])
	}

	// 5 consecutive not-running polls trip exited (never started).
	st = NewWaitState([]string{"web"})
	var elapsed time.Duration
	done = false
	for i := 0; i < 5; i++ {
		st, done = EvaluateWait(st, notRunning, opts, elapsed)
		elapsed += opts.Poll
	}
	if !done {
		t.Fatalf("expected done after 5 consecutive not-running")
	}
	if st.Verdicts["web"] != VerdictExitedNeverStart {
		t.Errorf("verdict = %q, want %q", st.Verdicts["web"], VerdictExitedNeverStart)
	}
}

func TestEvaluateWait_NeverRunningResetsThenStarts(t *testing.T) {
	opts := testWaitOpts()
	notRunning := map[string]ServiceStatus{"web": {Running: false}}
	healthy := map[string]ServiceStatus{"web": {Running: true, Health: "healthy"}}
	// A few not-running polls, then it comes up healthy: must pass, not fail —
	// the never-running counter never reaches the threshold.
	polls := []map[string]ServiceStatus{notRunning, notRunning, healthy}
	st, done, _ := feedPolls([]string{"web"}, opts, polls)
	if !done {
		t.Fatalf("expected done once healthy")
	}
	if st.Verdicts["web"] != VerdictHealthy {
		t.Errorf("verdict = %q, want healthy", st.Verdicts["web"])
	}
}

func TestEvaluateWait_TimeoutWhileStarting(t *testing.T) {
	opts := WaitOptions{Timeout: 6 * time.Second, Grace: 10 * time.Second, Poll: 2 * time.Second}
	starting := map[string]ServiceStatus{"web": {Running: true, Health: "starting"}}
	// Never resolves; once elapsed >= timeout, verdict is timed out.
	st := NewWaitState([]string{"web"})
	var elapsed time.Duration
	done := false
	for i := 0; i < 10 && !done; i++ {
		st, done = EvaluateWait(st, starting, opts, elapsed)
		if !done {
			elapsed += opts.Poll
		}
	}
	if !done {
		t.Fatalf("expected done once timeout elapsed")
	}
	if elapsed != 6*time.Second {
		t.Errorf("timed out at %v, want 6s", elapsed)
	}
	if st.Verdicts["web"] != VerdictTimedOut {
		t.Errorf("verdict = %q, want %q", st.Verdicts["web"], VerdictTimedOut)
	}
}

func TestEvaluateWait_ResolutionWinsOnTimeoutPoll(t *testing.T) {
	// A service that goes healthy exactly on the timeout poll keeps healthy.
	opts := WaitOptions{Timeout: 4 * time.Second, Grace: 10 * time.Second, Poll: 2 * time.Second}
	st := NewWaitState([]string{"web"})
	st, done := EvaluateWait(st, map[string]ServiceStatus{"web": {Running: true, Health: "starting"}}, opts, 0)
	if done {
		t.Fatalf("unexpected done at elapsed 0")
	}
	st, done = EvaluateWait(st, map[string]ServiceStatus{"web": {Running: true, Health: "starting"}}, opts, 2*time.Second)
	if done {
		t.Fatalf("unexpected done at elapsed 2s")
	}
	// elapsed 4s == timeout, but the poll reports healthy: healthy wins.
	st, done = EvaluateWait(st, map[string]ServiceStatus{"web": {Running: true, Health: "healthy"}}, opts, 4*time.Second)
	if !done {
		t.Fatalf("expected done at timeout")
	}
	if st.Verdicts["web"] != VerdictHealthy {
		t.Errorf("verdict = %q, want healthy (resolution wins over timeout)", st.Verdicts["web"])
	}
}

func TestEvaluateWait_TimeoutPrecedesPostDeadlineHealthy(t *testing.T) {
	// Firm boundary (C3): a service that first reports healthy STRICTLY AFTER the
	// deadline must NOT flip a would-be-timeout into a pass — it times out. This
	// complements TestEvaluateWait_ResolutionWinsOnTimeoutPoll (healthy AT the
	// deadline still wins).
	opts := WaitOptions{Timeout: 4 * time.Second, Grace: 10 * time.Second, Poll: 2 * time.Second}
	st := NewWaitState([]string{"web"})
	st, done := EvaluateWait(st, map[string]ServiceStatus{"web": {Running: true, Health: "starting"}}, opts, 2*time.Second)
	if done {
		t.Fatalf("unexpected done before the deadline")
	}
	// elapsed 6s > timeout 4s: the healthy poll must be overridden by the timeout.
	st, done = EvaluateWait(st, map[string]ServiceStatus{"web": {Running: true, Health: "healthy"}}, opts, 6*time.Second)
	if !done {
		t.Fatalf("expected done past the deadline")
	}
	if st.Verdicts["web"] != VerdictTimedOut {
		t.Errorf("verdict = %q, want %q (timeout precedes post-deadline resolution)", st.Verdicts["web"], VerdictTimedOut)
	}
}

func TestEvaluateWait_GracePassBlockedAfterDeadline(t *testing.T) {
	// A no-healthcheck service whose grace window would only be satisfied AFTER
	// the deadline must time out, not pass — the grace pass is gated on the
	// deadline just like the healthy pass.
	opts := WaitOptions{Timeout: 4 * time.Second, Grace: 2 * time.Second, Poll: 2 * time.Second}
	running := map[string]ServiceStatus{"web": {Running: true}}
	st := NewWaitState([]string{"web"})
	// First observation at elapsed 6s (> timeout): firstRunningAt=6, grace not yet
	// met AND past the deadline ⇒ swept to timed out rather than passing later.
	st, done := EvaluateWait(st, running, opts, 6*time.Second)
	if !done {
		t.Fatalf("expected done past the deadline")
	}
	if st.Verdicts["web"] != VerdictTimedOut {
		t.Errorf("verdict = %q, want %q", st.Verdicts["web"], VerdictTimedOut)
	}
}

func TestEvaluateWait_MixedCheckedAndUnchecked(t *testing.T) {
	opts := testWaitOpts() // Grace 10s
	// web has a healthcheck (resolves on healthy); worker has none (grace).
	polls := make([]map[string]ServiceStatus, 0, 6)
	for i := 0; i < 6; i++ {
		health := "starting"
		if i >= 1 {
			health = "healthy"
		}
		polls = append(polls, map[string]ServiceStatus{
			"web":    {Running: true, Health: health},
			"worker": {Running: true},
		})
	}
	st, done, elapsed := feedPolls([]string{"web", "worker"}, opts, polls)
	// web healthy early, worker passes only after 10s grace (elapsed 10s).
	if !done {
		t.Fatalf("expected done once worker passes grace")
	}
	if elapsed != 10*time.Second {
		t.Errorf("done at %v, want 10s (worker grace-bound)", elapsed)
	}
	if st.Verdicts["web"] != VerdictHealthy {
		t.Errorf("web verdict = %q, want healthy", st.Verdicts["web"])
	}
	if st.Verdicts["worker"] != VerdictRunningNoHC {
		t.Errorf("worker verdict = %q, want %q", st.Verdicts["worker"], VerdictRunningNoHC)
	}
}

func TestEvaluateWait_ServiceAbsentFromStatusMap(t *testing.T) {
	opts := testWaitOpts()
	// "web" never appears in the status map: treated as not-running and feeds
	// the never-running counter identically to an explicit Running:false.
	empty := map[string]ServiceStatus{}
	st := NewWaitState([]string{"web"})
	var elapsed time.Duration
	done := false
	for i := 0; i < 5; i++ {
		st, done = EvaluateWait(st, empty, opts, elapsed)
		elapsed += opts.Poll
	}
	if !done {
		t.Fatalf("expected done after 5 absent polls")
	}
	if st.Verdicts["web"] != VerdictExitedNeverStart {
		t.Errorf("verdict = %q, want %q", st.Verdicts["web"], VerdictExitedNeverStart)
	}
}

func TestEvaluateWait_PurityInputNotMutated(t *testing.T) {
	opts := testWaitOpts()
	prev := NewWaitState([]string{"web"})
	_, _ = EvaluateWait(prev, map[string]ServiceStatus{"web": {Running: true, Health: "healthy"}}, opts, 0)
	if prev.Verdicts["web"] != VerdictPending {
		t.Errorf("EvaluateWait mutated its input state: verdict = %q", prev.Verdicts["web"])
	}
}

func TestWaitVerdict_Labels(t *testing.T) {
	// The exact strings are rendered by the CLI table and TUI; pin them.
	tests := []struct {
		v    WaitVerdict
		want string
		ok   bool
	}{
		{VerdictPending, "", false},
		{VerdictHealthy, "healthy", true},
		{VerdictRunningNoHC, "running (no healthcheck)", true},
		{VerdictUnhealthy, "unhealthy", false},
		{VerdictExited, "exited", false},
		{VerdictExitedNeverStart, "exited (never started)", false},
		{VerdictRestarting, "restarting", false},
		{VerdictTimedOut, "timed out (still starting)", false},
	}
	for _, tt := range tests {
		if string(tt.v) != tt.want {
			t.Errorf("verdict string = %q, want %q", string(tt.v), tt.want)
		}
		if tt.v.OK() != tt.ok {
			t.Errorf("%q.OK() = %v, want %v", tt.v, tt.v.OK(), tt.ok)
		}
		if tt.v.Terminal() != (tt.v != VerdictPending) {
			t.Errorf("%q.Terminal() = %v, want %v", tt.v, tt.v.Terminal(), tt.v != VerdictPending)
		}
	}
}

// TestWaitVerdict_Icon pins the shared glyphs (the CLI table and TUI both render
// WaitVerdict.Icon and style it themselves — single source of truth).
func TestWaitVerdict_Icon(t *testing.T) {
	cases := map[WaitVerdict]string{
		VerdictHealthy:     "♥",
		VerdictRunningNoHC: "●",
		VerdictPending:     "~",
		VerdictUnhealthy:   "✗",
		VerdictExited:      "✗",
		VerdictRestarting:  "✗",
		VerdictTimedOut:    "✗",
	}
	for v, want := range cases {
		if got := v.Icon(); got != want {
			t.Errorf("%q.Icon() = %q, want %q", v, got, want)
		}
	}
}

func TestWaitOptions_Normalize(t *testing.T) {
	got := WaitOptions{}.normalize()
	if got.Timeout != DefaultWaitTimeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, DefaultWaitTimeout)
	}
	if got.Grace != DefaultWaitGrace {
		t.Errorf("Grace = %v, want %v", got.Grace, DefaultWaitGrace)
	}
	if got.Poll != DefaultWaitPoll {
		t.Errorf("Poll = %v, want %v", got.Poll, DefaultWaitPoll)
	}
	// Explicit values are preserved.
	custom := WaitOptions{Timeout: time.Minute, Grace: time.Second, Poll: 500 * time.Millisecond}
	if custom.normalize() != custom {
		t.Errorf("normalize mutated explicit options: %+v", custom.normalize())
	}
}

func TestNewWaitState_SeedsPending(t *testing.T) {
	st := NewWaitState([]string{"a", "b"})
	if len(st.Services) != 2 {
		t.Fatalf("Services = %v, want 2 entries", st.Services)
	}
	for _, svc := range st.Services {
		if st.Verdicts[svc] != VerdictPending {
			t.Errorf("%s verdict = %q, want pending", svc, st.Verdicts[svc])
		}
	}
}

// --- WaitHealthy (blocking wrapper) ---

// statusResult is a single scripted ContainerStatus outcome.
type statusResult struct {
	status map[string]ServiceStatus
	err    error
}

// scriptedComposer feeds WaitHealthy a scripted sequence of ContainerStatus
// results. Once the sequence is exhausted it repeats the last entry, so
// never-resolving scenarios (timeout, ctx-cancel) need only one trailing entry.
// It embeds mockComposer for the no-op pipeline methods and overrides
// ListServices / ContainerStatus.
type scriptedComposer struct {
	mockComposer
	results       []statusResult
	idx           int
	statusCalls   int
	listServices  []string // non-nil overrides the embedded default
	listErr       error
	listCallCount int
}

func (s *scriptedComposer) ListServices(ctx context.Context) ([]string, error) {
	s.listCallCount++
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.listServices != nil {
		return s.listServices, nil
	}
	return s.mockComposer.ListServices(ctx)
}

func (s *scriptedComposer) ContainerStatus(ctx context.Context) (map[string]ServiceStatus, error) {
	s.statusCalls++
	if len(s.results) == 0 {
		return map[string]ServiceStatus{}, nil
	}
	i := s.idx
	if i >= len(s.results) {
		i = len(s.results) - 1 // repeat the last scripted result
	} else {
		s.idx++
	}
	return s.results[i].status, s.results[i].err
}

// fastWaitOpts uses millisecond durations so the wrapper's real-time polling
// loop runs quickly while leaving generous margins against flakiness.
func fastWaitOpts() WaitOptions {
	return WaitOptions{Timeout: time.Hour, Grace: time.Hour, Poll: time.Millisecond}
}

func healthyStatus(svcs ...string) map[string]ServiceStatus {
	m := make(map[string]ServiceStatus, len(svcs))
	for _, s := range svcs {
		m[s] = ServiceStatus{Running: true, Health: "healthy"}
	}
	return m
}

func TestWaitHealthy_Success(t *testing.T) {
	c := &scriptedComposer{results: []statusResult{
		{status: healthyStatus("web", "db")},
	}}
	rep, err := WaitHealthy(context.Background(), c, []string{"web", "db"}, fastWaitOpts())
	if err != nil {
		t.Fatalf("WaitHealthy err = %v, want nil", err)
	}
	if !rep.OK {
		t.Errorf("report OK = false, want true: %+v", rep.Verdicts)
	}
	if rep.Verdicts["web"] != VerdictHealthy || rep.Verdicts["db"] != VerdictHealthy {
		t.Errorf("verdicts = %v, want both healthy", rep.Verdicts)
	}
}

func TestWaitHealthy_FailFast(t *testing.T) {
	// An unhealthy service fails the wait but is NOT an operational error:
	// the wrapper returns (report, nil) with report.OK == false.
	c := &scriptedComposer{results: []statusResult{
		{status: map[string]ServiceStatus{"web": {Running: true, Health: "unhealthy"}}},
	}}
	rep, err := WaitHealthy(context.Background(), c, []string{"web"}, fastWaitOpts())
	if err != nil {
		t.Fatalf("WaitHealthy err = %v, want nil (verdict failure, not operational)", err)
	}
	if rep.OK {
		t.Errorf("report OK = true, want false")
	}
	if rep.Verdicts["web"] != VerdictUnhealthy {
		t.Errorf("verdict = %q, want %q", rep.Verdicts["web"], VerdictUnhealthy)
	}
}

func TestWaitHealthy_Timeout(t *testing.T) {
	// Never resolves (has a healthcheck stuck "starting"); small timeout so the
	// reducer's timeout sweep terminates the loop.
	opts := WaitOptions{Timeout: 25 * time.Millisecond, Grace: time.Hour, Poll: time.Millisecond}
	c := &scriptedComposer{results: []statusResult{
		{status: map[string]ServiceStatus{"web": {Running: true, Health: "starting"}}},
	}}
	rep, err := WaitHealthy(context.Background(), c, []string{"web"}, opts)
	if err != nil {
		t.Fatalf("WaitHealthy err = %v, want nil", err)
	}
	if rep.OK {
		t.Errorf("report OK = true, want false")
	}
	if rep.Verdicts["web"] != VerdictTimedOut {
		t.Errorf("verdict = %q, want %q", rep.Verdicts["web"], VerdictTimedOut)
	}
}

func TestWaitHealthy_TimeoutIsHardDeadline(t *testing.T) {
	// C3: the Poll interval far exceeds the timeout. Without capping the next
	// poll at the remaining time, the driver would sleep a full Poll (1s) past
	// the deadline before noticing the timeout. The cap makes the wait terminate
	// at ~Timeout (15ms). A stuck-starting service must time out, not hang.
	opts := WaitOptions{Timeout: 15 * time.Millisecond, Grace: time.Hour, Poll: time.Second}
	c := &scriptedComposer{results: []statusResult{
		{status: map[string]ServiceStatus{"web": {Running: true, Health: "starting"}}},
	}}
	start := time.Now()
	rep, err := WaitHealthy(context.Background(), c, []string{"web"}, opts)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitHealthy err = %v, want nil", err)
	}
	if rep.Verdicts["web"] != VerdictTimedOut {
		t.Errorf("verdict = %q, want %q", rep.Verdicts["web"], VerdictTimedOut)
	}
	// Generous margin: uncapped this would take ~1s (a full Poll); capped it is ~15ms.
	if elapsed > 500*time.Millisecond {
		t.Errorf("wait took %v, want ~Timeout — the Poll interval was not capped to the deadline", elapsed)
	}
}

func TestWaitHealthy_CtxCancelPartialReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the first poll
	// Never-resolving status; the loop must bail on the cancelled context.
	c := &scriptedComposer{results: []statusResult{
		{status: map[string]ServiceStatus{"web": {Running: true, Health: "starting"}}},
	}}
	rep, err := WaitHealthy(ctx, c, []string{"web"}, fastWaitOpts())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitHealthy err = %v, want context.Canceled", err)
	}
	// Partial report: the pending verdict survives, OK is false.
	if rep.OK {
		t.Errorf("report OK = true, want false")
	}
	if rep.Verdicts["web"] != VerdictPending {
		t.Errorf("verdict = %q, want pending (partial report)", rep.Verdicts["web"])
	}
}

func TestWaitHealthy_PollErrorTolerance(t *testing.T) {
	// Two transient poll errors (< threshold) are skipped; the third healthy
	// poll resolves the wait.
	pollErr := errors.New("ssh: connection reset")
	c := &scriptedComposer{results: []statusResult{
		{err: pollErr},
		{err: pollErr},
		{status: healthyStatus("web")},
	}}
	rep, err := WaitHealthy(context.Background(), c, []string{"web"}, fastWaitOpts())
	if err != nil {
		t.Fatalf("WaitHealthy err = %v, want nil (errors below threshold are transient)", err)
	}
	if !rep.OK || rep.Verdicts["web"] != VerdictHealthy {
		t.Errorf("report = %+v, want OK with web healthy", rep)
	}
}

func TestWaitHealthy_PollErrorThreeStrike(t *testing.T) {
	pollErr := errors.New("ssh: could not resolve hostname")
	// Every poll errors (repeat-last exhausts to the error entry).
	c := &scriptedComposer{results: []statusResult{{err: pollErr}}}
	rep, err := WaitHealthy(context.Background(), c, []string{"web"}, fastWaitOpts())
	if err == nil {
		t.Fatalf("WaitHealthy err = nil, want a poll-failure error")
	}
	if !errors.Is(err, pollErr) {
		t.Errorf("err = %v, want wrapped %v", err, pollErr)
	}
	// Exactly pollErrorThreshold poll attempts before giving up.
	if c.statusCalls != pollErrorThreshold {
		t.Errorf("statusCalls = %d, want %d", c.statusCalls, pollErrorThreshold)
	}
	// Partial report: nothing resolved.
	if rep.OK || rep.Verdicts["web"] != VerdictPending {
		t.Errorf("report = %+v, want not-OK with web pending", rep)
	}
}

func TestWaitHealthy_PollErrorCounterResets(t *testing.T) {
	// Two errors, a good poll (resets the counter), two more errors, another
	// good poll that resolves: the run of errors never reaches the threshold
	// because a successful poll in between resets the consecutive count.
	pollErr := errors.New("transient")
	starting := map[string]ServiceStatus{"web": {Running: true, Health: "starting"}}
	c := &scriptedComposer{results: []statusResult{
		{err: pollErr},
		{err: pollErr},
		{status: starting}, // resets counter
		{err: pollErr},
		{err: pollErr},
		{status: healthyStatus("web")},
	}}
	rep, err := WaitHealthy(context.Background(), c, []string{"web"}, fastWaitOpts())
	if err != nil {
		t.Fatalf("WaitHealthy err = %v, want nil (counter resets between error runs)", err)
	}
	if !rep.OK || rep.Verdicts["web"] != VerdictHealthy {
		t.Errorf("report = %+v, want OK with web healthy", rep)
	}
}

func TestWaitHealthy_EmptyServicesResolvesViaListServices(t *testing.T) {
	c := &scriptedComposer{
		listServices: []string{"api"},
		results:      []statusResult{{status: healthyStatus("api")}},
	}
	rep, err := WaitHealthy(context.Background(), c, nil, fastWaitOpts())
	if err != nil {
		t.Fatalf("WaitHealthy err = %v, want nil", err)
	}
	if c.listCallCount != 1 {
		t.Errorf("ListServices called %d times, want 1", c.listCallCount)
	}
	if _, ok := rep.Verdicts["api"]; !ok {
		t.Fatalf("report missing resolved service %q: %+v", "api", rep.Verdicts)
	}
	if !rep.OK || rep.Verdicts["api"] != VerdictHealthy {
		t.Errorf("report = %+v, want OK with api healthy", rep)
	}
}

func TestWaitHealthy_ListServicesError(t *testing.T) {
	listErr := errors.New("compose config --services failed")
	c := &scriptedComposer{listErr: listErr}
	_, err := WaitHealthy(context.Background(), c, nil, fastWaitOpts())
	if err == nil {
		t.Fatalf("WaitHealthy err = nil, want ListServices error")
	}
	if !errors.Is(err, listErr) {
		t.Errorf("err = %v, want wrapped %v", err, listErr)
	}
}

// blockingComposer's ContainerStatus blocks until the (wait-scoped) context is
// done, simulating a hung Docker daemon or a stalled SSH status call. When retErr
// is set it returns that error after the context fires (a poll that errors past
// the deadline); otherwise it returns ctx.Err().
type blockingComposer struct {
	mockComposer
	retErr error
	calls  int
}

func (b *blockingComposer) ListServices(ctx context.Context) ([]string, error) {
	return []string{"web"}, nil
}

func (b *blockingComposer) ContainerStatus(ctx context.Context) (map[string]ServiceStatus, error) {
	b.calls++
	<-ctx.Done()
	if b.retErr != nil {
		return nil, b.retErr
	}
	return nil, ctx.Err()
}

func TestWaitHealthy_HungPollInterruptedAtDeadline(t *testing.T) {
	// C5(a): a ContainerStatus that never returns must not block past the timeout.
	// The wait-scoped deadline context interrupts the hung poll AT the deadline and
	// the wait resolves to a non-OK timed-out report with a NIL error (the CLI maps
	// that to exit 2), rather than hanging on the unbounded parent context.
	opts := WaitOptions{Timeout: 40 * time.Millisecond, Grace: time.Hour, Poll: time.Second}
	c := &blockingComposer{}
	start := time.Now()
	rep, err := WaitHealthy(context.Background(), c, []string{"web"}, opts)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitHealthy err = %v, want nil (deadline expiry is not an operational error)", err)
	}
	if rep.OK {
		t.Errorf("report OK = true, want false")
	}
	if rep.Verdicts["web"] != VerdictTimedOut {
		t.Errorf("verdict = %q, want %q", rep.Verdicts["web"], VerdictTimedOut)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("wait took %v — a hung poll was not interrupted at the deadline", elapsed)
	}
}

func TestWaitHealthy_HungPollParentCancelIsCanceled(t *testing.T) {
	// C5(b): a parent cancellation (user Ctrl-C) while a poll is hung must still
	// return context.Canceled RAW (→ exit 1), NOT a timed-out report (→ exit 2).
	// The wait-scoped deadline context must not swallow the parent cancel — the
	// Q4/C4 exit-1-vs-exit-2 distinction is preserved.
	opts := WaitOptions{Timeout: time.Hour, Grace: time.Hour, Poll: time.Second}
	c := &blockingComposer{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	rep, err := WaitHealthy(ctx, c, []string{"web"}, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitHealthy err = %v, want context.Canceled (exit 1)", err)
	}
	if rep.OK {
		t.Errorf("report OK = true, want false (partial report)")
	}
	if rep.Verdicts["web"] != VerdictPending {
		t.Errorf("verdict = %q, want pending — a cancel is not a timeout", rep.Verdicts["web"])
	}
}

func TestWaitHealthy_PollErrorPastDeadlineTimesOut(t *testing.T) {
	// C5(c): a poll that ERRORS past the deadline yields a timed-out report (nil
	// error → exit 2) and terminates at ~deadline — the 3-strike operational-error
	// path only accumulates while INSIDE the deadline, so a slow/failing poll can
	// never overrun into an infinite loop.
	opts := WaitOptions{Timeout: 40 * time.Millisecond, Grace: time.Hour, Poll: time.Second}
	c := &blockingComposer{retErr: errors.New("ssh: connection reset")}
	start := time.Now()
	rep, err := WaitHealthy(context.Background(), c, []string{"web"}, opts)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitHealthy err = %v, want nil (deadline reached before 3-strike)", err)
	}
	if rep.Verdicts["web"] != VerdictTimedOut {
		t.Errorf("verdict = %q, want %q", rep.Verdicts["web"], VerdictTimedOut)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("wait took %v — a failing poll overran the deadline", elapsed)
	}
}
