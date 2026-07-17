package tui

import (
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// TestLogBarLineNeverWraps is the log-view analogue of TestSearchBarLineNeverWraps.
// The log viewport reserves exactly ONE physical row for logBarLine(); if the
// bar's DISPLAY width exceeds m.width the terminal wraps it to a second line,
// pushing the help footer past its reserved space (content overflows / the tail
// scrolls off). This drives NARROW terminals (40, 20, and the EXTREME 5 and 1)
// with LONG content — a long typed query in each typing mode, a bad-regex typing
// state, and the dual committed summary — and asserts the rendered bar's
// ANSI-aware display width is clamped to m.width in every mode. Widths 5/1 pin the
// unconditional final clampToWidth: at those widths no per-mode budgeting could
// keep the counter suffix under m.width, so only the final clamp guarantees the
// invariant.
func TestLogBarLineNeverWraps(t *testing.T) {
	longRaw := []string{
		"svc | this-is-an-extremely-long-log-line-way-past-forty-columns alpha",
		"svc | this-is-an-extremely-long-log-line-way-past-forty-columns beta",
	}
	longQuery := "this-is-an-extremely-long-search-query-that-overflows-a-narrow-terminal"
	longFilter := "this-is-an-extremely-long-filter-query-that-overflows-a-narrow-terminal-too"

	build := func(width int) Model {
		m := Model{}
		m.screen = screenLogs
		m.width = width
		m.logsRawLines = longRaw
		return m
	}

	modes := []struct {
		name  string
		setup func(m *Model)
	}{
		{"typing-search-long-query", func(m *Model) {
			m.logSearchInput = textinput.New()
			m.logSearchInput.SetValue(longQuery)
			m.logSearchInput.Focus()
			m.logSearching = true
			m.logSearchMatches = []int{0, 1}
			m.logSearchCur = 0
		}},
		{"typing-search-no-match", func(m *Model) {
			m.logSearchInput = textinput.New()
			m.logSearchInput.SetValue(longQuery)
			m.logSearchInput.Focus()
			m.logSearching = true
			m.logSearchMatches = nil // → "(no match)" suffix
		}},
		{"typing-filter-long-query", func(m *Model) {
			m.logFilterInput = textinput.New()
			m.logFilterInput.SetValue(longQuery)
			m.logFilterInput.Focus()
			m.logFiltering = true
		}},
		{"typing-filter-bad-regex", func(m *Model) {
			m.logFilterInput = textinput.New()
			m.logFilterInput.SetValue(longQuery + "[") // "[" never closes → invalid RE2
			m.logFilterInput.Focus()
			m.logFiltering = true
			m.logFilterIsRegex = true
		}},
		{"committed-dual-summary", func(m *Model) {
			m.logFilterQuery = longFilter
			m.logSearchQuery = longQuery
			m.logSearchMatches = []int{0, 1}
			m.logSearchCur = 1
		}},
	}

	for _, width := range []int{40, 20, 5, 1} {
		for _, md := range modes {
			m := build(width)
			md.setup(&m)
			bar := m.logBarLine()
			if w := ansi.StringWidth(bar); w > width {
				t.Errorf("width=%d mode=%s: log bar display width %d exceeds terminal width — it will wrap (bar=%q)",
					width, md.name, w, bar)
			}
			if strings.Contains(bar, "\n") {
				t.Errorf("width=%d mode=%s: log bar contains an embedded newline (bar=%q)", width, md.name, bar)
			}
		}
	}

	// width == 0 (unknown terminal size in some tests): must NOT truncate to
	// nothing — return the full unclamped bar, mirroring searchBarLine.
	m := build(0)
	modes[0].setup(&m)
	if bar := m.logBarLine(); ansi.StringWidth(bar) == 0 {
		t.Errorf("width=0: log bar truncated to empty; expected full unclamped content")
	}
}

// TestLogInputWidthPersists pins the log-view analogue of TestSearchInputWidthPersists:
// the open input's Width must be set on the PERSISTED model (the one returned by
// Update/handleKey), NOT inside the value-receiver logBarLine() where the assignment
// would land on a throwaway copy. A bounded Width lets bubbles scroll the value
// horizontally so a long typed query keeps the cursor + newest chars visible instead
// of clipping from the right; the bar's ansi width must still be <= m.width in every
// case (the final clampToWidth is the hard guarantee). Driven through the real
// f/'/' open + typing paths. It FAILS under the pre-fix code (Width stays 0).
func TestLogInputWidthPersists(t *testing.T) {
	longQuery := "this-is-an-extremely-long-query-that-overflows-a-narrow-terminal-many-times"

	t.Run("filter open + typing sets Width and never wraps", func(t *testing.T) {
		m := setupFilterableLogsModel()
		updated, _ := m.Update(runeKey('f'))
		om := updated.(Model)
		if !om.logFiltering {
			t.Fatal("precondition: f should open the filter bar")
		}
		if want := om.logFilterInputWidth(); om.logFilterInput.Width != want {
			t.Errorf("after f open: logFilterInput.Width = %d, want %d (persisted budget)", om.logFilterInput.Width, want)
		}
		if om.logFilterInput.Width <= 0 {
			t.Errorf("after f open: logFilterInput.Width = %d, want > 0 at width %d (bubbles never scrolls to keep cursor visible)", om.logFilterInput.Width, om.width)
		}
		tm := typeInto(om, longQuery)
		if want := tm.logFilterInputWidth(); tm.logFilterInput.Width != want {
			t.Errorf("after typing: logFilterInput.Width = %d, want %d (re-persisted budget)", tm.logFilterInput.Width, want)
		}
		if w := ansi.StringWidth(tm.logBarLine()); w > tm.width {
			t.Errorf("after typing long query: log bar width %d exceeds terminal width %d — it will wrap", w, tm.width)
		}
	})

	t.Run("search open + typing sets Width and never wraps", func(t *testing.T) {
		m := setupFilterableLogsModel()
		updated, _ := m.Update(runeKey('/'))
		om := updated.(Model)
		if !om.logSearching {
			t.Fatal("precondition: / should open the search bar")
		}
		if want := om.logSearchInputWidth(); om.logSearchInput.Width != want {
			t.Errorf("after / open: logSearchInput.Width = %d, want %d (persisted budget)", om.logSearchInput.Width, want)
		}
		if om.logSearchInput.Width <= 0 {
			t.Errorf("after / open: logSearchInput.Width = %d, want > 0 at width %d", om.logSearchInput.Width, om.width)
		}
		tm := typeInto(om, longQuery)
		if want := tm.logSearchInputWidth(); tm.logSearchInput.Width != want {
			t.Errorf("after typing: logSearchInput.Width = %d, want %d (re-persisted budget)", tm.logSearchInput.Width, want)
		}
		if w := ansi.StringWidth(tm.logBarLine()); w > tm.width {
			t.Errorf("after typing long query: log bar width %d exceeds terminal width %d — it will wrap", w, tm.width)
		}
	})

	t.Run("resize refreshes an open input's Width", func(t *testing.T) {
		m := setupFilterableLogsModel()
		updated, _ := m.Update(runeKey('/'))
		om := updated.(Model)
		resized, _ := om.Update(tea.WindowSizeMsg{Width: 40, Height: 26})
		rm := resized.(Model)
		if rm.width != 40 {
			t.Fatalf("resize: m.width = %d, want 40", rm.width)
		}
		if want := rm.logSearchInputWidth(); rm.logSearchInput.Width != want {
			t.Errorf("resize: logSearchInput.Width = %d, want %d (stale viewport not refreshed)", rm.logSearchInput.Width, want)
		}
	})
}

// TestLogBarReservedLineHeightAccounting pins the -6→-7 viewport-height change:
// one physical row is reserved for the bar, so the log viewport is sized
// m.height-7 — exactly one row less than the pre-bar m.height-6 baseline. Driven
// through the real WindowSizeMsg path. Also asserts the idle bar is blank (spaces
// only) yet still occupies a reserved physical line.
func TestLogBarReservedLineHeightAccounting(t *testing.T) {
	m := Model{}
	m.screen = screenLogs
	m.logsViewport = viewport.New(76, 10)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	if got, want := m.logsViewport.Height, 24-7; got != want {
		t.Errorf("logsViewport.Height = %d, want %d (m.height-7); the no-bar baseline would be %d", got, want, 24-6)
	}

	// The idle bar renders blank (only spaces) but is still a physical line.
	if bar := m.logBarLine(); strings.TrimSpace(bar) != "" {
		t.Errorf("idle log bar should be blank, got %q", bar)
	}
}

// TestLogViewFooterReservationConstant renders viewLogs() across idle,
// committed-filter, and committed-search states and asserts the total physical
// line count is CONSTANT — the reserved bar line means switching filter/search
// states never resizes the visible region ("height never jumps"). Driven at a
// wide width so the help footer stays a single line and the only variable is the
// one-line bar.
func TestLogViewFooterReservationConstant(t *testing.T) {
	newLogsModel := func() Model {
		mc := &mockComposer{services: []string{"app"}}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		m.screen = screenLogs
		m.logsService = "app"
		m.height = 24
		m.width = 200 // wide: help never wraps, isolating the bar's contribution
		m.logsWrap = true
		m.logsViewport = viewport.New(196, 24-7)
		m.logsRawLines = logChunkLines(50)
		m.applyLogFormat()
		return m
	}
	physLines := func(out string) int {
		return strings.Count(strings.TrimRight(out, "\n"), "\n") + 1
	}

	states := []struct {
		name  string
		setup func(m *Model)
	}{
		{"idle", func(m *Model) {}},
		{"committed-filter", func(m *Model) {
			m.logFilterQuery = "line" // matches all 50 → viewport stays full
			m.rederiveLogs()
		}},
		{"committed-search", func(m *Model) {
			m.logSearchInput = textinput.New()
			m.logSearchQuery = "line 2"
			m.setLogViewportContent()
		}},
	}

	var first int
	for i, st := range states {
		m := newLogsModel()
		st.setup(&m)
		out := m.viewLogs()
		p := physLines(out)
		if i == 0 {
			first = p
		} else if p != first {
			t.Errorf("state=%s: viewLogs physical line count %d differs from idle %d (reserved bar must keep height constant)",
				st.name, p, first)
		}
	}
}

// TestLogBarLineContent pins the per-mode content precedence of logBarLine.
func TestLogBarLineContent(t *testing.T) {
	base := func() Model {
		m := Model{}
		m.screen = screenLogs
		m.width = 200 // wide so nothing is clamped away
		m.logsRawLines = []string{
			"app | INFO starting",
			"app | ERROR disk full",
			"app | ERROR timeout",
		}
		return m
	}

	t.Run("typing search shows query mode and counter", func(t *testing.T) {
		m := base()
		m.logSearchInput = textinput.New()
		m.logSearchInput.SetValue("ERROR")
		m.logSearching = true
		m.logSearchMatches = []int{1, 2}
		m.logSearchCur = 0
		bar := m.logBarLine()
		for _, want := range []string{"/", "ERROR", "[literal]", "(1/2)"} {
			if !strings.Contains(bar, want) {
				t.Errorf("typing-search bar %q missing %q", bar, want)
			}
		}
	})

	t.Run("typing search no match", func(t *testing.T) {
		m := base()
		m.logSearchInput = textinput.New()
		m.logSearchInput.SetValue("zzz")
		m.logSearching = true
		m.logSearchMatches = nil
		if bar := m.logBarLine(); !strings.Contains(bar, "(no match)") {
			t.Errorf("typing-search no-match bar %q missing (no match)", bar)
		}
	})

	t.Run("typing filter shows N/M shown count", func(t *testing.T) {
		m := base()
		m.logFilterInput = textinput.New()
		m.logFilterInput.SetValue("ERROR")
		m.logFiltering = true
		m.logFilterQuery = "ERROR" // committed last-good drives the count
		bar := m.logBarLine()
		for _, want := range []string{"f", "ERROR", "2/3 shown"} {
			if !strings.Contains(bar, want) {
				t.Errorf("typing-filter bar %q missing %q", bar, want)
			}
		}
	})

	t.Run("typing filter bad regex", func(t *testing.T) {
		m := base()
		m.logFilterInput = textinput.New()
		m.logFilterInput.SetValue("ERROR[") // invalid RE2
		m.logFiltering = true
		m.logFilterIsRegex = true
		if bar := m.logBarLine(); !strings.Contains(bar, "(bad regex)") {
			t.Errorf("typing-filter bad-regex bar %q missing (bad regex)", bar)
		}
	})

	t.Run("committed dual summary", func(t *testing.T) {
		m := base()
		m.logFilterQuery = "ERROR"
		m.logSearchQuery = "timeout"
		m.logSearchMatches = []int{0}
		m.logSearchCur = 0
		bar := m.logBarLine()
		for _, want := range []string{"filter: ERROR", "2/3", "search: timeout", "(1/1)"} {
			if !strings.Contains(bar, want) {
				t.Errorf("committed-dual bar %q missing %q", bar, want)
			}
		}
	})

	t.Run("committed regex filter shows rx tag", func(t *testing.T) {
		m := base()
		if _, _, ok := buildMatcher("ERR.*", true, true); !ok {
			t.Fatal("precondition: ERR.* should compile")
		}
		m.logFilterQuery = "ERR.*"
		m.logFilterCommittedRegex = true
		if bar := m.logBarLine(); !strings.Contains(bar, "[rx]") {
			t.Errorf("committed regex-filter bar %q missing [rx] tag", bar)
		}
	})

	t.Run("idle blank", func(t *testing.T) {
		m := base()
		if bar := m.logBarLine(); strings.TrimSpace(bar) != "" {
			t.Errorf("idle bar should be blank, got %q", bar)
		}
	})
}

// TestLogViewportZeroMatchPlaceholder pins that an active filter which hides every
// complete raw line renders the "(no lines match filter)" placeholder in the
// viewport rather than a blank screen, and that the placeholder is filter-
// conditional (absent when the filter matches something).
func TestLogViewportZeroMatchPlaceholder(t *testing.T) {
	t.Run("no survivors shows placeholder", func(t *testing.T) {
		m := setupFilterableLogsModel()
		updated, _ := m.Update(runeKey('f'))
		m = updated.(Model)
		m = typeInto(m, "zzz-no-such-line")

		if s := strings.TrimSpace(m.derivedLogContent()); s != "" {
			t.Fatalf("precondition: expected no survivors, got derived content %q", s)
		}
		if !strings.Contains(m.logsViewport.View(), "no lines match filter") {
			t.Errorf("viewport should show the zero-match placeholder, got:\n%s", m.logsViewport.View())
		}
	})

	t.Run("matching filter hides placeholder", func(t *testing.T) {
		m := setupFilterableLogsModel()
		updated, _ := m.Update(runeKey('f'))
		m = updated.(Model)
		m = typeInto(m, "ERROR")

		if strings.Contains(m.logsViewport.View(), "no lines match filter") {
			t.Errorf("placeholder must not show when the filter matches lines, got:\n%s", m.logsViewport.View())
		}
	})
}
