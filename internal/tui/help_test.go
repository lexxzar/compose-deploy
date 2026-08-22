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

// allScreens is every screen constant, pinned complete by
// TestAllScreens_Complete below: the constants are a contiguous iota run, so a
// new screen that is not listed here fails that test instead of silently
// skipping every screen-table test in this file.
var allScreens = []screen{
	screenSelectServer,
	screenSelectProject,
	screenSelectContainers,
	screenProgress,
	screenLogs,
	screenConfig,
	screenSettingsList,
	screenSettingsForm,
	screenInspect,
}

// allProgressPhases is every progressPhase constant, pinned complete by
// TestAllProgressPhases_Complete.
var allProgressPhases = []progressPhase{progressRunning, progressWaiting, progressFinished}

// TestAllProgressPhases_Complete mirrors TestAllScreens_Complete for the
// progress sub-state enum: a fourth phase that is not listed here would
// silently escape the drift pin and the per-phase table test.
func TestAllProgressPhases_Complete(t *testing.T) {
	if want := int(progressFinished) + 1; len(allProgressPhases) != want {
		t.Fatalf("allProgressPhases has %d entries, want %d — add the new phase here and to progressGroups()",
			len(allProgressPhases), want)
	}
	for i, p := range allProgressPhases {
		if int(p) != i {
			t.Errorf("allProgressPhases[%d] = %d, want the constants in declaration order", i, p)
		}
	}
}

// TestAllScreens_Complete closes the loop the other screen-table tests depend
// on. screen is a contiguous iota run (screenSelectServer..screenInspect), so
// the count is the whole check. The bound anchors to the LAST constant, so
// appending a new screen without listing it here goes red — see the residual
// recorded in CLAUDE.md: it used to anchor to screenSettingsForm, which made
// the promise above false.
func TestAllScreens_Complete(t *testing.T) {
	if want := int(screenInspect) + 1; len(allScreens) != want {
		t.Fatalf("allScreens has %d entries, want %d — add the new screen here and to helpGroupsFor()",
			len(allScreens), want)
	}
	for i, s := range allScreens {
		if int(s) != i {
			t.Errorf("allScreens[%d] = %d, want the constants in declaration order", i, s)
		}
	}
}

