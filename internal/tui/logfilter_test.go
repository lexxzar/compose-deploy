package tui

import (
	"strings"
	"testing"
)

func TestBuildMatcher(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		isRegex     bool
		allowNegate bool
		wantValid   bool
		wantRe      bool            // whether re should be non-nil
		probes      map[string]bool // input -> expected pred result (checked only when valid)
	}{
		{
			name:      "literal substring match",
			query:     "error",
			wantValid: true,
			probes: map[string]bool{
				"an error occurred": true,
				"all good":          false,
			},
		},
		{
			name:      "case-insensitive: uppercase query matches lowercase line",
			query:     "ERROR",
			wantValid: true,
			probes: map[string]bool{
				"an error occurred": true,
				"ERROR: boom":       true,
				"nothing here":      false,
			},
		},
		{
			name:      "regex alternation ERROR|WARN",
			query:     "ERROR|WARN",
			isRegex:   true,
			wantValid: true,
			wantRe:    true,
			probes: map[string]bool{
				"ERROR: boom":   true,
				"WARN: careful": true,
				"INFO: fine":    false,
			},
		},
		{
			name:      "bad regex is invalid",
			query:     "[unclosed",
			isRegex:   true,
			wantValid: false,
		},
		{
			name:        "negation excludes matching lines",
			query:       "!healthcheck",
			allowNegate: true,
			wantValid:   true,
			probes: map[string]bool{
				"healthcheck ok": false,
				"request served": true,
			},
		},
		{
			name:      "empty query is invalid",
			query:     "",
			wantValid: false,
		},
		{
			name:        "lone bang is invalid",
			query:       "!",
			allowNegate: true,
			wantValid:   false,
		},
		{
			name:        "bang is literal when negate not allowed",
			query:       "!foo",
			allowNegate: false,
			wantValid:   true,
			probes: map[string]bool{
				"!foo here": true,
				"foo here":  false,
			},
		},
		{
			name:        "negated regex inverts the compiled pattern",
			query:       "!ERROR|WARN",
			isRegex:     true,
			allowNegate: true,
			wantValid:   true,
			wantRe:      true,
			probes: map[string]bool{
				"ERROR: boom": false,
				"INFO: fine":  true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pred, re, valid := buildMatcher(tt.query, tt.isRegex, tt.allowNegate)
			if valid != tt.wantValid {
				t.Fatalf("buildMatcher(%q, isRegex=%v, allowNegate=%v) valid = %v, want %v",
					tt.query, tt.isRegex, tt.allowNegate, valid, tt.wantValid)
			}
			if !valid {
				if pred != nil {
					t.Errorf("invalid matcher returned non-nil pred")
				}
				if re != nil {
					t.Errorf("invalid matcher returned non-nil re")
				}
				return
			}
			if pred == nil {
				t.Fatalf("valid matcher returned nil pred")
			}
			if tt.wantRe && re == nil {
				t.Errorf("expected non-nil re for regex matcher")
			}
			if !tt.wantRe && re != nil {
				t.Errorf("expected nil re for substring matcher, got %v", re)
			}
			for input, want := range tt.probes {
				if got := pred(input); got != want {
					t.Errorf("pred(%q) = %v, want %v", input, got, want)
				}
			}
		})
	}
}

