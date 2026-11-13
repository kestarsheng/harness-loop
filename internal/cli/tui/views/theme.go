package views

import "github.com/charmbracelet/lipgloss"

type Theme struct {
	NoColor bool

	Title  lipgloss.Style
	Accent lipgloss.Style
	Muted  lipgloss.Style
	OK     lipgloss.Style
	Warn   lipgloss.Style
	Err    lipgloss.Style
}

func NewTheme(noColor bool) Theme {
	if noColor {
		return Theme{NoColor: true}
	}
	return Theme{
		Title:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		Accent: lipgloss.NewStyle().Foreground(lipgloss.Color("14")),
		Muted:  lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		OK:     lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
		Warn:   lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
		Err:    lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
	}
}

func (t Theme) paint(s string, style lipgloss.Style) string {
	if t.NoColor {
		return s
	}
	return style.Render(s)
}
