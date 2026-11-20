package views

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func boxLines(t *testing.T, rendered string) []string {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	if len(lines) < 3 {
		t.Fatalf("box must have a top border, content and a bottom border:\n%s", rendered)
	}
	return lines
}

func assertUniformWidth(t *testing.T, rendered string, want int) {
	t.Helper()
	for i, line := range boxLines(t, rendered) {
		if got := lipgloss.Width(line); got != want {
			t.Fatalf("line %d width = %d, want %d:\n%s", i, got, want, rendered)
		}
	}
}

func TestRenderBoxRoundedBorder(t *testing.T) {
	out := RenderBox("goal", "> fix the bug", 60)
	for _, glyph := range []string{"╭", "╮", "╰", "╯", "│"} {
		if !strings.Contains(out, glyph) {
			t.Fatalf("rounded border glyph %q missing:\n%s", glyph, out)
		}
	}
	for _, want := range []string{"goal", "> fix the bug"} {
		if !strings.Contains(out, want) {
			t.Fatalf("box missing %q:\n%s", want, out)
		}
	}
	assertUniformWidth(t, out, 60)
}

func TestRenderBoxWidthBounds(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{in: 10, want: MinBoxWidth},
		{in: 39, want: MinBoxWidth},
		{in: 40, want: 40},
		{in: 80, want: 80},
		{in: 120, want: MaxBoxWidth},
		{in: 400, want: MaxBoxWidth},
	}
	for _, tc := range cases {
		out := RenderBox("title", "body", tc.in)
		assertUniformWidth(t, out, tc.want)
	}
}

func TestRenderBoxWrapsLongBody(t *testing.T) {
	body := strings.Repeat("abcdefghij ", 20)
	out := RenderBox("", body, 50)
	if n := strings.Count(out, "abcdefghij"); n != 20 {
		t.Fatalf("wrapped body lost text: %d of 20 chunks survived:\n%s", n, out)
	}
	if lines := boxLines(t, out); len(lines) < 5 {
		t.Fatalf("long body must wrap onto extra rows, got %d lines:\n%s", len(lines), out)
	}
	assertUniformWidth(t, out, 50)
}

func TestRenderBoxEmptyBodyStillCloses(t *testing.T) {
	out := RenderBox("", "", 40)
	assertUniformWidth(t, out, 40)
	if !strings.Contains(out, "╰") || !strings.Contains(out, "╯") {
		t.Fatalf("empty box must still be closed:\n%s", out)
	}
}

// TestRenderOpenComposerClosesLastRow pins the T-P2-51 open composer: the top
// rule keeps ╭───╮, the first input row opens with ╰─ and the final row closes
// with ─╯, filling the gap with ─ so both rows end in the same column.
func TestRenderOpenComposerClosesLastRow(t *testing.T) {
	out := RenderOpenComposer(NewTheme(true), "> hi", 60)
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("a single-line input must render rule + one row, got %d:/n%s", len(lines), out)
	}
	if !strings.HasPrefix(strings.TrimLeft(lines[0], " "), "╭") || !strings.HasSuffix(lines[0], "╮") {
		t.Fatalf("top rule broken: %q", lines[0])
	}
	if !strings.Contains(lines[1], ComposerCap+"> hi") {
		t.Fatalf("input row lost its cap or text: %q", lines[1])
	}
	if !strings.HasSuffix(lines[1], "╯") {
		t.Fatalf("input row must close with ╯: %q", lines[1])
	}
	if lipgloss.Width(lines[0]) != lipgloss.Width(lines[1]) {
		t.Fatalf("frame ragged: rule=%d row=%d", lipgloss.Width(lines[0]), lipgloss.Width(lines[1]))
	}
	if !strings.Contains(lines[1], " > hi ") {
		t.Fatalf("text must keep one blank cell before the close: %q", lines[1])
	}
}

// TestRenderOpenComposerOnlyLastWrappedRowCloses checks continuation rows: they
// are left open so the block reads as a field still being typed into.
func TestRenderOpenComposerOnlyLastWrappedRowCloses(t *testing.T) {
	body := "> " + strings.Repeat("中文", 40)
	out := RenderOpenComposer(NewTheme(true), body, 40)
	lines := strings.Split(out, "\n")
	if len(lines) < 4 {
		t.Fatalf("long input must wrap, got %d rows:/n%s", len(lines), out)
	}
	for _, row := range lines[1 : len(lines)-1] {
		if strings.Contains(row, "╯") {
			t.Fatalf("only the last input row may close: %q", row)
		}
	}
	if !strings.HasSuffix(lines[len(lines)-1], "╯") {
		t.Fatalf("last row must close: %q", lines[len(lines)-1])
	}
	if n := strings.Count(out, "╯"); n != 1 {
		t.Fatalf("exactly one ╯ expected, got %d:/n%s", n, out)
	}
}

// TestComposerContentWidthReservesTheClose makes sure click mapping and the
// renderer agree on the text area: the frame minus cap, tail and one blank.
func TestComposerContentWidthReservesTheClose(t *testing.T) {
	for _, width := range []int{40, 60, 76, 120} {
		inner := ComposerInnerWidth(width)
		want := inner - lipgloss.Width(ComposerCap) - lipgloss.Width(ComposerTail) - 1
		if got := ComposerContentWidth(width); got != want {
			t.Fatalf("width %d: content = %d, want %d", width, got, want)
		}
		// lipgloss.Width of a multi-line string is the widest row, so this is
		// the frame width the composer actually occupies.
		if got, wantFrame := lipgloss.Width(RenderOpenComposer(NewTheme(true), "> x", width)), ComposerIndent+inner; got != wantFrame {
			t.Fatalf("width %d: frame width = %d, want %d", width, got, wantFrame)
		}
	}
}
