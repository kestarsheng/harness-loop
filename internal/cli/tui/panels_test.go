package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	harnessevent "agent-platform/pkg/harnessloop/event"
)

type fakeSource struct {
	state TaskState
}

func (f *fakeSource) Task(context.Context) (TaskState, error) { return f.state, nil }
func (f *fakeSource) Abort(context.Context) error             { return nil }
func (f *fakeSource) ListEvents(context.Context, int64) ([]Event, int64, error) {
	return nil, 0, nil
}
func (f *fakeSource) Watch(context.Context, int64) (<-chan WatchEvent, error) {
	ch := make(chan WatchEvent)
	close(ch)
	return ch, nil
}
func (f *fakeSource) Subtasks(context.Context) ([]Subtask, error) {
	return []Subtask{
		{ID: "sub-1", Role: "planner", Status: "accepted", Depth: 0, Index: 0, Attempt: 1, Tokens: 1200, AllocTokens: 4000, Artifacts: []string{"out/plan.md"}},
		{ID: "sub-2", Role: "builder", Status: "running", Depth: 1, Index: 0, Attempt: 2, DependsOn: []string{"sub-1"}, HandoffID: "ho-1"},
	}, nil
}
func (f *fakeSource) ModelSelections(context.Context) ([]ModelSelection, error) {
	return []ModelSelection{
		{Stage: "c", Attempt: 1, ModelID: "qwen3.7-plus", Strategy: "primary", TokensIn: 1000, TokensOut: 200, Cost: 0.01, ContextWindow: 128000},
		{Stage: "c", Attempt: 2, ModelID: "deepseek-v4.1-flash", Strategy: "fallback", Fallback: true, ResolvedFrom: "qwen3.7-plus", Reason: "unavailable"},
	}, nil
}
func (f *fakeSource) Handoffs(context.Context) ([]Handoff, error) {
	return []Handoff{{ID: "ho-1", ParentID: "task-1", SubtaskID: "sub-2", From: "planner", To: "builder", Status: "accepted", Reason: "plan ready"}}, nil
}
func (f *fakeSource) Checkpoints(context.Context) ([]Checkpoint, error) {
	return []Checkpoint{{ID: "cp-1", Kind: "stage_entry", Stage: "c", Attempt: 1, Watermark: 42, Payload: "cp/payload.json"}}, nil
}

type fakeKB struct{}

func (fakeKB) Search(_ context.Context, query string, _ int) ([]KBSearchResult, int64, time.Duration, error) {
	return []KBSearchResult{{
		Title: "Design doc", Snippet: "the harness loop design", Source: "/docs/design.md", DocumentID: "doc-1", ChunkID: "ch-1", Score: 0.842,
	}}, 1, 12 * time.Millisecond, nil
}

func newPanelModel(t *testing.T) *Model {
	t.Helper()
	src := &fakeSource{state: TaskState{TaskID: "task-1", Status: "running", CurrentStage: "c", Attempts: map[string]int{"a": 1, "b": 1, "c": 1}}}
	m := NewModel(src.state, src, Options{NoColor: true, KB: fakeKB{}})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 150, Height: 50})
	return next.(*Model)
}

func press(t *testing.T, m *Model, key string) tea.Cmd {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	next, cmd := m.Update(msg)
	*m = *(next.(*Model))
	return cmd
}

func runCmd(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	msg := cmd()
	next, _ := m.Update(msg)
	*m = *(next.(*Model))
}

