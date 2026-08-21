package tui

import (
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

// allScreens is every screen constant. A new screen must be added here and to
// helpGroups(), or TestHelpGroups_EveryScreen fails.
var allScreens = []screen{
	screenSelectServer,
	screenSelectProject,
	screenSelectContainers,
	screenProgress,
	screenLogs,
	screenConfig,
	screenSettingsList,
	screenSettingsForm,
}

func TestHelpGroups_EveryScreen(t *testing.T) {
	for _, s := range allScreens {
		groups := helpGroups(s)
		if len(groups) == 0 {
			t.Errorf("screen %d: helpGroups returned no groups", s)
			continue
		}
		if screenName(s) == "" {
			t.Errorf("screen %d: screenName returned empty", s)
		}
		for _, g := range groups {
			if g.title == "" {
				t.Errorf("screen %d: group with empty title", s)
			}
			if len(g.entries) == 0 {
				t.Errorf("screen %d: group %q has no entries", s, g.title)
			}
			for _, e := range g.entries {
				if e.keys == "" || e.desc == "" {
					t.Errorf("screen %d: group %q has an incomplete entry %+v", s, g.title, e)
				}
			}
		}
	}
}

// helpKeyAliases maps the overlay's display tokens onto the tea.KeyMsg strings
// that handleKey switches on.
var helpKeyAliases = map[string]string{
	"↑":     "up",
	"↓":     "down",
	"←":     "left",
	"→":     "right",
	"pgdn":  "pgdown",
	"space": " ",
}

func helpKeyTokens(s screen) map[string]bool {
	out := map[string]bool{}
	for _, g := range helpGroups(s) {
		for _, e := range g.entries {
			for _, tok := range strings.Fields(e.keys) {
				if alias, ok := helpKeyAliases[tok]; ok {
					tok = alias
				}
				out[tok] = true
			}
		}
	}
	return out
}

// TestHelpGroups_NamesEveryBoundKey is the drift pin. The expected sets are
// hand-maintained from each screen's handleKey case in app.go — a new binding
// must be added to both places.
func TestHelpGroups_NamesEveryBoundKey(t *testing.T) {
	bound := map[screen][]string{
		screenSelectServer:  {"q", "ctrl+c", "up", "k", "down", "j", "enter", "s"},
		screenSelectProject: {"q", "ctrl+c", "esc", "up", "k", "down", "j", "enter"},
		screenSelectContainers: {
			"q", "ctrl+c", "esc", "enter", "up", "k", "down", "j", " ", "a",
			"r", "d", "s", "R", "n", "N", "/", "l", "c", "x", "U",
		},
		screenProgress: {"q", "ctrl+c", "esc"},
		screenLogs: {
			"q", "ctrl+c", "esc", "enter", "ctrl+r", "w", "p", "G", "f", "/",
			"n", "N", "up", "down", "left", "right", "pgup", "pgdown",
		},
		screenConfig:       {"q", "ctrl+c", "esc", "r", "e", "up", "down", "pgup", "pgdown"},
		screenSettingsList: {"q", "ctrl+c", "esc", "up", "k", "down", "j", "a", "enter", "e", "d", "y", "n"},
		screenSettingsForm: {"q", "ctrl+c", "esc", "tab", "shift+tab", "up", "down", "left", "right", "enter"},
	}

	for _, s := range allScreens {
		keys, ok := bound[s]
		if !ok {
			t.Errorf("screen %d has no expected key set", s)
			continue
		}
		named := helpKeyTokens(s)
		for _, k := range keys {
			if !named[k] {
				t.Errorf("screen %s: help table does not name bound key %q", screenName(s), k)
			}
		}
	}
}

func TestViewHelp_ReplacesScreen(t *testing.T) {
	m := Model{
		screen:   screenSelectContainers,
		services: []string{"web-frontend"},
		width:    120,
		height:   24,
		helpOpen: true,
	}
	view := m.View()
	if !strings.Contains(view, "OPERATE") {
		t.Errorf("overlay should list the OPERATE group, got: %q", view)
	}
	if !strings.Contains(view, "rollback") {
		t.Errorf("overlay should name the rollback key, got: %q", view)
	}
	if strings.Contains(view, "web-frontend") {
		t.Errorf("overlay should replace the container list, got: %q", view)
	}
	if !strings.Contains(view, "close") {
		t.Errorf("overlay should show the close hint, got: %q", view)
	}
}

func TestViewHelp_ContentMatchesScreen(t *testing.T) {
	tests := []struct {
		s      screen
		expect string
	}{
		{screenSelectServer, "settings"},
		{screenSelectContainers, "rollback"},
		{screenLogs, "regex mode"},
		{screenConfig, "$EDITOR"},
		{screenSettingsForm, "cycle color"},
	}
	for _, tt := range tests {
		m := Model{screen: tt.s, width: 120, height: 24, helpOpen: true}
		view := m.View()
		if !strings.Contains(view, tt.expect) {
			t.Errorf("screen %s: overlay missing %q, got: %q", screenName(tt.s), tt.expect, view)
		}
		if !strings.Contains(view, screenName(tt.s)) {
			t.Errorf("screen %s: overlay title should name the screen, got: %q", screenName(tt.s), view)
		}
	}
}

// TestViewHelp_FitsShortTerminal pins the two-column layout claim: the densest
// screen must fit a 24-line terminal.
func TestViewHelp_FitsShortTerminal(t *testing.T) {
	for _, s := range allScreens {
		m := Model{screen: s, width: 120, height: 24, helpOpen: true}
		lines := strings.Split(strings.TrimRight(m.View(), "\n"), "\n")
		if len(lines) > 24 {
			t.Errorf("screen %s: overlay is %d lines, want <= 24", screenName(s), len(lines))
		}
	}
}

// TestViewHelp_NeverExceedsWidth pins the never-wrap invariant: below the
// two-column threshold the layout falls back to one column, and every row is
// clamped as a backstop.
func TestViewHelp_NeverExceedsWidth(t *testing.T) {
	for _, w := range []int{120, 80, 60, 40} {
		for _, s := range allScreens {
			m := Model{screen: s, width: w, height: 24, helpOpen: true}
			for i, line := range strings.Split(m.View(), "\n") {
				if got := ansi.StringWidth(line); got > w {
					t.Errorf("screen %s width %d: row %d is %d cells: %q",
						screenName(s), w, i, got, line)
				}
			}
		}
	}
}

func TestLayoutHelpColumns_SingleColumnFallback(t *testing.T) {
	groups := helpGroups(screenSelectContainers)

	wide := layoutHelpColumns(groups, 120)
	narrow := layoutHelpColumns(groups, 40)

	wideRows := strings.Count(wide, "\n") + 1
	narrowRows := strings.Count(narrow, "\n") + 1
	if narrowRows <= wideRows {
		t.Errorf("narrow layout should stack the columns: %d rows vs %d wide", narrowRows, wideRows)
	}
	for _, title := range []string{"MOVE", "SELECT", "LEAVE", "OPERATE", "INSPECT"} {
		if !strings.Contains(narrow, title) {
			t.Errorf("single-column fallback dropped group %q", title)
		}
	}
}

func TestLayoutHelpColumns_Empty(t *testing.T) {
	if got := layoutHelpColumns(nil, 80); got != "" {
		t.Errorf("layoutHelpColumns(nil) = %q, want empty", got)
	}
}

func TestSplitHelpGroups_Balances(t *testing.T) {
	left, right := splitHelpGroups(helpGroups(screenSelectContainers))
	if len(left) == 0 || len(right) == 0 {
		t.Fatalf("container groups should split into two columns, got %d/%d", len(left), len(right))
	}
	lh, rh := 0, 0
	for _, g := range left {
		lh += helpGroupLines(g)
	}
	for _, g := range right {
		rh += helpGroupLines(g)
	}
	if lh < rh {
		t.Errorf("left column (%d lines) should not be shorter than right (%d)", lh, rh)
	}
	if lh-rh > 6 {
		t.Errorf("columns are unbalanced: left %d lines, right %d", lh, rh)
	}
}

func TestSplitHelpGroups_SingleGroup(t *testing.T) {
	groups := []helpGroup{{title: "ONLY", entries: []helpEntry{{"a", "b"}}}}
	left, right := splitHelpGroups(groups)
	if len(left) != 1 || right != nil {
		t.Errorf("single group should not split, got %d/%d", len(left), len(right))
	}
}

// --- `?` open/close intercept -------------------------------------------
//
// The intercept lives in handleKey (app.go) but belongs to the overlay
// feature, so its tests sit beside the renderer's.

func helpContainerModel() Model {
	return Model{
		screen:   screenSelectContainers,
		services: []string{"web", "db"},
		selected: make(map[int]bool),
		width:    120,
		height:   24,
	}
}

func pressKey(m Model, r rune) Model {
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return updated.(Model)
}

func TestHelpOverlay_QuestionOpens(t *testing.T) {
	um := pressKey(helpContainerModel(), '?')
	if !um.helpOpen {
		t.Fatal("helpOpen = false, want true after ?")
	}
	if um.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers (the overlay is a flag, not a screen)", um.screen)
	}
	if !strings.Contains(um.View(), "OPERATE") {
		t.Error("View() does not render the overlay after ?")
	}
}

