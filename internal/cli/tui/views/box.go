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

// ComposerInnerWidth is the frame width of the composer measured from
// ComposerIndent: the top rule and the closed input row both span this many
// cells. Unlike a component box it does not enforce MinBoxWidth: on a narrow
// terminal the composer must shrink instead of overflowing the screen.
func ComposerInnerWidth(width int) int {
	outer := width - ComposerIndent
	if outer > MaxBoxWidth {
		outer = MaxBoxWidth
	}
	if outer < boxBorder+2 {
		outer = boxBorder + 2
	}
	return outer
}

// ComposerContentWidth is the display width available to the input text on one
// row: the frame minus the ╰─ cap, the ─╯ tail and one cell of breathing room
// before the tail. SetCursorByCell uses it to count wrapped rows, so a click on
// the right-hand close never lands past the text.
func ComposerContentWidth(width int) int {
	w := ComposerInnerWidth(width) - lipgloss.Width(ComposerCap) - lipgloss.Width(ComposerTail) - 1
	if w < 1 {
		w = 1
	}
	return w
}

// ComposerCap opens the input row on the left and ComposerTail closes it on the
// right: together they make the field read as finished ("╰─ > text ──╯")
// without drawing anything below the input row. The terminal caret and the IME
// candidate window keep their own row, which T-P2-49 requires.
const (
	ComposerCap  = "╰─ "
	ComposerTail = "─╯"
)

// ComposerPromptColumn is the cell where the input's "> " prefix starts on the
// given display row: the first row carries the cap, later rows only the indent.
func ComposerPromptColumn(row int) int {
	if row <= 0 {
		return ComposerIndent + lipgloss.Width(ComposerCap)
	}
	return ComposerIndent
}

// RenderOpenComposer draws the focused composer: the ╭───╮ top rule, then the
// input rows opened on the left by ╰─. Only the last input row is closed on the
// right (with ─╯), so the block reads as a finished field while nothing is drawn
// below it: the caret and the IME popup stay on the bottom line. Wrapped
// continuation rows stay open, exactly like a real framed editor that is still
// being typed into.
func RenderOpenComposer(theme Theme, body string, width int) string {
	inner := ComposerInnerWidth(width)
	textWidth := ComposerContentWidth(width)
	indent := strings.Repeat(" ", ComposerIndent)

	top := "╭" + strings.Repeat("─", inner-boxBorder) + "╮"
	cap := ComposerCap
	if !theme.NoColor {
		top = theme.Border.Render(top)
		cap = theme.Border.Render(cap)
	}

	// Wrap on display cells, not runes: a CJK glyph or emoji is two cells wide
	// and must never push the frame past the right edge. The wrap is hard (no
	// word breaking): Chinese has no spaces, and lipgloss' word wrap would
	// shove a whole run onto the next row and leave "╰─ >" alone.
	var bodyRows []string
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		bodyRows = append(bodyRows, wrapCells(line, textWidth)...)
	}
	if len(bodyRows) == 0 {
		bodyRows = []string{""}
	}

	rows := []string{indent + top}
	for i, row := range bodyRows {
		prefix := indent
		if i == 0 {
			prefix = indent + cap
		}
		// The Width style pads to the text width; the padding is invisible but
		// noisy in captured output, so drop it. Only the last row is padded
		// again, by the close below.
		rows = append(rows, strings.TrimRight(prefix+row, " "))
	}
	rows[len(rows)-1] = closeComposerRow(theme, rows[len(rows)-1], inner)
	return strings.Join(rows, "\n")
}

// closeComposerRow fills the gap between the last input row and the right edge
// with a rule and caps it with ─╯. The row keeps exactly one blank cell before
// the tail so a full line of text never touches the corner glyph.
func closeComposerRow(theme Theme, row string, inner int) string {
	target := ComposerIndent + inner - lipgloss.Width(ComposerTail)
	if cur := lipgloss.Width(row); cur > target {
		row = clipCells(row, target)
	} else if pad := target - cur; pad > 0 {
		filler := " "
		if pad > 1 {
			filler += strings.Repeat("─", pad-1)
		}
		if !theme.NoColor {
			filler = theme.Border.Render(filler)
		}
		row += filler
	}
	tail := ComposerTail
	if !theme.NoColor {
		tail = theme.Border.Render(tail)
	}
	return row + tail
}

// clipCells truncates s to at most width display cells without cutting a rune
// or an escape sequence in half.
func clipCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	var b strings.Builder
	cells := 0
	for _, r := range s {
		w := lipgloss.Width(string(r))
		if w < 0 {
			w = 0
		}
		if cells+w > width {
			break
		}
		b.WriteRune(r)
		cells += w
	}
	return b.String()
}

// wrapCells hard-wraps s at width display cells, keeping runes whole. Unlike
// lipgloss' Width it never breaks on word boundaries, so a long CJK run stays
// on the row it started on and every produced row measures <= width cells.
func wrapCells(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	cells := 0
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur.String())
			cur.Reset()
			cells = 0
			continue
		}
		w := lipgloss.Width(string(r))
		if w < 0 {
			w = 0
		}
		if cells+w > width && cells > 0 {
			out = append(out, cur.String())
			cur.Reset()
			cells = 0
		}
		cur.WriteRune(r)
		cells += w
	}
	out = append(out, cur.String())
	return out
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
