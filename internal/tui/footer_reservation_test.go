package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// TestContainerFooterReservation is a regression pin for the container-list
// footer-line reservation. It renders a windowed 30-service list at a fixed
// terminal height and asserts two invariants that the search feature's initial
// footer accounting violated:
//
//  1. The number of service rows physically rendered EXACTLY equals
//     svcVisibleCount() — the reservation must not over- or under-count.
//     The search feature's first cut over-reserved one footer line (it counted
//     the reserved search-bar line separately from helpStyle's MarginTop(1)
//     blank, but the bar content — written with no trailing newline immediately
//     before helpStyle.Render — merges onto that same physical line), so the
//     list showed ONE FEWER service than fit at a given height.
//
//  2. The total physical line count is CONSTANT across idle / searching /
//     committed / confirming — the "height never jumps" invariant.
//
// Together these pin both that the reservation is honest (rows == count) and
// that switching search states does not resize the list.
func TestContainerFooterReservation(t *testing.T) {
	const nServices = 30
	svcs := make([]string, nServices)
	for i := range svcs {
		svcs[i] = fmt.Sprintf("svc%02d", i)
	}

	newModel := func(width int) Model {
		m := Model{}
		m.screen = screenSelectContainers
		m.services = svcs
		m.selected = map[int]bool{}
		m.height = 24
		m.width = width
		m.showPicker = true
		return m
	}

	countServiceRows := func(out string) int {
		n := 0
		for _, s := range svcs {
			if strings.Contains(out, s) {
				n++
			}
		}
		return n
	}
	physLines := func(out string) int {
		return strings.Count(strings.TrimRight(out, "\n"), "\n") + 1
	}

	type state struct {
		name  string
		setup func(m *Model)
	}
	states := []state{
		{"idle", func(m *Model) {}},
		{"searching", func(m *Model) {
			m.searchInput = textinput.New()
			m.searchInput.SetValue("svc1")
			m.searchInput.Focus()
			m.searching = true
			m.searchQuery = "svc1"
			m.searchMatches = computeMatches(m.services, "svc1")
			m.svcCursor = m.searchMatches[0]
		}},
		{"committed", func(m *Model) {
			m.searchInput = textinput.New()
			m.searchQuery = "svc1"
			m.searchMatches = computeMatches(m.services, "svc1")
			m.svcCursor = m.searchMatches[0]
		}},
		{"confirming", func(m *Model) {
			m.confirming = true
			m.selected = map[int]bool{0: true}
		}},
		{"confirming+committed", func(m *Model) {
			m.searchInput = textinput.New()
			m.searchQuery = "svc1"
			m.searchMatches = computeMatches(m.services, "svc1")
			m.confirming = true
			m.selected = map[int]bool{0: true}
		}},
	}

	for _, width := range []int{160 /* one-line help */, 40 /* two-line help */} {
		var firstPhys int
		for i, st := range states {
			m := newModel(width)
			st.setup(&m)
			want := m.svcVisibleCount()
			out := m.viewSelectContainers()
			rows := countServiceRows(out)
			phys := physLines(out)

			if rows != want {
				t.Errorf("width=%d state=%s: rendered %d service rows, svcVisibleCount()=%d (reservation must match rendered rows)",
					width, st.name, rows, want)
			}
			if i == 0 {
				firstPhys = phys
			} else if phys != firstPhys {
				t.Errorf("width=%d state=%s: physical line count %d differs from idle %d (list height must stay constant across search states)",
					width, st.name, phys, firstPhys)
			}
		}
	}
}

