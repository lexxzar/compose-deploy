package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// helpColumnGap separates the two rendered help columns.
const helpColumnGap = 2

type helpEntry struct{ keys, desc string }

type helpGroup struct {
	title   string
	entries []helpEntry
	// actions marks a group whose keys are advertised nowhere else — the
	// container footer carries six tokens and navigation keys are guessable, so
	// these are the rows a truncated overlay must keep. singleColumnOrder emits
	// them first; budgetHelpRows is where the truncation happens.
	actions bool
}

// helpContext carries the per-render facts the key tables branch on. The two
// booleans travel in named fields rather than as adjacent parameters: a
// transposed pair would compile, and helpGroupsFor(s, true, false, ...) told a
// reader nothing about which fact was which.
type helpContext struct {
	canGoBack bool
	readOnly  bool
	phase     progressPhase
}

// helpGroups returns the key reference for the current screen, with the LEAVE
// group resolved from the same back-navigation predicate the footer uses, the
// container table resolved from the same read-only predicate that gates the
// write keys, and the progress table resolved from the operation's phase.
func (m Model) helpGroups() []helpGroup {
	return helpGroupsFor(m.screen, helpContext{
		canGoBack: m.canGoBack(),
		readOnly:  m.readOnly(),
		phase:     m.progressPhase(),
	})
}

// progressPhase names the three key regimes of screenProgress. The predicate
// order below mirrors viewProgress's footer switch (waiting first, because
// waiting implies done), so the overlay and the footer cannot disagree about
// which keys are live.
type progressPhase int

const (
	progressRunning progressPhase = iota
	progressWaiting
	progressFinished
)

func (m Model) progressPhase() progressPhase {
	switch {
	case m.waiting:
		return progressWaiting
	case m.done || m.failed:
		return progressFinished
	}
	return progressRunning
}

// progressGroups is the screenProgress key table. Unlike every other screen it
// varies by sub-state, because esc changes MEANING across the three phases:
// while the pipeline runs it CANCELS the operation, during the health gate it
// skips the wait, and once finished it navigates back. A static table would
// name the wrong action in the highest-risk phase. The key tokens are the same
// three in every phase, so the drift pin holds whichever phase it samples.
func progressGroups(phase progressPhase) []helpGroup {
	switch phase {
	case progressWaiting:
		return []helpGroup{
			{title: "WAIT", entries: []helpEntry{
				{"q esc", "skip health wait"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"ctrl+c", "quit"},
			}},
		}
	case progressFinished:
		return []helpGroup{
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "back"},
				{"ctrl+c", "quit"},
			}},
		}
	}
	return []helpGroup{
		{title: "RUN", entries: []helpEntry{
			{"esc", "cancel the operation"},
			{"q ctrl+c", "ignored while running"},
		}},
	}
}

// leaveGroup is the LEAVE group for the two screens whose back binding is
// conditional (project and containers). With a parent screen, q and esc both
// navigate back; without one, q quits and esc does nothing — so the overlay
// must not advertise a back key that exits the program. canGoBack comes from
// Model.canGoBack(), the predicate that also picks the footer's back label.
func leaveGroup(canGoBack bool) helpGroup {
	if !canGoBack {
		return helpGroup{title: "LEAVE", entries: []helpEntry{
			{"q", "quit"},
			{"ctrl+c", "quit"},
		}}
	}
	return helpGroup{title: "LEAVE", entries: []helpEntry{
		{"q esc", "back"},
		{"ctrl+c", "quit"},
	}}
}

// findGroup is the container screen's search table, shared by both variants —
// `/` and `n N` work the same whether or not the composer accepts writes.
//
// It is split out of MOVE, and flagged as actions, because the footer trim left
// `/` and `n N` with no other home (see containerFooter for the six tokens the
// footer does carry). Neither is guessable, so the group must survive
// truncation.
func findGroup() helpGroup {
	return helpGroup{title: "FIND", actions: true, entries: []helpEntry{
		{"/", "search"},
		{"n N", "next / prev match"},
		// esc appears here as well as in LEAVE because a committed search
		// takes the first esc — and the q that rewrites to it — as "clear",
		// and only the next one navigates back.
		{"esc", "clear an active search"},
	}}
}

