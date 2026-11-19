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
	return style.Render(strings.Join(rows, "\n"))
}
