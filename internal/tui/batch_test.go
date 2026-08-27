// Tests for the multi-batch progress sequence: one runner pipeline per project,
// run in order, each gated by its own health wait. The grouped screen that
// produces the batches lives in grouped_test.go.

package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

// batchComposer records the pipeline calls one project's composer received, so
// a sequence test can assert BOTH which services each batch ran and the order
// the batches ran in. The log is shared across the sequence's composers.
type batchComposer struct {
	mockComposer
	name   string
	log    *[]string
	failOn string // runner step name to fail at; "" never fails
}

func (c *batchComposer) step(name string, containers []string) error {
	*c.log = append(*c.log, c.name+":"+name+"("+strings.Join(containers, ",")+")")
	if c.failOn == name {
		return errors.New(c.name + " " + name + " failed")
	}
	return nil
}

func (c *batchComposer) Stop(_ context.Context, containers []string, _ io.Writer) error {
	return c.step(runner.StepStopping, containers)
}
func (c *batchComposer) Remove(_ context.Context, containers []string, _ io.Writer) error {
	return c.step(runner.StepRemoving, containers)
}
func (c *batchComposer) Pull(_ context.Context, containers []string, _ io.Writer) error {
	return c.step(runner.StepPulling, containers)
}
func (c *batchComposer) Create(_ context.Context, containers []string, _ io.Writer) error {
	return c.step(runner.StepCreating, containers)
}
func (c *batchComposer) Start(_ context.Context, containers []string, _ io.Writer) error {
	return c.step(runner.StepStarting, containers)
}

// drainBatch stands in for the bubbletea loop for ONE batch: it feeds every
// event the running pipeline produced through Update, then delivers that
// batch's pipelineDoneMsg. The test reads the channel itself instead of
// invoking waitForEvent, so exactly one reader exists.
func drainBatch(t *testing.T, m Model) Model {
	t.Helper()
	return resolveWaitPhase(t, drainPipeline(t, m), true)
}

// drainPipeline runs ONLY the pipeline half of a batch, stopping at the health
// gate it arms. Tests that drive the gate by hand (esc-skip, an unhealthy
// verdict) start from here.
func drainPipeline(t *testing.T, m Model) Model {
	t.Helper()
	ch := m.eventCh
	if ch == nil {
		t.Fatal("no batch is running: eventCh is nil")
	}
	idx, session := m.batchIdx, m.batchSession
	done := make(chan []runner.StepEvent, 1)
	go func() {
		var evs []runner.StepEvent
		for ev := range ch {
			evs = append(evs, ev)
		}
		done <- evs
	}()
	var evs []runner.StepEvent
	select {
	case evs = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("batch pipeline did not finish")
	}
	for _, ev := range evs {
		updated, _ := m.Update(stepEventMsg{event: ev, batchIdx: idx, session: session})
		m = updated.(Model)
	}
	updated, _ := m.Update(pipelineDoneMsg{batchIdx: idx, session: session})
	return updated.(Model)
}

// resolveWaitPhase stands in for the bubbletea loop across ONE batch's health
// gate: it answers the poll with a status map that reports every waited service
// healthy (or unhealthy, when healthy is false) and then delivers the
// batchDoneMsg the resolution produced, which is what starts the next batch.
// A batch with no gate (StopOnly, or a wait already skipped) passes through.
func resolveWaitPhase(t *testing.T, m Model, healthy bool) Model {
	t.Helper()
	if !m.waiting {
		return m
	}
	status := map[string]runner.ServiceStatus{}
	for _, svc := range m.waitState.Services {
		status[svc] = runner.ServiceStatus{Running: true, Health: "healthy"}
		if !healthy {
			status[svc] = runner.ServiceStatus{Running: true, Health: "unhealthy"}
		}
	}
	updated, cmd := m.Update(waitStatusMsg{status: status, session: m.waitSession})
	m = updated.(Model)
	if cmd == nil {
		return m
	}
	msg := cmd()
	done, ok := msg.(batchDoneMsg)
	if !ok {
		t.Fatalf("wait resolution produced %T, want batchDoneMsg", msg)
	}
	next, _ := m.Update(done)
	return next.(Model)
}

// twoBatchModel arms a cross-project Deploy over blog/web + shop/api and
// confirms it, leaving the model on the progress screen with batch 0 running.
func twoBatchModel(t *testing.T, log *[]string, failOn map[string]string) (Model, map[string]*batchComposer) {
	t.Helper()
	g, projects := groupedFixture()
	made := map[string]*batchComposer{}
	m := groupedTestModel(g, projects)
	m.composerFactory = func(p compose.Project) runner.Composer {
		if p.Unmanaged {
			return g
		}
		if c, ok := made[p.Name]; ok {
			return c
		}
		c := &batchComposer{name: p.Name, log: log, failOn: failOn[p.Name]}
		made[p.Name] = c
		return c
	}
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	m.width, m.height = 100, 24
	m.selected["blog/web"] = true
	m.selected["shop/api"] = true

	armed, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = armed.(Model)
	if !m.confirming {
		t.Fatalf("precondition: d must arm the confirmation; warning = %q", m.warning)
	}
	running, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = running.(Model)
	if m.screen != screenProgress {
		t.Fatalf("precondition: screen = %d, want screenProgress", m.screen)
	}
	return m, made
}

// The step rows for BOTH batches exist from the start, project-prefixed so the
// five repeated names stay distinguishable, and the batches run in screen order
// with each project's own composer seeing only its own services.
func TestBatchSequence_TwoBatchesRunInOrder(t *testing.T) {
	var log []string
	m, made := twoBatchModel(t, &log, nil)

	perBatch := len(runner.Steps(runner.Deploy))
	if m.batchStepCount != perBatch {
		t.Errorf("batchStepCount = %d, want %d", m.batchStepCount, perBatch)
	}
	if len(m.steps) != 2*perBatch {
		t.Fatalf("steps = %d, want %d (two batches)", len(m.steps), 2*perBatch)
	}
	if m.steps[0].label != "blog: "+runner.StepStopping {
		t.Errorf("steps[0].label = %q, want the blog prefix", m.steps[0].label)
	}
	if m.steps[perBatch].label != "shop: "+runner.StepStopping {
		t.Errorf("steps[%d].label = %q, want the shop prefix", perBatch, m.steps[perBatch].label)
	}
	if m.steps[0].name != runner.StepStopping || m.steps[perBatch].name != runner.StepStopping {
		t.Error("name must stay the RUNNER step name in both batches — it is what a StepEvent matches")
	}

	// Batch 0 (blog). The events carry the same five names batch 1 will, so a
	// global name scan would mark blog's rows for shop's events too.
	m = drainBatch(t, m)
	if m.batchIdx != 1 {
		t.Fatalf("batchIdx = %d, want 1 (the sequence must advance)", m.batchIdx)
	}
	for i := 0; i < perBatch; i++ {
		if m.steps[i].status != runner.StatusDone {
			t.Errorf("steps[%d] (blog) status = %q, want done", i, m.steps[i].status)
		}
	}
	for i := perBatch; i < len(m.steps); i++ {
		if m.steps[i].status != "" {
			t.Errorf("steps[%d] (shop) status = %q before its batch ran, want pending", i, m.steps[i].status)
		}
	}
	if m.done {
		t.Error("done must not be set while a batch is still to run")
	}

	// Batch 1 (shop) — a FRESH events channel, because runner.Run closes the
	// one it is given.
	m = drainBatch(t, m)
	for i := range m.steps {
		if m.steps[i].status != runner.StatusDone {
			t.Errorf("steps[%d] status = %q, want done after both batches", i, m.steps[i].status)
		}
	}
	if !m.done {
		t.Error("done must be set once the last batch finishes")
	}

	want := []string{
		"blog:Stopping(web)", "blog:Removing(web)", "blog:Pulling(web)", "blog:Creating(web)", "blog:Starting(web)",
		"shop:Stopping(api)", "shop:Removing(api)", "shop:Pulling(api)", "shop:Creating(api)", "shop:Starting(api)",
	}
	if !slices.Equal(log, want) {
		t.Errorf("pipeline calls =\n%v\nwant\n%v", log, want)
	}
	if len(made) != 2 {
		t.Errorf("factory built %d composers, want one per batch", len(made))
	}
	// The wait phase seeds from the LAST batch's own services, in bare form.
	if !slices.Equal(m.waitState.Services, []string{"api"}) {
		t.Errorf("waitState.Services = %v, want [api] (the last batch)", m.waitState.Services)
	}
}