func TestHelpGroups_EveryScreen(t *testing.T) {
	for _, s := range allScreens {
		for _, readOnly := range []bool{false, true} {
			groups := helpGroupsFor(s, helpContext{canGoBack: true, readOnly: readOnly})
			if len(groups) == 0 {
				t.Errorf("screen %d readOnly=%v: helpGroups returned no groups", s, readOnly)
				continue
			}
			if screenName(s) == "" {
				t.Errorf("screen %d: screenName returned empty", s)
			}
			for _, g := range groups {
				if g.title == "" {
					t.Errorf("screen %d readOnly=%v: group with empty title", s, readOnly)
				}
				if len(g.entries) == 0 {
					t.Errorf("screen %d readOnly=%v: group %q has no entries", s, readOnly, g.title)
				}
				for _, e := range g.entries {
					if e.keys == "" || e.desc == "" {
						t.Errorf("screen %d readOnly=%v: group %q has an incomplete entry %+v", s, readOnly, g.title, e)
					}
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
	"space": " ",
}

// helpKeyTokens unions the tokens one variant of a screen's table names over
// every progressPhase. Only screenProgress varies by phase, so for the other
// seven this is the same set three times — but it means the drift pin below
// covers all three progress tables instead of whichever one it happened to
// sample.
func helpKeyTokens(s screen, canGoBack, readOnly bool) map[string]bool {
	out := map[string]bool{}
	for _, phase := range allProgressPhases {
		for _, g := range helpGroupsFor(s, helpContext{canGoBack: canGoBack, readOnly: readOnly, phase: phase}) {
			for _, e := range g.entries {
				for _, tok := range strings.Fields(e.keys) {
					if alias, ok := helpKeyAliases[tok]; ok {
						tok = alias
					}
					out[tok] = true
				}
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
			"r", "d", "s", "R", "n", "N", "/", "l", "c", "x", "U", "i",
		},
		screenProgress: {"q", "ctrl+c", "esc"},
		screenLogs: {
			"q", "ctrl+c", "esc", "enter", "ctrl+r", "w", "p", "G", "f", "/",
			"n", "N", "up", "down", "left", "right", "pgup", "pgdown",
		},
		screenConfig:       {"q", "ctrl+c", "esc", "r", "e", "up", "down", "pgup", "pgdown"},
		screenSettingsList: {"q", "ctrl+c", "esc", "up", "k", "down", "j", "a", "enter", "e", "d", "y", "n"},
		screenSettingsForm: {"q", "ctrl+c", "esc", "tab", "shift+tab", "up", "down", "left", "right", "enter"},
		screenInspect: {
			"q", "ctrl+c", "esc", "r", "up", "down", "left", "right",
			"pgup", "pgdown",
		},
	}

	for _, s := range allScreens {
		keys, ok := bound[s]
		if !ok {
			t.Errorf("screen %d has no expected key set", s)
			continue
		}
		// The WRITABLE variant only. Unioning the read-only table in would
		// weaken this direction of the pin: a key deleted from the writable
		// table but still named by the read-only one would go unnoticed. The
		// read-only table has its own two-directional pin below.
		named := helpKeyTokens(s, true, false)
		boundSet := map[string]bool{}
		for _, k := range keys {
			boundSet[k] = true
			if !named[k] {
				t.Errorf("screen %s: help table does not name bound key %q", screenName(s), k)
			}
		}
		// And the other direction: the overlay must not advertise a key the
		// screen does not bind. That is the likelier drift — a binding gets
		// removed or renamed in app.go and the help table keeps promising it.
		for k := range named {
			if !boundSet[k] {
				t.Errorf("screen %s: help table names %q, which the screen does not bind", screenName(s), k)
			}
		}
		// The standalone variant (canGoBack false) is pinned in the
		// names-nothing-unbound direction only. The other direction does not
		// hold there by design: esc stays bound on a standalone container
		// screen (it clears a committed search) but leaveGroup(false) must not
		// advertise it as a way out, and on a standalone project screen esc is
		// a no-op. What the LEAVE group says in that variant is pinned exactly
		// by TestHelpGroups_LeaveGroupMatchesFooter.
		for k := range helpKeyTokens(s, false, false) {
			if !boundSet[k] {
				t.Errorf("screen %s (standalone): help table names %q, which the screen does not bind", screenName(s), k)
			}
		}
	}
}

// TestHelpGroups_ReadOnlyNamesEveryBoundKey is the same drift pin for the
// container table a read-only composer gets. The set below is what survives the
// gates in handleKey: d, r, s, R, c, space and a early-return, so naming any of
// them would advertise a no-op — and enter, bound only by the x prompt, must
// survive the loss of OPERATE, which was its only home. i is the one container
// key added without a read-only gate (inspect needs no compose file), so it is
// named by BOTH variants and appears in both bound sets.
func TestHelpGroups_ReadOnlyNamesEveryBoundKey(t *testing.T) {
	bound := []string{
		"q", "ctrl+c", "esc", "enter", "up", "k", "down", "j",
		"n", "N", "/", "l", "x", "U", "i",
	}
	boundSet := map[string]bool{}
	for _, k := range bound {
		boundSet[k] = true
	}

	named := helpKeyTokens(screenSelectContainers, true, true)
	for _, k := range bound {
		if !named[k] {
			t.Errorf("read-only services table does not name bound key %q", k)
		}
	}
	for k := range named {
		if !boundSet[k] {
			t.Errorf("read-only services table names %q, which a read-only composer gates or does not bind", k)
		}
	}
	// The standalone variant is pinned in the names-nothing-unbound direction
	// only, for the same reason as the writable table: esc stays bound (it
	// clears a committed search) but leaveGroup(false) must not offer it as a
	// way out.
	for k := range helpKeyTokens(screenSelectContainers, false, true) {
		if !boundSet[k] {
			t.Errorf("read-only services table (standalone) names %q, which a read-only composer gates or does not bind", k)
		}
	}
}

// TestHelpGroups_ReadOnlyDropsGatedKeys states the AC5 half the drift pin above
// proves only by arithmetic: the seven gated keys, their group titles and their
// descriptions are absent from the read-only table entirely.
func TestHelpGroups_ReadOnlyDropsGatedKeys(t *testing.T) {
	gated := []string{"d", "r", "s", "R", "c", " ", "a"}
	for _, canGoBack := range []bool{true, false} {
		named := helpKeyTokens(screenSelectContainers, canGoBack, true)
		for _, k := range gated {
			if named[k] {
				t.Errorf("canGoBack=%v: read-only table names gated key %q", canGoBack, k)
			}
		}
		for _, g := range readOnlyContainerGroups(canGoBack) {
			if g.title == "SELECT" || g.title == "OPERATE" {
				t.Errorf("canGoBack=%v: read-only table still carries the %q group", canGoBack, g.title)
			}
			for _, e := range g.entries {
				for _, dead := range []string{"toggle", "all", "deploy", "restart", "stop", "rollback", "config"} {
					if strings.Contains(e.desc, dead) {
						t.Errorf("canGoBack=%v: read-only entry %+v describes the gated action %q", canGoBack, e, dead)
					}
				}
			}
		}
	}
}

// TestHelpGroups_LeaveGroupMatchesFooter pins the two screens whose LEAVE
// binding is conditional. On a standalone run q QUITS and esc does nothing, so
// an overlay that says "back" would tell the user to press the key that exits
// the program — and would contradict the footer rendered a moment earlier.
func TestHelpGroups_LeaveGroupMatchesFooter(t *testing.T) {
	tests := []struct {
		name      string
		m         Model
		wantKeys  string
		wantDesc  string
		wantFoot  string
		checkFoot bool
	}{
		{
			name:     "project with servers",
			m:        Model{screen: screenSelectProject, servers: testServers},
			wantKeys: "q esc", wantDesc: "back",
		},
		{
			name:     "project standalone",
			m:        Model{screen: screenSelectProject},
			wantKeys: "q", wantDesc: "quit",
		},
		{
			name:     "containers from the picker",
			m:        Model{screen: screenSelectContainers, showPicker: true},
			wantKeys: "q esc", wantDesc: "back",
			wantFoot: "q back", checkFoot: true,
		},
		{
			name:     "containers standalone",
			m:        Model{screen: screenSelectContainers},
			wantKeys: "q", wantDesc: "quit",
			wantFoot: "q quit", checkFoot: true,
		},
		// The read-only footer drops `space toggle` from line1, so the back
		// label is the first token on it. The overlay reads the same predicate
		// and must not disagree.
		{
			name:     "read-only containers from the picker",
			m:        Model{screen: screenSelectContainers, showPicker: true, composer: &readOnlyMockComposer{}},
			wantKeys: "q esc", wantDesc: "back",
			wantFoot: "q back", checkFoot: true,
		},
		{
			name:     "read-only containers standalone",
			m:        Model{screen: screenSelectContainers, composer: &readOnlyMockComposer{}},
			wantKeys: "q", wantDesc: "quit",
			wantFoot: "q quit", checkFoot: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var leave helpGroup
			for _, g := range tt.m.helpGroups() {
				if g.title == "LEAVE" {
					leave = g
				}
			}
			if len(leave.entries) == 0 {
				t.Fatal("no LEAVE group")
			}
			got := leave.entries[0]
			if got.keys != tt.wantKeys || got.desc != tt.wantDesc {
				t.Errorf("LEAVE first entry = %q %q, want %q %q",
					got.keys, got.desc, tt.wantKeys, tt.wantDesc)
			}
			if !tt.checkFoot {
				return
			}
			footer := tt.m.containerFooter()
			if !strings.Contains(ansi.Strip(footer), tt.wantFoot) {
				t.Errorf("footer = %q, want it to contain %q (the overlay says %q %q)",
					ansi.Strip(footer), tt.wantFoot, got.keys, got.desc)
			}
		})
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

// readOnlyOverlayModel builds a read-only container model with the overlay
// open. The composer is readOnlyMockComposer, not the real HostContainers, so
// no test here shells out to docker.
func readOnlyOverlayModel(width, height int) Model {
	return Model{
		screen:     screenSelectContainers,
		services:   []string{"watchtower", "portainer"},
		selected:   make(map[int]bool),
		showPicker: true,
		composer:   &readOnlyMockComposer{},
		width:      width,
		height:     height,
		helpOpen:   true,
	}
}

// TestViewHelp_ReadOnlyOverlay renders the read-only table end to end: INSPECT
// survives with enter folded into it, and the two write groups are gone.
func TestViewHelp_ReadOnlyOverlay(t *testing.T) {
	view := ansi.Strip(readOnlyOverlayModel(120, 24).View())
	for _, want := range []string{"INSPECT", "confirm the exec prompt", "logs", "exec", "check updates"} {
		if !strings.Contains(view, want) {
			t.Errorf("read-only overlay is missing %q, got:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"OPERATE", "SELECT"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("read-only overlay still shows the %q group, got:\n%s", unwanted, view)
		}
	}
}

// TestReadOnly_NoGatedKeyAdvertised is the AC5 pin across both surfaces a user
// reads keys from. The overlay is checked at token level (a rendered key and
// its description are separated by variable padding) AND by the group titles
// and the descriptions unique to the gated actions; "deploy" is deliberately
// not among them, because the overlay title reads "cdeploy > keys > services".
func TestReadOnly_NoGatedKeyAdvertised(t *testing.T) {
	gatedTokens := []string{"d", "r", "s", "R", "c", " ", "a"}
	// Only the three tokens the WRITABLE footer actually renders. The other
	// gated keys live in the overlay alone, so asserting their absence from a
	// footer that never carried them could not fail; gatedOverlay covers them.
	gatedFooter := []string{"space toggle", "d deploy", "r restart"}
	gatedOverlay := []string{"OPERATE", "SELECT", "toggle", "rollback", "config", "restart"}

	for _, picker := range []bool{true, false} {
		m := readOnlyOverlayModel(120, 24)
		m.showPicker = picker

		named := map[string]bool{}
		for _, g := range m.helpGroups() {
			for _, e := range g.entries {
				for _, tok := range strings.Fields(e.keys) {
					if alias, ok := helpKeyAliases[tok]; ok {
						tok = alias
					}
					named[tok] = true
				}
			}
		}
		for _, k := range gatedTokens {
			if named[k] {
				t.Errorf("showPicker=%v: read-only overlay names gated key %q", picker, k)
			}
		}
		overlay := ansi.Strip(m.View())
		for _, tok := range gatedOverlay {
			if strings.Contains(overlay, tok) {
				t.Errorf("showPicker=%v: read-only overlay advertises %q, got:\n%s", picker, tok, overlay)
			}
		}

		m.helpOpen = false
		footer := ansi.Strip(m.containerFooter())
		for _, tok := range gatedFooter {
			if strings.Contains(footer, tok) {
				t.Errorf("showPicker=%v: read-only footer advertises %q, got: %q", picker, tok, footer)
			}
		}
	}
}

// TestViewHelp_ReadOnlyNeverExceedsBudget sweeps the new render path through the
// same width and height clamps the writable table is pinned against: the
// read-only table is a different group list, so a different column split and a
// different truncation point.
func TestViewHelp_ReadOnlyNeverExceedsBudget(t *testing.T) {
	for _, w := range helpWidths {
		for _, h := range []int{24, 18, 12, 6, 5} {
			m := readOnlyOverlayModel(w, h)
			view := m.View()
			if lines := strings.Split(view, "\n"); len(lines) > h {
				t.Errorf("width %d height %d: read-only overlay is %d lines, want <= %d", w, h, len(lines), h)
			}
			if strings.HasSuffix(view, "\n") {
				t.Errorf("width %d height %d: read-only overlay ends in a newline", w, h)
			}
			for i, line := range strings.Split(view, "\n") {
				if got := ansi.StringWidth(line); w > 0 && got > w {
					t.Errorf("width %d height %d: row %d is %d cells: %q", w, h, i, got, line)
				}
			}
		}
	}
	// The read-only table is short enough that a 24-line pane keeps every key
	// that lives nowhere but the overlay — the discoverability contract the
	// writable table needs singleColumnOrder for.
	for _, w := range []int{30, 40, 50, 59} {
		view := ansi.Strip(readOnlyOverlayModel(w, 24).View())
		for _, want := range []string{"search", "next / prev match", "logs", "exec", "check updates"} {
			if !strings.Contains(view, want) {
				t.Errorf("width %d: truncated read-only overlay dropped %q:\n%s", w, want, view)
			}
		}
	}
}

// helpWidths is the width matrix both overlay pins sweep. 20 is narrow enough
// that the clamp actually fires (the title alone is 30 cells); 0 is the
// NewModel default, before the first WindowSizeMsg.
var helpWidths = []int{0, 20, 40, 50, 60, 80, 120}

// TestViewHelp_FitsShortTerminal pins the height budget. The single-column
// fallback stacks the container table to 24 rows, and bubbletea's renderer
// keeps only the LAST height lines — so without a budget the title and the
// first groups scroll off a 24-line terminal with no way to scroll back.
//
// The split is deliberately RAW — no TrimRight, no TrimSuffix. bubbletea
// v1.3.10 hands View() straight to standardRenderer.write() and flush() does
// `strings.Split(r.buf.String(), "\n")` with no trimming before it keeps the
// last r.height elements, so a trailing newline IS an extra line to the
// renderer. Trimming here would measure a quantity the renderer never sees and
// would hide exactly the off-by-one this test exists to catch.
func TestViewHelp_FitsShortTerminal(t *testing.T) {
	// 5 is the smallest height the budget can honour — see
	// TestViewHelp_MinimumHeightFloor for the 1-4 band.
	for _, h := range []int{24, 18, 12, 6, 5} {
		for _, w := range helpWidths {
			for _, s := range allScreens {
				m := Model{screen: s, width: w, height: h, helpOpen: true}
				lines := strings.Split(m.View(), "\n")
				if len(lines) > h {
					t.Errorf("screen %s width %d height %d: overlay is %d lines, want <= %d",
						screenName(s), w, h, len(lines), h)
				}
			}
		}
	}
}

// TestViewHelp_MinimumHeightFloor pins the bottom of the height budget. The
// chrome alone is four physical lines (titleStyle's MarginBottom and helpStyle's
// MarginTop each add one) and budgetHelpRows floors the body at one row, so a
// pane of height 1-4 renders five lines and bubbletea keeps the last five — the
// close hint survives, the title is what falls off. Pinned so the floor is a
// decision rather than an accident: heights that short are pathological, and
// dropping the body entirely would buy four lines at the cost of the title on
// every short pane.
func TestViewHelp_MinimumHeightFloor(t *testing.T) {
	const floor = 5
	for _, h := range []int{1, 2, 3, 4} {
		for _, s := range allScreens {
			m := Model{screen: s, width: 80, height: h, helpOpen: true}
			lines := strings.Split(m.View(), "\n")
			if len(lines) != floor {
				t.Errorf("screen %s height %d: overlay is %d lines, want the %d-line floor",
					screenName(s), h, len(lines), floor)
			}
			if !strings.Contains(lines[len(lines)-1], "close") {
				t.Errorf("screen %s height %d: last line %q must carry the close hint",
					screenName(s), h, lines[len(lines)-1])
			}
		}
	}
	// Height 0 means no WindowSizeMsg yet: the budget is skipped entirely
	// rather than clamped to the floor, mirroring svcVisibleCount.
	m := Model{screen: screenSelectContainers, width: 80, height: 0, helpOpen: true}
	if got := len(strings.Split(m.View(), "\n")); got <= floor {
		t.Errorf("height 0 rendered %d lines, want the full unbudgeted overlay", got)
	}
}

// TestViewHelp_NoTrailingNewline is the direct pin on the renderer contract the
// budget above depends on. Kept separate so a regression reports the cause
// ("the view ends in a newline") and not just the symptom ("one line too many").
func TestViewHelp_NoTrailingNewline(t *testing.T) {
	for _, s := range allScreens {
		m := Model{screen: s, width: 80, height: 24, helpOpen: true}
		if v := m.View(); strings.HasSuffix(v, "\n") {
			t.Errorf("screen %s: overlay ends in a newline; bubbletea counts it as an extra line", screenName(s))
		}
	}
}

// TestViewHelp_TruncationIsReported: when the budget cuts rows, the overlay
// must say so rather than silently dropping groups.
func TestViewHelp_TruncationIsReported(t *testing.T) {
	m := Model{screen: screenSelectContainers, width: 50, height: 24, helpOpen: true}
	view := m.View()
	if !strings.Contains(view, "more") {
		t.Errorf("truncated overlay has no `▼ N more` marker, got:\n%s", view)
	}
	// The chrome must survive the cut — the title names the screen and the
	// close hint is the only way out.
	if !strings.Contains(view, "services") {
		t.Errorf("truncated overlay dropped its title, got:\n%s", view)
	}
	if !strings.Contains(view, "close") {
		t.Errorf("truncated overlay dropped the close hint, got:\n%s", view)
	}
}

// TestBudgetHelpRows covers the pure helper's boundaries. A budget below 1 is
// clamped up to 1 (the svcVisibleCount floor) rather than passed through: a
// pane too short for the chrome alone must not dump all 24 rows at the
// renderer. viewHelp handles the unknown-height case by not calling this at all.
func TestBudgetHelpRows(t *testing.T) {
	rows := []string{"a", "b", "c", "d"}
	for _, budget := range []int{-3, 0, 1} {
		got := budgetHelpRows(rows, budget, 80)
		if len(got) != 1 {
			t.Errorf("budget %d returned %d rows, want 1 (clamped floor)", budget, len(got))
		} else if !strings.Contains(got[0], "4 more") {
			t.Errorf("budget %d marker = %q, want it to report all 4 hidden rows", budget, got[0])
		}
	}
	if got := budgetHelpRows(rows, 9, 80); len(got) != 4 {
		t.Errorf("budget above the row count should leave rows untouched, got %d", len(got))
	}
	got := budgetHelpRows(rows, 3, 80)
	if len(got) != 3 {
		t.Fatalf("budget 3 returned %d rows, want 3", len(got))
	}
	if !strings.Contains(got[2], "2 more") {
		t.Errorf("marker = %q, want it to report the 2 hidden rows", got[2])
	}
	if rows[2] != "c" {
		t.Errorf("budgetHelpRows overwrote its input: %v", rows)
	}
}

// TestViewHelp_NeverExceedsWidth pins the never-wrap invariant: below the
// two-column threshold the layout falls back to one column, and every row is
// clamped as a backstop.
func TestViewHelp_NeverExceedsWidth(t *testing.T) {
	for _, w := range helpWidths {
		for _, s := range allScreens {
			m := Model{screen: s, width: w, height: 24, helpOpen: true}
			for i, line := range strings.Split(m.View(), "\n") {
				if got := ansi.StringWidth(line); w > 0 && got > w {
					t.Errorf("screen %s width %d: row %d is %d cells: %q",
						screenName(s), w, i, got, line)
				}
			}
		}
	}
}

func TestLayoutHelpColumns_SingleColumnFallback(t *testing.T) {
	groups := helpGroupsFor(screenSelectContainers, helpContext{canGoBack: true})

	wide := layoutHelpColumns(groups, 120)
	narrow := layoutHelpColumns(groups, 40)

	if len(narrow) <= len(wide) {
		t.Errorf("narrow layout should stack the columns: %d rows vs %d wide", len(narrow), len(wide))
	}
	joined := strings.Join(narrow, "\n")
	for _, g := range groups {
		if !strings.Contains(joined, g.title) {
			t.Errorf("single-column fallback dropped group %q", g.title)
		}
	}
	// Stacked, the table is taller than a short terminal, so the height budget
	// cuts the tail. The contract is the ORDER of the RENDER, not one group
	// list: every actions group must appear above every non-actions one, so
	// truncation only ever reaches keys that are guessable or already in the
	// footer. Asserted over whatever groups the screen declares, so moving a
	// group between the two halves needs no test edit.
	lastAction, firstPlain := -1, len(joined)
	for _, g := range groups {
		at := strings.Index(joined, g.title)
		if at < 0 {
			continue
		}
		if g.actions && at > lastAction {
			lastAction = at
		}
		if !g.actions && at < firstPlain {
			firstPlain = at
		}
	}
	if lastAction > firstPlain {
		t.Errorf("an action group renders below a non-action one (last action at %d, first plain at %d); truncation would drop keys that live nowhere else",
			lastAction, firstPlain)
	}
	// And the rendered rows must follow singleColumnOrder within each half.
	prev := -1
	for _, g := range singleColumnOrder(groups) {
		at := strings.Index(joined, g.title)
		if at < prev {
			t.Errorf("rendered fallback does not follow singleColumnOrder: %q at %d after %d", g.title, at, prev)
		}
		prev = at
	}
}

// TestViewHelp_NarrowTerminalKeepsActionKeys is the discoverability pin. The
// container footer carries six tokens (see containerFooter), so most of the
// screen's keys live ONLY in the overlay. If the height budget truncates one
// away on a narrow pane, that feature becomes unreachable-by-discovery.
// singleColumnOrder plus the separator-free stacked render exist for exactly
// this case; the want list below is the full set of action-group keys.
func TestViewHelp_NarrowTerminalKeepsActionKeys(t *testing.T) {
	// Descriptions, not key letters: a bare "R" or "a" would match anything.
	// The first eight have no other home at all; `deploy`, `restart` and `logs`
	// hold a footer slot today but must not be the first thing a narrow pane
	// drops either. `all` and `search` were the round-4 gap: the footer trim
	// removed `a all` and `/ search` from line1 while SELECT and the search keys
	// were still unflagged, so both fell off the bottom of a 50-column pane.
	want := []string{
		"all", "search", "next / prev match",
		"rollback", "config", "exec", "check updates", "inspect",
		"deploy", "restart", "stop", "logs",
	}
	// Below 65 columns the overlay stacks to one column, which is where the
	// budget bites; 24 is the classic short terminal.
	for _, w := range []int{30, 40, 50, 59} {
		m := Model{screen: screenSelectContainers, width: w, height: 24, showPicker: true, helpOpen: true}
		view := ansi.Strip(m.View())
		for _, s := range want {
			if !strings.Contains(view, s) {
				t.Errorf("width %d: truncated overlay dropped %q:\n%s", w, s, view)
			}
		}
	}
}

// TestViewHelp_InspectSurvivesTheFirstTruncation pins i's POSITION inside
// inspectGroup, which TestViewHelp_NarrowTerminalKeepsActionKeys cannot: that
// test samples height 24, where all 19 action rows still fit, so appending i
// last would pass it. INSPECT is the last action group singleColumnOrder emits,
// so its trailing entries are the first keys the budget sacrifices — and i
// lives nowhere but the overlay. The re-measured single-column table (widths
// 30-64, identical across them): height >= 24 keeps everything, 23 loses
// `U check updates`, 22 loses `x exec` too, 21 loses `c config` too, 20 loses
// `i inspect` too, 19 loses `l logs` too.
func TestViewHelp_InspectSurvivesTheFirstTruncation(t *testing.T) {
	for _, w := range []int{30, 40, 50, 59} {
		// 23 is one below the height that fits every action row, so it is
		// exactly the first notch where something must go.
		m := Model{screen: screenSelectContainers, width: w, height: 23, showPicker: true, helpOpen: true}
		view := ansi.Strip(m.View())
		if !strings.Contains(view, "inspect") {
			t.Errorf("width %d: i was the first key sacrificed; move it up inside inspectGroup:\n%s", w, view)
		}
	}
	// The full-fit height, so a future row added to any action group shows up
	// here as the threshold moving rather than as a silent loss — and i's
	// POSITION, as an ordering against the key the budget sacrifices first.
	// Asserting `check updates` is ABSENT at height 23 instead would pin the
	// whole overlay's first sacrifice, so any future row in any action group
	// would fail this test with a message about inspect.
	m := Model{screen: screenSelectContainers, width: 50, height: 24, showPicker: true, helpOpen: true}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "check updates") {
		t.Fatalf("height 24 no longer fits every action row:\n%s", view)
	}
	if at, uAt := strings.Index(view, "inspect"), strings.Index(view, "check updates"); at > uAt {
		t.Errorf("inspect renders below check updates, so it goes first under the budget:\n%s", view)
	}
}

func TestLayoutHelpColumns_Empty(t *testing.T) {
	if got := layoutHelpColumns(nil, 80); got != nil {
		t.Errorf("layoutHelpColumns(nil) = %v, want nil", got)
	}
}

// TestSplitHelpGroups_Balances measures the RENDERED row counts, not the
// helpGroupLines estimate splitHelpGroups balances on — the estimate counts a
// trailing separator that helpColumnRows only emits between groups, so the two
// disagree by one per column and only the rendered count decides the overlay's
// height.
func TestSplitHelpGroups_Balances(t *testing.T) {
	left, right := splitHelpGroups(helpGroupsFor(screenSelectContainers, helpContext{canGoBack: true}))
	if len(left) == 0 || len(right) == 0 {
		t.Fatalf("container groups should split into two columns, got %d/%d", len(left), len(right))
	}
	lh, rh := len(helpColumnRows(left)), len(helpColumnRows(right))
	if lh < rh {
		t.Errorf("left column (%d lines) should not be shorter than right (%d)", lh, rh)
	}
	// The container table renders as 28 rows over 6 groups once split (the two
	// columns each drop their own trailing separator); the split may not leave
	// one column more than a group's worth (5 rows) taller than the other.
	if lh-rh > 5 {
		t.Errorf("columns are unbalanced: left %d rendered rows, right %d", lh, rh)
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

// TestHelpOverlay_ClearedOnConnect pins the departure-site cleanup on BOTH
// branches of connectResultMsg. The success branch is the one that reassigns
// m.screen, which an open overlay would ride into with a stale key table; the
// error branch clears alongside quitting / clearSearch / clearWaitState.
//
// Neither state is reachable in production today — the connect runs through
// tea.ExecProcess, which suspends key input, and the overlay swallows the enter
// that starts a connect — so this pins the defence, not a live path.
func TestHelpOverlay_ClearedOnConnect(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  connectResultMsg
	}{
		{"error", connectResultMsg{err: errors.New("connection refused")}},
		{"success", connectResultMsg{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mc := &mockComposer{}
			m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
			installFakeTick(&m)
			m.screen = screenSelectServer
			m.helpOpen = true

			updated, _ := m.Update(tt.msg)
			if updated.(Model).helpOpen {
				t.Error("helpOpen = true, want false after connectResultMsg")
			}
		})
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

// --- Task 4: `?` key-reference overlay acceptance pins ------------------

// TestHelpOverlay_OpensFromEveryScreen proves the "? opens the overlay from
// all 8 screens, and the content matches the screen it was opened from"
// criterion. TestViewHelp_ReplacesScreen renders viewHelp with the flag
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
		// A bare Model is the RUNNING phase, where esc cancels the operation.
		// The other two phases are pinned by TestHelpGroups_ProgressPhases.
		{s: screenProgress, want: "cancel the operation", notWant: "rollback"},
		{s: screenLogs, want: "regex mode", notWant: "rollback"},
		{s: screenConfig, want: "$EDITOR", notWant: "regex mode"},
		{s: screenSettingsList, want: "delete server", notWant: "rollback"},
		// settingsField 4 is the color picker. Fields 0-3 are text inputs where
		// `?` deliberately lands in the field instead — see
		// TestHelpOverlay_QuestionReachesSettingsFormInput.
		{s: screenSettingsForm, setup: func(m *Model) { m.settingsField = 4 },
			want: "cycle color", notWant: "delete server"},
		// notWant is screenConfig's $EDITOR: inspect is the other viewport
		// screen with an `r` toggle, so config is the table it could be
		// confused with.
		{s: screenInspect, want: "summary / raw JSON", notWant: "$EDITOR"},
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

// TestHelpOverlay_OpensOnProgress drives the real d → enter → ? sequence.
// enterProgress does NOT clear m.confirming (only the esc back-nav out of the
// progress screen does), so an unscoped `!m.confirming` open gate would keep
// the overlay shut for the whole life of the progress screen — hiding the
// running phase's esc, which CANCELS the deploy. A Model literal with
// confirming=false cannot catch that; this sequence can.
func TestHelpOverlay_OpensOnProgress(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true
	m.width, m.height = 120, 24

	m = pressKey(m, 'd')
	if !m.confirming {
		t.Fatal("precondition: d must arm the confirmation")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.screen != screenProgress {
		t.Fatalf("precondition: enter must enter the progress screen, got screen %d", m.screen)
	}
	if !m.confirming {
		t.Fatal("precondition changed: enterProgress now clears confirming — " +
			"the screen-scoped gate in confirmPromptArmed() can be simplified")
	}

	um := pressKey(m, '?')

	if !um.helpOpen {
		t.Fatal("helpOpen = false, want true — ? must open the overlay on the progress screen")
	}
	if !strings.Contains(um.View(), "cancel the operation") {
		t.Errorf("progress overlay does not name esc's running-phase meaning, got:\n%s", um.View())
	}
}

// TestHelpGroups_ProgressPhases is the pin the external review asked for. `?`
// opens on screenProgress in every sub-state, and esc changes MEANING across
// them: mid-pipeline it CANCELS the operation, during the health gate it skips
// the wait, and once finished it navigates back. One static table advertised
// "skip health wait" and "ctrl+c quit" in all three — wrong in the riskiest one.
// Each case drives the real keys and checks the observed effect against the
// description the overlay renders in that same state.
func TestHelpGroups_ProgressPhases(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}

	base := func() Model {
		m := Model{screen: screenProgress, width: 120, height: 24}
		m.composer = mc
		m.selected = map[int]bool{}
		m.services = mc.services
		return m
	}

	tests := []struct {
		name      string
		setup     func(m *Model)
		wantPhase progressPhase
		wantDesc  string
		notDesc   []string
	}{
		{
			name:      "running",
			setup:     func(m *Model) {},
			wantPhase: progressRunning,
			wantDesc:  "cancel the operation",
			notDesc:   []string{"skip health wait", "back"},
		},
		{
			name:      "waiting",
			setup:     func(m *Model) { m.done = true; m.waiting = true },
			wantPhase: progressWaiting,
			wantDesc:  "skip health wait",
			notDesc:   []string{"cancel the operation", "ignored while running"},
		},
		{
			name:      "finished",
			setup:     func(m *Model) { m.done = true },
			wantPhase: progressFinished,
			wantDesc:  "back",
			notDesc:   []string{"cancel the operation", "skip health wait"},
		},
		{
			name:      "failed",
			setup:     func(m *Model) { m.failed = true },
			wantPhase: progressFinished,
			wantDesc:  "back",
			notDesc:   []string{"cancel the operation", "skip health wait"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base()
			tt.setup(&m)
			if got := m.progressPhase(); got != tt.wantPhase {
				t.Fatalf("progressPhase() = %d, want %d", got, tt.wantPhase)
			}

			um := pressKey(m, '?')
			if !um.helpOpen {
				t.Fatal("helpOpen = false, want true — ? must open on every progress sub-state")
			}
			view := um.View()
			if !strings.Contains(view, tt.wantDesc) {
				t.Errorf("overlay missing %q, got:\n%s", tt.wantDesc, view)
			}
			for _, no := range tt.notDesc {
				if strings.Contains(view, no) {
					t.Errorf("overlay shows %q, which is another phase's meaning, got:\n%s", no, view)
				}
			}
		})
	}

	// The drift pin unions the tokens over the phases, so a phase that dropped
	// a key would still look covered. Every phase must name all three.
	for _, phase := range allProgressPhases {
		got := map[string]bool{}
		for _, g := range progressGroups(phase) {
			for _, e := range g.entries {
				for _, tok := range strings.Fields(e.keys) {
					got[tok] = true
				}
			}
		}
		for _, want := range []string{"q", "esc", "ctrl+c"} {
			if !got[want] {
				t.Errorf("phase %d does not name %q", phase, want)
			}
		}
	}
}

// TestProgressPhases_BehaviourMatchesLabels drives esc, q and ctrl+c through
// handleKey in each phase and asserts what actually happens. It is the other
// half of TestHelpGroups_ProgressPhases: that one pins what the overlay SAYS,
// this one pins what the keys DO, so a change to either side breaks a test.
func TestProgressPhases_BehaviourMatchesLabels(t *testing.T) {
	quits := func(cmd tea.Cmd) bool {
		if cmd == nil {
			return false
		}
		_, ok := cmd().(tea.QuitMsg)
		return ok
	}

	t.Run("running esc cancels", func(t *testing.T) {
		cancelled := false
		m := Model{screen: screenProgress, width: 120, height: 24}
		m.cancel = func() { cancelled = true }
		updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		if !cancelled {
			t.Error("esc did not cancel the operation")
		}
		if updated.(Model).screen != screenProgress {
			t.Error("esc navigated away mid-run, want it to stay on the progress screen")
		}
	})

	t.Run("running q and ctrl+c are ignored", func(t *testing.T) {
		m := Model{screen: screenProgress, width: 120, height: 24}
		m.cancel = func() { t.Error("q or ctrl+c cancelled the operation") }
		for _, key := range []tea.KeyMsg{
			{Type: tea.KeyRunes, Runes: []rune("q")},
			{Type: tea.KeyCtrlC},
		} {
			updated, cmd := m.handleKey(key)
			um := updated.(Model)
			if quits(cmd) || um.quitting {
				t.Errorf("%v quit mid-run, want a no-op", key)
			}
			if um.screen != screenProgress {
				t.Errorf("%v navigated away mid-run, want a no-op", key)
			}
		}
	})

	t.Run("waiting q and esc skip the wait", func(t *testing.T) {
		for _, key := range []tea.KeyMsg{
			{Type: tea.KeyEsc},
			{Type: tea.KeyRunes, Runes: []rune("q")},
		} {
			m := Model{screen: screenProgress, width: 120, height: 24, done: true, waiting: true}
			updated, _ := m.handleKey(key)
			um := updated.(Model)
			if um.waiting {
				t.Errorf("%v did not skip the health wait", key)
			}
			if um.screen != screenProgress {
				t.Errorf("%v left the progress screen, want the skip to stay", key)
			}
		}
	})

	t.Run("waiting ctrl+c quits", func(t *testing.T) {
		m := Model{screen: screenProgress, width: 120, height: 24, done: true, waiting: true}
		_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
		if !quits(cmd) {
			t.Error("ctrl+c during the health wait did not quit")
		}
	})

	t.Run("finished q and esc go back", func(t *testing.T) {
		mc := &mockComposer{services: []string{"web"}}
		for _, key := range []tea.KeyMsg{
			{Type: tea.KeyEsc},
			{Type: tea.KeyRunes, Runes: []rune("q")},
		} {
			m := Model{screen: screenProgress, width: 120, height: 24, done: true}
			m.composer = mc
			m.selected = map[int]bool{}
			updated, _ := m.handleKey(key)
			if got := updated.(Model).screen; got != screenSelectContainers {
				t.Errorf("%v left screen %d, want the container screen", key, got)
			}
		}
	})

	t.Run("finished ctrl+c quits", func(t *testing.T) {
		m := Model{screen: screenProgress, width: 120, height: 24, done: true}
		_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
		if !quits(cmd) {
			t.Error("ctrl+c after the operation finished did not quit")
		}
	})
}

// TestHelpOverlay_ClosedByAsyncRollbackConfirm: rollbackSnapshotMsg is the only
// confirmation armed by a message rather than a key, so the `?` open gate
// cannot have seen it. Press R, open the overlay while the fetch is in flight,
// then land the result — the overlay must give way to the live prompt.
func TestHelpOverlay_ClosedByAsyncRollbackConfirm(t *testing.T) {
	snap := rollbackTestSnapshot()
	m := Model{
		screen:               screenSelectContainers,
		services:             []string{"web", "db"},
		selected:             map[int]bool{0: true},
		rollbackTargets:      []string{"web"},
		rollbackFetchSession: 1,
		width:                120,
		height:               24,
		helpOpen:             true, // ? pressed while the async fetch is running
	}

	updated, _ := m.Update(rollbackSnapshotMsg{snap: snap, live: []string{"web", "db"}, session: 1})
	um := updated.(Model)

	if !um.confirming {
		t.Fatal("precondition: the snapshot must arm the rollback confirmation")
	}
	if um.helpOpen {
		t.Error("helpOpen = true, want false — the overlay must not cover an armed rollback prompt")
	}
	if strings.Contains(um.View(), "OPERATE") {
		t.Error("the overlay is still rendered over the live confirmation prompt")
	}
}

// TestHelpOverlay_YieldsToQuitPrompt pins the order of the two global
// intercepts: the quitting block runs FIRST, so ? during a remote disconnect
// prompt is swallowed instead of drawing the overlay over a live prompt.
func TestHelpOverlay_YieldsToQuitPrompt(t *testing.T) {
	m := helpContainerModel()
	m.serverName = "prod-server"
	m.disconnectFunc = func() error { return nil }
	m.quitting = true

	um := pressKey(m, '?')

	if um.helpOpen {
		t.Error("helpOpen = true, want false while the disconnect prompt is up")
	}
	if !um.quitting {
		t.Error("quitting = false, want true (the prompt must stay up)")
	}
	if !strings.Contains(um.View(), "Disconnect from prod-server") {
		t.Errorf("View() no longer renders the disconnect prompt, got:\n%s", um.View())
	}
}

// TestHelpOverlay_SwallowsEveryActionKey widens the swallow pin past the single
// `d` case. The keys that matter most are the ones that would change m.screen
// from under the overlay (l/c/x/i), start work (U), open an input (/), or
// mutate the selection (space/enter/a).
func TestHelpOverlay_SwallowsEveryActionKey(t *testing.T) {
	containerKeys := []string{"l", "c", "x", "i", "/", "U", "r", "s", "R", "a", " ", "j", "k", "n", "N", "enter"}
	for _, key := range containerKeys {
		m := helpContainerModel()
		m.selected[0] = true
		m.svcCursor = 1
		m.helpOpen = true

		updated, cmd := m.Update(keyMsgFor(key))
		um := updated.(Model)

		if !um.helpOpen {
			t.Errorf("%q: helpOpen = false, want true (the key must be swallowed, not acted on)", key)
		}
		if um.screen != screenSelectContainers {
			t.Errorf("%q: screen changed to %d behind the overlay", key, um.screen)
		}
		if um.confirming || um.searching || um.svcCursor != 1 || len(um.selected) != 1 {
			t.Errorf("%q: state changed behind the overlay (confirming=%v searching=%v cursor=%d selected=%v)",
				key, um.confirming, um.searching, um.svcCursor, um.selected)
		}
		if cmd != nil {
			t.Errorf("%q: swallowed key returned a command", key)
		}
	}

	// The swallow block is global, so spot-check a second screen's keys too.
	for _, key := range []string{"w", "p", "G", "f", "/"} {
		m := Model{screen: screenLogs, width: 120, height: 24, helpOpen: true, logsWrap: true}
		m.logsRawLines = []string{"web | hello"}

		updated, _ := m.Update(keyMsgFor(key))
		um := updated.(Model)

		if !um.helpOpen {
			t.Errorf("logs %q: helpOpen = false, want true", key)
		}
		if um.screen != screenLogs {
			t.Errorf("logs %q: screen changed to %d behind the overlay", key, um.screen)
		}
		if !um.logsWrap || um.logsPretty || um.logFiltering || um.logSearching {
			t.Errorf("logs %q: state changed behind the overlay", key)
		}
	}
}

// keyMsgFor builds the tea.KeyMsg for a handleKey key string.
func keyMsgFor(key string) tea.KeyMsg {
	switch key {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

// TestHelpGroups_EscClearsQueryBeforeBack is the state-blindness pin the
// external review asked for. LEAVE says `q esc back` on both layered screens,
// but a COMMITTED search (container) or filter (logs) takes the FIRST press as
// "clear" — q rewrites to esc, so both keys do it — and only the next one
// navigates back. The overlay names that meaning in FIND, beside the keys that
// create the state; each case drives the real keys and checks the effect
// against what the overlay renders in that same state.
func TestHelpGroups_EscClearsQueryBeforeBack(t *testing.T) {
	t.Run("containers", func(t *testing.T) {
		for _, key := range []string{"q", "esc"} {
			m := helpContainerModel()
			m.showPicker = true
			m = pressKey(m, '/')
			for _, r := range "web" {
				m = pressKey(m, r)
			}
			updated, _ := m.Update(keyMsgFor("enter"))
			m = updated.(Model)
			if m.searchQuery != "web" || m.searching {
				t.Fatalf("precondition: want a committed search, got query %q searching %v",
					m.searchQuery, m.searching)
			}

			view := ansi.Strip(pressKey(m, '?').View())
			if !strings.Contains(view, "clear an active search") {
				t.Errorf("overlay does not name esc's clear meaning, got:\n%s", view)
			}
			if !strings.Contains(view, "back") {
				t.Errorf("overlay dropped the LEAVE back entry, got:\n%s", view)
			}

			updated, _ = m.Update(keyMsgFor(key))
			um := updated.(Model)
			if um.searchQuery != "" {
				t.Errorf("%q: searchQuery = %q, want the first press to clear it", key, um.searchQuery)
			}
			if um.screen != screenSelectContainers {
				t.Errorf("%q: screen = %d, want the container screen (the first press only clears)", key, um.screen)
			}
			updated, _ = um.Update(keyMsgFor(key))
			if got := updated.(Model).screen; got != screenSelectProject {
				t.Errorf("%q: second press left screen %d, want the project screen", key, got)
			}
		}
	})

	t.Run("logs", func(t *testing.T) {
		for _, key := range []string{"q", "esc"} {
			m := Model{screen: screenLogs, width: 120, height: 24}
			m.logsRawLines = []string{"web | hello", "db | world"}
			m = pressKey(m, 'f')
			for _, r := range "web" {
				m = pressKey(m, r)
			}
			updated, _ := m.Update(keyMsgFor("enter"))
			m = updated.(Model)
			if m.logFilterQuery != "web" || m.logFiltering {
				t.Fatalf("precondition: want a committed filter, got query %q filtering %v",
					m.logFilterQuery, m.logFiltering)
			}

			view := ansi.Strip(pressKey(m, '?').View())
			if !strings.Contains(view, "clear an active query") {
				t.Errorf("overlay does not name esc's clear meaning, got:\n%s", view)
			}
			if !strings.Contains(view, "back") {
				t.Errorf("overlay dropped the LEAVE back entry, got:\n%s", view)
			}

			updated, _ = m.Update(keyMsgFor(key))
			um := updated.(Model)
			if um.logFilterQuery != "" {
				t.Errorf("%q: logFilterQuery = %q, want the first press to clear it", key, um.logFilterQuery)
			}
			if um.screen != screenLogs {
				t.Errorf("%q: screen = %d, want the log screen (the first press only clears)", key, um.screen)
			}
			updated, _ = um.Update(keyMsgFor(key))
			if got := updated.(Model).screen; got != screenSelectContainers {
				t.Errorf("%q: second press left screen %d, want the container screen", key, got)
			}
		}
	})
}