// TestSearchBarLineNeverWraps is a regression pin for the search bar's
// one-physical-line invariant. svcVisibleCount() reserves exactly ONE row for
// the reserved bar; if the bar's DISPLAY width exceeds m.width the terminal
// wraps it, pushing the footer past its reserved space (the list overflows /
// the cursor scrolls off). This test drives NARROW terminals (widths 40, 20, and
// the EXTREME 5 and 1 — both smaller than any counter suffix like "  (no match)")
// with LONG content — a long typed query, a long committed match name, and the
// off-match committed form (query + hint) — and asserts the rendered bar's
// ANSI-aware display width is clamped to m.width in every mode. Widths 5/1 pin
// the unconditional-final-clamp fix: the per-mode counter-budgeting heuristic
// leaves the typing-mode suffix un-truncated when m.width < suffix width, so
// only the final clampToWidth guarantees the invariant there. It FAILS under
// the pre-fix code (typing-mode bar measured display width 12 at both width 5
// and width 1 — the un-truncated "  (no match)" suffix).
func TestSearchBarLineNeverWraps(t *testing.T) {
	longNames := []string{
		"this-is-an-extremely-long-service-name-way-past-forty-columns-alpha",
		"this-is-an-extremely-long-service-name-way-past-forty-columns-beta",
		"api",
	}
	longQuery := "this-is-an-extremely-long-search-query-that-overflows-a-narrow-terminal"

	build := func(width int) Model {
		m := Model{}
		m.screen = screenSelectContainers
		m.services = longNames
		m.selected = map[int]bool{}
		m.height = 24
		m.width = width
		m.showPicker = true
		m.searchInput = textinput.New()
		return m
	}

	// mode setup: searching (typing a long query), committed-on-match (cursor on
	// a long matching name), committed-off-match (cursor moved off all matches).
	modes := []struct {
		name  string
		setup func(m *Model)
	}{
		{"searching-long-query", func(m *Model) {
			m.searchInput.SetValue(longQuery)
			m.searchInput.Focus()
			m.searching = true
			m.searchQuery = longQuery
			m.searchMatches = computeMatches(m.services, longQuery)
		}},
		{"committed-on-long-match", func(m *Model) {
			m.searchQuery = "extremely-long-service"
			m.searchMatches = computeMatches(m.services, "extremely-long-service")
			m.svcCursor = m.searchMatches[0]
		}},
		{"committed-off-match", func(m *Model) {
			m.searchQuery = "extremely-long-service"
			m.searchMatches = computeMatches(m.services, "extremely-long-service")
			m.svcCursor = 2 // "api" is not a match → off-match committed form
		}},
		{"typing-no-match", func(m *Model) {
			// The failing case at extreme narrow widths: the "  (no match)"
			// suffix (width 12) is wider than m.width, so the per-mode left-
			// budget truncates the left to nothing but WITHOUT the final clamp
			// the un-truncated suffix still exceeds m.width and wraps.
			m.searchInput.SetValue("zzz-no-such-service")
			m.searchInput.Focus()
			m.searching = true
			m.searchQuery = "zzz-no-such-service"
			m.searchMatches = computeMatches(m.services, "zzz-no-such-service")
		}},
	}

	for _, width := range []int{40, 20, 5, 1} {
		for _, md := range modes {
			m := build(width)
			md.setup(&m)
			bar := m.searchBarLine()
			if w := ansi.StringWidth(bar); w > width {
				t.Errorf("width=%d mode=%s: search bar display width %d exceeds terminal width — it will wrap to a second physical line (bar=%q)",
					width, md.name, w, bar)
			}
			// The clamped bar must contain no embedded newline either.
			if strings.Contains(bar, "\n") {
				t.Errorf("width=%d mode=%s: search bar contains an embedded newline (bar=%q)", width, md.name, bar)
			}
		}
	}

	// width == 0 (unknown terminal size in some tests): must NOT truncate to
	// nothing — return the full bar unchanged, mirroring svcVisibleCount's
	// height==0 fallback.
	m := build(0)
	modes[0].setup(&m)
	if bar := m.searchBarLine(); ansi.StringWidth(bar) == 0 {
		t.Errorf("width=0: search bar truncated to empty; expected full unclamped content")
	}
}

// TestSearchInputWidthPersists pins finding #2: searchInput.Width must be set on
// the PERSISTED model (in the '/' open handler and the typing intercept, both of
// which return a stored model), NOT inside the value-receiver searchBarLine()
// where the assignment lands on a throwaway copy. When Width is bounded, bubbles'
// textinput scrolls the value horizontally to keep the cursor visible; without
// it, a long query is clipped from the right by the outer clamp — hiding the
// cursor / the most-recently-typed characters. The test drives the real
// Update()/handleKey paths and asserts Width == searchInputWidth() on the model
// that would be stored back. It FAILS under the pre-fix code (Width stays 0).
func TestSearchInputWidthPersists(t *testing.T) {
	base := func() Model {
		m := Model{}
		m.screen = screenSelectContainers
		m.services = []string{"web-frontend", "api", "postgres"}
		m.selected = map[int]bool{}
		m.height = 24
		m.width = 80
		return m
	}

	// '/' open: fresh input must have Width bounded on the returned model.
	m := base()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	om := updated.(Model)
	if !om.searching {
		t.Fatal("precondition: not in searching mode after /")
	}
	if want := om.searchInputWidth(); om.searchInput.Width != want {
		t.Errorf("after / open: searchInput.Width = %d, want %d (persisted budget)", om.searchInput.Width, want)
	}
	if om.searchInput.Width <= 0 {
		t.Errorf("after / open: searchInput.Width = %d, want > 0 at width 80 (bubbles never scrolls to keep cursor visible)", om.searchInput.Width)
	}

	// Type a character: the typing intercept must re-persist Width on the
	// returned model (the counter suffix — and thus the budget — changes with
	// the match count).
	typed, _ := om.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	tm := typed.(Model)
	if tm.searchQuery != "w" {
		t.Fatalf("precondition: searchQuery = %q, want \"w\"", tm.searchQuery)
	}
	if want := tm.searchInputWidth(); tm.searchInput.Width != want {
		t.Errorf("after typing: searchInput.Width = %d, want %d (re-persisted budget)", tm.searchInput.Width, want)
	}

	// width == 0 (unknown size): Width must be left unbounded (0) so tests that
	// never set m.width behave as before.
	m0 := base()
	m0.width = 0
	updated0, _ := m0.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if w := updated0.(Model).searchInput.Width; w != 0 {
		t.Errorf("width=0: searchInput.Width = %d, want 0 (unbounded)", w)
	}
}