func TestHelpOverlay_CloseKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'?'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
	} {
		m := helpContainerModel()
		m.helpOpen = true

		updated, cmd := m.Update(key)
		um := updated.(Model)

		if um.helpOpen {
			t.Errorf("%q: helpOpen = true, want false", key.String())
		}
		if um.screen != screenSelectContainers {
			t.Errorf("%q: screen = %d, want screenSelectContainers (close must not navigate)", key.String(), um.screen)
		}
		if cmd != nil {
			if _, ok := cmd().(tea.QuitMsg); ok {
				t.Errorf("%q: close produced a QuitMsg, want none", key.String())
			}
		}
	}
}

// TestHelpOverlay_SwallowsActionKeys: the overlay must consume every key it
// does not close on, or a stray d would arm a deploy behind it.
func TestHelpOverlay_SwallowsActionKeys(t *testing.T) {
	m := helpContainerModel()
	m.selected[0] = true
	m.helpOpen = true

	um := pressKey(m, 'd')

	if um.confirming {
		t.Error("confirming = true, want false (d must be swallowed)")
	}
	if um.pendingOp == runner.Deploy {
		t.Error("pendingOp = Deploy, want it unset (d must be swallowed)")
	}
	if !um.helpOpen {
		t.Error("helpOpen = false, want true (a swallowed key must not close the overlay)")
	}
}