// A failed batch stops the sequence: the batches behind it never run and say so
// on screen rather than sitting at ○, which reads as "still to come".
func TestBatchSequence_FailureStopsSequence(t *testing.T) {
	var log []string
	m, made := twoBatchModel(t, &log, map[string]string{"blog": runner.StepRemoving})

	perBatch := len(runner.Steps(runner.Deploy))
	m = drainBatch(t, m)

	if !m.failed {
		t.Fatal("a failed step must mark the operation failed")
	}
	if m.batchIdx != 0 {
		t.Errorf("batchIdx = %d, want 0 — a failed batch must not advance", m.batchIdx)
	}
	if m.done {
		t.Error("done must never be set after a failure")
	}
	if m.steps[1].status != runner.StatusFailed {
		t.Errorf("steps[1] status = %q, want failed", m.steps[1].status)
	}
	// The failed batch's unreached steps stay pending (unchanged behaviour);
	// the batches behind it are skipped.
	for i := 2; i < perBatch; i++ {
		if m.steps[i].status != "" {
			t.Errorf("steps[%d] status = %q, want pending inside the failed batch", i, m.steps[i].status)
		}
	}
	for i := perBatch; i < len(m.steps); i++ {
		if m.steps[i].status != statusSkipped {
			t.Errorf("steps[%d] status = %q, want %q", i, m.steps[i].status, statusSkipped)
		}
	}
	if _, ok := made["shop"]; ok {
		t.Error("the second batch's composer must never be built after a failure")
	}
	want := []string{"blog:Stopping(web)", "blog:Removing(web)"}
	if !slices.Equal(log, want) {
		t.Errorf("pipeline calls = %v, want %v", log, want)
	}
	if view := m.viewProgress(); !strings.Contains(view, "(skipped)") {
		t.Errorf("the progress screen must name the skipped rows, got:\n%s", view)
	}
}

// esc cancels the running batch. The pipelineDoneMsg it already queued must NOT
// start the next one — that is what the sequence session gates.
func TestBatchSequence_EscDoesNotAdvance(t *testing.T) {
	var log []string
	m, made := twoBatchModel(t, &log, nil)

	idx, session := m.batchIdx, m.batchSession
	perBatch := len(runner.Steps(runner.Deploy))

	cancelled, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = cancelled.(Model)
	if m.screen != screenProgress {
		t.Fatalf("esc mid-op must stay on the progress screen, got screen %d", m.screen)
	}
	if m.batchSession == session {
		t.Error("esc must bump batchSession so the queued done message cannot advance")
	}
	for i := perBatch; i < len(m.steps); i++ {
		if m.steps[i].status != statusSkipped {
			t.Errorf("steps[%d] status = %q, want %q after a cancel", i, m.steps[i].status, statusSkipped)
		}
	}

	// The message the cancelled batch queued lands now.
	stale, cmd := m.Update(pipelineDoneMsg{batchIdx: idx, session: session})
	m = stale.(Model)
	if m.batchIdx != 0 {
		t.Errorf("batchIdx = %d, want 0 — a cancelled sequence must not advance", m.batchIdx)
	}
	if m.done {
		t.Error("a stale done message must not resolve the operation")
	}
	if cmd != nil {
		t.Error("a stale done message must produce no command")
	}
	if _, ok := made["shop"]; ok {
		t.Error("the second batch must never start after a cancel")
	}
}

// A done message from an earlier batch of the SAME sequence is stale too: only
// the batch now running may report itself finished.
func TestBatchSequence_RejectsStaleBatchIndex(t *testing.T) {
	var log []string
	m, _ := twoBatchModel(t, &log, nil)
	m = drainBatch(t, m) // batch 0 done, batch 1 running

	replayed, cmd := m.Update(pipelineDoneMsg{batchIdx: 0, session: m.batchSession})
	got := replayed.(Model)
	if got.batchIdx != 1 {
		t.Errorf("batchIdx = %d, want 1 — a replayed batch-0 message must be dropped", got.batchIdx)
	}
	if cmd != nil {
		t.Error("a replayed done message must produce no command")
	}
	// Off-screen delivery is dropped as well.
	m.screen = screenSelectContainers
	offScreen, cmd := m.Update(pipelineDoneMsg{batchIdx: 1, session: m.batchSession})
	if got := offScreen.(Model); got.done {
		t.Error("a done message that arrives off the progress screen must be dropped")
	}
	if cmd != nil {
		t.Error("an off-screen done message must produce no command")
	}
}

// One project is still one batch, and it must render and behave exactly as it
// did before the sequence existed: bare step labels, no skipped rows.
func TestBatchSequence_SingleBatchEquivalence(t *testing.T) {
	var log []string
	mc := &batchComposer{name: "app", log: &log, failOn: runner.StepRemoving}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.projName, m.projDir = "app", "/srv/app"
	m.setSingleGroup([]string{"web"})
	m.composer = mc
	m.selected[m.svcKeyAt(0)] = true
	m.width, m.height = 80, 24

	armed, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = armed.(Model)
	if !m.confirming {
		t.Fatal("precondition: d must arm the confirmation on the drilled screen")
	}
	running, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = running.(Model)

	if len(m.batches) != 1 || m.batches[0].proj.Name != "app" {
		t.Fatalf("batches = %+v, want one app batch", m.batches)
	}
	if len(m.steps) != len(runner.Steps(runner.Deploy)) {
		t.Fatalf("steps = %d, want one batch's worth", len(m.steps))
	}
	for i, s := range m.steps {
		if s.label != s.name {
			t.Errorf("steps[%d].label = %q, want the bare name %q for a single batch", i, s.label, s.name)
		}
	}

	m = drainBatch(t, m)
	if !m.failed {
		t.Fatal("the failing step must mark the operation failed")
	}
	for i := 2; i < len(m.steps); i++ {
		if m.steps[i].status != "" {
			t.Errorf("steps[%d] status = %q, want pending — a single batch has nothing to skip", i, m.steps[i].status)
		}
	}
	if view := m.viewProgress(); strings.Contains(view, "(skipped)") {
		t.Errorf("a single-batch failure must render exactly as before, got:\n%s", view)
	}
}

