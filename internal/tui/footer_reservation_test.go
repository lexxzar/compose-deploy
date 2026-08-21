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

	// Width labels: 160 only became one-line with the byte-vs-cell width fix —
	// the old len() guard over-counted 2 cells per `•` and split the footer up
	// to 173 columns. 80 is the baseline the trimmed footer must fit. 40 still
	// falls back to two lines.
	for _, width := range []int{160 /* one-line help */, 80 /* one-line help */, 40 /* two-line help */} {
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

// containerFooterSearchStates drives the container footer through its three
// search states. line2 is replaced wholesale while a search is open or
// committed, so anything that must always be visible has to live in line1.
var containerFooterSearchStates = []struct {
	name  string
	setup func(m *Model)
}{
	{"idle", func(m *Model) {}},
	{"searching", func(m *Model) {
		m.searchInput = textinput.New()
		m.searchInput.SetValue("web")
		m.searchInput.Focus()
		m.searching = true
		m.searchQuery = "web"
		m.searchMatches = computeMatches(m.services, "web")
	}},
	{"committed", func(m *Model) {
		m.searchInput = textinput.New()
		m.searchQuery = "web"
		m.searchMatches = computeMatches(m.services, "web")
	}},
}

// TestContainerFooter_AlwaysShowsBackAndKeys pins that the back key and the `?`
// pointer survive every search state. Both call sites replace line2 wholesale
// while a search is open or committed, so a token parked there would vanish in
// exactly the state where the overlay matters most — `/ search` has already
// left the footer by then.
func TestContainerFooter_AlwaysShowsBackAndKeys(t *testing.T) {
	for _, picker := range []bool{true, false} {
		back := "q quit"
		if picker {
			back = "q back"
		}
		for _, st := range containerFooterSearchStates {
			m := Model{}
			m.screen = screenSelectContainers
			m.services = []string{"web", "db"}
			m.selected = map[int]bool{}
			m.showPicker = picker
			m.width, m.height = 80, 24
			st.setup(&m)

			out := m.viewSelectContainers()
			for _, tok := range []string{back, "? keys"} {
				if !strings.Contains(out, tok) {
					t.Errorf("showPicker=%v state=%s: footer missing %q; got:\n%s", picker, st.name, tok, out)
				}
			}
		}
	}
}

// TestContainerFooter_OneLineFitsEighty pins the width budget the trim was made
// for: the joined one-line footer plus the 2-cell guard margin must fit an
// 80-column terminal in every search state, so the two-line fallback stops
// firing on a normal terminal and the list keeps that row.
func TestContainerFooter_OneLineFitsEighty(t *testing.T) {
	for _, back := range []string{"q quit", "q back"} {
		line1, idle := containerHelpLines(back)
		states := []struct{ name, line2 string }{
			{"idle", idle},
			{"searching", "  enter jump  •  esc cancel"},
			{"committed", "  n/N cycle  •  esc clear"},
		}
		for _, st := range states {
			oneLine := line1 + "  •  " + st.line2[2:]
			if w := ansi.StringWidth(oneLine) + 2; w > 80 {
				t.Errorf("back=%q state=%s: one-line footer needs %d columns, must fit 80: %q",
					back, st.name, w, oneLine)
			}
		}
	}
}

// TestContainerFooter_RendersOneLineAtEighty proves the criterion the trim was
// made for: at width 80 the footer is ONE physical line in every search state.
// TestContainerFooter_OneLineFitsEighty measures the budget; this one measures
// the actual render. `? keys` comes from line1 and the probe token opens line2,
// so finding both on one physical line proves the one-line branch was taken.
func TestContainerFooter_RendersOneLineAtEighty(t *testing.T) {
	line2Head := map[string]string{
		"idle":      "d deploy",
		"searching": "enter jump",
		"committed": "n/N cycle",
	}

	for _, picker := range []bool{true, false} {
		for _, st := range containerFooterSearchStates {
			m := Model{}
			m.screen = screenSelectContainers
			m.services = []string{"web", "db"}
			m.selected = map[int]bool{}
			m.showPicker = picker
			m.width, m.height = 80, 24
			st.setup(&m)

			var found bool
			for _, line := range strings.Split(m.viewSelectContainers(), "\n") {
				if !strings.Contains(line, "? keys") {
					continue
				}
				found = true
				if !strings.Contains(line, line2Head[st.name]) {
					t.Errorf("showPicker=%v state=%s: footer split into two lines at width 80 (%q missing from the `? keys` line): %q",
						picker, st.name, line2Head[st.name], line)
				}
				if w := ansi.StringWidth(line); w > 80 {
					t.Errorf("showPicker=%v state=%s: footer line is %d cells, exceeds width 80: %q",
						picker, st.name, w, line)
				}
			}
			if !found {
				t.Errorf("showPicker=%v state=%s: no footer line contains `? keys`", picker, st.name)
			}
		}
	}
}

// oldContainerHelpLine2 is the pre-trim 12-token footer legend, copied verbatim
// from the const it replaced. Kept here — and only here — so the row gain below
// is proven against the previous build instead of asserted from memory.
const oldContainerHelpLine2 = "  r restart  •  d deploy  •  s stop  •  R rollback  •  l logs  •  c config  •  x exec  •  U updates"

// oldContainerFooterLines replays the pre-change footer-height math: the old
// line1, the old line2 legend, and the old BYTE-counting width guard (each `•`
// is 3 bytes but one display cell, so len() over-counted 2 per separator).
func oldContainerFooterLines(m Model) int {
	back := "q quit"
	if m.showPicker {
		back = "q back"
	}
	line1 := fmt.Sprintf("  space toggle  •  a all  •  / search  •  %s", back)
	line2 := oldContainerHelpLine2
	if m.searching {
		line2 = "  enter jump  •  esc cancel"
	} else if m.searchQuery != "" {
		line2 = "  n/N cycle  •  esc clear"
	}
	oneLine := line1 + "  •  " + line2[2:]
	if m.width >= len(oneLine)+2 {
		return 3
	}
	return 4
}

// TestSvcVisibleCount_GainsRowVersusOldFooter proves the "the service list
// gains one row at widths between 76 and 173" criterion. The header math is
// identical in both builds (3 lines, no status columns here), so the visible
// count differs only by the footer height — which oldContainerFooterLines
// reproduces exactly. 76 is the new one-line threshold (74 cells + the 2-cell
// guard margin); 174 is the old byte-counting one.
func TestSvcVisibleCount_GainsRowVersusOldFooter(t *testing.T) {
	const headerLines = 3

	svcs := make([]string, 30)
	for i := range svcs {
		svcs[i] = fmt.Sprintf("svc%02d", i)
	}
	svcs[0] = "web" // so the committed-search state has a real match

	build := func(width int, picker bool) Model {
		m := Model{}
		m.screen = screenSelectContainers
		m.services = svcs
		m.selected = map[int]bool{}
		m.height = 24
		m.width = width
		m.showPicker = picker
		return m
	}

	// Idle: the exact boundary. gain 1 from 76 through 173, 0 outside.
	for _, tc := range []struct{ width, gain int }{
		{40, 0}, {60, 0}, {75, 0},
		{76, 1}, {80, 1}, {100, 1}, {120, 1}, {160, 1}, {173, 1},
		{174, 0}, {200, 0},
	} {
		for _, picker := range []bool{true, false} {
			m := build(tc.width, picker)
			old := m.height - headerLines - oldContainerFooterLines(m)
			got := m.svcVisibleCount()
			if got != old+tc.gain {
				t.Errorf("width=%d showPicker=%v: svcVisibleCount()=%d, old build=%d, want a gain of %d",
					tc.width, picker, got, old, tc.gain)
			}
		}
	}

	// Across every width and search state the new footer must never cost a row.
	for _, st := range containerFooterSearchStates {
		for width := 20; width <= 200; width++ {
			m := build(width, true)
			st.setup(&m)
			old := m.height - headerLines - oldContainerFooterLines(m)
			if got := m.svcVisibleCount(); got < old {
				t.Errorf("width=%d state=%s: svcVisibleCount()=%d regressed below the old build's %d",
					width, st.name, got, old)
			}
		}
	}
}
