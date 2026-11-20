package views

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	// MinBoxWidth and MaxBoxWidth bound the component box width so the layout
	// stays readable on a narrow terminal (>= 40) and never runs away on an
	// ultra-wide one (<= 120).
	MinBoxWidth = 40
	MaxBoxWidth = 120

	// boxBorder is the horizontal space the left/right border consumes.
	// lipgloss' Width already covers the style padding, so the content width is
	// outer-boxBorder and the rendered row measures exactly outer cells.
	boxBorder = 2
)

// ClampBoxWidth folds width into the [MinBoxWidth, MaxBoxWidth] range.
func ClampBoxWidth(width int) int {
	if width < MinBoxWidth {
		return MinBoxWidth
	}
	if width > MaxBoxWidth {
		return MaxBoxWidth
	}
	return width
}

// RenderBox draws body inside a rounded component box whose total width
// (border included) is clamped to [MinBoxWidth, MaxBoxWidth]. title is rendered
// as the first inner row and may be empty. Long body lines are wrapped by
// lipgloss, so the border never breaks.
func RenderBox(title, body string, width int) string {
	return RenderBoxWithTheme(Theme{NoColor: true}, title, body, width)
}

// ComposerIndent is the left margin the composer block uses in the session
// layout. Content starts this many cells in, so the caret column is measured
// from here.
const ComposerIndent = 2

// ComposerContentWidth is the display width available to composer content at a
// given terminal width, matching RenderOpenComposer. It is what the model needs
// to map a click column onto a rune index when the input wraps.
func ComposerContentWidth(width int) int {
	outer := ClampBoxWidth(width - ComposerIndent)
	if outer < boxBorder+2 {
		outer = boxBorder + 2
	}
	return outer - boxBorder
}

// RenderOpenComposer draws the focused composer: a single top rule plus the
// input rows, with no bottom frame. Leaving the bottom open keeps the terminal
// caret (and the IME candidate window) free of box glyphs. width is the
// terminal width, so the block fits with its indent.
func RenderOpenComposer(theme Theme, body string, width int) string {
	content := ComposerContentWidth(width)
	indent := strings.Repeat(" ", ComposerIndent)
	top := "╭" + strings.Repeat("─", content+boxBorder-2) + "╮"
	if !theme.NoColor {
		top = theme.Border.Render(top)
	}
	rows := []string{indent + top}
	wrapped := lipgloss.NewStyle().Width(content).Render(strings.TrimRight(body, "\n"))
	for _, row := range strings.Split(wrapped, "\n") {
		// The Width style pads to the content width; the padding is invisible
		// but noisy in captured output, so drop it.
		rows = append(rows, strings.TrimRight(indent+row, " "))
	}
	return strings.Join(rows, "\n")
}

// RenderBoxWithTheme draws the same rounded box with the theme's dim border.
// NO_COLOR keeps the glyphs and drops only the colour.
func RenderBoxWithTheme(theme Theme, title, body string, width int) string {
	outer := ClampBoxWidth(width)
	rows := make([]string, 0, 2)
	if t := strings.TrimSpace(title); t != "" {
		rows = append(rows, t)
	}
	if b := strings.TrimRight(body, "\n"); strings.TrimSpace(b) != "" {
		rows = append(rows, strings.Split(b, "\n")...)
	}
	if len(rows) == 0 {
		rows = []string{""}
	}
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(outer - boxBorder)
	if !theme.NoColor {
		if color := theme.Border.GetForeground(); color != nil {
			style = style.BorderForeground(color)
		}
	}
	return style.Render(strings.Join(rows, "\n"))
}
