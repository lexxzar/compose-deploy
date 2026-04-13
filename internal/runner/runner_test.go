package runner

import (
	"context"
	"fmt"
	"io"
	"testing"
)

// mockComposer records calls and can be configured to fail at a specific step.
type mockComposer struct {
	calls   []string
	failAt  string // step name to fail at (empty = no failure)
	failErr error
}

func (m *mockComposer) Stop(ctx context.Context, containers []string, w io.Writer) error {
	m.calls = append(m.calls, StepStopping)
	if m.failAt == StepStopping {
		return m.failErr
	}
	return nil
}

func (m *mockComposer) Remove(ctx context.Context, containers []string, w io.Writer) error {
	m.calls = append(m.calls, StepRemoving)
	if m.failAt == StepRemoving {
		return m.failErr
	}
	return nil
}

func (m *mockComposer) Pull(ctx context.Context, containers []string, w io.Writer) error {
	m.calls = append(m.calls, StepPulling)
	if m.failAt == StepPulling {
		return m.failErr
	}
	return nil
}

func (m *mockComposer) Create(ctx context.Context, containers []string, w io.Writer) error {
	m.calls = append(m.calls, StepCreating)
	if m.failAt == StepCreating {
		return m.failErr
	}
	return nil
}

func (m *mockComposer) Start(ctx context.Context, containers []string, w io.Writer) error {
	m.calls = append(m.calls, StepStarting)
	if m.failAt == StepStarting {
		return m.failErr
	}
	return nil
}

func (m *mockComposer) ListServices(ctx context.Context) ([]string, error) {
	return []string{"nginx", "postgres"}, nil
}

func (m *mockComposer) ContainerStatus(ctx context.Context) (map[string]bool, error) {
	return map[string]bool{"nginx": true, "postgres": true}, nil
}

func (m *mockComposer) Logs(ctx context.Context, service string, follow bool, tail int, w io.Writer) error {
	return nil
}

func collectEvents(events <-chan StepEvent) []StepEvent {
	var result []StepEvent
	for e := range events {
		result = append(result, e)
	}
	return result
}

func TestRun_RestartSequence(t *testing.T) {
	mc := &mockComposer{}
	events := make(chan StepEvent, 20)

	Run(context.Background(), mc, Restart, []string{"nginx"}, io.Discard, events)

	wantCalls := []string{StepStopping, StepRemoving, StepCreating, StepStarting}
	if len(mc.calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", mc.calls, wantCalls)
	}
	for i, want := range wantCalls {
		if mc.calls[i] != want {
			t.Errorf("call[%d] = %q, want %q", i, mc.calls[i], want)
		}
	}
}

func TestRun_DeploySequence(t *testing.T) {
	mc := &mockComposer{}
	events := make(chan StepEvent, 20)

	Run(context.Background(), mc, Deploy, []string{"nginx"}, io.Discard, events)

	wantCalls := []string{StepStopping, StepRemoving, StepPulling, StepCreating, StepStarting}
	if len(mc.calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", mc.calls, wantCalls)
	}
	for i, want := range wantCalls {
		if mc.calls[i] != want {
			t.Errorf("call[%d] = %q, want %q", i, mc.calls[i], want)
		}
	}
}

func TestRun_DeployEvents(t *testing.T) {
	mc := &mockComposer{}
	events := make(chan StepEvent, 20)

	Run(context.Background(), mc, Deploy, []string{"nginx"}, io.Discard, events)

	evts := collectEvents(events)

	// Deploy has 5 steps, each produces running + done = 10 events
	if len(evts) != 10 {
		t.Fatalf("got %d events, want 10: %+v", len(evts), evts)
	}

	// Verify pattern: running, done, running, done, ...
	for i, e := range evts {
		if i%2 == 0 {
			if e.Status != StatusRunning {
				t.Errorf("event[%d] status = %q, want %q", i, e.Status, StatusRunning)
			}
		} else {
			if e.Status != StatusDone {
				t.Errorf("event[%d] status = %q, want %q", i, e.Status, StatusDone)
			}
		}
	}
}