// inspectGroup is the container screen's read-path table. The read-only variant
// drops c (config needs a compose file) and gains enter, because the x prompt
// still binds it and OPERATE — enter's only home on the writable table — is
// gone whole there; the description names that sub-state, matching the
// convention for every key bound only inside one.
//
// i sits SECOND, not last, and the position is load-bearing. It is a shared
// append (i is not gated on readOnly, so it lands in both variants), and this
// group is the LAST action group singleColumnOrder emits — so its trailing
// entries are the first keys the height budget sacrifices on a short narrow
// pane. i lives nowhere but here (the footer carries six tokens and i is not
// one of them), so appending it after U would make it the first key dropped.
// Second keeps the existing sacrifice order (U, then x, then c) unchanged, and
// TestViewHelp_InspectSurvivesTheFirstTruncation pins that.
func inspectGroup(readOnly bool) helpGroup {
	entries := []helpEntry{{"l", "logs"}, {"i", "inspect"}}
	if !readOnly {
		entries = append(entries, helpEntry{"c", "config"})
	}
	entries = append(entries, helpEntry{"x", "exec"}, helpEntry{"U", "check updates"})
	if readOnly {
		entries = append(entries, helpEntry{"enter", "confirm the exec prompt"})
	}
	return helpGroup{title: "INSPECT", actions: true, entries: entries}
}

// readOnlyContainerGroups is the container table for a composer that refuses
// every write (compose.HostContainers). SELECT and OPERATE are gone whole, and
// c leaves INSPECT: space, a, d, r, s, R and c all early-return on a read-only
// composer, and a table that still named them would advertise a no-op — the
// exact failure the overlay exists to prevent.
//
// Group ORDER carries the same load as the writable table: LEAVE sits 3rd of 4
// so splitHelpGroups leaves it in the left column, and the actions flags still
// drive singleColumnOrder's truncation order.
func readOnlyContainerGroups(canGoBack bool) []helpGroup {
	return []helpGroup{
		{title: "MOVE", entries: []helpEntry{
			{"↑ k", "up"},
			{"↓ j", "down"},
		}},
		findGroup(),
		leaveGroup(canGoBack),
		inspectGroup(true),
	}
}

// helpGroupsFor returns the key reference for one screen, resolved against the
// render context. Data only — the layout (one column or two) is decided by
// layoutHelpColumns from the terminal width. Every key a screen's handleKey
// case binds must be named here; the drift pin in help_test.go fails otherwise.
//
// Group ORDER is load-bearing, not cosmetic: splitHelpGroups cuts the slice
// sequentially, so where a group sits decides which column it lands in. LEAVE
// is 4th of 6 on screenSelectContainers, 3rd of 4 in readOnlyContainerGroups
// and 3rd of 4 on screenLogs to keep it in the left column; every other screen
// ends with it. Reordering a group for readability changes the rendered layout.
func helpGroupsFor(s screen, hc helpContext) []helpGroup {
	canGoBack := hc.canGoBack
	switch s {
	case screenSelectServer:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ k", "up"},
				{"↓ j", "down"},
			}},
			{title: "ACT", actions: true, entries: []helpEntry{
				{"enter", "select"},
				{"s", "settings"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q", "quit"},
				{"ctrl+c", "quit"},
			}},
		}

	case screenSelectProject:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ k", "up"},
				{"↓ j", "down"},
			}},
			{title: "ACT", actions: true, entries: []helpEntry{
				{"enter", "select"},
			}},
			leaveGroup(canGoBack),
		}

	case screenSelectContainers:
		if hc.readOnly {
			return readOnlyContainerGroups(canGoBack)
		}
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ k", "up"},
				{"↓ j", "down"},
			}},
			findGroup(),
			// SELECT is flagged for the same reason FIND is: the footer trim
			// left `a` with no other home.
			{title: "SELECT", actions: true, entries: []helpEntry{
				{"space", "toggle"},
				{"a", "all"},
			}},
			leaveGroup(canGoBack),
			{title: "OPERATE", actions: true, entries: []helpEntry{
				{"d", "deploy"},
				{"r", "restart"},
				{"s", "stop"},
				{"R", "rollback"},
				// enter is bound only while the confirmation prompt is up, so
				// the description names that state: the overlay itself can only
				// be opened from the idle screen, where enter does nothing.
				{"enter", "confirm the prompt"},
			}},
			inspectGroup(false),
		}

	case screenProgress:
		return progressGroups(hc.phase)

	case screenLogs:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ ↓", "scroll"},
				{"← →", "scroll sideways"},
				{"pgup pgdown", "page"},
				{"G", "jump to live bottom"},
			}},
			{title: "VIEW", actions: true, entries: []helpEntry{
				{"w", "soft wrap"},
				{"p", "pretty JSON"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "back"},
				{"ctrl+c", "quit"},
			}},
			{title: "FIND", actions: true, entries: []helpEntry{
				{"/", "search"},
				{"f", "filter"},
				{"n N", "next / prev match"},
				{"ctrl+r", "regex mode"},
				{"enter", "commit query"},
				{"esc", "clear an active query"},
			}},
		}

	case screenConfig:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ ↓", "scroll"},
				{"pgup pgdown", "page"},
			}},
			{title: "VIEW", actions: true, entries: []helpEntry{
				{"r", "raw / resolved"},
				{"e", "edit in $EDITOR"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "back"},
				{"ctrl+c", "quit"},
			}},
		}

	case screenSettingsList:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ k", "up"},
				{"↓ j", "down"},
			}},
			{title: "EDIT", actions: true, entries: []helpEntry{
				{"a", "add server"},
				{"enter e", "edit server"},
				{"d", "delete server"},
				{"y n", "confirm / cancel delete"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "back"},
				{"ctrl+c", "quit"},
			}},
		}

	case screenSettingsForm:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"tab ↓", "next field"},
				{"shift+tab ↑", "previous field"},
				{"← →", "cycle color"},
			}},
			{title: "ACT", actions: true, entries: []helpEntry{
				{"enter", "save"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "discard"},
				{"ctrl+c", "quit"},
			}},
		}

	case screenInspect:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ ↓", "scroll"},
				{"pgup pgdown", "page"},
			}},
			{title: "VIEW", actions: true, entries: []helpEntry{
				{"r", "summary / raw JSON"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "back"},
				{"ctrl+c", "quit"},
			}},
		}
	}
	return nil
}

