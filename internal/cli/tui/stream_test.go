package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"agent-platform/internal/cli/tui/views"
)

func TestSpinnerIntervalIsFortyPercentFaster(t *testing.T) {
	if spinnerInterval != 275*time.Millisecond {
		t.Fatalf("spinner interval = %v, want 275ms (385/1.4)", spinnerInterval)
	}
}

func TestSpinnerFrames(t *testing.T) {
	want := "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
	if spinnerFrames != want {
		t.Fatalf("spinner frames = %q, want %q", spinnerFrames, want)
	}
	seen := map[string]bool{}
	for i := 0; i < len([]rune(want)); i++ {
		seen[string([]rune(want)[i])] = true
	}
	if len(seen) != 10 {
		t.Fatalf("spinner frames must be distinct, got %d", len(seen))
	}
}

func TestSpinnerTickAdvancesAndStops(t *testing.T) {
	fake := &fakeChat{}
	m := sessionModel(t, SessionConfig{Chat: fake.fn})
	m, _ = submitChat(t, m, "hi")

	next, cmd := m.Update(spinTickMsg{Gen: m.chatGen})
	m = next.(*Model)
	if m.spinnerFrame != 1 {
		t.Fatalf("spinner frame = %d, want 1", m.spinnerFrame)
	}
	if cmd == nil {
		t.Fatal("an active stream must schedule the next spinner tick")
	}

	m, _ = pushChunk(t, m, nil, ChatChunk{Type: "final", Content: "done"})
	next, cmd = m.Update(spinTickMsg{Gen: m.chatGen})
	m = next.(*Model)
	if cmd != nil {
		t.Fatal("a finished stream must stop the spinner")
	}
}

func TestStreamingStatusShowsThinkingAndInterrupt(t *testing.T) {
	fake := &fakeChat{}
	m := sessionModel(t, SessionConfig{Chat: fake.fn})
	m, _ = submitChat(t, m, "hi")
	m.thinkingStarted = m.now().Add(-3 * time.Second)

	view := m.View()
	for _, want := range []string{"Thinking… 3s", "esc to interrupt"} {
		if !strings.Contains(view, want) {
			t.Fatalf("streaming status missing %q:\n%s", want, view)
		}
	}
}

func TestToolLineSuccessAndFailure(t *testing.T) {
	fake := &fakeChat{}
	m := sessionModel(t, SessionConfig{Chat: fake.fn})
	m, wait := submitChat(t, m, "hi")

	m, wait = pushChunk(t, m, wait, ChatChunk{Type: "tool_start", ToolName: "Read", ToolArgs: "src/main.ts"})
	if !strings.Contains(m.View(), views.ToolMarkPending+" Read(src/main.ts)") {
		t.Fatalf("pending tool row missing %s:\n%s", views.ToolMarkPending, m.View())
	}
	m, _ = pushChunk(t, m, wait, ChatChunk{Type: "tool_result", ToolName: "Read", ToolResult: "ok"})
	if !strings.Contains(m.View(), "✓ Read(src/main.ts)") {
		t.Fatalf("successful tool row missing ✓:\n%s", m.View())
	}

	fake2 := &fakeChat{}
	f := sessionModel(t, SessionConfig{Chat: fake2.fn})
	f, wait = submitChat(t, f, "hi")
	f, wait = pushChunk(t, f, wait, ChatChunk{Type: "tool_start", ToolName: "Write", ToolArgs: "out.txt"})
	f, _ = pushChunk(t, f, wait, ChatChunk{Type: "tool_result", ToolName: "Write", ToolResult: "error: permission denied"})
	if !strings.Contains(f.View(), "✗ Write(out.txt)") {
		t.Fatalf("failed tool row missing ✗:\n%s", f.View())
	}
}

func TestEscInterruptsStream(t *testing.T) {
	fake := &fakeChat{}
	m := sessionModel(t, SessionConfig{Chat: fake.fn})
	m, wait := submitChat(t, m, "hi")
	m, _ = pushChunk(t, m, wait, ChatChunk{Type: "token", Content: "partial"})

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(*Model)
	if cmd != nil {
		t.Fatal("interrupt must not quit the program")
	}
	if m.chatStreaming || !m.awaitingInput {
		t.Fatalf("interrupt state: streaming=%v awaiting=%v", m.chatStreaming, m.awaitingInput)
	}
	if !strings.Contains(m.View(), "(interrupted)") {
		t.Fatalf("interrupted reply must be marked:\n%s", m.View())
	}
}

func TestCursorBlinks(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	m.spinnerFrame = 0
	if got := m.cursorChar(); got != "▍" {
		t.Fatalf("frame 0 cursor = %q", got)
	}
	m.spinnerFrame = 1
	if got := m.cursorChar(); got != "▌" {
		t.Fatalf("frame 1 cursor = %q", got)
	}
	view := m.View()
	if view == "" {
		t.Fatal("empty view")
	}
}

func TestFinalReRendersMarkdown(t *testing.T) {
	fake := &fakeChat{}
	m := sessionModel(t, SessionConfig{Chat: fake.fn})
	m, wait := submitChat(t, m, "hi")
	m, wait = pushChunk(t, m, wait, ChatChunk{Type: "token", Content: "**bold**"})
	if !strings.Contains(m.View(), "**bold**") {
		t.Fatalf("streaming tokens must stay raw markdown:\n%s", m.View())
	}
	m, _ = pushChunk(t, m, wait, ChatChunk{Type: "final", Content: "**bold**"})
	view := m.View()
	if strings.Contains(view, "**bold**") {
		t.Fatalf("final reply must be re-rendered as markdown:\n%s", view)
	}
	if !strings.Contains(view, "bold") {
		t.Fatalf("markdown text lost:\n%s", view)
	}
}

func TestChatChunkUsageFeedsStatusBar(t *testing.T) {
	fake := &fakeChat{}
	m := sessionModel(t, SessionConfig{Chat: fake.fn})
	m, wait := submitChat(t, m, "hi")
	m, _ = pushChunk(t, m, wait, ChatChunk{Type: "final", Content: "done", Usage: 1234})
	if !strings.Contains(m.View(), "1.2k tokens") {
		t.Fatalf("usage not shown in status bar:\n%s", m.View())
	}
}
