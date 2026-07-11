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

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			MarginTop(1)

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