// Leaving the progress screen drops the sequence and invalidates any done
// message still in flight.
func TestBatchSequence_DepartureClearsSequence(t *testing.T) {
	var log []string
	m, _ := twoBatchModel(t, &log, nil)
	m = drainBatch(t, m)
	m = drainBatch(t, m)
	if !m.done {
		t.Fatal("precondition: both batches must finish")
	}
	session := m.batchSession

	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // first esc skips the wait
	m = back.(Model)
	back, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // second esc leaves the screen
	m = back.(Model)

	if m.screen != screenSelectContainers {
		t.Fatalf("screen = %d, want screenSelectContainers", m.screen)
	}
	if m.batches != nil || m.batchIdx != 0 || m.batchStepCount != 0 {
		t.Errorf("batch state survived the departure: %+v idx=%d count=%d", m.batches, m.batchIdx, m.batchStepCount)
	}
	if m.batchSession == session {
		t.Error("the departure must bump batchSession")
	}
}

// A sequence touches one project per batch, and each keeps its own update-cache
// entry — the current key names at most one of them.
func TestBatchSequence_InvalidatesEveryBatchCacheKey(t *testing.T) {
	var log []string
	m, _ := twoBatchModel(t, &log, nil)
	m.serverName = "prod"
	m.updateCache = map[string]updateEntry{
		"/srv/blog|prod":  {fetchedAt: time.Now(), results: map[string]bool{"web": true}},
		"/srv/shop|prod":  {fetchedAt: time.Now(), results: map[string]bool{"api": true}},
		"/srv/other|prod": {fetchedAt: time.Now(), results: map[string]bool{"x": true}},
	}
	m = drainBatch(t, m)
	m = drainBatch(t, m)

	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // skip the wait
	m = back.(Model)
	back, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // leave the screen
	m = back.(Model)

	for _, key := range []string{"/srv/blog|prod", "/srv/shop|prod"} {
		if _, ok := m.updateCache[key]; ok {
			t.Errorf("cache entry %q survived a successful deploy of that project", key)
		}
	}
	if _, ok := m.updateCache["/srv/other|prod"]; !ok {
		t.Error("a project the sequence never touched must keep its cache entry")
	}
}

// The progress title names every batch, because a bare service list cannot say
// which "api" a two-project sequence means.
func TestBatchSequence_ProgressTitleNamesEveryBatch(t *testing.T) {
	var log []string
	m, _ := twoBatchModel(t, &log, nil)
	if view := m.viewProgress(); !strings.Contains(view, "blog (web) → shop (api)") {
		t.Errorf("title must name both batches, got:\n%s", view)
	}
}

// --- Task 11: per-batch wait phase ------------------------------------------

// Every batch has its OWN health gate, seeded from that batch's own services in
// bare form. Batch i+1 must not start until batch i is healthy.
func TestBatchWait_EachBatchWaitsOnItsOwnServices(t *testing.T) {
	var log []string
	m, made := twoBatchModel(t, &log, nil)

	m = drainPipeline(t, m)
	if !m.waiting {
		t.Fatal("batch 0's pipeline must arm a health gate before the next batch starts")
	}
	if m.batchIdx != 0 {
		t.Errorf("batchIdx = %d, want 0 — the gate, not the pipeline, releases the next batch", m.batchIdx)
	}
	if !slices.Equal(m.waitState.Services, []string{"web"}) {
		t.Errorf("waitState.Services = %v, want [web] — batch 0's own services", m.waitState.Services)
	}
	if m.waitDeadline.IsZero() {
		t.Error("waitDeadline must be set for a mid-sequence gate too")
	}
	if _, ok := made["shop"]; ok {
		t.Error("the next batch's composer must not be built while the gate is still open")
	}
	if m.done {
		t.Error("done must not be set while a batch is still to run")
	}

	m = resolveWaitPhase(t, m, true)
	if m.batchIdx != 1 {
		t.Fatalf("batchIdx = %d, want 1 — a passing gate releases the next batch", m.batchIdx)
	}
	if m.waiting || len(m.waitState.Services) != 0 {
		t.Errorf("the finished batch's verdicts must be dropped before the next one arms its own: waiting=%v services=%v", m.waiting, m.waitState.Services)
	}
	if _, ok := made["shop"]; !ok {
		t.Error("batch 1 must start once batch 0's gate passed")
	}

	m = drainBatch(t, m)
	if !slices.Equal(m.waitState.Services, []string{"api"}) {
		t.Errorf("waitState.Services = %v, want [api] — batch 1's own services", m.waitState.Services)
	}
	if !m.done {
		t.Error("done must be set once the last batch finishes")
	}
}

// An unhealthy batch stops the sequence exactly as a failed pipeline step does:
// the batches behind it never run and say "(skipped)" on screen.
func TestBatchWait_FailureStopsSequence(t *testing.T) {
	var log []string
	m, made := twoBatchModel(t, &log, nil)

	perBatch := len(runner.Steps(runner.Deploy))
	m = drainPipeline(t, m)
	m = resolveWaitPhase(t, m, false)

	if !m.failed {
		t.Fatal("an unhealthy mid-sequence batch must mark the operation failed")
	}
	if m.batchIdx != 0 {
		t.Errorf("batchIdx = %d, want 0 — a failed gate must not advance", m.batchIdx)
	}
	if m.done {
		t.Error("done must never be set when the sequence stopped early")
	}
	if _, ok := made["shop"]; ok {
		t.Error("the second batch must never start behind a failed gate")
	}
	for i := perBatch; i < len(m.steps); i++ {
		if m.steps[i].status != statusSkipped {
			t.Errorf("steps[%d] status = %q, want %q", i, m.steps[i].status, statusSkipped)
		}
	}
	if view := m.viewProgress(); !strings.Contains(view, "(skipped)") {
		t.Errorf("the progress screen must name the skipped rows, got:\n%s", view)
	}
	// The verdicts stay on screen so the user can see WHICH service failed.
	if m.waitState.Verdicts["web"].OK() {
		t.Error("the failing verdict must survive for rendering")
	}
	want := []string{"blog:Stopping(web)", "blog:Removing(web)", "blog:Pulling(web)", "blog:Creating(web)", "blog:Starting(web)"}
	if !slices.Equal(log, want) {
		t.Errorf("pipeline calls = %v, want only batch 0's", log)
	}
}

// The LAST batch's failing gate is NOT a failed operation — that is what the
// single-project screen has always drawn (red verdicts plus the rollback hint).
func TestBatchWait_LastBatchFailureKeepsDoneState(t *testing.T) {
	var log []string
	m, _ := twoBatchModel(t, &log, nil)
	m = drainBatch(t, m)    // batch 0 through its gate
	m = drainPipeline(t, m) // batch 1's pipeline
	m = resolveWaitPhase(t, m, false)

	if m.failed {
		t.Error("a failing gate on the LAST batch must not mark the operation failed")
	}
	if !m.done {
		t.Error("the last batch's pipeline resolves the operation regardless of its verdicts")
	}
	if view := m.viewProgress(); !strings.Contains(view, "press R on the services screen to roll back") {
		t.Errorf("the rollback hint must still render, got:\n%s", view)
	}
}

