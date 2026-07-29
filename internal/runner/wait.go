package runner

import "time"

// Default wait tuning. Grace and Poll are internal constants in v1 (no flags);
// only Timeout is user-exposed via --wait-timeout.
const (
	DefaultWaitTimeout = 2 * time.Minute
	DefaultWaitGrace   = 10 * time.Second
	DefaultWaitPoll    = 2 * time.Second
)

// Fail-fast thresholds for the never-stable failure modes. A restarting
// container reports Running==false AND Uptime=="restarting" simultaneously,
// so restarting is checked before any exited rule (see EvaluateWait).
const (
	// restartingThreshold is the number of consecutive "restarting" polls that
	// trips the restarting verdict (docker's own restart loop debounce).
	restartingThreshold = 3
	// neverRunningThreshold is the number of consecutive not-running polls for a
	// service that has never been observed running that trips the
	// "exited (never started)" verdict. The >=2-poll debounce absorbs the
	// first-poll race where a container is caught mid-transition right after
	// Start returns.
	neverRunningThreshold = 5
)

// WaitVerdict is the resolved outcome for a single service during a health wait.
// The empty string is the "pending / not yet resolved" state.
type WaitVerdict string

// The seven terminal verdicts plus the pending zero value. These exact strings
// are rendered by both the CLI verdict table and the TUI progress screen.
const (
	VerdictPending          WaitVerdict = ""
	VerdictHealthy          WaitVerdict = "healthy"
	VerdictRunningNoHC      WaitVerdict = "running (no healthcheck)"
	VerdictUnhealthy        WaitVerdict = "unhealthy"
	VerdictExited           WaitVerdict = "exited"
	VerdictExitedNeverStart WaitVerdict = "exited (never started)"
	VerdictRestarting       WaitVerdict = "restarting"
	VerdictTimedOut         WaitVerdict = "timed out (still starting)"
)

// Terminal reports whether the verdict is resolved (pass or fail).
func (v WaitVerdict) Terminal() bool { return v != VerdictPending }

// OK reports whether the verdict is a passing outcome.
func (v WaitVerdict) OK() bool {
	return v == VerdictHealthy || v == VerdictRunningNoHC
}

// WaitOptions tunes the health wait. Zero-value fields fall back to the
// package defaults via normalize().
type WaitOptions struct {
	Timeout time.Duration
	Grace   time.Duration
	Poll    time.Duration
}

// normalize returns a copy with any non-positive field replaced by its default.
func (o WaitOptions) normalize() WaitOptions {
	if o.Timeout <= 0 {
		o.Timeout = DefaultWaitTimeout
	}
	if o.Grace <= 0 {
		o.Grace = DefaultWaitGrace
	}
	if o.Poll <= 0 {
		o.Poll = DefaultWaitPoll
	}
	return o
}

// WaitState is the accumulator threaded through successive EvaluateWait calls.
// It is safe to copy by value across package boundaries (the TUI holds it on
// its Model and feeds it back to EvaluateWait); only Services and Verdicts are
// exported for rendering, the rest is reducer bookkeeping.
type WaitState struct {
	// Services is the target set, order preserved for deterministic rendering.
	Services []string
	// Verdicts maps each target service to its current verdict (VerdictPending
	// until resolved).
	Verdicts map[string]WaitVerdict

	everRunning       map[string]bool          // service observed Running==true at least once
	firstRunningAt    map[string]time.Duration // elapsed at the start of the current continuous-running window (grace timer anchor)
	restartingCount   map[string]int           // consecutive "restarting" polls
	neverRunningCount map[string]int           // consecutive not-running polls while never-run
}

// NewWaitState seeds a fresh WaitState for the given target services. Every
// service starts VerdictPending. Callers MUST seed via this constructor before
// the first EvaluateWait call — the reducer iterates the seeded target set, so
// a zero-value WaitState resolves to an empty (immediately-done) wait.
func NewWaitState(services []string) WaitState {
	s := WaitState{
		Services:          append([]string(nil), services...),
		Verdicts:          make(map[string]WaitVerdict, len(services)),
		everRunning:       make(map[string]bool, len(services)),
		firstRunningAt:    make(map[string]time.Duration, len(services)),
		restartingCount:   make(map[string]int, len(services)),
		neverRunningCount: make(map[string]int, len(services)),
	}
	for _, svc := range services {
		s.Verdicts[svc] = VerdictPending
	}
	return s
}

// clone deep-copies the state so EvaluateWait never mutates its input.
func (s WaitState) clone() WaitState {
	n := WaitState{
		Services:          append([]string(nil), s.Services...),
		Verdicts:          make(map[string]WaitVerdict, len(s.Verdicts)),
		everRunning:       make(map[string]bool, len(s.everRunning)),
		firstRunningAt:    make(map[string]time.Duration, len(s.firstRunningAt)),
		restartingCount:   make(map[string]int, len(s.restartingCount)),
		neverRunningCount: make(map[string]int, len(s.neverRunningCount)),
	}
	for k, v := range s.Verdicts {
		n.Verdicts[k] = v
	}
	for k, v := range s.everRunning {
		n.everRunning[k] = v
	}
	for k, v := range s.firstRunningAt {
		n.firstRunningAt[k] = v
	}
	for k, v := range s.restartingCount {
		n.restartingCount[k] = v
	}
	for k, v := range s.neverRunningCount {
		n.neverRunningCount[k] = v
	}
	return n
}

