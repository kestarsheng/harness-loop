package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	harnesspb "agent-platform/pkg/pb/harnessloop"
)

// fakeChat records what the session sent to agent-service and hands the test
// the channel it can push chunks into.
type fakeChat struct {
	messages []string
	ch       chan ChatChunk
	openErr  error
}

func (f *fakeChat) fn(ctx context.Context, message string) (<-chan ChatChunk, error) {
	f.messages = append(f.messages, message)
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.ch = make(chan ChatChunk, 16)
	return f.ch, nil
}

// submitChat types a line and runs the two commands a chat submission produces:
// the Chat call itself, then the first read off the stream.
func submitChat(t *testing.T, m *Model, line string) (*Model, tea.Cmd) {
	t.Helper()
	m = typeLine(t, m, line)
	next, submitCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*Model)
	if submitCmd == nil {
		t.Fatalf("chat submission produced no command")
	}
	// Level 1: the composer emits promptSubmitMsg; level 2: the session turns
	// it into the Chat call; level 3: the call opens the stream.
	next, chatCmd := m.Update(submitCmd())
	m = next.(*Model)
	if chatCmd == nil {
		t.Fatalf("chat submission produced no chat command (errText=%q)", m.errText)
	}
	next, waitCmd := m.Update(chatCmd())
	return next.(*Model), waitCmd
}

func pushChunk(t *testing.T, m *Model, wait tea.Cmd, chunk ChatChunk) (*Model, tea.Cmd) {
	t.Helper()
	// The pending read command is what would consume the stream in the real
	// UI; the chunk is delivered directly so the test controls the order.
	next, cmd := m.Update(chatChunkMsg{Gen: m.chatGen, Chunk: chunk, OK: true})
	return next.(*Model), cmd
}

func TestPlainLineChatsInsteadOfCreatingTask(t *testing.T) {
	created := 0
	fake := &fakeChat{}
	cfg := SessionConfig{
		Chat: fake.fn,
		CreateTask: func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
			created++
			return &harnesspb.TaskView{TaskId: "task-1", Status: "running", CurrentStage: "a"}, nil
		},
	}
	m := sessionModel(t, cfg)

	m, wait := submitChat(t, m, "hi")
	if created != 0 {
		t.Fatalf("a plain line must not create a task, CreateTask called %d time(s)", created)
	}
	if len(fake.messages) != 1 || fake.messages[0] != "hi" {
		t.Fatalf("chat messages = %v", fake.messages)
	}
	if !m.chatMode {
		t.Fatal("the conversation view must stay active for direct chat")
	}
	if !m.chatStreaming || m.awaitingInput {
		t.Fatalf("streaming state wrong: streaming=%v awaiting=%v", m.chatStreaming, m.awaitingInput)
	}

	// Token chunks append to the same assistant turn.
	for _, tok := range []string{"你好", "，", "我在"} {
		m, wait = pushChunk(t, m, wait, ChatChunk{Type: "token", Content: tok})
	}
	if got := m.chat[len(m.chat)-1].Text; got != "你好，我在" {
		t.Fatalf("streamed text = %q", got)
	}

	// final carries the whole reply: it replaces instead of duplicating.
	m, wait = pushChunk(t, m, wait, ChatChunk{Type: "final", Content: "你好，我在。有什么可以帮你？"})
	if got := m.chat[len(m.chat)-1].Text; got != "你好，我在。有什么可以帮你？" {
		t.Fatalf("final text = %q (duplicate append?)", got)
	}
	if m.chatStreaming {
		t.Fatal("final chunk must end the streaming state")
	}
	if !m.awaitingInput {
		t.Fatal("the composer must come back after the reply")
	}
	if !strings.Contains(m.View(), "有什么可以帮你") {
		t.Fatalf("reply missing from the conversation view:\n%s", m.View())
	}
	if wait == nil {
		t.Fatal("the model must keep reading until the stream closes")
	}
}