// screenName labels the overlay with the screen it was opened from.
func screenName(s screen) string {
	switch s {
	case screenSelectServer:
		return "servers"
	case screenSelectProject:
		return "projects"
	case screenSelectContainers:
		return "services"
	case screenProgress:
		return "progress"
	case screenLogs:
		return "logs"
	case screenConfig:
		return "config"
	case screenSettingsList:
		return "settings"
	case screenSettingsForm:
		return "settings form"
	case screenInspect:
		return "inspect"
	}
	return ""
}

// helpGroupLines estimates one group's height for the column balance: title +
// entries + a separator. It over-counts by one per column, because
// helpColumnRows emits the blank only BETWEEN groups — harmless for a
// heuristic, but do not read it as the rendered row count (the tests measure
// len(helpColumnRows(...)) instead).
func helpGroupLines(g helpGroup) int { return len(g.entries) + 2 }

// splitHelpGroups balances the groups across two columns by rendered height.
// right is empty when a single group cannot be split.
func splitHelpGroups(groups []helpGroup) (left, right []helpGroup) {
	total := 0
	for _, g := range groups {
		total += helpGroupLines(g)
	}
	half := (total + 1) / 2
	acc := 0
	for i, g := range groups {
		acc += helpGroupLines(g)
		left = append(left, g)
		if acc >= half && i < len(groups)-1 {
			return left, groups[i+1:]
		}
	}
	return left, nil
}

// helpColumnRows renders one column of the two-column layout.
func helpColumnRows(groups []helpGroup) []string {
	return helpRows(groups, true)
}

// helpStackedRows renders the single-column fallback.
func helpStackedRows(groups []helpGroup) []string {
	return helpRows(groups, false)
}

// helpRows renders a key table: a bold title per group, then one row per entry
// with the descriptions aligned on the widest key across ALL the groups passed
// in. separate adds a blank line BETWEEN groups — the two-column layout keeps
// it, the single-column fallback drops it because that is where the height
// budget bites. Those five blanks are the container table's 29-versus-24 rows,
// and 24 is what makes its 18 action rows fit the 19 a 24-line pane keeps.
// Every other row-count claim in this file refers back to those numbers.
func helpRows(groups []helpGroup, separate bool) []string {
	keyw := 0
	for _, g := range groups {
		for _, e := range g.entries {
			if w := ansi.StringWidth(e.keys); w > keyw {
				keyw = w
			}
		}
	}
	var rows []string
	for i, g := range groups {
		if i > 0 && separate {
			rows = append(rows, "")
		}
		rows = append(rows, "  "+helpGroupTitleStyle.Render(g.title))
		for _, e := range g.entries {
			pad := strings.Repeat(" ", keyw-ansi.StringWidth(e.keys))
			rows = append(rows, "    "+helpKeyStyle.Render(e.keys)+pad+"  "+descStyle.Render(e.desc))
		}
	}
	return rows
}

