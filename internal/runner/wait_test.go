package runner

import (
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
		{"web": {Running: false}},                     // then exited
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
