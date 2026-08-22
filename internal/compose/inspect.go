package compose

import "time"

// pickInspectContainer selects which replica of a compose service to inspect.
//
// The selection rule MIRRORS the Uptime aggregation gate in parseContainerStatus
// (compose.go, the svcAgg block): any running replica beats a restarting one, and
// among running replicas the longest actual uptime wins. A naive max-duration
// picker would disagree with the Uptime column the container screen renders on a
// running+restarting mix, so the two must stay in step.
//
// When no replica satisfies the gate (a stopped or exited service has no uptime at
// all) the first matching entry is returned, so a stopped container is still
// inspectable. Only a service with no matching entry at all yields ("", false).
func pickInspectContainer(entries []psEntry, service string) (string, bool) {
	if service == "" {
		return "", false
	}

	var (
		fallback           string
		haveFallback       bool
		chosen             string
		longestUpDur       time.Duration
		longestFromRunning bool
		haveUptime         bool
	)

	for _, entry := range entries {
		if entry.Service != service || entry.ID == "" {
			continue
		}
		if !haveFallback {
			fallback, haveFallback = entry.ID, true
		}

		uptime := formatUptime(entry.Status)
		switch {
		case entry.State == "running" && uptime != "":
			dur := parseUptimeDuration(uptime)
			if !longestFromRunning || dur > longestUpDur {
				longestUpDur, longestFromRunning = dur, true
				haveUptime, chosen = true, entry.ID
			}
		case uptime == "restarting" && !haveUptime:
			haveUptime, chosen = true, entry.ID
		}
	}

	if chosen != "" {
		return chosen, true
	}
	if haveFallback {
		return fallback, true
	}
	return "", false
}

// pickHostInspectContainer selects the unmanaged host container to inspect.
//
// Docker enforces unique container names per host and HostContainers keys its
// status map one-to-one, so this is a first match with no longest-running rule.
func pickHostInspectContainer(entries []hostPsEntry, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	for _, e := range entries {
		if e.ID != "" && hostContainerName(e.Names) == name {
			return e.ID, true
		}
	}
	return "", false
}