// A whole-project batch carries an EMPTY target slice (the runner reads it as
// "all services"), which the reducer cannot evaluate — ListServices names them.
func TestBatchWait_WholeProjectBatchResolvesTargetsViaListServices(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "db"}}
	m := Model{
		screen:    screenProgress,
		grouped:   true,
		pendingOp: runner.Deploy,
		composer:  mc,
		ctx:       context.Background(),
		batches:   []opBatch{{proj: compose.Project{Name: "shop", ConfigDir: "/srv/shop"}}},
		// Host-wide rows in grouped mode, so they must NEVER seed one
		// project's gate: drilledServices() answers nil for more than one
		// group, which is what forces the ListServices round-trip.
		svcGroups: []svcGroup{
			svcGroupOf("shop", "api", "db"),
			svcGroupOf("blog", "web"),
		},
	}

	updated, cmd := m.Update(pipelineDoneMsg{})
	m = updated.(Model)
	if !m.waiting {
		t.Fatal("a whole-project batch must still open a health gate")
	}
	if len(m.waitState.Services) != 0 {
		t.Errorf("waitState.Services = %v, want empty until ListServices answers", m.waitState.Services)
	}
	if cmd == nil {
		t.Fatal("expected a target-resolution cmd")
	}
	targets, ok := cmd().(waitTargetsMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want waitTargetsMsg", cmd())
	}
	if !slices.Equal(targets.targets, []string{"api", "db"}) {
		t.Errorf("targets = %v, want the batch composer's own service list", targets.targets)
	}

	seeded, cmd := m.Update(targets)
	m = seeded.(Model)
	if !slices.Equal(m.waitState.Services, []string{"api", "db"}) {
		t.Errorf("waitState.Services = %v, want [api db]", m.waitState.Services)
	}
	if cmd == nil {
		t.Error("expected the first poll to be dispatched once the targets landed")
	}
}

// A resolution that fails (or names nothing) skips the gate rather than
// stalling the sequence on it.
func TestBatchWait_UnresolvableTargetsSkipTheGate(t *testing.T) {
	tests := []struct {
		name string
		msg  waitTargetsMsg
	}{
		{"list error", waitTargetsMsg{err: errors.New("boom")}},
		{"no services", waitTargetsMsg{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				screen:      screenProgress,
				grouped:     true,
				pendingOp:   runner.Deploy,
				waiting:     true,
				waitSession: 3,
				batches:     []opBatch{{proj: compose.Project{Name: "shop"}}},
			}
			tt.msg.session = 3
			updated, cmd := m.Update(tt.msg)
			got := updated.(Model)
			if got.waiting {
				t.Error("an unresolvable target set must close the gate")
			}
			if got.failed {
				t.Error("failing to NAME the services is not a health failure")
			}
			if cmd != nil {
				t.Error("a single-batch sequence has nothing left to release")
			}
		})
	}
}

// A stale waitTargetsMsg (skipped gate, departed screen, superseded session)
// must not seed a reducer that has moved on.
func TestBatchWait_StaleTargetsRejected(t *testing.T) {
	base := Model{screen: screenProgress, pendingOp: runner.Deploy, waiting: true, waitSession: 4}
	cases := []struct {
		name string
		m    Model
		msg  waitTargetsMsg
	}{
		{"stale session", base, waitTargetsMsg{targets: []string{"web"}, session: 3}},
		{"gate already skipped", func() Model { m := base; m.waiting = false; return m }(), waitTargetsMsg{targets: []string{"web"}, session: 4}},
		{"off screen", func() Model { m := base; m.screen = screenSelectContainers; return m }(), waitTargetsMsg{targets: []string{"web"}, session: 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, cmd := tc.m.Update(tc.msg)
			if got := updated.(Model); len(got.waitState.Services) != 0 {
				t.Errorf("waitState seeded from a stale message: %v", got.waitState.Services)
			}
			if cmd != nil {
				t.Error("a stale message must produce no command")
			}
		})
	}
}

// esc on a MID-SEQUENCE health gate STOPS the sequence. The gate is where a
// multi-project deploy spends most of its wall clock, so it is where a user who
// wants to abort is sitting — and esc is the abort key everywhere else on this
// screen. Releasing the next batch from it made the universal abort key launch
// a whole project's stop → rm → pull → create → start.
func TestBatchWait_EscStopsTheSequence(t *testing.T) {
	var log []string
	m, made := twoBatchModel(t, &log, nil)
	m = drainPipeline(t, m)
	if !m.waiting {
		t.Fatal("precondition: batch 0 must be waiting")
	}
	perBatch := m.batchStepCount

	stopped, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = stopped.(Model)
	if m.waiting {
		t.Error("esc must close the gate")
	}
	if m.screen != screenProgress {
		t.Errorf("screen = %d, want screenProgress — the stop resolves the screen, it does not navigate", m.screen)
	}
	if cmd != nil {
		t.Fatalf("esc at a mid-sequence gate must release NOTHING, got %T", cmd())
	}
	if !m.failed {
		t.Error("a stopped sequence needs a terminal state so the next esc navigates back")
	}
	if _, ok := made["shop"]; ok {
		t.Error("batch 1 must NOT start after esc")
	}
	for i := perBatch; i < len(m.steps); i++ {
		if m.steps[i].status != statusSkipped {
			t.Errorf("steps[%d].status = %q, want %q — the batches behind the stop say so", i, m.steps[i].status, statusSkipped)
		}
	}
	// A batchDoneMsg already on its way is discarded by the session bump.
	stale, _ := m.Update(batchDoneMsg{batchIdx: 0, session: 0})
	if stale.(Model).batchIdx != 0 {
		t.Error("a stopped sequence must not advance on a stale batchDoneMsg")
	}
	// And the next esc leaves the screen.
	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if back.(Model).screen != screenSelectContainers {
		t.Error("the second esc must leave the progress screen")
	}
}

// enter is the key that carries the old "skip the wait and carry on" meaning:
// mid-sequence it releases the next batch.
func TestBatchWait_EnterReleasesTheNextBatch(t *testing.T) {
	var log []string
	m, made := twoBatchModel(t, &log, nil)
	m = drainPipeline(t, m)
	if !m.waiting {
		t.Fatal("precondition: batch 0 must be waiting")
	}

	skipped, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = skipped.(Model)
	if m.waiting {
		t.Error("enter must close the gate")
	}
	if m.failed {
		t.Error("enter is a skip, not a stop — it must not mark the operation failed")
	}
	if m.screen != screenProgress {
		t.Errorf("screen = %d, want screenProgress — enter on the gate is a skip, not a back-nav", m.screen)
	}
	if cmd == nil {
		t.Fatal("a skipped mid-sequence gate must release the next batch")
	}
	done, ok := cmd().(batchDoneMsg)
	if !ok {
		t.Fatalf("enter produced %T, want batchDoneMsg", cmd())
	}
	advanced, _ := m.Update(done)
	m = advanced.(Model)
	if m.batchIdx != 1 {
		t.Errorf("batchIdx = %d, want 1", m.batchIdx)
	}
	if _, ok := made["shop"]; !ok {
		t.Error("batch 1 must start after the skip")
	}
}