// singleColumnOrder moves the action groups (see helpGroup.actions) ahead of
// the navigation ones, keeping the relative order inside each half. It applies
// only to the single-column fallback, where the height budget bites (see
// helpRows for the row counts). In table order the tail that fell off a 24-line
// pane was `a all`, `/ search`, `n N`, `R rollback`, `c config`, `x exec` and
// `U check updates` — every one of them a key that lives nowhere but here.
// Reordered, what the budget truncates instead is MOVE and LEAVE: guessable
// arrow keys, and a back key the footer already names.
func singleColumnOrder(groups []helpGroup) []helpGroup {
	out := make([]helpGroup, 0, len(groups))
	for _, g := range groups {
		if g.actions {
			out = append(out, g)
		}
	}
	for _, g := range groups {
		if !g.actions {
			out = append(out, g)
		}
	}
	return out
}

// layoutHelpColumns renders the groups two-up, falling back to a single column
// when two would not fit width. Every row is clamped so the overlay can never
// wrap on a narrow terminal (same guarantee as searchBarLine/logBarLine).
func layoutHelpColumns(groups []helpGroup, width int) []string {
	if len(groups) == 0 {
		return nil
	}
	var rows []string
	if left, right := splitHelpGroups(groups); len(right) > 0 {
		block := lipgloss.NewStyle().PaddingRight(helpColumnGap).
			Render(strings.Join(helpColumnRows(left), "\n"))
		two := lipgloss.JoinHorizontal(lipgloss.Top, block, strings.Join(helpColumnRows(right), "\n"))
		if width <= 0 || lipgloss.Width(two) <= width {
			rows = strings.Split(two, "\n")
		}
	}
	if rows == nil {
		rows = helpStackedRows(singleColumnOrder(groups))
	}
	for i, r := range rows {
		rows[i] = clampToWidth(r, width)
	}
	return rows
}

// viewHelp renders the `?` overlay. Like viewQuitConfirm it replaces the whole
// screen, so it is invisible to svcVisibleCount and the reserved-bar math. The
// body is budgeted against m.height because bubbletea's renderer keeps the LAST
// height lines, and the overlay swallows every scroll key: overflow is cut and
// reported with the same `▼ N more` marker the service list uses.
//
// The returned string must NOT end in a newline. bubbletea v1.3.10 hands
// View() straight to standardRenderer.write() and flush() then does a bare
// strings.Split(buf, "\n") with no TrimSuffix, so a trailing newline is a whole
// extra element and the budget is off by one — the renderer drops the title,
// the exact failure the budget exists to prevent. viewSelectContainers and
// viewLogs end without one for the same reason.
func (m Model) viewHelp() string {
	title := titleStyle.Render(clampToWidth("cdeploy > keys > "+screenName(m.screen), m.width))
	hint := helpStyle.Render(clampToWidth("  ? esc q  close", m.width))
	rows := layoutHelpColumns(m.helpGroups(), m.width)
	if m.height > 0 {
		rows = budgetHelpRows(rows, m.height-lipgloss.Height(title)-lipgloss.Height(hint), m.width)
	}

	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n")
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n")
	b.WriteString(hint)
	return b.String()
}

// budgetHelpRows truncates rows to budget lines, spending the last one on a
// `▼ N more` marker that names the remedy — the overlay swallows every scroll
// key, so a wider or taller pane is the only way to see the rest. A budget
// below 1 is clamped up to 1, the same floor svcVisibleCount applies; the
// unknown-height case (m.height == 0) is handled by the caller, which skips
// this helper entirely. The floor puts the overlay's minimum at five physical
// lines, so a pane of height 1-4 over-renders by design — pinned by
// TestViewHelp_MinimumHeightFloor.
func budgetHelpRows(rows []string, budget, width int) []string {
	if budget < 1 {
		budget = 1
	}
	if len(rows) <= budget {
		return rows
	}
	keep := budget - 1
	marker := descStyle.Render(clampToWidth(
		fmt.Sprintf("  ▼ %d more · resize for the rest", len(rows)-keep), width))
	return append(rows[:keep:keep], marker)
}
