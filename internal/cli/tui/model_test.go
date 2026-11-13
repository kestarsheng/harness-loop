package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTUIModel(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 3, 0, 0, time.UTC)
	newModel := func() *Model {
		m := NewModel(TaskState{
			TaskID:         "7f3a1b2c9d",
			Status:         "running",
			CurrentStage:   "b",
			Attempts:       map[string]int{"a": 1, "b": 1},
			RetryCount:     1,
			BudgetTokens:   120000,
			BudgetCost:     1.5,
			ConsumedTokens: 38200,
			ConsumedCost:   0.42,
			StageStartedAt: now.Add(-4*time.Minute - 12*time.Second),
		}, nil, Options{NoColor: true, Now: func() time.Time { return now }})
		next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		return next.(*Model)
	}

	m := newModel()
	view := m.View()
	for _, want := range []string{"Harness Loop", "task 7f3a1b2c", "b 方案设计", "attempt #1", "elapsed 04:12", "tokens 38.2k/120.0k", "q quit"} {
		if !strings.Contains(view, want) {
			t.Fatalf("initial view missing %q:\n%s", want, view)
		}
	}

	stageEvent := Event{
		Seq: 10, Type: "stage.transition", Stage: "c", Severity: "info", Time: now,
		Payload: []byte(`{"from":"b","to":"c","attempt_id":"att-c-2"}`),
	}
	next, _ := m.Update(eventMsg{Event: stageEvent})
	m = next.(*Model)
	if m.state.CurrentStage != "c" {
		t.Fatalf("current stage = %q, want c", m.state.CurrentStage)
	}
	if !strings.Contains(m.View(), "c 执行编码") {
		t.Fatalf("stage change not reflected in view:\n%s", m.View())
	}
	if m.lastSeq != 10 {
		t.Fatalf("last seq = %d, want 10", m.lastSeq)
	}

	acceptancePayload := []byte(`{
		"verdict":"failed",
		"verified_by":"program",
		"rollback_target":"c",
		"evidence":[
			{"kind":"command","ref":"go test ./...","excerpt":"FAIL 2 tests"},
			{"kind":"test","ref":"TestFoo","excerpt":"expected 1 got 2"},
			{"kind":"file","ref":"main.go","excerpt":"missing symbol"},
			{"kind":"command","ref":"go vet","excerpt":"ignored fourth"}
		],
		"failed_criteria":[
			{"criterion_id":"criterion-1","category":"test"},
			{"criterion_id":"criterion-2","category":"build"}
		]
	}`)
	next, _ = m.Update(eventMsg{Event: Event{
		Seq: 11, Type: "acceptance.failed", Stage: "c", Severity: "warn", Attempt: 2, Time: now, Payload: acceptancePayload,
	}})
	m = next.(*Model)
	if m.verdicts["c"] != "failed" {
		t.Fatalf("verdict c = %q", m.verdicts["c"])
	}
	if len(m.evidence) != 3 {
		t.Fatalf("evidence count = %d, want 3", len(m.evidence))
	}
	if len(m.failed) != 2 {
		t.Fatalf("failed criteria count = %d, want 2", len(m.failed))
	}
	next, _ = m.Update(flushMsg{})
	m = next.(*Model)
	view = m.View()
	for _, want := range []string{"c:failed (program)", "evidence", "criterion-1", "acceptance failed → c"} {
		if !strings.Contains(view, want) {
			t.Fatalf("acceptance view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "ignored fourth") {
		t.Fatalf("evidence must be capped at 3 entries:\n%s", view)
	}

	next, _ = m.Update(eventMsg{Event: Event{
		Seq: 12, Type: "error", Stage: "c", Severity: "warn", Attempt: 2, Time: now,
		Payload: []byte(`{"category":"network","reason":"connection reset"}`),
	}})
	m = next.(*Model)
	if m.retryByCategory["network"] != 1 {
		t.Fatalf("network retry count = %d", m.retryByCategory["network"])
	}
	if !strings.Contains(m.View(), "network=1") {
		t.Fatalf("retry category not reflected:\n%s", m.View())
	}

	next, reconnect := m.Update(watchErrMsg{Err: errors.New("stream closed")})
	m = next.(*Model)
	if !m.reconnecting || m.connected {
		t.Fatalf("model must enter reconnecting state")
	}
	if m.reconnectAttempt != 1 {
		t.Fatalf("reconnect attempt = %d", m.reconnectAttempt)
	}
	if reconnect == nil {
		t.Fatal("stream failure must schedule a reconnect tick")
	}
	if !strings.Contains(m.View(), "reconnecting") {
		t.Fatalf("reconnecting banner missing:\n%s", m.View())
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = next.(*Model)
	if m.mode != ViewLogs || !strings.Contains(m.View(), "Logs") {
		t.Fatal("l must open the logs view")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(*Model)
	if m.mode != ViewMain {
		t.Fatal("esc must return to the main view")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	m = next.(*Model)
	if m.mode != ViewMemory || !strings.Contains(m.View(), "Memory") {
		t.Fatal("m must open the memory view")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = next.(*Model)
	if m.mode != ViewHelp || !strings.Contains(m.View(), "Keys") {
		t.Fatal("? must open help")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(*Model)
	if m.mode != ViewMain {
		t.Fatal("esc must leave help")
	}

	_, quit := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if quit == nil {
		t.Fatal("q must return a quit command")
	}
	if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatalf("q command returned %T", quit())
	}

	_, terminalCmd := m.Update(taskMsg{State: TaskState{
		TaskID: "7f3a1b2c9d", Status: "completed", TerminalReason: "completed", CurrentStage: "done",
	}})
	if terminalCmd == nil {
		t.Fatal("terminal task must quit the program")
	}
	if _, ok := terminalCmd().(tea.QuitMsg); !ok {
		t.Fatalf("terminal command returned %T", terminalCmd())
	}
}