// esc on the LAST batch's gate is a plain skip — there is nothing to release,
// and a second esc still leaves the screen.
func TestBatchWait_EscSkipOnLastBatchReleasesNothing(t *testing.T) {
	var log []string
	m, _ := twoBatchModel(t, &log, nil)
	m = drainBatch(t, m)
	m = drainPipeline(t, m)
	if !m.waiting || !m.done {
		t.Fatalf("precondition: last batch waiting=%v done=%v", m.waiting, m.done)
	}

	skipped, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = skipped.(Model)
	if cmd != nil {
		t.Error("the last batch's skipped gate has no successor to release")
	}
	if m.screen != screenProgress {
		t.Errorf("screen = %d, want screenProgress on the first esc", m.screen)
	}
	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := back.(Model); got.screen != screenSelectContainers {
		t.Errorf("screen = %d, want the container screen on the second esc", got.screen)
	}
}

// A batchDoneMsg is gated on screen, sequence and index — a message from a
// cancelled or already-advanced sequence can never start a successor.
func TestBatchWait_StaleBatchDoneRejected(t *testing.T) {
	var log []string
	m, made := twoBatchModel(t, &log, nil)
	m = drainPipeline(t, m)
	idx, session := m.batchIdx, m.batchSession

	cancelled, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // skips the gate
	m = cancelled.(Model)
	cancelled, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // cancels the sequence
	m = cancelled.(Model)
	if m.batchSession == session {
		t.Fatal("precondition: the cancel must bump batchSession")
	}

	stale, cmd := m.Update(batchDoneMsg{batchIdx: idx, session: session})
	m = stale.(Model)
	if m.batchIdx != 0 {
		t.Errorf("batchIdx = %d, want 0 — a cancelled sequence must not advance", m.batchIdx)
	}
	if cmd != nil {
		t.Error("a stale batchDoneMsg must produce no command")
	}
	if _, ok := made["shop"]; ok {
		t.Error("the second batch must never start after a cancel")
	}

	// Wrong index and off-screen delivery are dropped too.
	fresh, cmd := m.Update(batchDoneMsg{batchIdx: 5, session: m.batchSession})
	if got := fresh.(Model); got.batchIdx != 0 {
		t.Errorf("batchIdx = %d, want 0 for a wrong-index message", got.batchIdx)
	}
	if cmd != nil {
		t.Error("a wrong-index batchDoneMsg must produce no command")
	}
	m.screen = screenSelectContainers
	offScreen, cmd := m.Update(batchDoneMsg{batchIdx: 0, session: m.batchSession})
	if got := offScreen.(Model); got.batchIdx != 0 {
		t.Errorf("batchIdx = %d, want 0 off screen", got.batchIdx)
	}
	if cmd != nil {
		t.Error("an off-screen batchDoneMsg must produce no command")
	}
}

// StopOnly never opens a health gate, in a sequence as in a single batch: the
// pipeline itself releases the next batch.
func TestBatchWait_StopOnlyAdvancesWithoutAGate(t *testing.T) {
	m := Model{
		screen:    screenProgress,
		grouped:   true,
		pendingOp: runner.StopOnly,
		batches: []opBatch{
			{proj: compose.Project{Name: "blog"}, services: []string{"web"}},
			{proj: compose.Project{Name: "shop"}, services: []string{"api"}},
		},
		opContainers: []string{"web"},
	}

	updated, cmd := m.Update(pipelineDoneMsg{})
	m = updated.(Model)
	if m.waiting {
		t.Error("StopOnly must never enter the health-wait phase")
	}
	if m.done {
		t.Error("done must not be set while a batch is still to run")
	}
	if cmd == nil {
		t.Fatal("StopOnly must still release the next batch")
	}
	if _, ok := cmd().(batchDoneMsg); !ok {
		t.Fatalf("cmd produced %T, want batchDoneMsg", cmd())
	}
}

// Leaving screenProgress runs the rollback-prep cleanup and drops BOTH the wait
// sub-state and the batch bookkeeping — on the main goroutine, never deferred.
func TestBatchWait_DepartureRunsCleanupAndClearsWait(t *testing.T) {
	var log []string
	m, _ := twoBatchModel(t, &log, nil)
	m = drainBatch(t, m)
	m = drainPipeline(t, m) // last batch waiting

	cleaned := 0
	m.rollbackCleanup = func() { cleaned++ }
	waitSession, batchSession := m.waitSession, m.batchSession

	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // skip the gate
	m = back.(Model)
	back, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // leave the screen
	m = back.(Model)

	if m.screen != screenSelectContainers {
		t.Fatalf("screen = %d, want screenSelectContainers", m.screen)
	}
	if cleaned != 1 {
		t.Errorf("rollback cleanup ran %d times, want exactly 1", cleaned)
	}
	if m.rollbackCleanup != nil {
		t.Error("the cleanup must be cleared so a later departure cannot double-invoke it")
	}
	if m.waiting || len(m.waitState.Services) != 0 || !m.waitDeadline.IsZero() || m.opContainers != nil {
		t.Errorf("wait state survived the departure: waiting=%v state=%v deadline=%v targets=%v",
			m.waiting, m.waitState.Services, m.waitDeadline, m.opContainers)
	}
	if m.waitSession == waitSession {
		t.Error("the departure must bump waitSession so a straggling poll is dropped")
	}
	if m.batches != nil || m.batchIdx != 0 || m.batchStepCount != 0 {
		t.Errorf("batch state survived the departure: %+v idx=%d count=%d", m.batches, m.batchIdx, m.batchStepCount)
	}
	if m.batchSession == batchSession {
		t.Error("the departure must bump batchSession")
	}
}

// ---------------------------------------------------------------------------
// Grouped host view (Task 12): updates.
// ---------------------------------------------------------------------------

// TestEscCancel_LeavesATerminalState is the hang pin. esc on a running pipeline
// bumps batchSession, which invalidates the pipelineDoneMsg the closing channel
// produces — the one message that used to set m.done. Without a terminal state
// set HERE, a cancel that lands after the last step succeeded leaves
// done == false && failed == false, and then esc re-cancels, q is a no-op and
// ctrl+c falls through to the viewport: the TUI has no exit.
func TestEscCancel_LeavesATerminalState(t *testing.T) {
	cancelled := false
	m := Model{
		screen:         screenProgress,
		pendingOp:      runner.Deploy,
		ctx:            context.Background(),
		batchStepCount: 1,
		batches:        []opBatch{{proj: compose.Project{Name: "shop", ConfigDir: "/srv/shop"}}},
		steps:          []stepState{progressStep(runner.StepStopping, runner.StatusRunning)},
		cancel:         func() { cancelled = true },
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)

	if !cancelled {
		t.Error("esc must cancel the running batch context")
	}
	if !got.failed {
		t.Fatal("esc-cancel must resolve the screen: done and failed both false is an unescapable state")
	}
	// A second esc now navigates back, which is the whole point.
	back, _ := got.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if back.(Model).screen != screenSelectContainers {
		t.Error("the second esc must leave the progress screen")
	}
	// And the sequence is still stopped: the stale done message cannot advance.
	stale, _ := got.Update(pipelineDoneMsg{batchIdx: 0, session: 0})
	if stale.(Model).batchIdx != 0 {
		t.Error("a cancelled batch must not advance the sequence")
	}
}