func TestRun_FailureStopsPipeline(t *testing.T) {
	testErr := fmt.Errorf("pull failed: network error")
	mc := &mockComposer{failAt: StepPulling, failErr: testErr}
	events := make(chan StepEvent, 20)

	Run(context.Background(), mc, Deploy, []string{"nginx"}, io.Discard, events)

	// Should have called: stop, remove, pull (failed). NOT create or start.
	wantCalls := []string{StepStopping, StepRemoving, StepPulling}
	if len(mc.calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", mc.calls, wantCalls)
	}

	evts := collectEvents(events)

	// Events: stop running, stop done, remove running, remove done,
	//         pull running, pull failed = 6 events
	if len(evts) != 6 {
		t.Fatalf("got %d events, want 6: %+v", len(evts), evts)
	}

	lastEvent := evts[len(evts)-1]
	if lastEvent.Status != StatusFailed {
		t.Errorf("last event status = %q, want %q", lastEvent.Status, StatusFailed)
	}
	if lastEvent.Err != testErr {
		t.Errorf("last event error = %v, want %v", lastEvent.Err, testErr)
	}
}

func TestRun_FailureAtFirstStep(t *testing.T) {
	testErr := fmt.Errorf("stop failed")
	mc := &mockComposer{failAt: StepStopping, failErr: testErr}
	events := make(chan StepEvent, 20)

	Run(context.Background(), mc, Restart, []string{"nginx"}, io.Discard, events)

	if len(mc.calls) != 1 {
		t.Fatalf("calls = %v, want just [%s]", mc.calls, StepStopping)
	}

	evts := collectEvents(events)
	if len(evts) != 2 {
		t.Fatalf("got %d events, want 2", len(evts))
	}
	if evts[0].Status != StatusRunning {
		t.Errorf("event[0] status = %q, want %q", evts[0].Status, StatusRunning)
	}
	if evts[1].Status != StatusFailed {
		t.Errorf("event[1] status = %q, want %q", evts[1].Status, StatusFailed)
	}
}

func TestRun_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	mc := &mockComposer{}
	events := make(chan StepEvent, 20)

	// The compose methods will get a cancelled context.
	// Since our mock doesn't check ctx, it will still run.
	// In real usage, exec.CommandContext would fail on cancelled ctx.
	Run(ctx, mc, Restart, []string{"nginx"}, io.Discard, events)
	// Just verify it doesn't deadlock
}

func TestSteps_Restart(t *testing.T) {
	steps := Steps(Restart)
	want := []string{StepStopping, StepRemoving, StepCreating, StepStarting}
	if len(steps) != len(want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	for i, w := range want {
		if steps[i] != w {
			t.Errorf("step[%d] = %q, want %q", i, steps[i], w)
		}
	}
}

func TestSteps_Deploy(t *testing.T) {
	steps := Steps(Deploy)
	want := []string{StepStopping, StepRemoving, StepPulling, StepCreating, StepStarting}
	if len(steps) != len(want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	for i, w := range want {
		if steps[i] != w {
			t.Errorf("step[%d] = %q, want %q", i, steps[i], w)
		}
	}
}

func TestRun_StopOnlySequence(t *testing.T) {
	mc := &mockComposer{}
	events := make(chan StepEvent, 20)

	Run(context.Background(), mc, StopOnly, []string{"nginx"}, io.Discard, events)

	wantCalls := []string{StepStopping}
	if len(mc.calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", mc.calls, wantCalls)
	}

	evts := collectEvents(events)
	if len(evts) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evts), evts)
	}
	if evts[0].Status != StatusRunning || evts[1].Status != StatusDone {
		t.Errorf("events = %+v, want running then done", evts)
	}
}

func TestSteps_StopOnly(t *testing.T) {
	steps := Steps(StopOnly)
	want := []string{StepStopping}
	if len(steps) != len(want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
}

func TestOperation_String(t *testing.T) {
	tests := []struct {
		op   Operation
		want string
	}{
		{Restart, "Restart"},
		{Deploy, "Deploy"},
		{StopOnly, "Stop"},
	}
	for _, tt := range tests {
		if got := tt.op.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.op, got, tt.want)
		}
	}
}