// TestHelpOverlay_CtrlCQuitsLocal: ctrl+c clears the flag and falls through,
// so a local session still quits.
func TestHelpOverlay_CtrlCQuitsLocal(t *testing.T) {
	m := helpContainerModel()
	m.helpOpen = true

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	um := updated.(Model)

	if um.helpOpen {
		t.Error("helpOpen = true, want false after ctrl+c")
	}
	if cmd == nil {
		t.Fatal("ctrl+c returned no command, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c command = %T, want tea.QuitMsg", cmd())
	}
}

// TestHelpOverlay_CtrlCMidProgressIsNoOp: the fall-through preserves each
// screen's own ctrl+c semantics. Mid-progress that is a no-op, NOT a quit.
func TestHelpOverlay_CtrlCMidProgressIsNoOp(t *testing.T) {
	m := Model{screen: screenProgress, width: 120, height: 24, helpOpen: true}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	um := updated.(Model)

	if um.helpOpen {
		t.Error("helpOpen = true, want false after ctrl+c")
	}
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Error("ctrl+c mid-progress produced a QuitMsg, want a no-op")
		}
	}
}

// TestHelpOverlay_QuestionReachesLogFilterInput is the load-bearing regression
// pin: `?` is a regex metacharacter and ctrl+r puts the filter in RE2 mode, so
// a user typing (?:web|db) must get a filter, not a help overlay.
func TestHelpOverlay_QuestionReachesLogFilterInput(t *testing.T) {
	m := Model{screen: screenLogs, width: 120, height: 24}
	m.logsRawLines = []string{"web | hello"}

	m = pressKey(m, 'f')
	if !m.logFiltering {
		t.Fatal("precondition: the log filter must be open")
	}

	um := pressKey(m, '?')

	if um.helpOpen {
		t.Error("helpOpen = true, want false (? must reach the filter input)")
	}
	if !um.logFiltering {
		t.Error("logFiltering = false, want true")
	}
	if !strings.Contains(um.logFilterInput.Value(), "?") {
		t.Errorf("logFilterInput = %q, want it to contain %q", um.logFilterInput.Value(), "?")
	}
}