// A mid-sequence health gate opens with done AND failed both false, because
// pipelineDoneMsg sets done only for the LAST batch. q, enter and ctrl+c must
// still work there — the `?` overlay's WAIT group names all three.
func TestProgressWait_QAndCtrlCAreLiveMidSequence(t *testing.T) {
	newModel := func() Model {
		return Model{
			screen:         screenProgress,
			pendingOp:      runner.Deploy,
			ctx:            context.Background(),
			waiting:        true,
			batchIdx:       0,
			batchStepCount: 1,
			batches: []opBatch{
				{proj: compose.Project{Name: "blog", ConfigDir: "/srv/blog"}},
				{proj: compose.Project{Name: "shop", ConfigDir: "/srv/shop"}},
			},
			steps: []stepState{
				progressStep(runner.StepStopping, runner.StatusDone),
				progressStep(runner.StepStopping, ""),
			},
			waitState: runner.NewWaitState([]string{"api"}),
		}
	}

	m := newModel()
	if m.done || m.failed {
		t.Fatal("precondition: a mid-sequence gate carries neither terminal flag")
	}
	if m.progressPhase() != progressWaiting {
		t.Fatal("precondition: the phase must resolve to waiting")
	}

	// q rewrites to esc, which closes the gate and STOPS the sequence.
	stopped, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	got := stopped.(Model)
	if got.waiting {
		t.Error("q must close the health gate, not be swallowed")
	}
	if cmd != nil {
		t.Fatalf("q at a mid-sequence gate must release nothing, got %T", cmd())
	}
	if !got.failed {
		t.Error("q must resolve the screen so the next press navigates back")
	}

	// enter is the key that releases the next batch there.
	released, ecmd := newModel().Update(tea.KeyMsg{Type: tea.KeyEnter})
	if released.(Model).waiting {
		t.Error("enter must close the health gate")
	}
	if ecmd == nil {
		t.Fatal("enter at a mid-sequence gate must release the next batch")
	}
	if _, ok := ecmd().(batchDoneMsg); !ok {
		t.Errorf("cmd produced %T, want batchDoneMsg", ecmd())
	}

	// ctrl+c quits (no remote connection here, so tryQuit returns tea.Quit).
	_, qcmd := newModel().Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if qcmd == nil {
		t.Fatal("ctrl+c must quit from an open health gate")
	}
	if _, ok := qcmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c produced %T, want tea.QuitMsg", qcmd())
	}
}

// A running pipeline still swallows both keys — that half must not regress.
func TestProgressRunning_QAndCtrlCStayInert(t *testing.T) {
	m := Model{
		screen:         screenProgress,
		pendingOp:      runner.Deploy,
		ctx:            context.Background(),
		batchStepCount: 1,
		batches:        []opBatch{{proj: compose.Project{Name: "shop"}}},
		steps:          []stepState{progressStep(runner.StepStopping, runner.StatusRunning)},
	}
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyCtrlC},
	} {
		got, cmd := m.Update(k)
		if got.(Model).screen != screenProgress || cmd != nil {
			t.Errorf("%v must be a no-op while a pipeline runs", k)
		}
	}
}

// TestProgressWindow_KeepsTheRunningBatchOnScreen pins the windowing rule: a
// multi-batch plan emits more step rows than a short terminal holds, and
// bubbletea keeps only the LAST m.height lines, so an unwindowed render drops
// the title and the batch that is actually running.
func TestProgressWindow_KeepsTheRunningBatchOnScreen(t *testing.T) {
	batches := make([]opBatch, 0, 5)
	var steps []stepState
	for _, name := range []string{"p0", "p1", "p2", "p3", "p4"} {
		batches = append(batches, opBatch{proj: compose.Project{Name: name, ConfigDir: "/srv/" + name}})
		for _, step := range runner.Steps(runner.Deploy) {
			steps = append(steps, stepState{name: step, label: name + ": " + step})
		}
	}
	m := Model{
		screen:         screenProgress,
		pendingOp:      runner.Deploy,
		ctx:            context.Background(),
		batches:        batches,
		batchStepCount: len(runner.Steps(runner.Deploy)),
		batchIdx:       4, // the LAST project is running
		steps:          steps,
		width:          100,
		height:         24,
	}

	out := ansi.Strip(m.viewProgress())
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > m.height {
		t.Errorf("viewProgress rendered %d lines into a %d-row terminal:\n%s", len(lines), m.height, out)
	}
	if !strings.Contains(lines[0], "Deploy") {
		t.Errorf("the title must survive the window, first line = %q", lines[0])
	}
	if !strings.Contains(out, "p4: ") {
		t.Errorf("the running batch must stay on screen:\n%s", out)
	}
	if !strings.Contains(out, "more") {
		t.Errorf("a windowed list must say how much is hidden:\n%s", out)
	}
}

// With room to spare, nothing is windowed and no marker is drawn.
func TestProgressWindow_NoMarkersWhenEverythingFits(t *testing.T) {
	m := Model{
		screen:         screenProgress,
		pendingOp:      runner.Restart,
		ctx:            context.Background(),
		batches:        []opBatch{{proj: compose.Project{Name: "shop"}}},
		batchStepCount: 2,
		steps: []stepState{
			progressStep("Stop", runner.StatusDone),
			progressStep("Start", runner.StatusDone),
		},
		done:   true,
		width:  100,
		height: 40,
	}
	if out := ansi.Strip(m.viewProgress()); strings.Contains(out, "more") {
		t.Errorf("nothing should be hidden at height 40:\n%s", out)
	}
}

