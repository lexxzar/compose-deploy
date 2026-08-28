package compose

import (
	"time"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

// svcAgg accumulates the replicas of ONE service into a single ServiceStatus.
// It is the single home of the scaled-service merge rules, shared by the
// per-project parser (parseContainerStatus) and the host-wide grouper
// (groupHostContainers): Running is an OR, Health takes the worst case, Created
// the oldest replica, Uptime the longest-running one, and Ports accumulate for
// a later dedup. Two copies of these rules drifted apart once; one type is what
// keeps a scaled service reading the same in both views.
type svcAgg struct {
	running            bool
	health             string
	oldestCreated      time.Time
	oldestCreatedValid bool
	longestUpDur       time.Duration
	longestUpStr       string
	longestFromRunning bool
	ports              []runner.Port
}

// merge folds one replica in. The caller resolves the inputs from whatever ps
// shape it holds — that is the only difference between the two call sites.
// status is the raw docker Status text, read for the uptime only; health
// arrives already extracted, because compose ps reports it as its own field
// while host ps carries it in the Status annotation.
func (a *svcAgg) merge(running bool, health, createdAt, status string, ports []runner.Port) {
	a.running = a.running || running
	if healthPriority(health) > healthPriority(a.health) {
		a.health = health
	}
	if t, ok := parseCreatedAt(createdAt); ok && (!a.oldestCreatedValid || t.Before(a.oldestCreated)) {
		a.oldestCreated, a.oldestCreatedValid = t, true
	}
	uptime := formatUptime(status)
	switch {
	case running && uptime != "":
		// A running replica always beats a restarting one, and the longest of
		// the running replicas wins: CreatedAt is older after a restart, so it
		// is the wrong tiebreaker.
		if d := parseUptimeDuration(uptime); !a.longestFromRunning || d > a.longestUpDur {
			a.longestUpDur, a.longestUpStr, a.longestFromRunning = d, uptime, true
		}
	case uptime == "restarting" && a.longestUpStr == "":
		a.longestUpStr = uptime
	}
	a.ports = append(a.ports, ports...)
}

func (a *svcAgg) status() runner.ServiceStatus {
	st := runner.ServiceStatus{
		Running: a.running,
		Health:  a.health,
		Uptime:  a.longestUpStr,
	}
	if a.oldestCreatedValid {
		st.Created = a.oldestCreated.Format("2006-01-02 15:04")
	}
	st.Ports = dedupAndSortPorts(a.ports)
	return st
}