func TestHelpOverlay_QuestionReachesLogSearchInput(t *testing.T) {
	m := Model{screen: screenLogs, width: 120, height: 24}
	m.logsRawLines = []string{"web | hello"}
	m.logFilterShown = 1

	m = pressKey(m, '/')
	if !m.logSearching {
		t.Fatal("precondition: the log search must be open")
	}

	um := pressKey(m, '?')

	if um.helpOpen {
		t.Error("helpOpen = true, want false (? must reach the search input)")
	}
	if !strings.Contains(um.logSearchInput.Value(), "?") {
		t.Errorf("logSearchInput = %q, want it to contain %q", um.logSearchInput.Value(), "?")
	}
}

func TestHelpOverlay_QuestionReachesContainerSearchInput(t *testing.T) {
	m := pressKey(helpContainerModel(), '/')
	if !m.searching {
		t.Fatal("precondition: the container search must be open")
	}

	um := pressKey(m, '?')

	if um.helpOpen {
		t.Error("helpOpen = true, want false (? must reach the search input)")
	}
	if !um.searching {
		t.Error("searching = false, want true")
	}
	if um.searchInput.Value() != "?" {
		t.Errorf("searchInput = %q, want %q", um.searchInput.Value(), "?")
	}
}

// TestHelpOverlay_QuestionReachesSettingsFormInput covers all four text
// fields (name, host, project_dir, group). Field 4 is the color picker and is
// NOT a text input, so `?` opens the overlay there instead — pinned by
// TestHelpOverlay_OpensFromEveryScreen.
func TestHelpOverlay_QuestionReachesSettingsFormInput(t *testing.T) {
	for field := 0; field < 4; field++ {
		m := Model{screen: screenSettingsForm, width: 120, height: 24}
		m.settingsInputs = initSettingsInputs()
		m.settingsField = field
		m.settingsInputs[field].Focus()

		um := pressKey(m, '?')

		if um.helpOpen {
			t.Errorf("field %d: helpOpen = true, want false (? must reach the focused field)", field)
		}
		if !strings.Contains(um.settingsInputs[field].Value(), "?") {
			t.Errorf("field %d: settingsInputs[%d] = %q, want it to contain %q",
				field, field, um.settingsInputs[field].Value(), "?")
		}
	}
}

// TestHelpOverlay_SwallowedAtConfirmPrompt: opening the overlay over a live
// destructive prompt would hide it, and the single esc that closes the overlay
// would leave the prompt armed underneath.
func TestHelpOverlay_SwallowedAtConfirmPrompt(t *testing.T) {
	m := helpContainerModel()
	m.selected[0] = true
	m.confirming = true
	m.pendingOp = runner.Deploy

	um := pressKey(m, '?')

	if um.helpOpen {
		t.Error("helpOpen = true, want false at a confirmation prompt")
	}
	if !um.confirming {
		t.Error("confirming = false, want true (the prompt must stay armed)")
	}
	if um.pendingOp != runner.Deploy {
		t.Errorf("pendingOp = %v, want runner.Deploy", um.pendingOp)
	}
}

func TestHelpOverlay_SwallowedAtSettingsDeletePrompt(t *testing.T) {
	m := Model{screen: screenSettingsList, width: 120, height: 24, settingsDelete: true}

	um := pressKey(m, '?')

	if um.helpOpen {
		t.Error("helpOpen = true, want false at a delete confirmation")
	}
	if !um.settingsDelete {
		t.Error("settingsDelete = false, want true (the prompt must stay armed)")
	}
}