func TestDeriveFiltered(t *testing.T) {
	raw := []string{"web | started", "db | error", "web | ok", "db | error again"}

	t.Run("nil pred passes all through unchanged", func(t *testing.T) {
		got := deriveFiltered(raw, nil)
		if len(got) != len(raw) {
			t.Fatalf("deriveFiltered(raw, nil) len = %d, want %d", len(got), len(raw))
		}
		for i := range got {
			if got[i] != raw[i] {
				t.Errorf("deriveFiltered(raw, nil)[%d] = %q, want %q", i, got[i], raw[i])
			}
		}
	})

	t.Run("subset selection keeps only matching lines", func(t *testing.T) {
		pred, _, valid := buildMatcher("error", false, false)
		if !valid {
			t.Fatal("expected valid matcher")
		}
		got := deriveFiltered(raw, pred)
		want := []string{"db | error", "db | error again"}
		if len(got) != len(want) {
			t.Fatalf("deriveFiltered = %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("deriveFiltered[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("no matches returns empty", func(t *testing.T) {
		pred, _, _ := buildMatcher("zzz", false, false)
		got := deriveFiltered(raw, pred)
		if len(got) != 0 {
			t.Errorf("deriveFiltered = %v, want empty", got)
		}
	})
}

func TestLogComputeMatches(t *testing.T) {
	physical := []string{
		"web | GET /health 200",
		"db  | connection established",
		"web | GET /health 500",
		"web | POST /login 200",
	}

	tests := []struct {
		name  string
		query string
		want  []int
	}{
		{
			name:  "ascending indices for multiple matches",
			query: "health",
			want:  []int{0, 2},
		},
		{
			name:  "single match",
			query: "login",
			want:  []int{3},
		},
		{
			name:  "no match returns nil",
			query: "zzz",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pred, _, _ := buildMatcher(tt.query, false, false)
			got := logComputeMatches(physical, pred)
			if len(got) != len(tt.want) {
				t.Fatalf("logComputeMatches(%q) = %v, want %v", tt.query, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("logComputeMatches(%q) = %v, want %v", tt.query, got, tt.want)
					break
				}
			}
		})
	}

	t.Run("nil pred returns nil", func(t *testing.T) {
		if got := logComputeMatches(physical, nil); got != nil {
			t.Errorf("logComputeMatches(physical, nil) = %v, want nil", got)
		}
	})

	t.Run("empty physical returns nil", func(t *testing.T) {
		pred, _, _ := buildMatcher("health", false, false)
		if got := logComputeMatches(nil, pred); got != nil {
			t.Errorf("logComputeMatches(nil, pred) = %v, want nil", got)
		}
	})
}

// TestFoldNewRawLines_CursorAdvancesByRawCount pins the survivor-cursor trap:
// the returned cursor counts RAW lines scanned, not survivors folded. Advancing
// by survivor count would re-scan an already-filtered line and duplicate it.
func TestFoldNewRawLines_CursorAdvancesByRawCount(t *testing.T) {
	raw := []string{"keep0", "drop1", "keep2"}
	pred := func(s string) bool { return !strings.Contains(s, "drop") }

	delta, scanned := foldNewRawLines(raw, 0, 80, false, false, pred)
	// Survivors: keep0, keep2 (2 lines). The cursor MUST advance to 3 (raw
	// count), not 2 (survivor count).
	if scanned != 3 {
		t.Errorf("newScanned = %d, want 3 (raw count, not survivor count)", scanned)
	}
	if delta != "keep0\nkeep2" {
		t.Errorf("delta = %q, want %q", delta, "keep0\nkeep2")
	}

	// A second fold from the advanced cursor sees no new lines — empty delta,
	// cursor unchanged. This is the anti-duplication guarantee.
	delta2, scanned2 := foldNewRawLines(raw, scanned, 80, false, false, pred)
	if delta2 != "" {
		t.Errorf("second fold delta = %q, want empty (no new raw lines)", delta2)
	}
	if scanned2 != 3 {
		t.Errorf("second fold newScanned = %d, want 3", scanned2)
	}
}

func TestFoldNewRawLines_NilPredPassesAll(t *testing.T) {
	raw := []string{"a", "b", "c"}
	delta, scanned := foldNewRawLines(raw, 0, 80, false, false, nil)
	if delta != "a\nb\nc" {
		t.Errorf("delta = %q, want %q", delta, "a\nb\nc")
	}
	if scanned != 3 {
		t.Errorf("newScanned = %d, want 3", scanned)
	}
}

// TestFoldNewRawLines_AllFilteredStillAdvances verifies the cursor still moves
// to the raw count when every new line is filtered out, so those rejected lines
// are never re-scanned on the next fold.
func TestFoldNewRawLines_AllFilteredStillAdvances(t *testing.T) {
	raw := []string{"x", "y"}
	reject := func(string) bool { return false }
	delta, scanned := foldNewRawLines(raw, 0, 80, false, false, reject)
	if delta != "" {
		t.Errorf("delta = %q, want empty (all filtered)", delta)
	}
	if scanned != 2 {
		t.Errorf("newScanned = %d, want 2 (cursor advances even when all lines filtered)", scanned)
	}
}

func TestFoldNewRawLines_NothingNewIsNoop(t *testing.T) {
	raw := []string{"a", "b"}
	delta, scanned := foldNewRawLines(raw, 2, 80, false, false, nil)
	if delta != "" || scanned != 2 {
		t.Errorf("fold with scanned==len should be a no-op, got delta=%q scanned=%d", delta, scanned)
	}
}
