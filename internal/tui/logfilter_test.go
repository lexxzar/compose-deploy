package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
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

// TestHighlightMatches proves the highlight overlay wraps matched lines with the
// style escapes yet leaves the display width untouched (ANSI is zero-width), and
// that the current match gets the bold style while other matches get the plain
// one. Non-matching lines pass through byte-identical.
func TestHighlightMatches(t *testing.T) {
	// Force a color profile so the styles emit ANSI escapes (the default test
	// profile may be Ascii, which renders styles as plain text).
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	physical := []string{
		"web | GET /health 200",
		"db  | connection established",
		"web | GET /health 500",
	}
	matches := []int{0, 2}
	cur := 2 // current match is the physical line at index 2

	got := highlightMatches(physical, matches, cur)
	if len(got) != len(physical) {
		t.Fatalf("length changed: got %d, want %d", len(got), len(physical))
	}

	// Width invariant: every rendered line has the same display width as its
	// unstyled source — the style adds only zero-width ANSI escapes.
	for i := range physical {
		if w1, w2 := ansi.StringWidth(got[i]), ansi.StringWidth(physical[i]); w1 != w2 {
			t.Errorf("line %d width changed: got %d, want %d", i, w1, w2)
		}
	}

	// Non-matching line passes through unchanged (no style bytes).
	if got[1] != physical[1] {
		t.Errorf("non-matching line mutated: got %q, want %q", got[1], physical[1])
	}
	if strings.Contains(got[1], "\x1b[") {
		t.Errorf("non-matching line should carry no ANSI escapes: %q", got[1])
	}

	// Matching lines carry style escapes; the raw text is still present.
	for _, i := range matches {
		if !strings.Contains(got[i], "\x1b[") {
			t.Errorf("matched line %d should carry ANSI style escapes: %q", i, got[i])
		}
		if !strings.Contains(got[i], physical[i]) {
			t.Errorf("matched line %d should still contain its raw text: %q", i, got[i])
		}
	}

	// The current match (index 2) must render with the bold current style, which
	// differs from the plain match style used on the other match (index 0).
	if got[2] == logSearchMatchStyle.Render(physical[2]) {
		t.Error("current match should use the bold current style, not the plain match style")
	}
	if got[2] != logSearchCurrentStyle.Render(physical[2]) {
		t.Errorf("current match not styled with logSearchCurrentStyle: %q", got[2])
	}
	if got[0] != logSearchMatchStyle.Render(physical[0]) {
		t.Errorf("non-current match not styled with logSearchMatchStyle: %q", got[0])
	}

	t.Run("empty matches returns input unchanged", func(t *testing.T) {
		out := highlightMatches(physical, nil, -1)
		for i := range physical {
			if out[i] != physical[i] {
				t.Errorf("line %d mutated with no matches: got %q, want %q", i, out[i], physical[i])
			}
		}
	})

	t.Run("cur == -1 skips the bold pass", func(t *testing.T) {
		out := highlightMatches(physical, matches, -1)
		if out[0] != logSearchMatchStyle.Render(physical[0]) ||
			out[2] != logSearchMatchStyle.Render(physical[2]) {
			t.Error("with cur=-1 all matches should use the plain match style")
		}
	})
}

// TestFoldNewRawLines_CursorAdvancesByRawCount pins the survivor-cursor trap:
// the returned cursor counts RAW lines scanned, not survivors folded. Advancing
// by survivor count would re-scan an already-filtered line and duplicate it.
func TestFoldNewRawLines_CursorAdvancesByRawCount(t *testing.T) {
	raw := []string{"keep0", "drop1", "keep2"}
	pred := func(s string) bool { return !strings.Contains(s, "drop") }

	delta, scanned, survivors := foldNewRawLines(raw, 0, 80, false, false, pred)
	// Survivors: keep0, keep2 (2 lines). The cursor MUST advance to 3 (raw
	// count), not 2 (survivor count).
	if scanned != 3 {
		t.Errorf("newScanned = %d, want 3 (raw count, not survivor count)", scanned)
	}
	if delta != "keep0\nkeep2" {
		t.Errorf("delta = %q, want %q", delta, "keep0\nkeep2")
	}
	// The survivor count is the incremental signal the caller accumulates into
	// logFilterShown — it MUST report folded survivors (2), not raw scanned (3).
	if survivors != 2 {
		t.Errorf("survivors = %d, want 2 (folded survivors, not raw count)", survivors)
	}

	// A second fold from the advanced cursor sees no new lines — empty delta,
	// cursor unchanged, zero survivors. This is the anti-duplication guarantee.
	delta2, scanned2, survivors2 := foldNewRawLines(raw, scanned, 80, false, false, pred)
	if delta2 != "" {
		t.Errorf("second fold delta = %q, want empty (no new raw lines)", delta2)
	}
	if scanned2 != 3 {
		t.Errorf("second fold newScanned = %d, want 3", scanned2)
	}
	if survivors2 != 0 {
		t.Errorf("second fold survivors = %d, want 0 (no new raw lines)", survivors2)
	}
}

func TestFoldNewRawLines_NilPredPassesAll(t *testing.T) {
	raw := []string{"a", "b", "c"}
	delta, scanned, survivors := foldNewRawLines(raw, 0, 80, false, false, nil)
	if delta != "a\nb\nc" {
		t.Errorf("delta = %q, want %q", delta, "a\nb\nc")
	}
	if scanned != 3 {
		t.Errorf("newScanned = %d, want 3", scanned)
	}
	if survivors != 3 {
		t.Errorf("survivors = %d, want 3 (nil pred passes all)", survivors)
	}
}

// TestFoldNewRawLines_AllFilteredStillAdvances verifies the cursor still moves
// to the raw count when every new line is filtered out, so those rejected lines
// are never re-scanned on the next fold.
func TestFoldNewRawLines_AllFilteredStillAdvances(t *testing.T) {
	raw := []string{"x", "y"}
	reject := func(string) bool { return false }
	delta, scanned, survivors := foldNewRawLines(raw, 0, 80, false, false, reject)
	if delta != "" {
		t.Errorf("delta = %q, want empty (all filtered)", delta)
	}
	if scanned != 2 {
		t.Errorf("newScanned = %d, want 2 (cursor advances even when all lines filtered)", scanned)
	}
	if survivors != 0 {
		t.Errorf("survivors = %d, want 0 (all filtered)", survivors)
	}
}

func TestFoldNewRawLines_NothingNewIsNoop(t *testing.T) {
	raw := []string{"a", "b"}
	delta, scanned, survivors := foldNewRawLines(raw, 2, 80, false, false, nil)
	if delta != "" || scanned != 2 || survivors != 0 {
		t.Errorf("fold with scanned==len should be a no-op, got delta=%q scanned=%d survivors=%d", delta, scanned, survivors)
	}
}