// TestHelpOverlay_ClearedOnConnectError: connectResultMsg is one of only two
// non-key-driven m.screen assignments, so an open overlay would silently swap
// its key table.
func TestHelpOverlay_ClearedOnConnectError(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	installFakeTick(&m)
	m.screen = screenSelectServer
	m.helpOpen = true

	updated, _ := m.Update(connectResultMsg{err: errors.New("connection refused")})
	um := updated.(Model)

	if um.helpOpen {
		t.Error("helpOpen = true, want false after a failed connect")
	}
}

func TestTypingInInput(t *testing.T) {
	tests := []struct {
		name string
		m    Model
		want bool
	}{
		{"containers idle", Model{screen: screenSelectContainers}, false},
		{"containers searching", Model{screen: screenSelectContainers, searching: true}, true},
		{"logs idle", Model{screen: screenLogs}, false},
		{"logs filtering", Model{screen: screenLogs, logFiltering: true}, true},
		{"logs searching", Model{screen: screenLogs, logSearching: true}, true},
		{"settings form field 0", Model{screen: screenSettingsForm, settingsField: 0}, true},
		{"settings form field 3", Model{screen: screenSettingsForm, settingsField: 3}, true},
		{"settings form color picker", Model{screen: screenSettingsForm, settingsField: 4}, false},
		{"server select", Model{screen: screenSelectServer}, false},
		{"project select", Model{screen: screenSelectProject}, false},
		{"progress", Model{screen: screenProgress}, false},
		{"config", Model{screen: screenConfig}, false},
		{"settings list", Model{screen: screenSettingsList}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.typingInInput(); got != tt.want {
				t.Errorf("typingInInput() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Task 4 acceptance pins ---------------------------------------------

// TestHelpOverlay_OpensFromEveryScreen proves the "? opens the overlay from
// all 8 screens, and the content matches the screen it was opened from"
// criterion. TestViewHelp_ContentMatchesScreen renders viewHelp with the flag
// pre-set; this one drives the real `?` keypress through Update, so it also
// covers the intercept reaching every screen.
func TestHelpOverlay_OpensFromEveryScreen(t *testing.T) {
	tests := []struct {
		s       screen
		setup   func(m *Model)
		want    string
		notWant string
	}{
		{s: screenSelectServer, want: "settings", notWant: "rollback"},
		{s: screenSelectProject, want: "select", notWant: "settings"},
		{s: screenSelectContainers, want: "rollback", notWant: "regex mode"},
		{s: screenProgress, want: "skip health wait", notWant: "rollback"},
		{s: screenLogs, want: "regex mode", notWant: "rollback"},
		{s: screenConfig, want: "$EDITOR", notWant: "regex mode"},
		{s: screenSettingsList, want: "delete server", notWant: "rollback"},
		// settingsField 4 is the color picker. Fields 0-3 are text inputs where
		// `?` deliberately lands in the field instead — see
		// TestHelpOverlay_QuestionReachesSettingsFormInput.
		{s: screenSettingsForm, setup: func(m *Model) { m.settingsField = 4 },
			want: "cycle color", notWant: "delete server"},
	}

	for _, tt := range tests {
		m := Model{screen: tt.s, width: 120, height: 24, selected: map[int]bool{}}
		if tt.setup != nil {
			tt.setup(&m)
		}

		um := pressKey(m, '?')

		if !um.helpOpen {
			t.Errorf("screen %s: helpOpen = false, want true after ?", screenName(tt.s))
			continue
		}
		if um.screen != tt.s {
			t.Errorf("screen %s: screen changed to %d (the overlay is a flag, not a screen)",
				screenName(tt.s), um.screen)
		}
		view := um.View()
		if !strings.Contains(view, screenName(tt.s)) {
			t.Errorf("screen %s: overlay title does not name the screen, got:\n%s", screenName(tt.s), view)
		}
		if !strings.Contains(view, tt.want) {
			t.Errorf("screen %s: overlay missing %q, got:\n%s", screenName(tt.s), tt.want, view)
		}
		if strings.Contains(view, tt.notWant) {
			t.Errorf("screen %s: overlay shows %q, which belongs to another screen, got:\n%s",
				screenName(tt.s), tt.notWant, view)
		}
	}
}

// TestHelpOverlay_RegexFilterAcceptsQuestionMark is the worked example from the
// plan: ctrl+r puts the log filter into RE2 mode, where `(?:web|db)` is a valid
// non-capturing group. Every rune of it must land in the input.
func TestHelpOverlay_RegexFilterAcceptsQuestionMark(t *testing.T) {
	const query = "(?:web|db)"

	m := Model{screen: screenLogs, width: 120, height: 24}
	m.logsRawLines = []string{"web | hello", "db | world", "cache | ignored"}

	m = pressKey(m, 'f')
	if !m.logFiltering {
		t.Fatal("precondition: the log filter must be open")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = updated.(Model)
	if !m.logFilterIsRegex {
		t.Fatal("precondition: ctrl+r must put the filter into regex mode")
	}

	for _, r := range query {
		m = pressKey(m, r)
		if m.helpOpen {
			t.Fatalf("helpOpen = true after typing %q — the overlay stole a regex metacharacter", r)
		}
	}

	if got := m.logFilterInput.Value(); got != query {
		t.Errorf("logFilterInput = %q, want %q", got, query)
	}
}

// TestHelpOverlay_SwallowedAtEveryConfirmPrompt proves the "? at a
// deploy/stop/rollback confirm prompt does nothing" criterion for each op that
// arms the container confirmation, plus the exec confirmation.
func TestHelpOverlay_SwallowedAtEveryConfirmPrompt(t *testing.T) {
	ops := []struct {
		name string
		op   runner.Operation
		exec bool
	}{
		{name: "deploy", op: runner.Deploy},
		{name: "restart", op: runner.Restart},
		{name: "stop", op: runner.StopOnly},
		{name: "rollback", op: runner.Rollback},
		{name: "exec", op: runner.Restart, exec: true},
	}

	for _, tt := range ops {
		m := helpContainerModel()
		m.selected[0] = true
		m.confirming = true
		m.pendingOp = tt.op
		m.pendingExec = tt.exec

		um := pressKey(m, '?')

		if um.helpOpen {
			t.Errorf("%s: helpOpen = true, want false at a confirmation prompt", tt.name)
		}
		if !um.confirming {
			t.Errorf("%s: confirming = false, want true (the prompt must stay armed)", tt.name)
		}
		if um.pendingOp != tt.op {
			t.Errorf("%s: pendingOp = %v, want %v", tt.name, um.pendingOp, tt.op)
		}
		if um.pendingExec != tt.exec {
			t.Errorf("%s: pendingExec = %v, want %v", tt.name, um.pendingExec, tt.exec)
		}
		if strings.Contains(um.View(), "OPERATE") {
			t.Errorf("%s: the overlay replaced the live confirmation prompt", tt.name)
		}
	}
}

// TestHelpOverlay_CtrlCPromptsOnRemote completes the ctrl+c criterion: the
// fall-through must reach tryQuit, which shows the disconnect confirmation
// instead of quitting when a remote session is live.
func TestHelpOverlay_CtrlCPromptsOnRemote(t *testing.T) {
	m := helpContainerModel()
	m.serverName = "prod-server"
	m.disconnectFunc = func() error { return nil }
	m.helpOpen = true

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	um := updated.(Model)

	if um.helpOpen {
		t.Error("helpOpen = true, want false after ctrl+c")
	}
	if !um.quitting {
		t.Fatal("quitting = false, want true (a remote ctrl+c must prompt, not quit)")
	}
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Error("remote ctrl+c produced a QuitMsg, want the disconnect prompt")
		}
	}
	if !strings.Contains(um.View(), "Disconnect from prod-server") {
		t.Errorf("View() does not render the disconnect prompt, got:\n%s", um.View())
	}
}
