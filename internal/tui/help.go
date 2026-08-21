package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// helpColumnGap separates the two rendered help columns.
const helpColumnGap = 2

type helpEntry struct{ keys, desc string }

type helpGroup struct {
	title   string
	entries []helpEntry
}

// helpGroups returns the key reference for one screen. Pure data — the layout
// (one column or two) is decided by layoutHelpColumns from the terminal width.
// Every key a screen's handleKey case binds must be named here; the drift pin
// in help_test.go fails otherwise.
func helpGroups(s screen) []helpGroup {
	switch s {
	case screenSelectServer:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ k", "up"},
				{"↓ j", "down"},
			}},
			{title: "ACT", entries: []helpEntry{
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
			{title: "ACT", entries: []helpEntry{
				{"enter", "select"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "back"},
				{"ctrl+c", "quit"},
			}},
		}

	case screenSelectContainers:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ k", "up"},
				{"↓ j", "down"},
				{"/", "search"},
				{"n N", "next / prev match"},
			}},
			{title: "SELECT", entries: []helpEntry{
				{"space", "toggle"},
				{"a", "all"},
				{"enter", "confirm"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "back"},
				{"ctrl+c", "quit"},
			}},
			{title: "OPERATE", entries: []helpEntry{
				{"d", "deploy"},
				{"r", "restart"},
				{"s", "stop"},
				{"R", "rollback"},
			}},
			{title: "INSPECT", entries: []helpEntry{
				{"l", "logs"},
				{"c", "config"},
				{"x", "exec"},
				{"U", "check updates"},
			}},
		}

	case screenProgress:
		return []helpGroup{
			{title: "WAIT", entries: []helpEntry{
				{"esc", "skip health wait"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "back when finished"},
				{"ctrl+c", "quit"},
			}},
		}

	case screenLogs:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ ↓", "scroll"},
				{"← →", "scroll sideways"},
				{"pgup pgdn", "page"},
				{"G", "jump to live bottom"},
			}},
			{title: "VIEW", entries: []helpEntry{
				{"w", "soft wrap"},
				{"p", "pretty JSON"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "back"},
				{"ctrl+c", "quit"},
			}},
			{title: "FIND", entries: []helpEntry{
				{"/", "search"},
				{"f", "filter"},
				{"n N", "next / prev match"},
				{"ctrl+r", "regex mode"},
				{"enter", "commit query"},
			}},
		}

	case screenConfig:
		return []helpGroup{
			{title: "MOVE", entries: []helpEntry{
				{"↑ ↓", "scroll"},
				{"pgup pgdn", "page"},
			}},
			{title: "VIEW", entries: []helpEntry{
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
			{title: "EDIT", entries: []helpEntry{
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
			{title: "ACT", entries: []helpEntry{
				{"enter", "save"},
			}},
			{title: "LEAVE", entries: []helpEntry{
				{"q esc", "discard"},
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
	}
	return ""
}

// helpGroupLines is the rendered height of one group: title + entries + the
// blank separator that follows it. Used to balance the two columns.
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

// helpColumnRows renders one column, aligning descriptions on the widest key.
func helpColumnRows(groups []helpGroup) []string {
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
		if i > 0 {
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

// joinHelpColumns places right beside left, padding left to a common width.
func joinHelpColumns(left, right []string) []string {
	lw := 0
	for _, r := range left {
		if w := ansi.StringWidth(r); w > lw {
			lw = w
		}
	}
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var l, r string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if r == "" {
			rows = append(rows, l)
			continue
		}
		rows = append(rows, l+strings.Repeat(" ", lw-ansi.StringWidth(l)+helpColumnGap)+r)
	}
	return rows
}

// layoutHelpColumns renders the groups two-up, falling back to a single column
// when two would not fit width. Every row is clamped so the overlay can never
// wrap on a narrow terminal (same guarantee as searchBarLine/logBarLine).
func layoutHelpColumns(groups []helpGroup, width int) string {
	if len(groups) == 0 {
		return ""
	}
	rows := helpColumnRows(groups)
	if left, right := splitHelpGroups(groups); len(right) > 0 {
		two := joinHelpColumns(helpColumnRows(left), helpColumnRows(right))
		if width <= 0 || maxRowWidth(two) <= width {
			rows = two
		}
	}
	for i, r := range rows {
		rows[i] = clampToWidth(r, width)
	}
	return strings.Join(rows, "\n")
}

func maxRowWidth(rows []string) int {
	w := 0
	for _, r := range rows {
		if c := ansi.StringWidth(r); c > w {
			w = c
		}
	}
	return w
}

// viewHelp renders the `?` overlay. Like viewQuitConfirm it replaces the whole
// screen, so it is invisible to svcVisibleCount and the reserved-bar math.
func (m Model) viewHelp() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(clampToWidth("cdeploy · keys · "+screenName(m.screen), m.width)))
	b.WriteString("\n")
	b.WriteString(layoutHelpColumns(helpGroups(m.screen), m.width))
	b.WriteString("\n")
	b.WriteString(helpStyle.Render(clampToWidth("  ?  esc  q   close", m.width)))
	b.WriteString("\n")
	return b.String()
}

// typingInInput reports whether an open text input is capturing raw runes on
// the current screen. The `?` intercept skips those screens so `?` — a regex
// metacharacter the log filter accepts in RE2 mode — lands in the input
// instead of opening the overlay.
//
// The list duplicates the typing exceptions in the q->esc rewrite in app.go on
// purpose: that block mixes them with unrelated root-screen and mid-progress
// carve-outs. A new screen that opens a text input must be added to BOTH.
func (m Model) typingInInput() bool {
	switch m.screen {
	case screenSelectContainers:
		return m.searching
	case screenLogs:
		return m.logFiltering || m.logSearching
	case screenSettingsForm:
		return m.settingsField < 4 // 0-3 are textinputs; 4 is the color picker
	}
	return false
}