func TestTUIPanelsRenderBackendData(t *testing.T) {
	m := newPanelModel(t)

	cmd := press(t, m, "s")
	runCmd(t, m, cmd)
	view := m.View()
	for _, want := range []string{"Subtasks", "planner", "builder", "accepted", "running", "Handoffs", "plan ready", "DAG"} {
		if !strings.Contains(view, want) {
			t.Fatalf("subtask view missing %q:\n%s", want, view)
		}
	}

	cmd = press(t, m, "esc")
	_ = cmd
	cmd = press(t, m, "x")
	runCmd(t, m, cmd)
	view = m.View()
	for _, want := range []string{"Models", "qwen3.7-plus", "deepseek-v4.1-flash", "FALLBACK", "1 fallbacks"} {
		if !strings.Contains(view, want) {
			t.Fatalf("model view missing %q:\n%s", want, view)
		}
	}

	m = newPanelModel(t)
	m.Update(eventMsg{Event: Event{Seq: 1, Type: "security.violation", Severity: "error", Stage: "c", Time: time.Now(),
		Payload: []byte(`{"decision":"deny","target":"../etc/passwd"}`)}})
	m.Update(eventMsg{Event: Event{Seq: 2, Type: "tool.denied", Severity: "warn", Stage: "c", Time: time.Now(),
		Payload: []byte(`{"tool":"run_command","reason":"stage matrix"}`)}})
	m.Update(eventMsg{Event: Event{Seq: 3, Type: "approval.requested", Severity: "info", Stage: "c", Time: time.Now(),
		Payload: []byte(`{"approval_id":"ap-1","operator":"human"}`)}})
	press(t, m, "h")
	view = m.View()
	for _, want := range []string{"Health", "Incident drawer", "security", "tool", "approval", "pending=1", "../etc/passwd", "stage matrix"} {
		if !strings.Contains(view, want) {
			t.Fatalf("health view missing %q:\n%s", want, view)
		}
	}

	cmd = press(t, m, "esc")
	_ = cmd
	cmd = press(t, m, "c")
	runCmd(t, m, cmd)
	view = m.View()
	for _, want := range []string{"Artifacts & checkpoints", "out/plan.md", "cp-1", "stage_entry"} {
		if !strings.Contains(view, want) {
			t.Fatalf("artifact view missing %q:\n%s", want, view)
		}
	}
}

func TestTUIKnowledgeSidebar(t *testing.T) {
	m := newPanelModel(t)
	press(t, m, "k")
	if m.mode != ViewKB {
		t.Fatalf("k must open the knowledge sidebar, mode=%v", m.mode)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	if m.kb.Query != "hello" {
		t.Fatalf("kb query = %q", m.kb.Query)
	}
	cmd := press(t, m, "\x00")
	_ = cmd
	searchCmd := m.kbSearchCmd()
	runCmd(t, m, searchCmd)
	view := m.View()
	for _, want := range []string{"Knowledge", "hello", "Design doc", "0.8420", "/docs/design.md", "harness loop design"} {
		if !strings.Contains(view, want) {
			t.Fatalf("kb view missing %q:\n%s", want, view)
		}
	}
}

func TestTUIEmptyPanelsDoNotCrash(t *testing.T) {
	m := NewModel(TaskState{TaskID: "empty", Status: "running", CurrentStage: "a"}, nil, Options{NoColor: true})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(*Model)
	for _, key := range []string{"s", "x", "h", "c", "k"} {
		press(t, m, key)
		view := m.View()
		if strings.TrimSpace(view) == "" {
			t.Fatalf("view for %s is empty", key)
		}
		if strings.Contains(view, "panic") {
			t.Fatalf("view for %s panicked", key)
		}
		press(t, m, "esc")
		if m.mode != ViewMain {
			t.Fatalf("esc must return to main from %s", key)
		}
	}
}

func TestTUIAllHarnessEventTypesAreReadable(t *testing.T) {
	types := harnessevent.KnownTypes()
	if len(types) < 70 {
		t.Fatalf("expected at least 70 harness event types, got %d", len(types))
	}
	m := NewModel(TaskState{TaskID: "events", Status: "running", CurrentStage: "a"}, nil, Options{NoColor: true})
	now := time.Now()
	seq := int64(0)
	for _, typ := range types {
		seq++
		next, _ := m.Update(eventMsg{Event: Event{Seq: seq, Type: string(typ), Severity: "info", Stage: "a", Time: now}})
		m = next.(*Model)
		if len(m.events) == 0 {
			t.Fatalf("event %s produced no event line", typ)
		}
		last := m.events[len(m.events)-1]
		if strings.TrimSpace(last.Summary) == "" {
			t.Fatalf("event %s has no readable summary", typ)
		}
		if strings.Contains(last.Summary, ".") {
			t.Fatalf("event %s summary leaked a dotted identifier: %q", typ, last.Summary)
		}
	}
}
