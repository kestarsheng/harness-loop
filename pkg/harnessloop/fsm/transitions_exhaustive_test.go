package fsm

import (
	"errors"
	"sync"
	"testing"
)

var legalEdgePairs = [8][2]Stage{
	{StageRequirement, StagePlan},
	{StagePlan, StageRequirement},
	{StagePlan, StageExecute},
	{StageExecute, StagePlan},
	{StageExecute, StageVerify},
	{StageVerify, StageExecute},
	{StageVerify, StagePlan},
	{StageVerify, StageDone},
}

func TestExhaustiveLegalEdges(t *testing.T) {
	for _, pair := range legalEdgePairs {
		from, to := pair[0], pair[1]
		t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
			m, err := NewMachineAt("task-1", from)
			if err != nil {
				t.Fatalf("NewMachineAt: %v", err)
			}
			before := m.Stage()
			decision, err := m.Advance(to)
			if err != nil {
				t.Fatalf("advance %s->%s: %v", from, to, err)
			}
			if decision.Transition.ID == "" {
				t.Fatal("every legal edge must carry a transition id")
			}
			if !decision.Transition.Trigger.Valid() {
				t.Fatalf("invalid trigger on %s", decision.Transition.ID)
			}
			if before != decision.From || m.Stage() != to {
				t.Fatalf("state mismatch: before=%s from=%s now=%s", before, decision.From, m.Stage())
			}
			if len(decision.Events) == 0 {
				t.Fatal("legal edge must emit pending events")
			}
			if m.Stage().IsTerminal() && to != StageDone {
				t.Fatalf("%s must not be terminal", to)
			}
		})
	}
}

func TestExhaustiveIllegalEdges(t *testing.T) {
	legal := make(map[[2]Stage]bool, len(legalEdgePairs))
	for _, p := range legalEdgePairs {
		legal[p] = true
	}
	checked := 0
	for _, from := range allStages {
		for _, to := range allStages {
			if legal[[2]Stage{from, to}] {
				continue
			}
			checked++
			m, err := NewMachineAt("task-1", from)
			if err != nil {
				t.Fatalf("NewMachineAt: %v", err)
			}
			_, advErr := m.Advance(to)
			var fe *Error
			if !errors.As(advErr, &fe) || fe.Code != ErrIllegalTransition {
				t.Fatalf("%s->%s must be E_ILLEGAL_TRANSITION, got %v", from, to, advErr)
			}
			if m.Stage() != from {
				t.Fatalf("%s->%s mutated state to %s", from, to, m.Stage())
			}
		}
	}
	if checked != 17 {
		t.Fatalf("expected 17 illegal pairs, checked %d", checked)
	}
}

func TestExhaustiveSkipStages(t *testing.T) {
	skips := [][2]Stage{
		{StageRequirement, StageExecute},
		{StageRequirement, StageVerify},
		{StageRequirement, StageDone},
		{StagePlan, StageVerify},
		{StagePlan, StageDone},
		{StageExecute, StageDone},
	}
	for _, c := range skips {
		m, err := NewMachineAt("task-1", c[0])
		if err != nil {
			t.Fatalf("NewMachineAt: %v", err)
		}
		if _, err := m.Advance(c[1]); err == nil {
			t.Fatalf("skipping %s->%s must be rejected", c[0], c[1])
		}
	}
}

func TestExhaustiveTerminalHasNoOutbound(t *testing.T) {
	for _, to := range allStages {
		m, err := NewMachineAt("task-1", StageDone)
		if err != nil {
			t.Fatalf("NewMachineAt: %v", err)
		}
		if _, err := m.Advance(to); err == nil {
			t.Fatalf("e->%s must be rejected", to)
		}
		if m.Stage() != StageDone {
			t.Fatalf("terminal state mutated to %s", m.Stage())
		}
	}
	if len(LegalTargets(StageDone)) != 0 {
		t.Fatalf("e must have no legal targets: %v", LegalTargets(StageDone))
	}
}

func TestExhaustiveTenGoroutinesSingleWinner(t *testing.T) {
	m := NewMachine("task-1")
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		success   int
		conflicts int
	)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Advance(StagePlan); err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			} else {
				mu.Lock()
				conflicts++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 || conflicts != 9 {
		t.Fatalf("success=%d conflicts=%d, want 1/9", success, conflicts)
	}
	if m.Stage() != StagePlan {
		t.Fatalf("stage = %s", m.Stage())
	}
}

func TestExhaustiveStageInvariants(t *testing.T) {
	for _, from := range allStages {
		m, err := NewMachineAt("task-1", from)
		if err != nil {
			t.Fatalf("NewMachineAt: %v", err)
		}
		for _, to := range allStages {
			mCopy, _ := NewMachineAt("task-1", from)
			if _, advErr := mCopy.Advance(to); advErr == nil {
				got := mCopy.Stage()
				if !got.Valid() {
					t.Fatalf("post-transition stage %q is not a legal enum value", got)
				}
				if got != to {
					t.Fatalf("post-transition stage %s != target %s", got, to)
				}
			}
		}
		_ = m
	}
}

func TestExhaustiveTransitionIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, tr := range AllTransitions() {
		if tr.ID == "" {
			t.Fatal("transition id must not be empty")
		}
		if seen[tr.ID] {
			t.Fatalf("duplicate transition id %s", tr.ID)
		}
		seen[tr.ID] = true
		if !tr.From.Valid() || !tr.To.Valid() {
			t.Fatalf("transition %s uses invalid stage", tr.ID)
		}
		if tr.From.IsTerminal() {
			t.Fatalf("transition %s leaves terminal stage", tr.ID)
		}
	}
	if len(seen) != 8 {
		t.Fatalf("expected 8 transitions, got %d", len(seen))
	}
}
