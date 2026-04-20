package compose

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// formatUptime converts Docker's Status field into a compact human-readable uptime string.
// Examples:
//
//	"Up 3 hours"         → "3h"
//	"Up 2 days"          → "2d"
//	"Up About a minute"  → "~1m"
//	"Up 3 hours (healthy)" → "3h"
//	"Restarting ..."     → "restarting"
//	"Exited (0) ..."     → ""
//	""                   → ""
func formatUptime(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return ""
	}

	// Handle Restarting status
	if strings.HasPrefix(status, "Restarting") {
		return "restarting"
	}

	// Only "Up ..." statuses produce uptime; everything else (Exited, Created, etc.) → empty.
	if !strings.HasPrefix(status, "Up ") {
		return ""
	}

	// Strip "Up " prefix
	remainder := status[3:]

	// Strip health suffixes: (healthy), (unhealthy), (health: starting)
	remainder = stripHealthSuffix(remainder)
	remainder = strings.TrimSpace(remainder)

	if remainder == "" {
		return ""
	}

	// Handle special textual cases
	lower := strings.ToLower(remainder)
	switch lower {
	case "about a minute":
		return "~1m"
	case "about an hour":
		return "~1h"
	case "less than a second":
		return "<1s"
	}

	// Compact time units
	return compactDuration(remainder)
}

// healthSuffixRe matches trailing health annotations like (healthy), (unhealthy), (health: starting).
var healthSuffixRe = regexp.MustCompile(`\s*\([^)]*\)\s*$`)

func stripHealthSuffix(s string) string {
	return healthSuffixRe.ReplaceAllString(s, "")
}

// unitMap maps Docker's duration words to compact suffixes.
var unitMap = map[string]string{
	"seconds": "s",
	"second":  "s",
	"minutes": "m",
	"minute":  "m",
	"hours":   "h",
	"hour":    "h",
	"days":    "d",
	"day":     "d",
	"weeks":   "w",
	"week":    "w",
	"months":  "mo",
	"month":   "mo",
}

// durationTokenRe matches a number followed by a time unit word.
var durationTokenRe = regexp.MustCompile(`(\d+)\s+(seconds?|minutes?|hours?|days?|weeks?|months?)`)

// compactDuration converts multi-word Docker durations like "3 hours 15 minutes" into "3h 15m".
func compactDuration(s string) string {
	matches := durationTokenRe.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		// Fallback: return raw remainder trimmed
		return strings.TrimSpace(s)
	}

	var parts []string
	for _, m := range matches {
		num := m[1]
		unit := unitMap[m[2]]
		parts = append(parts, num+unit)
	}
	return strings.Join(parts, " ")
}

// compactUnitDuration maps compact suffixes to their time.Duration multipliers.
var compactUnitDuration = map[string]time.Duration{
	"s":  time.Second,
	"m":  time.Minute,
	"h":  time.Hour,
	"d":  24 * time.Hour,
	"w":  7 * 24 * time.Hour,
	"mo": 30 * 24 * time.Hour,
}

// compactTokenRe matches a number followed by a compact unit suffix (e.g. "3h", "15m", "30mo").
var compactTokenRe = regexp.MustCompile(`(\d+)(mo|[smhdw])`)

// parseUptimeDuration converts a compact uptime string (output of formatUptime) into a time.Duration.
// Returns 0 only for empty strings and "restarting".
// Non-empty strings that can't be parsed (including "<1s") return time.Millisecond
// so they still beat "restarting" in duration comparisons.
// Examples:
//
//	"3h"      → 3 * time.Hour
//	"2d 5h"   → 53 * time.Hour
//	"~1m"     → 1 * time.Minute
//	"<1s"     → time.Millisecond (minimal positive duration)
//	""        → 0
//	"restarting" → 0
func parseUptimeDuration(compact string) time.Duration {
	compact = strings.TrimSpace(compact)
	if compact == "" || compact == "restarting" {
		return 0
	}

	// Strip leading ~ (approximate marker)
	compact = strings.TrimPrefix(compact, "~")

	// "<1s" — negligible but still running, return minimal positive duration.
	if strings.HasPrefix(compact, "<") {
		return time.Millisecond
	}

	matches := compactTokenRe.FindAllStringSubmatch(compact, -1)
	if len(matches) == 0 {
		// Unrecognized format but non-empty — return minimal positive duration
		// so it beats "restarting" in comparisons.
		return time.Millisecond
	}

	var total time.Duration
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		mult, ok := compactUnitDuration[m[2]]
		if !ok {
			continue
		}
		total += time.Duration(n) * mult
	}
	return total
}
