package fsm

import (
	"errors"
	"testing"
)

func newGuard(t *testing.T, stage Stage, attemptID string) *FinishGuard {
	t.Helper()
	m, err := NewMachineAt("task-1", stage)
	if err != nil {
		t.Fatalf("NewMachineAt: %v", err)
	}
	return NewFinishGuard(m, attemptID)
}

func TestIsStageFinishTool(t *testing.T) {
	if !IsStageFinishTool("stage_finish") {
		t.Fatal("stage_finish must be recognized")
	}
	for _, name := range []string{"", "finish", "stage_finished", "done"} {
		if IsStageFinishTool(name) {
			t.Fatalf("%q must not be recognized", name)
		}
	}
}

func TestParseStageFinish(t *testing.T) {
	args, err := ParseStageFinish([]byte(`{"attempt_id":"t-c-1","stage":"c","summary":"done","artifacts":[{"path":"a.go"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if args.AttemptID != "t-c-1" || args.Stage != "c" || len(args.Artifacts) != 1 {
		t.Fatalf("unexpected args: %+v", args)
	}

	for _, raw := range []string{
		``,
		`not-json`,
		`{"stage":"c"}`,
		`{"attempt_id":"t-c-1"}`,
		`{}`,
	} {
		if _, err := ParseStageFinish([]byte(raw)); err == nil {
			t.Fatalf("payload %q must be rejected", raw)
		}
	}
}

func TestFinishGuardAcceptsMatchingFinish(t *testing.T) {
	g := newGuard(t, StageExecute, "task-1-c-1")
	res, err := g.Validate(StageFinishArgs{AttemptID: "task-1-c-1", Stage: "c", Summary: "ok"})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !res.Accepted || res.Duplicate {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !g.Received() {
		t.Fatal("guard must mark finish received")
	}
}

func TestFinishGuardDuplicateIsIdempotent(t *testing.T) {
	g := newGuard(t, StageExecute, "task-1-c-1")
	first, err := g.Validate(StageFinishArgs{AttemptID: "task-1-c-1", Stage: "c"})
	if err != nil || first.Duplicate {
		t.Fatalf("first finish: %+v err=%v", first, err)
	}
	second, err := g.Validate(StageFinishArgs{AttemptID: "task-1-c-1", Stage: "c"})
	if err != nil {
		t.Fatalf("second finish: %v", err)
	}
	if !second.Accepted || !second.Duplicate {
		t.Fatalf("second finish must be a duplicate: %+v", second)
	}
}

func TestFinishGuardRejectsStaleAttempt(t *testing.T) {
	g := newGuard(t, StageExecute, "task-1-c-1")
	_, err := g.Validate(StageFinishArgs{AttemptID: "task-1-c-0", Stage: "c"})
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != ErrStaleFinish {
		t.Fatalf("stale attempt: %v", err)
	}
	if g.Received() {
		t.Fatal("rejected finish must not mark received")
	}
}

func TestFinishGuardRejectsStageMismatch(t *testing.T) {
	g := newGuard(t, StageExecute, "task-1-c-1")
	_, err := g.Validate(StageFinishArgs{AttemptID: "task-1-c-1", Stage: "d"})
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != ErrStageMismatch {
		t.Fatalf("stage mismatch: %v", err)
	}
	if g.Received() {
		t.Fatal("rejected finish must not mark received")
	}
}

func TestFinishGuardRejectsInvalidStage(t *testing.T) {
	g := newGuard(t, StageExecute, "task-1-c-1")
	_, err := g.Validate(StageFinishArgs{AttemptID: "task-1-c-1", Stage: "z"})
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != ErrInvalidStage {
		t.Fatalf("invalid stage: %v", err)
	}
}

func TestFinishGuardRejectsOnTerminalStage(t *testing.T) {
	g := newGuard(t, StageDone, "task-1-e-1")
	_, err := g.Validate(StageFinishArgs{AttemptID: "task-1-e-1", Stage: "e"})
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != ErrFinishOnTerminal {
		t.Fatalf("terminal finish: %v", err)
	}
}

func TestFinishGuardAttemptID(t *testing.T) {
	g := newGuard(t, StagePlan, "task-1-b-3")
	if g.AttemptID() != "task-1-b-3" {
		t.Fatalf("attempt id = %q", g.AttemptID())
	}
	if g.Received() {
		t.Fatal("fresh guard must not be received")
	}
}

func TestInvalidFinishEvent(t *testing.T) {
	e := InvalidFinishEvent("task-1-c-1", StageExecute, "stale_attempt")
	if e.Type != "invalid_finish" || e.Severity != "warn" {
		t.Fatalf("unexpected event: %+v", e)
	}
	if e.Detail == "" {
		t.Fatal("detail must carry attempt and reason")
	}
}
