package tui

import "github.com/charmbracelet/lipgloss"

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12")).
			MarginBottom(1)

	itemStyle = lipgloss.NewStyle().
			PaddingLeft(2)

	selectedItemStyle = lipgloss.NewStyle().
				PaddingLeft(2).
				Foreground(lipgloss.Color("12")).
				Bold(true)

	descStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	checkboxOn = lipgloss.NewStyle().
			Foreground(lipgloss.Color("2")).
			Bold(true)

	checkboxOff = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	stepDone = lipgloss.NewStyle().
			Foreground(lipgloss.Color("2"))

	stepRunning = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3"))

	stepFailed = lipgloss.NewStyle().
			Foreground(lipgloss.Color("1")).
			Bold(true)

	stepWaiting = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	// Health-wait verdict styles (progress screen wait sub-state). Green pass,
	// red fail, yellow pending — mirroring the CLI verdict table's palette.
	waitPassStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("2"))

	waitFailStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("1")).
			Bold(true)

	waitPendingStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("3"))

	// waitHintStyle paints the "press R on the services screen to roll back"
	// hint shown after a failed deploy wait.
	waitHintStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3")).
			Bold(true)

	statusRunningDot = lipgloss.NewStyle().
				Foreground(lipgloss.Color("2"))

	statusStoppedDot = lipgloss.NewStyle().
				Foreground(lipgloss.Color("1"))

	healthHealthy = lipgloss.NewStyle().
			Foreground(lipgloss.Color("2"))

	healthUnhealthy = lipgloss.NewStyle().
			Foreground(lipgloss.Color("1"))

	healthStarting = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3"))

	groupHeaderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8")).
				PaddingLeft(2)

	warningStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3"))

	// updateGlyphStyle paints the update-available glyph (rendered after
	// the service name on the container-select screen and in CLI list output)
	// in the same yellow used for warnings — it is a soft, attention-drawing
	// signal, not an error.
	updateGlyphStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("3"))

	// searchMatchStyle paints service names that match the active container
	// search in the same yellow as the update glyph — a soft highlight.
	searchMatchStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("3"))

	// searchCurrentStyle marks the current match (the row the cursor sits on)
	// with the same yellow plus bold so it stands out among the matches.
	searchCurrentStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("3")).
				Bold(true)

	// logFollowStyle paints the "● following" indicator in the log view header
	// green — the tail is live and auto-scrolling with the stream.
	logFollowStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("2"))

	// logPauseStyle paints the "⏸ paused ▲ N below" indicator in the log view
	// header yellow (same as updateGlyphStyle) — the user has scrolled up and
	// auto-scroll is paused; N is the distance in rows to the live bottom.
	logPauseStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3"))

	// logSearchMatchStyle paints log lines that match the active log-view search
	// in the same yellow as the container search — a soft highlight overlaid at
	// SetContent time (mirrors searchMatchStyle).
	logSearchMatchStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("3"))

	// logSearchCurrentStyle marks the current match (the line n/N last jumped to)
	// with the same yellow plus bold so it stands out among the matches (mirrors
	// searchCurrentStyle).
	logSearchCurrentStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("3")).
				Bold(true)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			MarginTop(1)

	// helpGroupTitleStyle and helpKeyStyle paint the `?` overlay. Foreground
	// and Bold only, so ansi.StringWidth still measures the column math.
	helpGroupTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("8"))

	helpKeyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("12"))

	logBorder = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("8")).
			Padding(0, 1)

	colorMap = map[string]lipgloss.Color{
		"red":     lipgloss.Color("1"),
		"green":   lipgloss.Color("2"),
		"yellow":  lipgloss.Color("3"),
		"blue":    lipgloss.Color("4"),
		"magenta": lipgloss.Color("5"),
		"cyan":    lipgloss.Color("6"),
		"white":   lipgloss.Color("7"),
		"gray":    lipgloss.Color("8"),
	}
)

func serverBadgeStyle(color string) lipgloss.Style {
	c, ok := colorMap[color]
	if !ok {
		c = colorMap["gray"]
	}
	return lipgloss.NewStyle().
		Background(c).
		Foreground(lipgloss.Color("0")).
		Bold(true)
}
