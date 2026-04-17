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

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			MarginTop(1)

	logBorder = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("8")).
			Padding(0, 1)
)
