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
