package views

import "github.com/charmbracelet/lipgloss"

const (
	// LogoText is the product wordmark shown at the top of the resident TUI.
	LogoText = "harness loop"
	// LogoColor is the light blue accent used for the wordmark (#8FD3FF).
	LogoColor = "#8FD3FF"
)

// LogoStyle builds the wordmark style: bold and light blue. With noColor the
// foreground is dropped but the weight is kept, so the logo still reads as a
// wordmark on monochrome terminals. lipgloss additionally honours the NO_COLOR
// environment variable when the style is rendered.
func LogoStyle(noColor bool) lipgloss.Style {
	style := lipgloss.NewStyle().Bold(true)
	if noColor {
		return style
	}
	return style.Foreground(lipgloss.Color(LogoColor))
}

// RenderLogo renders the "harness loop" wordmark. noColor is the caller's
// resolved colour preference (--no-color / NO_COLOR).
func RenderLogo(noColor bool) string {
	return LogoStyle(noColor).Render(LogoText)
}
