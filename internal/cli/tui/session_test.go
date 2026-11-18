package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	harnesspb "agent-platform/pkg/pb/harnessloop"
)

func sessionModel(t *testing.T, cfg SessionConfig) *Model {
	t.Helper()
	m := NewModel(TaskState{}, nil, Options{NoColor: true, Session: &cfg})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(*Model)
	_ = m.Init()
	return m
}

func typeLine(t *testing.T, m *Model, text string) *Model {
	t.Helper()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
	return next.(*Model)
}

func submitLine(t *testing.T, m *Model) *Model {
	t.Helper()
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*Model)
	if cmd == nil {
		t.Fatalf("submit produced no command")
	}
	next, createCmd := m.Update(cmd())
	m = next.(*Model)
	if createCmd == nil {
		t.Fatalf("prompt submission did not start task creation")
	}
	next, _ = m.Update(createCmd())
	return next.(*Model)
}

func completeTask(t *testing.T, m *Model, taskID string) (tea.Model, tea.Cmd) {
	t.Helper()
	return m.Update(taskMsg{Gen: m.taskGen, State: TaskState{
		TaskID: taskID, Status: "completed", TerminalReason: "completed", CurrentStage: "e",
	}})
}

func TestSessionTerminalReturnsToInputAndAcceptsSecondTask(t *testing.T) {
	var goals []string
	factory := func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
		goals = append(goals, goal)
		return &harnesspb.TaskView{
			TaskId:       fmt.Sprintf("task-%d", len(goals)),
			Status:       "running",
			CurrentStage: "a",
		}, nil
	}
	m := sessionModel(t, SessionConfig{CreateTask: factory})
	if !m.awaitingInput {
		t.Fatal("a fresh session must wait at the input box")
	}

	m = submitLine(t, typeLine(t, m, "first goal"))
	if len(goals) != 1 || goals[0] != "first goal" {
		t.Fatalf("factory goals = %v", goals)
	}
	if m.awaitingInput || m.state.TaskID != "task-1" {
		t.Fatalf("first task not active: awaiting=%v task=%q", m.awaitingInput, m.state.TaskID)
	}

	next, quit := completeTask(t, m, "task-1")
	m = next.(*Model)
	if quit != nil {
		t.Fatalf("terminal task must not quit a resident session, got %T", quit())
	}
	if !m.awaitingInput {
		t.Fatal("terminal task must return to the input box")
	}
	if !strings.Contains(m.View(), "Enter submit") {
		t.Fatalf("input hint missing after terminal state:\n%s", m.View())
	}
	if !m.state.terminal() || m.state.TaskID != "task-1" {
		t.Fatalf("terminal result must be retained: status=%q task=%q", m.state.Status, m.state.TaskID)
	}

	// Second submission in the same session creates a second task.
	m = submitLine(t, typeLine(t, m, "second goal"))
	if len(goals) != 2 || goals[1] != "second goal" {
		t.Fatalf("factory goals = %v", goals)
	}
	if m.state.TaskID != "task-2" || m.taskGen != 2 {
		t.Fatalf("second task not active: task=%q gen=%d", m.state.TaskID, m.taskGen)
	}
	if len(m.taskIDs) != 2 || m.taskIDs[0] != "task-1" || m.taskIDs[1] != "task-2" {
		t.Fatalf("task ids = %v", m.taskIDs)
	}

	next, _ = completeTask(t, m, "task-2")
	m = next.(*Model)
	if !m.awaitingInput {
		t.Fatal("second terminal task must return to the input box")
	}

	// exit only ends this session.
	next, quit = m.Update(promptSubmitMsg{Value: "exit"})
	m = next.(*Model)
	if quit == nil {
		t.Fatal("exit must quit the session")
	}
	if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatalf("exit command returned %T", quit())
	}
	if !m.exitRequested {
		t.Fatal("exit must be recorded on the session")
	}
}

func TestSessionExitOnCompleteKeepsLegacyBehaviour(t *testing.T) {
	factory := func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
		return &harnesspb.TaskView{TaskId: "task-1", Status: "running", CurrentStage: "a"}, nil
	}
	m := sessionModel(t, SessionConfig{ExitOnComplete: true, CreateTask: factory})
	m = submitLine(t, typeLine(t, m, "legacy goal"))

	_, quit := completeTask(t, m, "task-1")
	if quit == nil {
		t.Fatal("--exit-on-complete must quit on terminal state")
	}
	if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatalf("terminal command returned %T", quit())
	}
}

func TestSessionInitialGoalSkipsInput(t *testing.T) {
	factory := func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
		return &harnesspb.TaskView{TaskId: "task-seeded", Status: "running", CurrentStage: "a"}, nil
	}
	m := NewModel(TaskState{}, nil, Options{
		NoColor: true,
		Session: &SessionConfig{InitialGoal: "seeded goal", CreateTask: factory},
	})
	createCmd := m.Init()
	if createCmd == nil {
		t.Fatal("initial goal must create a task on start")
	}
	next, _ := m.Update(createCmd())
	m = next.(*Model)
	if m.awaitingInput || m.state.TaskID != "task-seeded" {
		t.Fatalf("initial goal not submitted: awaiting=%v task=%q", m.awaitingInput, m.state.TaskID)
	}
}