func TestProgressStepWindow(t *testing.T) {
	tests := []struct {
		name               string
		total, rows, hi    int
		wantStart, wantEnd int
	}{
		{"everything fits", 10, 10, 5, 0, 10},
		{"budget larger than the plan", 4, 9, 4, 0, 4},
		{"zero budget renders everything", 4, 0, 4, 0, 4},
		{"first batch anchors at the top", 25, 10, 5, 0, 10},
		{"last batch scrolls into view", 25, 10, 25, 15, 25},
		// A batch taller than the budget anchors on its TAIL: the running
		// batch's LAST row is the last one drawn, so the step actually running
		// is on screen instead of scrolled off below the window. The old rule
		// pulled start back to the batch's FIRST row and drew (20, 23).
		{"a batch taller than the budget shows its tail", 25, 3, 25, 22, 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := progressStepWindow(tt.total, tt.rows, tt.hi)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("progressStepWindow(%d,%d,%d) = (%d,%d), want (%d,%d)",
					tt.total, tt.rows, tt.hi, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

// A sequence where batch 0 deployed and batch 1 failed DID change batch 0's
// image freshness. Keying the invalidation off m.done — which only the LAST
// batch sets — left those glyphs stale for the full 10-minute TTL.
func TestEscFromProgress_InvalidatesTheBatchesThatFinished(t *testing.T) {
	blog := compose.Project{Name: "blog", ConfigDir: "/srv/blog"}
	shop := compose.Project{Name: "shop", ConfigDir: "/srv/shop"}
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenProgress
	m.composer = mc
	m.pendingOp = runner.Deploy
	m.batches = []opBatch{{proj: blog}, {proj: shop}}
	m.batchIdx = 1
	m.failed = true // batch 1 failed; batch 0 finished
	m.updateCache = map[string]updateEntry{
		m.projUpdatesCacheKey(blog): {fetchedAt: time.Now(), results: map[string]bool{"web": true}},
		m.projUpdatesCacheKey(shop): {fetchedAt: time.Now(), results: map[string]bool{"api": true}},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)

	if _, present := got.updateCache[got.projUpdatesCacheKey(blog)]; present {
		t.Error("the batch that finished must have its verdicts invalidated")
	}
	if _, present := got.updateCache[got.projUpdatesCacheKey(shop)]; !present {
		t.Error("the batch that FAILED must keep its cache: a failed deploy changed nothing")
	}
}

// startBatch's bind-failure path: a factory that cannot produce a composer for
// batch i must fail the sequence and mark the rest skipped, never advance it.
func TestStartBatch_BindFailureStopsTheSequence(t *testing.T) {
	m := Model{
		screen:    screenSelectContainers,
		grouped:   true,
		ctx:       context.Background(),
		pendingOp: runner.Deploy,
		logWriter: io.Discard,
		composerFactory: func(p compose.Project) runner.Composer {
			if p.Name == "shop" {
				return nil // the factory refuses the second project
			}
			return &mockComposer{}
		},
	}
	batches := []opBatch{
		{proj: compose.Project{Name: "blog", ConfigDir: "/srv/blog"}},
		{proj: compose.Project{Name: "shop", ConfigDir: "/srv/shop"}},
	}
	updated, _ := m.enterProgress(batches)
	got := updated.(Model)
	got.batchIdx = 1
	cmd := got.startBatch()

	if cmd != nil {
		t.Error("a failed bind must launch no pipeline")
	}
	if !got.failed {
		t.Fatal("a failed bind must fail the operation")
	}
	stepsPerBatch := len(runner.Steps(runner.Deploy))
	for i := stepsPerBatch; i < len(got.steps); i++ {
		if got.steps[i].status != statusSkipped {
			t.Fatalf("step %d status = %q, want skipped", i, got.steps[i].status)
		}
	}
}

// handleStepEvent latches m.failed on any StatusFailed. The esc-cancel marks the
// screen terminal immediately, so the second esc leaves while the cancelled
// step's failure event is still in flight — and ungated it poisoned the NEXT
// operation: pipelineDoneMsg returns early on m.failed, so the health gate never
// opened and no batch past the first ever ran.
func TestStepEvent_StaleEventNeverLatchesFailed(t *testing.T) {
	ev := runner.StepEvent{Step: runner.StepPulling, Status: runner.StatusFailed}

	t.Run("off the progress screen", func(t *testing.T) {
		m := groupedScreenModel(svcGroupOf("shop", "api"))
		updated, _ := m.Update(stepEventMsg{event: ev})
		if updated.(Model).failed {
			t.Error("a step event that lands on the container screen must be dropped")
		}
	})

	t.Run("superseded sequence", func(t *testing.T) {
		m := Model{screen: screenProgress, batchSession: 4, batchStepCount: 1,
			steps: []stepState{{name: runner.StepPulling}}}
		updated, _ := m.Update(stepEventMsg{event: ev, session: 3})
		got := updated.(Model)
		if got.failed {
			t.Error("an event from a cancelled sequence must not fail the new one")
		}
		if got.steps[0].status != "" {
			t.Errorf("steps[0] = %q, want untouched", got.steps[0].status)
		}
	})

	t.Run("previous batch", func(t *testing.T) {
		m := Model{screen: screenProgress, batchIdx: 1, batchStepCount: 1,
			steps: []stepState{{name: runner.StepPulling}, {name: runner.StepPulling}}}
		updated, _ := m.Update(stepEventMsg{event: ev, batchIdx: 0})
		if updated.(Model).failed {
			t.Error("an event from the batch before the one running must be dropped")
		}
	})

	t.Run("the current batch is honoured", func(t *testing.T) {
		m := Model{screen: screenProgress, batchStepCount: 1,
			steps: []stepState{{name: runner.StepPulling}}}
		updated, _ := m.Update(stepEventMsg{event: ev})
		got := updated.(Model)
		if !got.failed || got.steps[0].status != runner.StatusFailed {
			t.Errorf("a current event must paint its row and fail the op: failed=%v status=%q",
				got.failed, got.steps[0].status)
		}
	})
}

// Defence in depth beside the stepEventMsg gate: a Model that reached the
// progress screen carrying a stale verdict would render "q back" over a live
// pipeline and stop pipelineDoneMsg from ever opening the health gate.
func TestEnterProgress_ResetsTheVerdict(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)
	m.pendingOp = runner.StopOnly
	m.done = true
	m.failed = true

	updated, _ := m.enterProgress([]opBatch{{proj: m.currentProject(), services: []string{"web"}}})
	got := updated.(Model)
	if got.done || got.failed {
		t.Errorf("enterProgress must start from a clean verdict, got done=%v failed=%v", got.done, got.failed)
	}
}

// A mid-sequence batch whose PIPELINE ran to the end pulled its images, so its
// update verdicts are stale even when its health GATE then failed and m.done was
// never set. Only a batch with an unreached or failed step keeps its cache.
func TestEscFromProgress_InvalidatesABatchWhoseGateFailed(t *testing.T) {
	blog := compose.Project{Name: "blog", ConfigDir: "/srv/blog"}
	shop := compose.Project{Name: "shop", ConfigDir: "/srv/shop"}
	perBatch := len(runner.Steps(runner.Deploy))

	newModel := func() Model {
		mc := &mockComposer{}
		m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
		installFakeTick(&m)
		m.screen = screenProgress
		m.composer = mc
		m.pendingOp = runner.Deploy
		m.batches = []opBatch{{proj: blog}, {proj: shop}}
		m.batchStepCount = perBatch
		m.batchIdx = 0
		m.steps = make([]stepState, 2*perBatch)
		for i := range m.steps {
			m.steps[i] = stepState{name: runner.Steps(runner.Deploy)[i%perBatch]}
		}
		m.updateCache = map[string]updateEntry{
			m.projUpdatesCacheKey(blog): {fetchedAt: time.Now(), results: map[string]bool{"web": true}},
			m.projUpdatesCacheKey(shop): {fetchedAt: time.Now(), results: map[string]bool{"api": true}},
		}
		return m
	}

	t.Run("pipeline finished, gate failed", func(t *testing.T) {
		m := newModel()
		for i := 0; i < perBatch; i++ {
			m.steps[i].status = runner.StatusDone
		}
		m.failed = true // the gate stopped the sequence

		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		got := updated.(Model)
		if _, present := got.updateCache[got.projUpdatesCacheKey(blog)]; present {
			t.Error("a batch whose pipeline ran to the end pulled its images; its verdicts are stale")
		}
		if _, present := got.updateCache[got.projUpdatesCacheKey(shop)]; !present {
			t.Error("the batch that never ran must keep its cache")
		}
	})

	t.Run("pipeline failed mid-way", func(t *testing.T) {
		m := newModel()
		m.steps[0].status = runner.StatusDone
		m.steps[1].status = runner.StatusFailed
		m.failed = true

		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		got := updated.(Model)
		if _, present := got.updateCache[got.projUpdatesCacheKey(blog)]; !present {
			t.Error("a failed pipeline must keep its cache rather than clear user-visible state")
		}
	})
}

// formatBatchTargets grows with every project and service a sequence names, and
// the step-row budget counts head's newlines — a title the TERMINAL wrapped adds
// rows strings.Count cannot see, so the window overflowed the screen.
func TestViewProgress_TitleIsClampedToWidth(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.composer = mc
	m.pendingOp = runner.Deploy
	m.width, m.height = 40, 24
	m.batchStepCount = 1
	m.steps = []stepState{{name: runner.StepPulling, label: runner.StepPulling}}
	for i := 0; i < 6; i++ {
		m.batches = append(m.batches, opBatch{
			proj:     compose.Project{Name: fmt.Sprintf("project-with-a-long-name-%d", i)},
			services: []string{"api", "db", "cache"},
		})
	}

	for _, line := range strings.Split(ansi.Strip(m.viewProgress()), "\n") {
		if w := ansi.StringWidth(line); w > m.width {
			t.Fatalf("line overruns width %d (%d cells): %q", m.width, w, line)
		}
	}
}

// --- third review round: pins for the fixes that round produced ---

// TestEscFromProgress_CancelsTheLastBatchContext pins the leak fix. batchDoneMsg
// cancels each batch as the next takes over m.cancel, but the LAST batch has no
// successor — so leaving the screen is where its context has to be released.
func TestEscFromProgress_CancelsTheLastBatchContext(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenProgress
	m.composer = mc
	m.pendingOp = runner.Deploy
	m.done = true
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = context.Background()
	m.cancel = cancel

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(Model).screen != screenSelectContainers {
		t.Fatalf("screen = %d, want screenSelectContainers", updated.(Model).screen)
	}
	select {
	case <-ctx.Done():
	default:
		t.Error("the final batch's context is still live after leaving the progress screen")
	}
	if updated.(Model).cancel != nil {
		t.Error("m.cancel must be nil after the departure")
	}
}

// opContainers is the BATCH's resolved target set, not wait state — the grouped
// progress title reads it. Clearing it on an esc-skipped gate re-titled a
// named-service operation "all services"; clearBatchSequence owns it instead.
func TestClearWaitState_KeepsOpContainers(t *testing.T) {
	m := Model{opContainers: []string{"api", "db"}, waiting: true}
	m.clearWaitState()
	if len(m.opContainers) != 2 {
		t.Errorf("opContainers = %v, want the batch's target set preserved", m.opContainers)
	}
	if m.waiting {
		t.Error("clearWaitState must still close the gate")
	}
	m.clearBatchSequence()
	if m.opContainers != nil {
		t.Errorf("opContainers = %v, want nil once the sequence itself is dropped", m.opContainers)
	}
}

// The visible half of the same rule: a gate skipped on the last batch keeps the
// title naming the services the batch actually ran.
func TestViewProgress_SkippedGateKeepsTheNamedTargets(t *testing.T) {
	m := Model{
		screen:         screenProgress,
		pendingOp:      runner.Deploy,
		grouped:        true,
		width:          100,
		height:         24,
		done:           true,
		waiting:        true,
		batchStepCount: 1,
		batches:        []opBatch{{proj: compose.Project{Name: "shop"}, services: []string{"api"}}},
		opContainers:   []string{"api"},
		steps:          []stepState{progressStep(runner.StepStopping, runner.StatusDone)},
	}
	m.waitState = runner.NewWaitState([]string{"api"})
	m.waitDeadline = time.Now().Add(time.Minute)

	skipped, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	out := ansi.Strip(skipped.(Model).viewProgress())
	if strings.Contains(out, "all services") {
		t.Errorf("the skipped gate re-titled a named operation:\n%s", out)
	}
	if !strings.Contains(out, "api") {
		t.Errorf("the title must still name the batch's services:\n%s", out)
	}
}

// TestViewProgress_MidSequenceGateNamesBothOutcomes pins the footer half of the
// esc/enter split. The gate binds two keys with opposite consequences, and the
// destructive one is the key that starts the next project.
func TestViewProgress_MidSequenceGateNamesBothOutcomes(t *testing.T) {
	base := func(batches int) Model {
		m := Model{
			screen:         screenProgress,
			pendingOp:      runner.Deploy,
			width:          120,
			height:         24,
			waiting:        true,
			batchStepCount: 1,
			steps:          []stepState{progressStep(runner.StepStopping, runner.StatusDone)},
		}
		for i := 0; i < batches; i++ {
			m.batches = append(m.batches, opBatch{proj: compose.Project{Name: fmt.Sprintf("p%d", i)}})
		}
		m.waitState = runner.NewWaitState([]string{"api"})
		m.waitDeadline = time.Now().Add(time.Minute)
		return m
	}

	mid := ansi.Strip(base(2).viewProgress())
	if !strings.Contains(mid, "enter skip wait") || !strings.Contains(mid, "q esc stop") {
		t.Errorf("a mid-sequence gate must name both outcomes, got:\n%s", mid)
	}

	last := ansi.Strip(base(1).viewProgress())
	if strings.Contains(last, "stop") {
		t.Errorf("with nothing left to release the footer must not promise a choice, got:\n%s", last)
	}
	if !strings.Contains(last, "enter q esc skip") {
		t.Errorf("the last batch's gate is a plain skip, got:\n%s", last)
	}
}

// TestProgressWindow_TallBatchKeepsTheRunningStep is the behavioural half of the
// window's tail anchor. On a terminal with fewer rows to spare than the batch
// has steps, pulling the window back to the batch's FIRST row showed its head
// while the step that was actually running scrolled off the bottom.
func TestProgressWindow_TallBatchKeepsTheRunningStep(t *testing.T) {
	stepNames := runner.Steps(runner.Deploy)
	var batches []opBatch
	var steps []stepState
	for _, name := range []string{"p0", "p1", "p2", "p3", "p4"} {
		batches = append(batches, opBatch{proj: compose.Project{Name: name, ConfigDir: "/srv/" + name}})
		for _, step := range stepNames {
			steps = append(steps, stepState{name: step, label: name + ": " + step, status: runner.StatusDone})
		}
	}
	// The LAST batch is running, and its LAST step is the one in flight.
	running := len(steps) - 1
	steps[running].status = runner.StatusRunning
	m := Model{
		screen:         screenProgress,
		pendingOp:      runner.Deploy,
		ctx:            context.Background(),
		batches:        batches,
		batchStepCount: len(stepNames),
		batchIdx:       len(batches) - 1,
		steps:          steps,
		width:          100,
		height:         12, // fewer rows to spare than the batch has steps
	}

	out := ansi.Strip(m.viewProgress())
	if !strings.Contains(out, steps[running].label) {
		t.Errorf("the running step %q scrolled out of the window:\n%s", steps[running].label, out)
	}
	if lines := strings.Split(strings.TrimRight(out, "\n"), "\n"); len(lines) > m.height {
		t.Errorf("viewProgress rendered %d lines into a %d-row terminal:\n%s", len(lines), m.height, out)
	}
}
