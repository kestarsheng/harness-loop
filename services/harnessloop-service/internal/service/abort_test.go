package service

import (
	"context"
	"testing"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/services/harnessloop-service/internal/repository"
)

func TestAbortIdempotentTriple(t *testing.T) {
	rt, tasks, events, _ := newTestRuntime(&stubStages{})

	first, err := rt.Abort(context.Background(), "tenant-a", "task-1", 42)
	if err != nil {
		t.Fatalf("first abort: %v", err)
	}
	if first.Duplicate {
		t.Fatal("first abort must not be duplicate")
	}
	for i := 0; i < 2; i++ {
		again, err := rt.Abort(context.Background(), "tenant-a", "task-1", 42)
		if err != nil {
			t.Fatalf("repeat abort: %v", err)
		}
		if !again.Duplicate {
			t.Fatal("repeat abort must be duplicate")
		}
		if again.Timestamp != first.Timestamp || again.TerminalReason != first.TerminalReason {
			t.Fatalf("idempotent result drifted: %+v vs %+v", again, first)
		}
	}
	if got := events.count(event.TaskAborted); got != 1 {
		t.Fatalf("task.aborted events = %d, want 1", got)
	}
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if len(tasks.statuses) != 1 {
		t.Fatalf("terminal status written %d times, want 1", len(tasks.statuses))
	}
	if tasks.task.Status != repository.TaskAborted {
		t.Fatalf("status = %s", tasks.task.Status)
	}
}

func TestAbortDifferentEpochsStillSingleTerminalEvent(t *testing.T) {
	rt, _, events, _ := newTestRuntime(&stubStages{})
	if _, err := rt.Abort(context.Background(), "tenant-a", "task-1", 1); err != nil {
		t.Fatalf("abort 1: %v", err)
	}
	if _, err := rt.Abort(context.Background(), "tenant-a", "task-1", 2); err != nil {
		t.Fatalf("abort 2: %v", err)
	}
	if got := events.count(event.TaskAborted); got != 1 {
		t.Fatalf("terminal event emitted %d times, want 1", got)
	}
}
