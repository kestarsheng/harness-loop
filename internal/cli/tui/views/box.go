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
// to map a click column onto a rune index when the input wraps. Unlike a
// component box it does not enforce MinBoxWidth: on a narrow terminal the
// composer must shrink instead of overflowing the screen.
func ComposerContentWidth(width int) int {
	outer := width - ComposerIndent
	if outer > MaxBoxWidth {
		outer = MaxBoxWidth
	}
	if outer < boxBorder+2 {
		outer = boxBorder + 2
	}
	return outer - boxBorder
}

// ComposerCap closes the open composer on its first input row: the top rule and
// the cap form the left frame, while the right side and the bottom stay open so
// the terminal caret keeps its own row (the IME requirement from T-P2-49).
const ComposerCap = "╰─ "

// ComposerPromptColumn is the cell where the input's "> " prefix starts on the
// given display row: the first row carries the cap, later rows only the indent.
func ComposerPromptColumn(row int) int {
	if row <= 0 {
		return ComposerIndent + lipgloss.Width(ComposerCap)
	}
	return ComposerIndent
}

// RenderOpenComposer draws the focused composer: a top rule, then the input
// rows with the ╰─ cap on the first one. Nothing is drawn below the last input
// row, so the caret and the IME popup stay on the bottom line.
func RenderOpenComposer(theme Theme, body string, width int) string {
	content := ComposerContentWidth(width)
	indent := strings.Repeat(" ", ComposerIndent)
	top := "╭" + strings.Repeat("─", content+boxBorder-2) + "╮"
	cap := ComposerCap
	if !theme.NoColor {
		top = theme.Border.Render(top)
		cap = theme.Border.Render(cap)
	}
	rows := []string{indent + top}
	for i, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		prefix, avail := indent, content
		if i == 0 {
			prefix = indent + cap
			avail = content - lipgloss.Width(ComposerCap)
		}
		if avail < 1 {
			avail = 1
		}
		wrapped := lipgloss.NewStyle().Width(avail).Render(line)
		for j, row := range strings.Split(wrapped, "\n") {
			if j > 0 {
				prefix = indent
			}
			// The Width style pads to the content width; the padding is
			// invisible but noisy in captured output, so drop it.
			rows = append(rows, strings.TrimRight(prefix+row, " "))
		}
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
