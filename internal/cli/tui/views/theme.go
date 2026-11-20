package views

import "github.com/charmbracelet/lipgloss"

// Theme is the whole palette: Primary, Secondary, Border and Err. Every view
// pulls one of these four styles so the TUI reads as one surface.
type Theme struct {
	NoColor bool

	Primary   lipgloss.Style
	Secondary lipgloss.Style
	Border    lipgloss.Style
	Err       lipgloss.Style
}

func NewTheme(noColor bool) Theme {
	if noColor {
		return Theme{NoColor: true}
	}
	dim := lipgloss.Color("8")
	return Theme{
		// Primary is the logo accent so role labels, panel titles and the
		// wordmark all read as one colour (T-P2-50).
		Primary:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(LogoColor)),
		Secondary: lipgloss.NewStyle().Foreground(dim),
		Border:    lipgloss.NewStyle().Foreground(dim),
		Err:       lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
	}
}

func (t Theme) paint(s string, style lipgloss.Style) string {
	if t.NoColor {
		return s
	}
	return style.Render(s)
}