// WaitReport is the final outcome of a health wait.
type WaitReport struct {
	Verdicts map[string]WaitVerdict
	Elapsed  time.Duration
	OK       bool
}

// Report snapshots the current verdicts into a WaitReport. OK is true only when
// every target service reached a passing verdict.
func (s WaitState) Report(elapsed time.Duration) WaitReport {
	verdicts := make(map[string]WaitVerdict, len(s.Services))
	ok := true
	for _, svc := range s.Services {
		v := s.Verdicts[svc]
		verdicts[svc] = v
		if !v.OK() {
			ok = false
		}
	}
	return WaitReport{Verdicts: verdicts, Elapsed: elapsed, OK: ok}
}

// EvaluateWait is the pure step function holding ALL pass/fail rules. It takes
// the previous state, the latest ContainerStatus snapshot, the (normalized)
// options, and the elapsed time since the wait began; it returns the next state
// and whether the wait is complete (every target service resolved, or the
// timeout elapsed). It performs no clock reads or IO — elapsed is an input.
//
// Per-service precedence (the order below IS the spec):
//  1. Uptime == "restarting" ⇒ bump restart counter; restartingThreshold
//     consecutive ⇒ restarting. Checked FIRST because a restarting container
//     reports Running==false simultaneously; otherwise the exited rules would
//     fire and the restart counter would never reach the threshold.
//  2. Health == "unhealthy" ⇒ unhealthy (docker already debounced via retries).
//  3. Running == false after everRunning ⇒ exited.
//  4. Running == false, never observed running, neverRunningThreshold
//     consecutive polls ⇒ exited (never started).
//  5. Health != "" ⇒ has a healthcheck ⇒ passes on "healthy".
//  6. Health == "" && Running ⇒ grace timer ⇒ passes after running
//     continuously >= Grace.
//
// Any service still pending once elapsed >= Timeout is marked timed out.
func EvaluateWait(prev WaitState, status map[string]ServiceStatus, opts WaitOptions, elapsed time.Duration) (WaitState, bool) {
	opts = opts.normalize()
	next := prev.clone()

	for _, svc := range next.Services {
		if next.Verdicts[svc].Terminal() {
			continue
		}

		// A service absent from the status map is treated as not-running with
		// no health — it feeds the same counters as an explicit not-running.
		st := status[svc]

		// Rule 1: restarting (must precede the exited rules).
		if st.Uptime == "restarting" {
			next.restartingCount[svc]++
			next.neverRunningCount[svc] = 0
			delete(next.firstRunningAt, svc)
			if next.restartingCount[svc] >= restartingThreshold {
				next.Verdicts[svc] = VerdictRestarting
			}
			continue
		}
		// A non-restarting observation resets the restart counter.
		next.restartingCount[svc] = 0

		// Rule 2: unhealthy.
		if st.Health == "unhealthy" {
			next.Verdicts[svc] = VerdictUnhealthy
			continue
		}

		// Rules 3 & 4: not running.
		if !st.Running {
			delete(next.firstRunningAt, svc)
			if next.everRunning[svc] {
				next.Verdicts[svc] = VerdictExited
				continue
			}
			next.neverRunningCount[svc]++
			if next.neverRunningCount[svc] >= neverRunningThreshold {
				next.Verdicts[svc] = VerdictExitedNeverStart
			}
			continue
		}

		// Running == true.
		next.everRunning[svc] = true
		next.neverRunningCount[svc] = 0

		// Rule 5: has a healthcheck ⇒ pass on healthy, else keep waiting
		// (e.g. "starting").
		if st.Health != "" {
			if st.Health == "healthy" {
				next.Verdicts[svc] = VerdictHealthy
			}
			continue
		}

		// Rule 6: no healthcheck ⇒ grace timer.
		if _, ok := next.firstRunningAt[svc]; !ok {
			next.firstRunningAt[svc] = elapsed
		}
		if elapsed-next.firstRunningAt[svc] >= opts.Grace {
			next.Verdicts[svc] = VerdictRunningNoHC
		}
	}

	// Timeout sweep: any service still pending once the deadline passed is
	// marked timed out. Normal resolution above wins on the same poll (a
	// service that goes healthy exactly at the timeout keeps its healthy
	// verdict).
	done := true
	timedOut := elapsed >= opts.Timeout
	for _, svc := range next.Services {
		if next.Verdicts[svc].Terminal() {
			continue
		}
		if timedOut {
			next.Verdicts[svc] = VerdictTimedOut
			continue
		}
		done = false
	}

	return next, done
}
