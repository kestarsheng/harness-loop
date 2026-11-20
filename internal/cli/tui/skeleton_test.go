package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// hasWordmark accepts either the single-line wordmark or the wide banner.
func hasWordmark(view string) bool {
	return strings.Contains(view, "harness loop") || strings.Contains(view, "| |__")
}

func sizedSessionModel(t *testing.T, width, height int, opts Options) *Model {
	t.Helper()
	cfg := SessionConfig{}
	opts.Session = &cfg
	m := NewModel(TaskState{}, nil, opts)
	next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m = next.(*Model)
	_ = m.Init()
	return m
}

func TestViewFixedSkeletonFillsTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 20}, {60, 24}, {120, 40}} {
		m := sizedSessionModel(t, size[0], size[1], Options{NoColor: true, WorkDir: "/tmp/work", GitBranch: "main", ModelName: "m"})
		lines := strings.Split(m.View(), "\n")
		if len(lines) != size[1] {
			t.Fatalf("%dx%d view has %d lines, want %d", size[0], size[1], len(lines), size[1])
		}
		// T-P2-49: the input line is the bottom row; the status bar moved above
		// the hint and the composer.
		if !strings.Contains(lines[len(lines)-1], promptPrefix) {
			t.Fatalf("%dx%d input line is not the last row: %q", size[0], size[1], lines[len(lines)-1])
		}
		composer := false
		for _, line := range lines[:len(lines)-1] {
			if strings.Contains(line, "╭") {
				composer = true
			}
		}
		if !composer {
			t.Fatalf("%dx%d composer frame missing above the input", size[0], size[1])
		}
	}
}

func TestStatusBarShowsModelCwdBranchTokens(t *testing.T) {
	m := sizedSessionModel(t, 120, 30, Options{NoColor: true, WorkDir: "/srv/app", GitBranch: "feature/p2-tui-v2", ModelName: "gpt-x"})
	view := m.View()
	want := "gpt-x · /srv/app · feature/p2-tui-v2 · 0 tokens · ? help"
	if !strings.Contains(view, want) {
		t.Fatalf("status bar missing %q:\n%s", want, view)
	}
}

func TestStatusBarDropsSegmentsOnNarrowTerminal(t *testing.T) {
	m := sizedSessionModel(t, 30, 20, Options{NoColor: true, WorkDir: "/a/very/long/path/to/the/workspace", GitBranch: "feature/long-branch", ModelName: "model"})
	status := ""
	for _, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(line, "? help") {
			status = line
		}
	}
	if status == "" {
		t.Fatal("status bar missing")
	}
	if lipgloss.Width(status) > 30 {
		t.Fatalf("status bar overflows narrow terminal: %q", status)
	}
	if !strings.Contains(status, "? help") {
		t.Fatalf("status bar must keep the help hint: %q", status)
	}
}

func TestViewNarrowTerminalDoesNotOverflow(t *testing.T) {
	m := sizedSessionModel(t, 50, 24, Options{NoColor: true, WorkDir: "/w", GitBranch: "b", ModelName: "m"})
	m = typeLine(t, m, "这是一段很长的多行输入，用来确认窄终端不会溢出并会硬换行")
	for _, line := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(line) > 50 {
			t.Fatalf("line overflows 50 columns (width %d): %q", lipgloss.Width(line), line)
		}
	}
}

func TestViewKeepsComposerWhenPanelOpen(t *testing.T) {
	m := sizedSessionModel(t, 80, 24, Options{NoColor: true, WorkDir: "/w", GitBranch: "b", ModelName: "m"})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = next.(*Model)
	lines := strings.Split(m.View(), "\n")
	if !strings.Contains(lines[len(lines)-1], promptPrefix) {
		t.Fatalf("input must stay the last row under the help panel: %q", lines[len(lines)-1])
	}
	if !strings.Contains(m.View(), "? help") {
		t.Fatal("status bar must survive the help panel")
	}
	if !strings.Contains(m.View(), "Keys") {
		t.Fatal("help layer missing")
	}
	if !strings.Contains(m.View(), "Enter to send") {
		t.Fatal("composer must stay visible under the help layer")
	}
}
