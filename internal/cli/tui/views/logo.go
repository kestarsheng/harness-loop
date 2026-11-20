package views

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

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

// WordmarkWidth is the terminal width at which the banner wordmark replaces
// the single-line wordmark.
const WordmarkWidth = 60

// wordmarkArt is the "harness loop" banner. Every row is 56 columns wide, so
// the banner still fits at WordmarkWidth.
var wordmarkArt = []string{
	"█  █ ████ ████ █  █ ████ ████ ████   ██   ████ ████ ████",
	"█  █ █  █ █  █ ██ █ ██   ██   ██     ██   █  █ █  █ █  █",
	"████ ████ ████ █ ██ ████ ████ ████   ██   █  █ █  █ ████",
	"█  █ █  █ █  █ █  █ ████ ████ ████   ██   ████ ████ █   ",
}

// RenderWordmark renders the width-aware wordmark. Narrow terminals get the
// single bold line; wide ones get the character-art banner. NO_COLOR keeps the
// glyphs but drops the colour.
func RenderWordmark(width int, noColor bool) string {
	if width > 0 && width < WordmarkWidth {
		return RenderLogo(noColor)
	}
	style := LogoStyle(noColor)
	rows := make([]string, 0, len(wordmarkArt))
	for _, row := range wordmarkArt {
		rows = append(rows, style.Render(row))
	}
	return strings.Join(rows, "\n")
}
