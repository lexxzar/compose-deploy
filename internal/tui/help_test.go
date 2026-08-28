package tui

import (
	"errors"
	"io"
	"slices"
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
	return helpKeyTokensCtx(s, helpContext{canGoBack: canGoBack, readOnly: readOnly})
}

// helpKeyTokensCtx is helpKeyTokens with the whole context spelled out, for the
// variants a bare (canGoBack, readOnly) pair cannot name.
func helpKeyTokensCtx(s screen, hc helpContext) map[string]bool {
	out := map[string]bool{}
	for _, phase := range allProgressPhases {
		hc.phase = phase
		for _, g := range helpGroupsFor(s, hc) {
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
		screenSelectServer: {"q", "ctrl+c", "up", "k", "down", "j", "enter", "s"},
		// The named variant is the DRILLED one (see the helpKeyTokens call
		// below), so the grouped-only fold keys — z, ← and → — are not in
		// this set; TestHelpGroups_GroupedNamesTheSameKeys pins those.
		screenSelectContainers: {
			"q", "ctrl+c", "esc", "enter", "up", "k", "down", "j",
			"pgup", "pgdown", " ", "a",
			"r", "d", "s", "R", "n", "N", "/", "l", "c", "x", "U", "i",
		},
		// enter is bound only in the waiting sub-state, where it releases the
		// health gate. progressGroups names it in both midSequence variants.
		screenProgress: {"q", "ctrl+c", "esc", "enter"},
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
		"q", "ctrl+c", "esc", "enter", "up", "k", "down", "j", "pgup", "pgdown",
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
		for _, grouped := range []bool{false, true} {
			named := helpKeyTokensCtx(screenSelectContainers, helpContext{canGoBack: canGoBack, readOnly: true, grouped: grouped})
			for _, k := range gated {
				if named[k] {
					t.Errorf("canGoBack=%v grouped=%v: read-only table names gated key %q", canGoBack, grouped, k)
				}
			}
			for _, g := range readOnlyContainerGroups(canGoBack, grouped) {
				if g.title == "SELECT" || g.title == "OPERATE" {
					t.Errorf("canGoBack=%v grouped=%v: read-only table still carries the %q group", canGoBack, grouped, g.title)
				}
				for _, e := range g.entries {
					for _, dead := range []string{"toggle", "all", "deploy", "restart", "stop", "rollback", "config"} {
						if strings.Contains(e.desc, dead) {
							t.Errorf("canGoBack=%v grouped=%v: read-only entry %+v describes the gated action %q", canGoBack, grouped, e, dead)
						}
					}
				}
			}
		}
	}
}

// TestHelpGroups_LeaveGroupMatchesFooter pins the container screen, whose
// LEAVE binding is conditional in both of its modes. On a standalone run q
// QUITS and esc does nothing, so an overlay that says "back" would tell the
// user to press the key that exits the program — and would contradict the
// footer rendered a moment earlier.
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
			name:     "grouped with servers",
			m:        Model{screen: screenSelectContainers, grouped: true, servers: testServers},
			wantKeys: "q esc", wantDesc: "back",
			wantFoot: "q back", checkFoot: true,
		},
		{
			name:     "grouped standalone",
			m:        Model{screen: screenSelectContainers, grouped: true},
			wantKeys: "q", wantDesc: "quit",
			wantFoot: "q quit", checkFoot: true,
		},
		{
			name:     "containers drilled in from the host view",
			m:        Model{screen: screenSelectContainers, drilledFromHost: true},
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
			name:     "read-only containers drilled in from the host view",
			m:        Model{screen: screenSelectContainers, drilledFromHost: true, composer: &readOnlyMockComposer{}},
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
		width:    120,
		height:   24,
		helpOpen: true,
	}
	m.setSingleGroup([]string{"web-frontend"})
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
	m := Model{
		screen:          screenSelectContainers,
		selected:        make(map[string]bool),
		drilledFromHost: true,
		composer:        &readOnlyMockComposer{},
		width:           width,
		height:          height,
		helpOpen:        true,
	}
	m.setSingleGroup([]string{"watchtower", "portainer"})
	return m
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
		m.drilledFromHost = picker

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
				t.Errorf("drilledFromHost=%v: read-only overlay names gated key %q", picker, k)
			}
		}
		overlay := ansi.Strip(m.View())
		for _, tok := range gatedOverlay {
			if strings.Contains(overlay, tok) {
				t.Errorf("drilledFromHost=%v: read-only overlay advertises %q, got:\n%s", picker, tok, overlay)
			}
		}

		m.helpOpen = false
		footer := ansi.Strip(m.containerFooter())
		for _, tok := range gatedFooter {
			if strings.Contains(footer, tok) {
				t.Errorf("drilledFromHost=%v: read-only footer advertises %q, got: %q", picker, tok, footer)
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
		// Half description for the same reason as
		// TestViewHelp_NarrowTerminalKeepsActionKeys: `pgup pgdown` widens the
		// key column, so width 30 clamps the value.
		for _, want := range []string{"search", "next / prev", "logs", "exec", "check updates"} {
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
	//
	// `next / prev` is deliberately the half description: `pgup pgdown` is the
	// widest key in the table, and helpRows aligns every description on the
	// widest key across ALL groups, so at width 30 the value column is 13
	// cells and clampToWidth cuts `next / prev match` short. The row is what
	// this pin is about, and the row is still there.
	want := []string{
		"all", "search", "next / prev",
		"rollback", "config", "exec", "check updates", "inspect",
		"deploy", "restart", "stop", "logs",
	}
	// Below 70 columns the overlay stacks to one column, which is where the
	// budget bites; 24 is the classic short terminal. Drilled variant only —
	// the grouped table carries two more action rows, so every threshold there
	// sits two notches higher (TestViewHelp_InspectSurvivesTheFirstTruncation
	// pins both).
	for _, w := range []int{30, 40, 50, 59} {
		m := Model{screen: screenSelectContainers, width: w, height: 24, drilledFromHost: true, helpOpen: true}
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
// test samples height 24, where the drilled variant's 19 action rows still fit,
// so appending i last would pass it. INSPECT is the last action group
// singleColumnOrder emits, so its trailing entries are the first keys the budget
// sacrifices — and i lives nowhere but the overlay.
//
// The thresholds are VARIANT-DEPENDENT and both are pinned. The grouped SELECT
// group carries two rows the drilled one does not (`← →`, `z`), and
// singleColumnOrder emits SELECT before INSPECT, so every grouped threshold
// sits two notches higher. Re-measured single-column tables (widths 30-59,
// identical across them; the layout goes two-column at 70 drilled / 87 grouped
// — `pgup pgdown` widened the left column by 5 cells, see
// TestViewHelp_KeyColumnClampsOnlyTheNarrowestPane):
//
//	drilled: >= 24 keeps everything, 23 loses `U check updates`, 22 loses
//	         `x exec` too, 21 loses `c config` too, 20 loses `i inspect` too,
//	         19 loses `l logs` too.
//	grouped: >= 26 keeps everything, 25 loses `U`, 24 loses `x`, 23 loses `c`,
//	         22 loses `i`, 21 loses `l`.
func TestViewHelp_InspectSurvivesTheFirstTruncation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		grouped           bool
		fullFit, firstCut int
	}{
		{name: "drilled", fullFit: 24, firstCut: 23},
		{name: "grouped", grouped: true, fullFit: 26, firstCut: 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range []int{30, 40, 50, 59} {
				// firstCut is one below the height that fits every action row,
				// so it is exactly the first notch where something must go.
				m := Model{screen: screenSelectContainers, width: w, height: tc.firstCut,
					grouped: tc.grouped, drilledFromHost: true, helpOpen: true}
				view := ansi.Strip(m.View())
				if !strings.Contains(view, "inspect") {
					t.Errorf("width %d: i was the first key sacrificed; move it up inside inspectGroup:\n%s", w, view)
				}
			}
			// The full-fit height, so a future row added to any action group
			// shows up here as the threshold moving rather than as a silent
			// loss — and i's POSITION, as an ordering against the key the
			// budget sacrifices first. Asserting `check updates` is ABSENT at
			// firstCut instead would pin the whole overlay's first sacrifice,
			// so any future row in any action group would fail this test with
			// a message about inspect.
			m := Model{screen: screenSelectContainers, width: 50, height: tc.fullFit,
				grouped: tc.grouped, drilledFromHost: true, helpOpen: true}
			view := ansi.Strip(m.View())
			if !strings.Contains(view, "check updates") {
				t.Fatalf("height %d no longer fits every action row:\n%s", tc.fullFit, view)
			}
			// One below it must lose exactly that key, or the threshold moved
			// up as well as down and the table above is stale.
			cut := Model{screen: screenSelectContainers, width: 50, height: tc.firstCut,
				grouped: tc.grouped, drilledFromHost: true, helpOpen: true}
			if strings.Contains(ansi.Strip(cut.View()), "check updates") {
				t.Errorf("height %d still fits every action row; the truncation threshold moved", tc.firstCut)
			}
			if at, uAt := strings.Index(view, "inspect"), strings.Index(view, "check updates"); at > uAt {
				t.Errorf("inspect renders below check updates, so it goes first under the budget:\n%s", view)
			}
		})
	}
}

// TestViewHelp_KeyColumnClampsOnlyTheNarrowestPane pins the width consequence
// `pgup pgdown` brought in, and is the other half of a trade two older pins
// made. helpRows aligns every description on the widest key across ALL the
// groups it renders, so an 11-cell key label pushed the value column 5 cells
// right; at width 30 the single-column row spends 4 on the indent, 11 on the
// key and 2 on the gap, leaving 13 for the description, and `next / prev match`
// (17 cells) is cut. TestViewHelp_NarrowTerminalKeepsActionKeys and
// TestViewHelp_ReadOnlyNeverExceedsBudget therefore assert the half string —
// at EVERY width they sample, 30 included, which is the strength this test
// gives back: after that edit, shortening the description itself to `next /
// prev` passes both of them and nothing else in the suite notices. The 40-59
// loop below is the only assertion that does.
//
// The widest-key assertion is the early-warning half. `check updates` is
// exactly 13 cells, so one more cell of key column cuts it and
// TestViewHelp_ReadOnlyNeverExceedsBudget fails with a misleading "dropped
// check updates" — a truncation report for what is really a column-width
// change. Failing here first names the real cause.
func TestViewHelp_KeyColumnClampsOnlyTheNarrowestPane(t *testing.T) {
	const widestKey = "pgup pgdown"
	for _, hc := range []helpContext{
		{canGoBack: true},
		{canGoBack: true, grouped: true},
		{canGoBack: true, readOnly: true},
		{canGoBack: true, readOnly: true, grouped: true},
	} {
		widest, keyw := "", 0
		for _, g := range helpGroupsFor(screenSelectContainers, hc) {
			for _, e := range g.entries {
				if w := ansi.StringWidth(e.keys); w > keyw {
					widest, keyw = e.keys, w
				}
			}
		}
		if widest != widestKey {
			t.Errorf("readOnly=%v grouped=%v: the widest key is %q (%d cells), want %q (%d) — the description budget moved, so re-measure the want lists in TestViewHelp_NarrowTerminalKeepsActionKeys and TestViewHelp_ReadOnlyNeverExceedsBudget",
				hc.readOnly, hc.grouped, widest, keyw, widestKey, ansi.StringWidth(widestKey))
		}
	}

	for _, tc := range []struct {
		name string
		view func(width int) string
	}{
		{"writable", func(width int) string {
			m := Model{screen: screenSelectContainers, width: width, height: 24,
				drilledFromHost: true, helpOpen: true}
			return ansi.Strip(m.View())
		}},
		{"read-only", func(width int) string { return ansi.Strip(readOnlyOverlayModel(width, 24).View()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			narrow := tc.view(30)
			// Clamped, not dropped: the ROW is what the two pins above are
			// about, and it must still be on screen.
			if !strings.Contains(narrow, "next / prev") {
				t.Errorf("width 30 lost the FIND row entirely:\n%s", narrow)
			}
			if strings.Contains(narrow, "next / prev match") {
				t.Errorf("width 30 no longer clamps the description — the key column narrowed, so TestViewHelp_NarrowTerminalKeepsActionKeys and TestViewHelp_ReadOnlyNeverExceedsBudget can go back to the full string:\n%s", narrow)
			}
			for _, w := range []int{40, 50, 59} {
				if view := tc.view(w); !strings.Contains(view, "next / prev match") {
					t.Errorf("width %d lost the FULL description; it was shortened, or the key column grew past %d cells:\n%s",
						w, ansi.StringWidth(widestKey), view)
				}
			}
		})
	}
}

// helpGroupTitles lists the titles of a group slice, so a test names the
// screen's declared groups rather than a hand-maintained copy of them.
func helpGroupTitles(groups []helpGroup) []string {
	titles := make([]string, 0, len(groups))
	for _, g := range groups {
		titles = append(titles, g.title)
	}
	return titles
}

func hasHelpGroup(groups []helpGroup, title string) bool {
	return slices.Contains(helpGroupTitles(groups), title)
}

// helpRendersTwoColumns answers whether a rendered overlay is two-up. Two group
// titles on ONE physical line is what that looks like: lipgloss.JoinHorizontal
// pairs left row i with right row i, and row 0 of each column is its first
// group's title, so the pairing survives the height budget.
//
// The titles come from the groups the SCREEN declares, never a list kept here:
// a renamed or added group would leave a hand-maintained copy silently blind,
// and this helper decides the two-column thresholds the overlay is pinned on.
func helpRendersTwoColumns(view string, groups []helpGroup) bool {
	titles := helpGroupTitles(groups)
	for _, line := range strings.Split(view, "\n") {
		n := 0
		for _, title := range titles {
			if strings.Contains(line, title) {
				n++
			}
		}
		if n >= 2 {
			return true
		}
	}
	return false
}

// TestViewHelp_TwoColumnThresholdPerVariant pins the width at which each
// container table stops stacking, for ALL FOUR variants — the blindness that
// let `pgup pgdown` move the grouped threshold from 77 to 87 unnoticed.
// TestSplitHelpGroups_Balances and TestViewHelp_NarrowTerminalKeepsActionKeys
// both sample the drilled table only, and the drilled threshold moved by the
// same 5 cells without costing anything: at 80 columns it is still two-column.
//
// **The grouped gap at 77-86 columns is ACCEPTED, not a bug.** `pgup pgdown`
// is the widest key label in the table, so every description shifted 5 cells
// right and the two-column block outgrew 80 columns. Below 87 the grouped
// table stacks to 28 rows, a 24-line pane keeps 19, and singleColumnOrder
// emits the 21 action rows first — so the last two, `x exec` and `U check
// updates`, fall off, and the overlay is their only home (the grouped footer
// is already at its six tokens). They come back at width >= 87, or at height
// >= 26 on a narrow pane. The alternative was shortening a description or a
// key label to buy the cells back; that was weighed and refused, so this test
// records the choice instead of leaving it to be rediscovered.
func TestViewHelp_TwoColumnThresholdPerVariant(t *testing.T) {
	for _, tc := range []struct {
		name              string
		model             func(w, h int) Model
		readOnly, grouped bool
		threshold         int
		// Whether `x exec` and `U check updates` fall off an 80x24 pane.
		dropsAtEighty bool
	}{
		{
			name: "drilled",
			model: func(w, h int) Model {
				return Model{screen: screenSelectContainers, width: w, height: h,
					drilledFromHost: true, helpOpen: true}
			},
			threshold: 70,
		},
		{
			name: "grouped",
			model: func(w, h int) Model {
				return Model{screen: screenSelectContainers, width: w, height: h,
					grouped: true, drilledFromHost: true, helpOpen: true}
			},
			grouped:       true,
			threshold:     87,
			dropsAtEighty: true,
		},
		{
			name:      "read-only drilled",
			model:     readOnlyOverlayModel,
			readOnly:  true,
			threshold: 75,
		},
		{
			// setSingleGroup stamps proj.Unmanaged from readOnly(), which the
			// read-only composer already answers true, so flipping grouped
			// leaves allGroupsUnmanaged() true — an unmanaged-only host.
			name: "read-only grouped",
			model: func(w, h int) Model {
				m := readOnlyOverlayModel(w, h)
				m.grouped = true
				return m
			},
			readOnly:  true,
			grouped:   true,
			threshold: 100,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.model(80, 24)
			if m.readOnly() != tc.readOnly || m.grouped != tc.grouped {
				t.Fatalf("fixture: readOnly() = %v, grouped = %v, want %v/%v",
					m.readOnly(), m.grouped, tc.readOnly, tc.grouped)
			}

			// The FIRST width that renders two columns is the threshold, so
			// this fails whichever way it moves.
			got := 0
			for w := 20; w <= 200; w++ {
				probe := tc.model(w, 24)
				if helpRendersTwoColumns(ansi.Strip(probe.View()), probe.helpGroups()) {
					got = w
					break
				}
			}
			if got != tc.threshold {
				t.Errorf("two-column threshold is %d, want %d — a key label or a description changed width:\n%s",
					got, tc.threshold, ansi.Strip(tc.model(tc.threshold, 24).View()))
			}
			// Wider must stay two-column: the fallback is a floor, not a band.
			for _, w := range []int{tc.threshold + 10, 200} {
				wide := tc.model(w, 24)
				if !helpRendersTwoColumns(ansi.Strip(wide.View()), wide.helpGroups()) {
					t.Errorf("width %d stacked to one column", w)
				}
			}

			view := ansi.Strip(tc.model(80, 24).View())
			for _, key := range []string{"exec", "check updates"} {
				switch shown := strings.Contains(view, key); {
				case tc.dropsAtEighty && shown:
					t.Errorf("80x24 renders %q again — the accepted gap closed, so re-measure it here and in helpRows:\n%s", key, view)
				case !tc.dropsAtEighty && !shown:
					t.Errorf("80x24 lost %q; the overlay is its only home:\n%s", key, view)
				}
			}
			if !tc.dropsAtEighty {
				return
			}
			// Both ways back out of the gap.
			for _, tv := range []struct {
				w, h int
			}{{tc.threshold, 24}, {80, 26}} {
				back := ansi.Strip(tc.model(tv.w, tv.h).View())
				for _, key := range []string{"exec", "check updates"} {
					if !strings.Contains(back, key) {
						t.Errorf("%dx%d should bring %q back:\n%s", tv.w, tv.h, key, back)
					}
				}
			}
		})
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
//
// BOTH container variants run: the grouped table carries three SELECT rows the
// drilled one does not, and covering the drilled context alone hid a 19/12
// split there — a 7-row imbalance this test's own limit would have failed.
//
// leaveLeft pins WHICH column LEAVE lands in, which the totals and the balance
// limit above cannot see. helpGroupsFor's group-order comment used to promise
// the left one unconditionally; the third grouped SELECT row moved the cut, so
// the promise now holds for the drilled table only and the split reads
// MOVE FIND SELECT | LEAVE OPERATE INSPECT there. That is recorded rather than
// corrected, so a future row that moves the cut again fails here instead of
// silently re-laying-out the overlay.
func TestSplitHelpGroups_Balances(t *testing.T) {
	for _, tc := range []struct {
		name string
		hc   helpContext
		// Rendered rows once split: the two columns each drop their own
		// trailing separator, so the total sits one under helpRows(_, true).
		total int
		// Whether the LEAVE group renders in the LEFT column.
		leaveLeft bool
	}{
		{name: "drilled", hc: helpContext{canGoBack: true}, total: 30, leaveLeft: true},
		{name: "grouped", hc: helpContext{canGoBack: true, grouped: true}, total: 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := splitHelpGroups(helpGroupsFor(screenSelectContainers, tc.hc))
			if len(left) == 0 || len(right) == 0 {
				t.Fatalf("container groups should split into two columns, got %d/%d", len(left), len(right))
			}
			if got := hasHelpGroup(left, "LEAVE"); got != tc.leaveLeft {
				t.Errorf("LEAVE in the left column = %v, want %v — the cut moved, so re-measure the split in helpGroupsFor's group-order comment: left %v, right %v",
					got, tc.leaveLeft, helpGroupTitles(left), helpGroupTitles(right))
			}
			if hasHelpGroup(right, "LEAVE") == tc.leaveLeft {
				t.Errorf("LEAVE renders in both columns or in neither: left %v, right %v",
					helpGroupTitles(left), helpGroupTitles(right))
			}
			lh, rh := len(helpColumnRows(left)), len(helpColumnRows(right))
			if lh+rh != tc.total {
				t.Errorf("split renders %d rows (%d/%d), want %d", lh+rh, lh, rh, tc.total)
			}
			if lh < rh {
				t.Errorf("left column (%d lines) should not be shorter than right (%d)", lh, rh)
			}
			// The split may not leave one column more than a group's worth
			// (5 rows) taller than the other.
			if lh-rh > 5 {
				t.Errorf("columns are unbalanced: left %d rendered rows, right %d", lh, rh)
			}
		})
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
	m := Model{
		screen:   screenSelectContainers,
		selected: make(map[string]bool),
		width:    120,
		height:   24,
	}
	m.setSingleGroup([]string{"web", "db"})
	return m
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
	m.selected[m.svcKeyAt(0)] = true
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
// every screen, and the content matches the screen it was opened from"
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
		m := Model{screen: tt.s, width: 120, height: 24, selected: map[string]bool{}}
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
		m.selected[m.svcKeyAt(0)] = true
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
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true
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
		m.selected = map[string]bool{}
		m.setSingleGroup(mc.services)
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
		for _, midSeq := range []bool{false, true} {
			got := map[string]bool{}
			for _, g := range progressGroups(phase, midSeq) {
				for _, e := range g.entries {
					for _, tok := range strings.Fields(e.keys) {
						got[tok] = true
					}
				}
			}
			for _, want := range []string{"q", "esc", "ctrl+c"} {
				if !got[want] {
					t.Errorf("phase %d (midSequence=%v) does not name %q", phase, midSeq, want)
				}
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
			m.selected = map[string]bool{}
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
		rollbackTargets:      []string{"web"},
		rollbackFetchSession: 1,
		width:                120,
		height:               24,
		helpOpen:             true, // ? pressed while the async fetch is running
	}
	m.setSingleGroup([]string{"web", "db"})
	m.selected = selectedIdx(m, 0)

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
//
// The grouped variant carries the fold keys on top. Their dispatch is gated on
// !m.grouped, so only a grouped model can pin them, and they are exactly the
// "state changed behind the overlay" this test is about: a fold hides rows AND
// re-aims the cursor.
func TestHelpOverlay_SwallowsEveryActionKey(t *testing.T) {
	shared := []string{"l", "c", "x", "i", "/", "U", "r", "s", "R", "a", " ", "j", "k",
		"pgup", "pgdown", "n", "N", "enter"}
	for _, tc := range []struct {
		name  string
		build func() Model
		keys  []string
	}{
		{name: "drilled", build: helpContainerModel, keys: shared},
		{
			name: "grouped",
			build: func() Model {
				return groupedScreenModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "postgres"))
			},
			keys: append(append([]string{}, shared...), "z", "left", "right"),
		},
	} {
		for _, key := range tc.keys {
			m := tc.build()
			// Row 1 is a service under the first group in both fixtures, so
			// one cursor position and one selection serve both.
			m.selected[m.svcKeyAt(1)] = true
			m.svcCursor = 1
			m.helpOpen = true
			rows := len(m.svcEntries)

			updated, cmd := m.Update(keyMsgFor(key))
			um := updated.(Model)

			if !um.helpOpen {
				t.Errorf("%s %q: helpOpen = false, want true (the key must be swallowed, not acted on)", tc.name, key)
			}
			if um.screen != screenSelectContainers {
				t.Errorf("%s %q: screen changed to %d behind the overlay", tc.name, key, um.screen)
			}
			if um.confirming || um.searching || um.svcCursor != 1 || len(um.selected) != 1 {
				t.Errorf("%s %q: state changed behind the overlay (confirming=%v searching=%v cursor=%d selected=%v)",
					tc.name, key, um.confirming, um.searching, um.svcCursor, um.selected)
			}
			if foldedCount(um) != 0 || len(um.svcEntries) != rows {
				t.Errorf("%s %q: fold state changed behind the overlay (%d folded, %d rows, want 0/%d)",
					tc.name, key, foldedCount(um), len(um.svcEntries), rows)
			}
			if cmd != nil {
				t.Errorf("%s %q: swallowed key returned a command", tc.name, key)
			}
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

// keyMsgFor builds the tea.KeyMsg for a handleKey key string. It is the
// package's ONE key-construction table: a named key missing a case here still
// stringifies back to its own name as a run of runes, so the dispatch matches
// and the test passes against a fake the terminal can never produce.
func keyMsgFor(key string) tea.KeyMsg {
	switch key {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
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
			m.drilledFromHost = true
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
			if got := updated.(Model); got.screen != screenSelectContainers || !got.grouped {
				t.Errorf("%q: second press left screen %d grouped %v, want the grouped host view",
					key, got.screen, got.grouped)
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

// TestHelpGroups_GroupedSelectNamesFold pins the one description that varies by
// mode. space carries two meanings on the grouped host view — fold a group
// header, select a service row — and a key bound in a sub-state must name that
// state. The drilled screen has no header row, so its description must NOT
// mention folding.
func TestHelpGroups_GroupedSelectNamesFold(t *testing.T) {
	selectDesc := func(grouped bool) string {
		t.Helper()
		for _, g := range helpGroupsFor(screenSelectContainers, helpContext{canGoBack: true, grouped: grouped}) {
			if g.title != "SELECT" {
				continue
			}
			for _, e := range g.entries {
				if e.keys == "space" {
					return e.desc
				}
			}
		}
		t.Fatalf("grouped=%v: no space entry in the SELECT group", grouped)
		return ""
	}

	got := selectDesc(true)
	for _, want := range []string{"service", "fold"} {
		if !strings.Contains(got, want) {
			t.Errorf("grouped space desc = %q, want it to name %q", got, want)
		}
	}
	if drilled := selectDesc(false); strings.Contains(drilled, "fold") {
		t.Errorf("drilled space desc = %q, want no mention of folding (no header rows exist there)", drilled)
	}
}

// The grouped variant must name every key the writable table names — that half
// rides the drift pin already checked against handleKey — plus exactly the fold
// keys the grouped host view binds and the drilled screen does not. Those three
// are the one hand-maintained list here; both directions run against it, so a
// grouped-only key added to the table without a binding (or bound without a
// row) still fails.
func TestHelpGroups_GroupedNamesTheSameKeys(t *testing.T) {
	groupedOnly := map[string]bool{"z": true, "left": true, "right": true}
	for _, canGoBack := range []bool{true, false} {
		drilled := helpKeyTokensCtx(screenSelectContainers, helpContext{canGoBack: canGoBack})
		grouped := helpKeyTokensCtx(screenSelectContainers, helpContext{canGoBack: canGoBack, grouped: true})
		for k := range drilled {
			if !grouped[k] {
				t.Errorf("canGoBack=%v: grouped table drops bound key %q", canGoBack, k)
			}
		}
		for k := range grouped {
			if !drilled[k] && !groupedOnly[k] {
				t.Errorf("canGoBack=%v: grouped table names %q, which the screen does not bind", canGoBack, k)
			}
		}
		for k := range groupedOnly {
			if !grouped[k] {
				t.Errorf("canGoBack=%v: grouped table drops grouped-only key %q", canGoBack, k)
			}
			if drilled[k] {
				t.Errorf("canGoBack=%v: drilled table names grouped-only key %q", canGoBack, k)
			}
		}
	}
}

// Group ORDER is load-bearing: splitHelpGroups cuts sequentially, so LEAVE must
// stay 4th of 6 in BOTH container variants.
func TestHelpGroups_GroupedKeepsGroupOrder(t *testing.T) {
	want := []string{"MOVE", "FIND", "SELECT", "LEAVE", "OPERATE", "INSPECT"}
	for _, grouped := range []bool{false, true} {
		groups := helpGroupsFor(screenSelectContainers, helpContext{canGoBack: true, grouped: grouped})
		if len(groups) != len(want) {
			t.Fatalf("grouped=%v: %d groups, want %d", grouped, len(groups), len(want))
		}
		for i, g := range groups {
			if g.title != want[i] {
				t.Errorf("grouped=%v: group %d is %q, want %q", grouped, i, g.title, want[i])
			}
		}
	}
}

// enter carries two meanings on the container screen — confirm a prompt, and
// drill into a project — and one key must not occupy two rows of one table. So
// the GROUPED table folds both onto one SELECT row and OPERATE drops its own,
// the way inspectGroup's read-only enter already folds them. The drilled table
// has no drill-in at all, so there OPERATE keeps the only enter row.
func TestHelpGroups_GroupedNamesDrillIn(t *testing.T) {
	enterDesc := func(grouped bool, title string) (string, bool) {
		for _, g := range helpGroupsFor(screenSelectContainers, helpContext{canGoBack: true, grouped: grouped}) {
			if g.title != title {
				continue
			}
			for _, e := range g.entries {
				if e.keys == "enter" {
					return e.desc, true
				}
			}
		}
		return "", false
	}

	desc, ok := enterDesc(true, "SELECT")
	if !ok {
		t.Fatal("grouped SELECT must name enter as the drill-in key")
	}
	// enter answers on EVERY grouped row — a header and a service row name the
	// same project — so the description must not restrict itself to a row kind.
	// It must also still name the confirmation, which is the meaning OPERATE
	// gave up.
	for _, want := range []string{"drill", "project", "confirm"} {
		if !strings.Contains(desc, want) {
			t.Errorf("grouped enter desc = %q, want it to name %q", desc, want)
		}
	}
	if strings.Contains(desc, "header") {
		t.Errorf("grouped enter desc = %q, want no header-only restriction (a service row drills too)", desc)
	}
	if _, ok := enterDesc(false, "SELECT"); ok {
		t.Error("the drilled table must not name a drill-in: it has no group headers")
	}

	if _, ok := enterDesc(true, "OPERATE"); ok {
		t.Error("grouped OPERATE must not name enter: SELECT already carries both meanings")
	}
	if desc, ok := enterDesc(false, "OPERATE"); !ok {
		t.Error("the drilled table's OPERATE must name enter as the confirmation key")
	} else if !strings.Contains(desc, "confirm") {
		t.Errorf("drilled OPERATE enter desc = %q, want it to name the confirmation", desc)
	}
}

// TestHelpGroups_ContainerNoKeyIsNamedTwice is the general form of the rule
// above: a reader scanning one rendered container table for a key must find
// exactly one row for it, or the two descriptions read as a contradiction. It
// runs all four variants (writable / read-only × grouped / drilled), so a
// future key folded into a second group fails here rather than only in the
// overlay a user happens to open.
//
// Scoped to the container screen on purpose, and esc is the one exemption:
// the two-stage esc ladder is a real pair of meanings the reader needs both
// halves of — `esc  clear an active search` in FIND takes the first press and
// `q esc  back` in LEAVE the next. screenLogs carries the same pair.
func TestHelpGroups_ContainerNoKeyIsNamedTwice(t *testing.T) {
	const ladderKey = "esc" // the documented two-stage esc; see findGroup
	for _, canGoBack := range []bool{true, false} {
		for _, readOnly := range []bool{true, false} {
			for _, grouped := range []bool{true, false} {
				hc := helpContext{canGoBack: canGoBack, readOnly: readOnly, grouped: grouped}
				seen := map[string]string{}
				for _, g := range helpGroupsFor(screenSelectContainers, hc) {
					for _, e := range g.entries {
						for _, tok := range strings.Fields(e.keys) {
							if tok == ladderKey {
								continue
							}
							if prev, dup := seen[tok]; dup {
								t.Errorf("%+v: key %q named in both %q and %q", hc, tok, prev, g.title)
								continue
							}
							seen[tok] = g.title
						}
					}
				}
			}
		}
	}
}

// TestHelpGroups_ReadOnlyGroupedNamesTheDrill pins the one key the read-only
// table used to mis-describe. enter is NOT gated on readOnly, so on a grouped
// host whose only group is the unmanaged bucket it really does drill into that
// group — while the table, which drops SELECT whole, named it as the exec
// prompt's confirmation and nothing else.
func TestHelpGroups_ReadOnlyGroupedNamesTheDrill(t *testing.T) {
	find := func(groups []helpGroup, key string) (string, bool) {
		for _, g := range groups {
			for _, e := range g.entries {
				for _, tok := range strings.Fields(e.keys) {
					if tok == key {
						return e.desc, true
					}
				}
			}
		}
		return "", false
	}

	grouped, ok := find(readOnlyContainerGroups(true, true), "enter")
	if !ok {
		t.Fatal("the read-only grouped table does not name enter at all")
	}
	if !strings.Contains(grouped, "drill") {
		t.Errorf("read-only grouped enter reads %q; it must name the drill it performs", grouped)
	}
	if !strings.Contains(grouped, "exec prompt") {
		t.Errorf("read-only grouped enter reads %q; the x prompt still binds it too", grouped)
	}

	// The DRILLED read-only screen has no group to drill into, so it keeps the
	// single meaning.
	drilled, ok := find(readOnlyContainerGroups(true, false), "enter")
	if !ok {
		t.Fatal("the read-only drilled table does not name enter at all")
	}
	if strings.Contains(drilled, "drill") {
		t.Errorf("read-only drilled enter reads %q; there is no group to drill into there", drilled)
	}
}

// TestHelpGroups_ProgressWaitingMidSequence pins the overlay half of the
// esc/enter split at the health gate. With a batch still to come the two keys
// have OPPOSITE consequences — enter starts the next project's pipeline, esc
// stops the sequence — so one row reading "skip health wait" under-described a
// destructive action.
func TestHelpGroups_ProgressWaitingMidSequence(t *testing.T) {
	descs := func(midSequence bool) string {
		var b strings.Builder
		for _, g := range progressGroups(progressWaiting, midSequence) {
			for _, e := range g.entries {
				b.WriteString(e.keys + " => " + e.desc + "\n")
			}
		}
		return b.String()
	}

	mid := descs(true)
	if !strings.Contains(mid, "enter => skip health wait, start the next project") {
		t.Errorf("the mid-sequence table must name what enter starts, got:\n%s", mid)
	}
	if !strings.Contains(mid, "q esc => stop, skip the projects left") {
		t.Errorf("the mid-sequence table must name esc as a stop, got:\n%s", mid)
	}

	last := descs(false)
	if !strings.Contains(last, "enter q esc => skip health wait") {
		t.Errorf("with nothing left to release the three keys share one row, got:\n%s", last)
	}
	if strings.Contains(last, "stop") {
		t.Errorf("the last batch's gate must not promise a choice, got:\n%s", last)
	}
}
