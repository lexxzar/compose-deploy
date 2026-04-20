package compose

import (
	"testing"
	"time"
)

func TestFormatUptime(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		// Basic "Up" durations
		{name: "up seconds", status: "Up 5 seconds", want: "5s"},
		{name: "up minutes", status: "Up 3 minutes", want: "3m"},
		{name: "up hours", status: "Up 3 hours", want: "3h"},
		{name: "up days", status: "Up 2 days", want: "2d"},
		{name: "up weeks", status: "Up 1 week", want: "1w"},
		{name: "up months", status: "Up 4 months", want: "4mo"},

		// Singular forms
		{name: "1 second", status: "Up 1 second", want: "1s"},
		{name: "1 minute", status: "Up 1 minute", want: "1m"},
		{name: "1 hour", status: "Up 1 hour", want: "1h"},
		{name: "1 day", status: "Up 1 day", want: "1d"},
		{name: "1 month", status: "Up 1 month", want: "1mo"},

		// Multi-unit durations
		{name: "hours and minutes", status: "Up 3 hours 15 minutes", want: "3h 15m"},
		{name: "days and hours", status: "Up 2 days 5 hours", want: "2d 5h"},

		// Special textual cases
		{name: "about a minute", status: "Up About a minute", want: "~1m"},
		{name: "about an hour", status: "Up About an hour", want: "~1h"},
		{name: "less than a second", status: "Up Less than a second", want: "<1s"},

		// Health suffix stripping
		{name: "healthy suffix", status: "Up 3 hours (healthy)", want: "3h"},
		{name: "unhealthy suffix", status: "Up 10 minutes (unhealthy)", want: "10m"},
		{name: "health starting suffix", status: "Up 5 seconds (health: starting)", want: "5s"},

		// Restarting
		{name: "restarting", status: "Restarting (1) 5 seconds ago", want: "restarting"},
		{name: "restarting simple", status: "Restarting", want: "restarting"},

		// Non-running statuses → empty
		{name: "exited", status: "Exited (0) 5 minutes ago", want: ""},
		{name: "created", status: "Created", want: ""},
		{name: "dead", status: "Dead", want: ""},

		// Edge cases
		{name: "empty string", status: "", want: ""},
		{name: "whitespace only", status: "   ", want: ""},
		{name: "unknown format", status: "SomethingElse entirely", want: ""},

		// Fallback: unrecognized "Up ..." → raw remainder
		{name: "up unknown remainder", status: "Up forever and ever", want: "forever and ever"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatUptime(tt.status)
			if got != tt.want {
				t.Errorf("formatUptime(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestStripHealthSuffix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"3 hours (healthy)", "3 hours"},
		{"10 minutes (unhealthy)", "10 minutes"},
		{"5 seconds (health: starting)", "5 seconds"},
		{"no suffix here", "no suffix here"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stripHealthSuffix(tt.input)
			if got != tt.want {
				t.Errorf("stripHealthSuffix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCompactDuration(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"3 hours", "3h"},
		{"15 minutes", "15m"},
		{"3 hours 15 minutes", "3h 15m"},
		{"2 days 5 hours", "2d 5h"},
		{"no match here", "no match here"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := compactDuration(tt.input)
			if got != tt.want {
				t.Errorf("compactDuration(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseUptimeDuration(t *testing.T) {
	tests := []struct {
		name    string
		compact string
		want    time.Duration
	}{
		{name: "empty", compact: "", want: 0},
		{name: "restarting", compact: "restarting", want: 0},
		{name: "seconds", compact: "5s", want: 5 * time.Second},
		{name: "minutes", compact: "3m", want: 3 * time.Minute},
		{name: "hours", compact: "3h", want: 3 * time.Hour},
		{name: "days", compact: "2d", want: 48 * time.Hour},
		{name: "weeks", compact: "1w", want: 7 * 24 * time.Hour},
		{name: "months", compact: "4mo", want: 4 * 30 * 24 * time.Hour},
		{name: "multi-unit hours and minutes", compact: "3h 15m", want: 3*time.Hour + 15*time.Minute},
		{name: "multi-unit days and hours", compact: "2d 5h", want: 53 * time.Hour},
		{name: "approximate minute", compact: "~1m", want: 1 * time.Minute},
		{name: "approximate hour", compact: "~1h", want: 1 * time.Hour},
		{name: "less than a second", compact: "<1s", want: time.Millisecond},
		{name: "unparseable fallback", compact: "forever and ever", want: time.Millisecond},
		{name: "whitespace only", compact: "   ", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseUptimeDuration(tt.compact)
			if got != tt.want {
				t.Errorf("parseUptimeDuration(%q) = %v, want %v", tt.compact, got, tt.want)
			}
		})
	}
}
