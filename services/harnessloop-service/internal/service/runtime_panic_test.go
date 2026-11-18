package service

import (
	"context"
	"errors"
	"testing"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/services/harnessloop-service/internal/repository"
)

// TestRuntimeRecoversStagePanic is the regression test for T-P2-35: a panic in
// the stage execution path (which is where the orchestrator plan/scheduler/
// aggregation work runs) must not take the process down. Before the fix this
// same panic unwound out of the task goroutine and killed harnessloop-service
// with exit code 2.
func TestRuntimeRecoversStagePanic(t *testing.T) {
	stages := &stubStages{fn: func(call int, ctx context.Context) (*StageRunResult, error) {
		panic("stage executor exploded")
	}}
	rt, tasks, events, _ := newTestRuntime(stages)

	res, err := rt.Run(context.Background(), runtimeInput())
	if err == nil {
		t.Fatal("a panicking stage must surface as an error, got nil")
	}
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *PanicError", err, err)
	}
	if pe.Scope != "runtime.Run" {
		t.Fatalf("scope = %q, want runtime.Run", pe.Scope)
	}
	if res != nil {
		t.Fatalf("result = %+v, want nil", res)
	}

	// The task must not stay `running`: an invisible half-dead task is what made
	// the crash look like a hang on the board.
	tasks.mu.Lock()
	status := tasks.task.Status
	tasks.mu.Unlock()
	if status != repository.TaskFailed {
		t.Fatalf("task status = %s, want failed", status)
	}

	if got := events.count(event.TaskFailed); got != 1 {
		t.Fatalf("task.failed events = %d, want 1", got)
	}
}

// TestPanicErrorKeepsDetail pins the two representations apart: Error() is what
// goes into an event payload, Detail() is what goes into the log.
func TestPanicErrorKeepsDetail(t *testing.T) {
	pe := NewPanicError("runtime.Run", errors.New("boom"))
	if got, want := pe.Error(), "runtime.Run: panic: boom"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(pe, pe.Unwrap()) {
		t.Fatal("Unwrap must expose the recovered error")
	}
	if len(pe.Detail()) <= len(pe.Error()) {
		t.Fatal("Detail must carry the stack on top of the message")
	}
}
