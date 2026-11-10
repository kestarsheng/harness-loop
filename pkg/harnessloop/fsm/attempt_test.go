package fsm

import (
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestParseEntryReason(t *testing.T) {
	for _, r := range []string{"initial", "acceptance_failed", "retry", "resume"} {
		got, err := ParseEntryReason(r)
		if err != nil {
			t.Fatalf("ParseEntryReason(%q): %v", r, err)
		}
		if string(got) != r {
			t.Fatalf("ParseEntryReason(%q) = %q", r, got)
		}
	}
	for _, r := range []string{"", "other", "INITIAL"} {
		if _, err := ParseEntryReason(r); err == nil {
			t.Fatalf("ParseEntryReason(%q) should fail", r)
		}
	}
}

func TestNewAttemptValidation(t *testing.T) {
	now := time.Unix(1000, 0)
	if _, err := NewAttempt("t", Stage("z"), 1, EntryInitial, now); err == nil {
		t.Fatal("invalid stage must fail")
	}
	if _, err := NewAttempt("t", StageDone, 1, EntryInitial, now); err == nil {
		t.Fatal("stage e must not record an attempt")
	}
	if _, err := NewAttempt("t", StagePlan, 0, EntryInitial, now); err == nil {
		t.Fatal("attempt_no < 1 must fail")
	}
	if _, err := NewAttempt("t", StagePlan, 1, EntryReason("bad"), now); err == nil {
		t.Fatal("invalid entry reason must fail")
	}
}

func TestAttemptLifecycle(t *testing.T) {
	now := time.Unix(1000, 0)
	a, err := NewAttempt("task-1", StageExecute, 2, EntryAcceptanceFailed, now)
	if err != nil {
		t.Fatalf("NewAttempt: %v", err)
	}
	if a.AttemptID != "task-1-c-2" {
		t.Fatalf("AttemptID = %q", a.AttemptID)
	}
	if a.FinishReceived || a.AcceptanceID != "" {
		t.Fatal("new attempt must not be finished")
	}
	if a.Completed() {
		t.Fatal("new attempt must not be completed")
	}
	if err := a.Finish(now.Add(time.Second)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := a.Finish(now.Add(2 * time.Second)); err != nil {
		t.Fatalf("Finish must be idempotent: %v", err)
	}
	if !a.Completed() {
		t.Fatal("attempt should be completed")
	}
	if err := a.AttachAcceptance("acc-1"); err != nil {
		t.Fatalf("AttachAcceptance: %v", err)
	}
	if a.AcceptanceID != "acc-1" {
		t.Fatalf("AcceptanceID = %q", a.AcceptanceID)
	}
}

func TestAcceptanceRequiresFinish(t *testing.T) {
	a, err := NewAttempt("task-1", StageVerify, 1, EntryInitial, time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("NewAttempt: %v", err)
	}
	err = a.AttachAcceptance("acc-1")
	if err == nil {
		t.Fatal("acceptance before finish must fail")
	}
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != ErrAcceptanceBeforeFinish {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.AcceptanceID != "" {
		t.Fatal("acceptance_id must stay empty")
	}
	if err := a.Finish(time.Unix(1001, 0)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := a.AttachAcceptance(""); err == nil {
		t.Fatal("empty acceptance_id must fail")
	}
}

func TestAttemptTrackerSequence(t *testing.T) {
	tr := NewAttemptTracker(nil)
	now := time.Unix(1000, 0)

	first, err := tr.Enter("task-1", StageRequirement, EntryInitial, now)
	if err != nil {
		t.Fatalf("enter a: %v", err)
	}
	second, err := tr.Enter("task-1", StageRequirement, EntryRetry, now)
	if err != nil {
		t.Fatalf("enter a again: %v", err)
	}
	if first.AttemptNo != 1 || second.AttemptNo != 2 {
		t.Fatalf("attempt numbers = %d, %d", first.AttemptNo, second.AttemptNo)
	}
	if second.EntryReason != EntryRetry {
		t.Fatalf("entry reason = %q", second.EntryReason)
	}

	plan, err := tr.Enter("task-1", StagePlan, EntryInitial, now)
	if err != nil {
		t.Fatalf("enter b: %v", err)
	}
	if plan.AttemptNo != 1 {
		t.Fatalf("b attempt_no = %d, want 1", plan.AttemptNo)
	}
	if tr.NextNo(StagePlan) != 2 {
		t.Fatalf("next b = %d", tr.NextNo(StagePlan))
	}
}

func TestAttemptTrackerBacktrackIncrements(t *testing.T) {
	tr := NewAttemptTracker(nil)
	now := time.Unix(1000, 0)
	for i := 1; i <= 2; i++ {
		a, err := tr.Enter("task-1", StageExecute, EntryInitial, now)
		if err != nil {
			t.Fatalf("enter c #%d: %v", i, err)
		}
		if a.AttemptNo != i {
			t.Fatalf("c attempt_no = %d, want %d", a.AttemptNo, i)
		}
	}
}

func TestAttemptTrackerTerminalStage(t *testing.T) {
	tr := NewAttemptTracker(nil)
	if _, err := tr.Enter("task-1", StageDone, EntryInitial, time.Unix(1000, 0)); err == nil {
		t.Fatal("stage e must not create attempts")
	}
	if got := tr.NextNo(StageDone); got != 0 {
		t.Fatalf("e should never advance, got %d", got)
	}
}

func TestAttemptTrackerRestoreAndSnapshot(t *testing.T) {
	tr := NewAttemptTracker(map[string]int{"c": 2, "b": 1, "a": -5})
	if got := tr.NextNo(StageExecute); got != 3 {
		t.Fatalf("restored next c = %d, want 3", got)
	}
	if got := tr.NextNo(StagePlan); got != 2 {
		t.Fatalf("restored next b = %d, want 2", got)
	}
	if got := tr.NextNo(StageRequirement); got != 1 {
		t.Fatalf("negative count must clamp to 1, got %d", got)
	}
	if _, err := tr.Enter("task-1", StageExecute, EntryResume, time.Unix(1000, 0)); err != nil {
		t.Fatalf("enter: %v", err)
	}
	snap := tr.Snapshot()
	if snap["c"] != 3 {
		t.Fatalf("snapshot c = %d, want 3", snap["c"])
	}
	if _, ok := snap["e"]; ok {
		t.Fatal("snapshot must not contain stage e")
	}
}

func TestAttemptTrackerConcurrent(t *testing.T) {
	tr := NewAttemptTracker(nil)
	now := time.Unix(1000, 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make([]int, 0, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := tr.Enter("task-1", StageExecute, EntryRetry, now)
			if err != nil {
				t.Errorf("enter: %v", err)
				return
			}
			mu.Lock()
			seen = append(seen, a.AttemptNo)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Ints(seen)
	if len(seen) != 20 {
		t.Fatalf("got %d attempts", len(seen))
	}
	for i, n := range seen {
		if n != i+1 {
			t.Fatalf("attempt numbers not contiguous: %v", seen)
		}
	}
}

func TestAttemptIDFor(t *testing.T) {
	tr := NewAttemptTracker(nil)
	a, err := tr.Enter("task-9", StageVerify, EntryInitial, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	if a.AttemptID != AttemptIDFor("task-9", StageVerify, 1) {
		t.Fatalf("AttemptID = %q", a.AttemptID)
	}
	if a.AttemptID != "task-9-d-1" {
		t.Fatalf("AttemptID = %q", a.AttemptID)
	}
}