// TestSearchInputWidthStableAcrossCounterChange pins the codex-iter-3 root cause:
// searchInputWidth() must NOT depend on the volatile current-match counter, so
// the input's Width — and thus bubbles' horizontal-scroll offset — stays constant
// as the counter grows/shrinks with each keystroke. Here typing crosses a
// counter-width boundary ("(1/1)"/"(2/2)" while matching → "(no match)" once the
// query stops matching, then back to matching). Under the pre-fix code the budget
// tracked the live counter and jumped per keystroke; now it must be identical at
// every step (and equal to searchInputWidth() on the stored model), while the
// rendered bar never exceeds m.width. Driven at a narrow width so the effect is
// load-bearing.
func TestSearchInputWidthStableAcrossCounterChange(t *testing.T) {
	base := func() Model {
		m := Model{}
		m.screen = screenSelectContainers
		m.services = []string{"web", "api", "worker"} // 3 services → stable max counter
		m.selected = map[int]bool{}
		m.height = 24
		m.width = 22 // narrow: budget arithmetic is load-bearing
		return m
	}

	m := base()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	om := updated.(Model)
	if !om.searching {
		t.Fatal("precondition: not searching after /")
	}

	// The budget is a fixed property of the service set + terminal width, so it
	// must equal maxCounterWidth-derived searchInputWidth() and NOT change as the
	// live counter changes below. Capture it right after open.
	wantWidth := om.searchInputWidth()
	if om.searchInput.Width != wantWidth {
		t.Fatalf("after open: searchInput.Width = %d, want %d", om.searchInput.Width, wantWidth)
	}

	// Type a sequence that walks the counter through match → no-match → match:
	// "w" matches web/worker → "(1/2)"; "we" matches web → "(1/1)"; "wez" matches
	// nothing → "(no match)"; backspace back to "we" → "(1/1)".
	cur := om
	feed := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'w'}},
		{Type: tea.KeyRunes, Runes: []rune{'e'}},
		{Type: tea.KeyRunes, Runes: []rune{'z'}},
		{Type: tea.KeyBackspace},
	}
	for i, k := range feed {
		next, _ := cur.Update(k)
		cur = next.(Model)
		// searchInputWidth() itself must be stable (independent of match count).
		if got := cur.searchInputWidth(); got != wantWidth {
			t.Errorf("step %d (%q): searchInputWidth() = %d, want stable %d — budget tracked the live counter",
				i, cur.searchQuery, got, wantWidth)
		}
		// The persisted input Width must match it (set before Update, keystroke-stable).
		if cur.searchInput.Width != wantWidth {
			t.Errorf("step %d (%q): searchInput.Width = %d, want %d",
				i, cur.searchQuery, cur.searchInput.Width, wantWidth)
		}
		// The rendered bar must still never wrap.
		if bar := cur.searchBarLine(); ansi.StringWidth(bar) > cur.width {
			t.Errorf("step %d (%q): bar display width %d exceeds m.width=%d (bar=%q)",
				i, cur.searchQuery, ansi.StringWidth(bar), cur.width, bar)
		}
	}
}

// TestSearchInputWidthTracksResize pins finding #2: a WindowSizeMsg while a search
// is open must refresh searchInput.Width to the new width, otherwise the input's
// horizontal-scroll viewport is stale (right-clipped) after a narrower resize
// until the next keystroke. It drives a real WindowSizeMsg through Update() and
// asserts the stored model's searchInput.Width tracks searchInputWidth() for the
// new width, both narrowing and widening.
func TestSearchInputWidthTracksResize(t *testing.T) {
	m := Model{}
	m.screen = screenSelectContainers
	m.services = []string{"web-frontend", "api", "postgres"}
	m.selected = map[int]bool{}
	m.height = 24
	m.width = 100

	// Open search at width 100.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	om := updated.(Model)
	if !om.searching {
		t.Fatal("precondition: not searching after /")
	}
	if om.searchInput.Width != om.searchInputWidth() {
		t.Fatalf("after open: searchInput.Width = %d, want %d", om.searchInput.Width, om.searchInputWidth())
	}

	for _, newWidth := range []int{40, 120, 30} {
		resized, _ := om.Update(tea.WindowSizeMsg{Width: newWidth, Height: 24})
		rm := resized.(Model)
		if rm.width != newWidth {
			t.Fatalf("resize to %d: m.width = %d", newWidth, rm.width)
		}
		if want := rm.searchInputWidth(); rm.searchInput.Width != want {
			t.Errorf("resize to %d: searchInput.Width = %d, want %d (stale viewport not refreshed)",
				newWidth, rm.searchInput.Width, want)
		}
		om = rm // carry forward so widening-after-narrowing is exercised
	}
}
