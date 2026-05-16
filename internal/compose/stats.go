package compose

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

// statsEntry matches the JSON schema of `docker stats --no-stream --format json`.
// Fields are camel-cased exactly as Docker emits them; only the fields we consume
// are declared. `ID` is the short container ID (12 chars); used as the join key
// against `docker compose ps`'s `ID` field.
type statsEntry struct {
	ID       string `json:"ID"`
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`  // e.g. "4.20%"
	MemUsage string `json:"MemUsage"` // e.g. "124MiB / 512MiB"
}

// parseStatsOutput parses the output of `docker stats --no-stream --format json`.
// Tolerant of both NDJSON (one JSON object per line, older Docker) and JSON-array
// form (newer Docker), matching the parseContainerStatus convention.
//
// Result is keyed by container ID (short form), values populated from CPUPerc
// and MemUsage. Containers with empty IDs are skipped silently.
func parseStatsOutput(data []byte) (map[string]runner.ServiceStats, error) {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "[]" {
		return map[string]runner.ServiceStats{}, nil
	}

	var entries []statsEntry

	if strings.HasPrefix(s, "[") {
		// JSON array form
		if err := json.Unmarshal([]byte(s), &entries); err != nil {
			return nil, fmt.Errorf("parsing stats output: %w", err)
		}
	} else {
		// NDJSON form (one JSON object per line)
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry statsEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				return nil, fmt.Errorf("parsing stats output: %w", err)
			}
			entries = append(entries, entry)
		}
	}

	out := make(map[string]runner.ServiceStats, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		cpu, err := parseCPUPercent(e.CPUPerc)
		if err != nil {
			return nil, fmt.Errorf("parsing CPU percent for %s: %w", e.ID, err)
		}
		used, limit, err := parseMemUsage(e.MemUsage)
		if err != nil {
			return nil, fmt.Errorf("parsing memory usage for %s: %w", e.ID, err)
		}
		out[e.ID] = runner.ServiceStats{
			CPUPercent:  cpu,
			MemoryUsed:  used,
			MemoryLimit: limit,
		}
	}
	return out, nil
}

// parseCPUPercent strips the trailing "%" from a docker stats CPU percentage
// string and parses the remainder as a float64.
//
// Examples:
//
//	"4.20%" → 4.2
//	"0.00%" → 0
//	""      → 0
//	"abc"   → error
func parseCPUPercent(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(s, "%")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing CPU percent %q: %w", s, err)
	}
	return v, nil
}

// parseMemUsage parses Docker's "used / limit" memory usage string into
// (used, limit) byte counts.
//
// Examples:
//
//	"124MiB / 512MiB" → (130023424, 536870912)
//	"1.5GiB / 2GiB"   → (1610612736, 2147483648)
func parseMemUsage(s string) (int64, int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("parsing memory usage %q: missing separator", s)
	}
	used, err := parseSize(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("parsing memory used in %q: %w", s, err)
	}
	limit, err := parseSize(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("parsing memory limit in %q: %w", s, err)
	}
	return used, limit, nil
}

// sizeUnits maps unit suffixes (case-insensitive on the letter, exact on the
// "i" infix) to their byte multipliers. Both IEC binary (KiB, MiB, GiB, TiB)
// and SI decimal (kB/KB, MB, GB, TB) suffixes are recognized — Docker emits
// either depending on engine settings.
var sizeUnits = map[string]int64{
	"b":   1,
	"kb":  1000,
	"mb":  1000 * 1000,
	"gb":  1000 * 1000 * 1000,
	"tb":  1000 * 1000 * 1000 * 1000,
	"kib": 1024,
	"mib": 1024 * 1024,
	"gib": 1024 * 1024 * 1024,
	"tib": 1024 * 1024 * 1024 * 1024,
}

// parseSize parses a size string with a unit suffix (e.g. "124MiB", "1.5GB",
// "512B") into a byte count. Handles both binary (1024-based, IEC) and decimal
// (1000-based, SI) suffixes; the unit letter is case-insensitive.
//
// Examples:
//
//	"512B"    → 512
//	"100kB"   → 100000
//	"100KB"   → 100000
//	"124MiB"  → 130023424
//	"1.5GiB"  → 1610612736
//	"1.5GB"   → 1500000000
//	""        → 0
//	"abc"     → error
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// Find where the unit suffix begins (first non-digit, non-dot character).
	idx := -1
	for i, r := range s {
		if !(r >= '0' && r <= '9') && r != '.' {
			idx = i
			break
		}
	}
	if idx < 0 {
		// No unit suffix → assume bytes.
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing size %q: %w", s, err)
		}
		return int64(v), nil
	}
	numStr := strings.TrimSpace(s[:idx])
	unitStr := strings.TrimSpace(strings.ToLower(s[idx:]))
	if numStr == "" {
		return 0, fmt.Errorf("parsing size %q: missing numeric part", s)
	}
	mult, ok := sizeUnits[unitStr]
	if !ok {
		return 0, fmt.Errorf("parsing size %q: unknown unit %q", s, unitStr)
	}
	v, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing size %q: %w", s, err)
	}
	return int64(v * float64(mult)), nil
}

// FormatBytes converts a byte count to a compact human-readable string with a
// single-letter binary suffix (no "i" infix). Used by both `cmd/list.go` and
// `internal/tui/app.go` as the single source of truth for rendering memory
// usage in tables — defined here next to parseSize to keep the round-trip
// pair colocated.
//
// Output forms (rounded):
//
//	0       → "0B"
//	512     → "512B"
//	2048    → "2K"
//	130023424 → "124M"
//	1610612736 → "1.5G"
//	1<<40   → "1T"
//
// Values < 1 KiB render as "<n>B". Values >= 1 KiB use the largest unit where
// the leading number is < 1024. Sub-unit decimals are shown only when the
// value is not an integer multiple of the unit AND less than 10 of that unit
// (so "1.5G" but "124M", not "124.0M").
func FormatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const (
		kib = 1024
		mib = kib * 1024
		gib = mib * 1024
		tib = gib * 1024
	)
	var unit int64
	var suffix string
	switch {
	case n >= tib:
		unit, suffix = tib, "T"
	case n >= gib:
		unit, suffix = gib, "G"
	case n >= mib:
		unit, suffix = mib, "M"
	case n >= kib:
		unit, suffix = kib, "K"
	default:
		return strconv.FormatInt(n, 10) + "B"
	}
	// Show one decimal only when the value is < 10 of the unit and not exact.
	if n < 10*unit && n%unit != 0 {
		v := float64(n) / float64(unit)
		// Round to one decimal place.
		v = float64(int64(v*10+0.5)) / 10
		// If rounding produced an integer value, drop the trailing ".0".
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10) + suffix
		}
		return strconv.FormatFloat(v, 'f', 1, 64) + suffix
	}
	// Integer rounding for larger values.
	v := (n + unit/2) / unit
	return strconv.FormatInt(v, 10) + suffix
}
