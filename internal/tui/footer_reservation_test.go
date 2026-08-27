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
		m.setSingleGroup(svcs)
		m.selected = map[string]bool{}
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
	commitSearch := func(m *Model) {
		m.searchInput = textinput.New()
		m.searchQuery = "svc1"
		m.searchMatches = computeMatches(m.svcEntries, "svc1")
		m.svcCursor = m.searchMatches[0]
	}
	typeSearch := func(m *Model) {
		commitSearch(m)
		m.searchInput.SetValue("svc1")
		m.searchInput.Focus()
		m.searching = true
	}
	states := []state{
		{"idle", func(m *Model) {}},
		{"searching", typeSearch},
		{"committed", commitSearch},
		{"confirming", func(m *Model) {
			m.confirming = true
			m.selected = selectedIdx(*m, 0)
		}},
		{"confirming+committed", func(m *Model) {
			commitSearch(m)
			m.confirming = true
			m.selected = selectedIdx(*m, 0)
		}},
	}
	// A read-only composer reaches the confirm prompt only through the x exec
	// key — d/r/s/R are gated — so the two confirming states are re-armed with
	// pendingExec. The footer text differs (containerHelpLines branches on
	// readOnly), which is exactly why the reservation has to be swept again:
	// the one-line/two-line threshold sits at a different width.
	readOnlyStates := []state{
		{"idle", func(m *Model) {}},
		{"searching", typeSearch},
		{"committed", commitSearch},
		{"exec-confirming", func(m *Model) {
			m.confirming = true
			m.pendingExec = true
		}},
		{"exec-confirming+committed", func(m *Model) {
			commitSearch(m)
			m.confirming = true
			m.pendingExec = true
		}},
	}

	// Swept, not spot-checked: the one-line/two-line threshold moves with the
	// footer text, so a drift of a few cells would otherwise slip between three
	// sample widths (76 is the current boundary; 160 only became one-line with
	// the byte-vs-cell width fix). Every width from 40 to 180 is cheap and pins
	// the honest-reservation invariant across the whole boundary band.
	for _, readOnly := range []bool{false, true} {
		sts := states
		if readOnly {
			sts = readOnlyStates
		}
		for width := 40; width <= 180; width++ {
			var firstPhys, firstRows int
			for i, st := range sts {
				m := newModel(width)
				if readOnly {
					m.composer = &readOnlyMockComposer{}
				}
				st.setup(&m)
				want := m.svcVisibleCount()
				out := m.viewSelectContainers()
				rows := countServiceRows(out)
				phys := physLines(out)

				if rows != want {
					t.Errorf("readOnly=%v width=%d state=%s: rendered %d service rows, svcVisibleCount()=%d (reservation must match rendered rows)",
						readOnly, width, st.name, rows, want)
				}
				if i == 0 {
					firstPhys, firstRows = phys, rows
					continue
				}
				if phys != firstPhys {
					t.Errorf("readOnly=%v width=%d state=%s: physical line count %d differs from idle %d (list height must stay constant across search states)",
						readOnly, width, st.name, phys, firstPhys)
				}
				// The VISIBLE-row count is the invariant a user sees. The physical
				// count above is trivially constant (svcVisibleCount absorbs any
				// footer delta by growing the list), so it cannot catch a footer
				// whose line count varies by search state — which is exactly how a
				// search-only footer once grew the list by a row below width 76.
				if rows != firstRows {
					t.Errorf("readOnly=%v width=%d state=%s: %d visible service rows vs idle %d (the list must not re-flow when the search bar opens)",
						readOnly, width, st.name, rows, firstRows)
				}
			}
		}
	}
}