func TestChatErrorChunkIsVisible(t *testing.T) {
	fake := &fakeChat{}
	m := sessionModel(t, SessionConfig{Chat: fake.fn})
	m, wait := submitChat(t, m, "hi")
	m, _ = pushChunk(t, m, wait, ChatChunk{Type: "error", Content: "exec denied: echo"})

	if !strings.Contains(m.View(), "exec denied: echo") {
		t.Fatalf("error chunk must be rendered in the conversation:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "error") {
		t.Fatalf("error label missing:\n%s", m.View())
	}
	if m.chatStreaming {
		t.Fatal("an error chunk must stop the stream")
	}
}

func TestChatOpenFailureIsVisibleAndNoTask(t *testing.T) {
	created := 0
	cfg := SessionConfig{
		Chat: func(ctx context.Context, message string) (<-chan ChatChunk, error) {
			return nil, errors.New("connection refused")
		},
		CreateTask: func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
			created++
			return &harnesspb.TaskView{TaskId: "task-1", Status: "running", CurrentStage: "a"}, nil
		},
	}
	m := sessionModel(t, cfg)
	m, _ = submitChat(t, m, "hi")
	if created != 0 {
		t.Fatal("a chat failure must never fall back to creating a task")
	}
	if !strings.Contains(m.View(), "connection refused") {
		t.Fatalf("open failure must be shown:\n%s", m.View())
	}
	if !m.awaitingInput {
		t.Fatal("the composer must return after a failed chat")
	}
}

func TestTaskPrefixKeepsAEView(t *testing.T) {
	fake := &fakeChat{}
	var goals []string
	cfg := SessionConfig{
		Chat: fake.fn,
		CreateTask: func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
			goals = append(goals, goal)
			return &harnesspb.TaskView{TaskId: "task-7", Status: "running", CurrentStage: "a"}, nil
		},
	}
	m := sessionModel(t, cfg)
	m = submitLine(t, typeLine(t, m, "/task 实现 登录 页面"))

	if len(goals) != 1 || goals[0] != "实现 登录 页面" {
		t.Fatalf("task goals = %v", goals)
	}
	if len(fake.messages) != 0 {
		t.Fatalf("chat must not be called for /task, got %v", fake.messages)
	}
	if m.chatMode {
		t.Fatal("/task must switch away from the conversation view")
	}
	if !strings.Contains(m.View(), "a 需求分析") {
		t.Fatalf("a~e stage view missing after /task:\n%s", m.View())
	}
}

func TestTaskCommandArg(t *testing.T) {
	cases := []struct {
		line string
		goal string
		ok   bool
	}{
		{"/task fix it", "fix it", true},
		{"/TASK fix it", "fix it", true},
		{"/task\tfix it", "fix it", true},
		{"/task", "", true},
		{"/tasks later", "", false},
		{"/taskfoo", "", false},
		{"please /task this", "", false},
		{"hi", "", false},
	}
	for _, tc := range cases {
		goal, ok := taskCommandArg(tc.line)
		if ok != tc.ok || goal != tc.goal {
			t.Errorf("taskCommandArg(%q) = (%q,%v), want (%q,%v)", tc.line, goal, ok, tc.goal, tc.ok)
		}
	}
}

func TestChatSessionWithoutChatReportsConfiguration(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	m = typeLine(t, m, "hi")
	next, submitCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*Model)
	next, cmd := m.Update(submitCmd())
	m = next.(*Model)
	if cmd != nil {
		t.Fatal("no Chat callable means no command")
	}
	if !strings.Contains(m.errText, "not configured") {
		t.Fatalf("errText = %q", m.errText)
	}
	if !m.awaitingInput {
		t.Fatal("the composer must stay available")
	}
}

func TestInitialGoalStillCreatesTask(t *testing.T) {
	created := 0
	cfg := SessionConfig{
		InitialGoal: "one shot goal",
		Chat: func(ctx context.Context, message string) (<-chan ChatChunk, error) {
			return nil, fmt.Errorf("chat must not be used for --goal")
		},
		CreateTask: func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
			created++
			return &harnesspb.TaskView{TaskId: "task-seed", Status: "running", CurrentStage: "a"}, nil
		},
	}
	m := NewModel(TaskState{}, nil, Options{NoColor: true, Session: &cfg})
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("--goal must start a task")
	}
	next, _ := m.Update(cmd())
	m = next.(*Model)
	if created != 1 {
		t.Fatalf("CreateTask calls = %d", created)
	}
	if m.chatMode || m.state.TaskID != "task-seed" {
		t.Fatalf("--goal must open the a~e view: chatMode=%v task=%q", m.chatMode, m.state.TaskID)
	}
}