// TestContainerView_ExcessLineWidthIsPaddingOnly closes the gap an external
// review raised: containerFooter() is appended to the reserved search-bar row
// with NO newline between them, and lipgloss's MarginTop(1) renders as a run of
// spaces followed by "\n" (applyMargins prepends strings.Repeat(" ", width)+"\n").
// So the bar row in the View() string really does measure wider than m.width
// whenever the bar is full — the question is whether any of the excess is
// CONTENT, which would be pushed off screen.
//
// It is not, and this sweep proves it: at every width and search state the
// excess is trailing whitespace only. bubbletea's standardRenderer.flush()
// truncates each line with ansi.Truncate(line, r.width, "") before writing, so
// that padding never reaches the terminal and cannot wrap the row. The second
// assertion pins the consequence the reservation math cares about — the frame
// stays exactly m.height physical lines.
//
// The existing reservation sweep uses queries that MATCH a service; this one
// drives a maximally long query so the bar clamps to exactly m.width, the
// worst case for the merge.
//
// The readOnly dimension is swept for the same reason its neighbour above
// sweeps it: containerFooterLines picks one line or two from the IDLE pair, and
// on the write path the committed-search substitution is SHORTER than the idle
// line2, so the one-line branch always held it. The read-only pair inverts
// that — idle line2 is `l logs • x exec` (19 cells) and the substitution is
// `n/N cycle • esc clear` (25) — which pushed 1-4 cells of CONTENT past the
// width at 43-46 until containerFooter clamped its rendered string.
func TestContainerView_ExcessLineWidthIsPaddingOnly(t *testing.T) {
	const nServices = 30
	svcs := make([]string, nServices)
	for i := range svcs {
		svcs[i] = fmt.Sprintf("svc%02d", i)
	}
	longQuery := strings.Repeat("z", 200)

	states := []struct {
		name  string
		setup func(m *Model)
	}{
		{"idle", func(m *Model) {}},
		{"typing-overlong-no-match", func(m *Model) {
			m.searchInput.SetValue(longQuery)
			m.searchInput.Focus()
			m.searching = true
			m.searchQuery = longQuery
		}},
		{"typing-overlong-match", func(m *Model) {
			q := "svc1" + longQuery[:100]
			m.searchInput.SetValue(q)
			m.searchInput.Focus()
			m.searching = true
			m.searchQuery = "svc1"
			m.searchMatches = computeMatches(m.svcEntries, "svc1")
			m.svcCursor = m.searchMatches[0]
		}},
		{"committed", func(m *Model) {
			m.searchQuery = "svc1"
			m.searchMatches = computeMatches(m.svcEntries, "svc1")
			m.svcCursor = m.searchMatches[0]
		}},
	}

	// The confirming state is deliberately absent: its prompt is built with a
	// bare fmt.Sprintf and no clampToWidth, so it overflows below ~46 columns.
	// That predates the search bar and the footer trim alike (same code on
	// main) and is out of scope here.
	for _, readOnly := range []bool{false, true} {
		for width := 40; width <= 180; width++ {
			for _, st := range states {
				m := Model{}
				m.screen = screenSelectContainers
				m.setSingleGroup(svcs)
				m.selected = map[string]bool{}
				m.height = 24
				m.width = width
				m.showPicker = true
				m.searchInput = textinput.New()
				if readOnly {
					m.composer = &readOnlyMockComposer{}
				}
				st.setup(&m)

				out := m.viewSelectContainers()
				lines := strings.Split(out, "\n")
				if len(lines) != m.height {
					t.Fatalf("readOnly=%v width=%d state=%s: %d physical lines, want %d",
						readOnly, width, st.name, len(lines), m.height)
				}
				for i, ln := range lines {
					visible := strings.TrimRight(ansi.Strip(ln), " ")
					if w := ansi.StringWidth(visible); w > width {
						t.Errorf("readOnly=%v width=%d state=%s: line %d carries %d cells of CONTENT past the width: %q",
							readOnly, width, st.name, i, w, visible)
					}
				}
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
		m.setSingleGroup(longNames)
		m.selected = map[string]bool{}
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
			m.searchMatches = computeMatches(m.svcEntries, longQuery)
		}},
		{"committed-on-long-match", func(m *Model) {
			m.searchQuery = "extremely-long-service"
			m.searchMatches = computeMatches(m.svcEntries, "extremely-long-service")
			m.svcCursor = m.searchMatches[0]
		}},
		{"committed-off-match", func(m *Model) {
			m.searchQuery = "extremely-long-service"
			m.searchMatches = computeMatches(m.svcEntries, "extremely-long-service")
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
			m.searchMatches = computeMatches(m.svcEntries, "zzz-no-such-service")
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
		m.setSingleGroup([]string{"web-frontend", "api", "postgres"})
		m.selected = map[string]bool{}
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
		m.setSingleGroup([]string{"web", "api", "worker"}) // 3 services → stable max counter
		m.selected = map[string]bool{}
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
	m.setSingleGroup([]string{"web-frontend", "api", "postgres"})
	m.selected = map[string]bool{}
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
// search states. line2 is replaced wholesale once a search is committed, so
// anything that must survive that state has to live in line1; while the input
// is OPEN the footer drops both lines for a search-specific one.
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
		m.searchMatches = computeMatches(m.svcEntries, "web")
	}},
	{"committed", func(m *Model) {
		m.searchInput = textinput.New()
		m.searchQuery = "web"
		m.searchMatches = computeMatches(m.svcEntries, "web")
	}},
}

// containerFooterModel is the 80x24 two-service model both footer-token pins
// drive through containerFooterSearchStates.
func containerFooterModel(picker bool) Model {
	m := Model{}
	m.screen = screenSelectContainers
	m.setSingleGroup([]string{"web", "db"})
	m.selected = map[string]bool{}
	m.showPicker = picker
	m.width, m.height = 80, 24
	return m
}

// TestContainerFooter_AdvertisesOnlyWorkingKeys pins the footer against what
// the keymap actually binds in each search state.
//
// Idle and committed: line2 is replaced wholesale once a search commits, so the
// back key and the `?` pointer must live in line1 — `q` and `?` both work
// there, and that is exactly when the overlay is most useful.
//
// Searching: the typing intercept routes every key except enter/esc/ctrl+c into
// the query, so `space`, `q` and `?` are literal runes. The footer must NOT
// name them; it names enter and esc instead.
func TestContainerFooter_AdvertisesOnlyWorkingKeys(t *testing.T) {
	want := map[string][]string{
		"idle":      {"? keys"},
		"committed": {"? keys"},
		"searching": {"enter jump", "esc cancel"},
	}
	// line2, which only the idle footer shows: a committed search swaps it and
	// the typing state replaces the whole footer. The read-only pair drops the
	// three write-path tokens for the two inspection keys that still work.
	wantIdleLine2 := map[bool][]string{
		false: {"space toggle", "d deploy", "r restart", "l logs"},
		true:  {"l logs", "x exec"},
	}
	unwanted := map[string][]string{
		"searching": {"? keys", "space toggle", "q back", "q quit"},
	}
	// Gated on a read-only composer, so absent from every search state — AC5:
	// no key and no widget advertises a no-op.
	unwantedReadOnly := []string{
		"space toggle", "d deploy", "r restart", "s stop",
		"R rollback", "c config", "a all",
	}

	for _, readOnly := range []bool{false, true} {
		for _, picker := range []bool{true, false} {
			back := "q quit"
			if picker {
				back = "q back"
			}
			for _, st := range containerFooterSearchStates {
				m := containerFooterModel(picker)
				if readOnly {
					m.composer = &readOnlyMockComposer{}
				}
				st.setup(&m)

				out := m.viewSelectContainers()
				toks := append([]string{}, want[st.name]...)
				if st.name != "searching" {
					toks = append(toks, back)
				}
				if st.name == "idle" {
					toks = append(toks, wantIdleLine2[readOnly]...)
				}
				for _, tok := range toks {
					if !strings.Contains(out, tok) {
						t.Errorf("readOnly=%v showPicker=%v state=%s: footer missing %q; got:\n%s",
							readOnly, picker, st.name, tok, out)
					}
				}
				bad := append([]string{}, unwanted[st.name]...)
				if readOnly {
					bad = append(bad, unwantedReadOnly...)
				}
				for _, tok := range bad {
					if strings.Contains(out, tok) {
						t.Errorf("readOnly=%v showPicker=%v state=%s: footer advertises %q, which does nothing here; got:\n%s",
							readOnly, picker, st.name, tok, out)
					}
				}
			}
		}
	}
}

// TestContainerFooter_RendersOneLineAtEighty proves the criterion the trim was
// made for: at width 80 the footer is ONE physical line in every search state.
// It measures the actual render, not the budget. anchor opens the footer and
// probe closes it, so finding both on one physical line proves the one-line
// branch was taken. Idle and committed span line1+line2 (`? keys` -> the line2
// head); while searching the footer is a single search-specific line, so the
// pair is its own two ends.
func TestContainerFooter_RendersOneLineAtEighty(t *testing.T) {
	ends := map[bool]map[string]struct{ anchor, probe string }{
		false: {
			"idle":      {"? keys", "d deploy"},
			"committed": {"? keys", "n/N cycle"},
			"searching": {"enter jump", "esc cancel"},
		},
		// The read-only pair is shorter than the writable one, so it fits on one
		// line at 80 with room to spare; the probe is its own line2 head.
		true: {
			"idle":      {"? keys", "l logs"},
			"committed": {"? keys", "n/N cycle"},
			"searching": {"enter jump", "esc cancel"},
		},
	}

	for _, readOnly := range []bool{false, true} {
		for _, picker := range []bool{true, false} {
			for _, st := range containerFooterSearchStates {
				m := containerFooterModel(picker)
				if readOnly {
					m.composer = &readOnlyMockComposer{}
				}
				st.setup(&m)

				end := ends[readOnly][st.name]
				var found bool
				for _, line := range strings.Split(m.viewSelectContainers(), "\n") {
					if !strings.Contains(line, end.anchor) {
						continue
					}
					found = true
					if !strings.Contains(line, end.probe) {
						t.Errorf("readOnly=%v showPicker=%v state=%s: footer split into two lines at width 80 (%q missing from the %q line): %q",
							readOnly, picker, st.name, end.probe, end.anchor, line)
					}
					if w := ansi.StringWidth(line); w > 80 {
						t.Errorf("readOnly=%v showPicker=%v state=%s: footer line is %d cells, exceeds width 80: %q",
							readOnly, picker, st.name, w, line)
					}
				}
				if !found {
					t.Errorf("readOnly=%v showPicker=%v state=%s: no footer line contains %q",
						readOnly, picker, st.name, end.anchor)
				}
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
	// Both builds share the header math, so the comparison below only needs the
	// footer delta. headerLines is asserted rather than assumed: an unrelated
	// header change would otherwise turn this red with a message blaming the
	// footer.
	const headerLines = 3

	svcs := make([]string, 30)
	for i := range svcs {
		svcs[i] = fmt.Sprintf("svc%02d", i)
	}
	svcs[0] = "web" // so the committed-search state has a real match

	build := func(width int, picker bool) Model {
		m := Model{}
		m.screen = screenSelectContainers
		m.setSingleGroup(svcs)
		m.selected = map[string]bool{}
		m.height = 24
		m.width = width
		m.showPicker = picker
		return m
	}

	// Self-check: svcVisibleCount() must equal height - headerLines - the new
	// footer height for the plain idle model this test builds.
	{
		m := build(120, true)
		if want := m.height - headerLines - (2 + m.containerFooterLines()); m.svcVisibleCount() != want {
			t.Fatalf("header math changed: svcVisibleCount()=%d, height-%d-footer=%d — update headerLines here",
				m.svcVisibleCount(), headerLines, want)
		}
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

// TestContainerFooter_NeverExceedsWidth pins the clamp directly, so a
// regression names the cause instead of surfacing as one odd row in the view
// sweep above.
//
// containerFooterLines must stay state-INDEPENDENT — it is decided from the
// idle pair and svcVisibleCount reserves rows from it, so making it read the
// substituted footer would grow the list by a row mid-search, the exact
// re-flow the reserved-bar design prevents. The cost is that a substitution
// WIDER than the idle line2 can overrun a width the idle pair fit. Only the
// read-only pair does that today (idle line2 19 cells, committed 25), at
// widths 43-46; containerFooter therefore clamps every rendered line, the same
// never-wrap guarantee searchBarLine and logBarLine give.
func TestContainerFooter_NeverExceedsWidth(t *testing.T) {
	states := []struct {
		name  string
		setup func(m *Model)
	}{
		{"idle", func(m *Model) {}},
		{"searching", func(m *Model) { m.searching = true }},
		{"committed", func(m *Model) { m.searchQuery = "svc1" }},
	}

	for _, readOnly := range []bool{false, true} {
		for width := 20; width <= 180; width++ {
			for _, st := range states {
				m := Model{}
				m.screen = screenSelectContainers
				m.width = width
				m.showPicker = true
				if readOnly {
					m.composer = &readOnlyMockComposer{}
				}
				st.setup(&m)

				for i, ln := range strings.Split(m.containerFooter(), "\n") {
					visible := strings.TrimRight(ansi.Strip(ln), " ")
					if w := ansi.StringWidth(visible); w > width {
						t.Errorf("readOnly=%v width=%d state=%s: footer line %d is %d cells wide: %q",
							readOnly, width, st.name, i, w, visible)
					}
				}
			}
		}
	}
}
