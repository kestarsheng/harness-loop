package fsm

import (
	"errors"
	"sync"
	"testing"
)

func TestParseStage(t *testing.T) {
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		got, err := ParseStage(s)
		if err != nil {
			t.Fatalf("ParseStage(%q): %v", s, err)
		}
		if string(got) != s {
			t.Fatalf("ParseStage(%q) = %q", s, got)
		}
	}
	for _, s := range []string{"", "f", "A", "abc", "1"} {
		if _, err := ParseStage(s); err == nil {
			t.Fatalf("ParseStage(%q) should fail", s)
		}
	}
}

func TestStagePredicates(t *testing.T) {
	for _, s := range allStages {
		if !s.Valid() {
			t.Fatalf("%q should be valid", s)
		}
		if s.IsTerminal() != (s == StageDone) {
			t.Fatalf("IsTerminal(%q) mismatch", s)
		}
		if IsTerminalStage(s) != (s == StageDone) {
			t.Fatalf("IsTerminalStage(%q) mismatch", s)
		}
	}
	if Stage("z").Valid() {
		t.Fatal("z should be invalid")
	}
}

func TestTransitions(t *testing.T) {
	want := map[string][2]Stage{
		"T1": {StageRequirement, StagePlan},
		"T2": {StagePlan, StageRequirement},
		"T3": {StagePlan, StageExecute},
		"T4": {StageExecute, StagePlan},
		"T5": {StageExecute, StageVerify},
		"T6": {StageVerify, StageExecute},
		"T7": {StageVerify, StagePlan},
		"T8": {StageVerify, StageDone},
	}
	all := AllTransitions()
	if len(all) != 8 {
		t.Fatalf("expected 8 transitions, got %d", len(all))
	}
	seen := make(map[string]bool)
	for _, tr := range all {
		pair, ok := want[tr.ID]
		if !ok {
			t.Fatalf("unexpected transition %s", tr.ID)
		}
		if tr.From != pair[0] || tr.To != pair[1] {
			t.Fatalf("%s: got %s->%s", tr.ID, tr.From, tr.To)
		}
		seen[tr.ID] = true
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("transition %s missing", id)
		}
	}
	for id := range want {
		tr, ok := TransitionByID(id)
		if !ok {
			t.Fatalf("TransitionByID(%s) not found", id)
		}
		if tr.ID != id {
			t.Fatalf("TransitionByID(%s) = %s", id, tr.ID)
		}
	}
	if _, ok := TransitionByID("T9"); ok {
		t.Fatal("T9 should not exist")
	}
}

func TestLegalTransitionsAdvance(t *testing.T) {
	for _, tr := range AllTransitions() {
		m, err := NewMachineAt("task-1", tr.From)
		if err != nil {
			t.Fatalf("NewMachineAt: %v", err)
		}
		d, err := m.Advance(tr.To)
		if err != nil {
			t.Fatalf("%s: advance %s->%s: %v", tr.ID, tr.From, tr.To, err)
		}
		if d.Transition.ID != tr.ID {
			t.Fatalf("%s: got transition %s", tr.ID, d.Transition.ID)
		}
		if m.Stage() != tr.To {
			t.Fatalf("%s: machine at %s, want %s", tr.ID, m.Stage(), tr.To)
		}
		if len(d.Events) == 0 {
			t.Fatalf("%s: no pending events", tr.ID)
		}
		var hasAcceptance bool
		for _, e := range d.Events {
			if e.Type == "acceptance.passed" || e.Type == "acceptance.failed" {
				hasAcceptance = true
			}
		}
		if !hasAcceptance {
			t.Fatalf("%s: acceptance event missing", tr.ID)
		}
	}
}

func TestIllegalTransitionsRejected(t *testing.T) {
	legal := make(map[[2]Stage]bool)
	for _, tr := range AllTransitions() {
		legal[[2]Stage{tr.From, tr.To}] = true
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
			d, err := m.Advance(to)
			if err == nil {
				t.Fatalf("%s->%s should be illegal", from, to)
			}
			var fe *Error
			if !errors.As(err, &fe) || fe.Code != ErrIllegalTransition {
				t.Fatalf("%s->%s: got %v", from, to, err)
			}
			if m.Stage() != from {
				t.Fatalf("%s->%s changed stage to %s", from, to, m.Stage())
			}
			if len(d.Events) != 0 {
				t.Fatalf("%s->%s produced events on failure", from, to)
			}
		}
	}
	if checked != 17 {
		t.Fatalf("expected 17 illegal pairs, got %d", checked)
	}
}

func TestTerminalHasNoOutgoing(t *testing.T) {
	m := NewMachineAt2(t, StageDone)
	for _, to := range allStages {
		if _, err := m.Advance(to); err == nil {
			t.Fatalf("e->%s should be rejected", to)
		}
	}
	if !m.Terminal() {
		t.Fatal("machine at e must be terminal")
	}
	if got := LegalTargets(StageDone); len(got) != 0 {
		t.Fatalf("e should have no legal targets, got %v", got)
	}
}

func TestSkipStageExplicitlyRejected(t *testing.T) {
	cases := [][2]Stage{
		{StageRequirement, StageDone},
		{StagePlan, StageDone},
		{StageExecute, StageDone},
	}
	for _, c := range cases {
		m, err := NewMachineAt("task-1", c[0])
		if err != nil {
			t.Fatalf("NewMachineAt: %v", err)
		}
		if _, err := m.Advance(c[1]); err == nil {
			t.Fatalf("%s->%s must be rejected", c[0], c[1])
		}
	}
}

func TestInvalidStageRejected(t *testing.T) {
	m := NewMachine("task-1")
	if _, err := m.Advance(Stage("f")); err == nil {
		t.Fatal("invalid target must be rejected")
	}
	if m.Stage() != StageRequirement {
		t.Fatal("state must not change on invalid target")
	}
	if _, err := NewMachineAt("task-1", Stage("x")); err == nil {
		t.Fatal("invalid resume stage must be rejected")
	}
}

func TestSelectTarget(t *testing.T) {
	cases := []struct {
		from     Stage
		verdict  Verdict
		category string
		want     Stage
		ok       bool
	}{
		{StageRequirement, VerdictPass, "", StagePlan, true},
		{StageRequirement, VerdictFail, "", "", false},
		{StagePlan, VerdictPass, "", StageExecute, true},
		{StagePlan, VerdictFail, "", StageRequirement, true},
		{StageExecute, VerdictPass, "", StageVerify, true},
		{StageExecute, VerdictFail, "plan_gap", StagePlan, true},
		{StageVerify, VerdictPass, "", StageDone, true},
		{StageVerify, VerdictFail, "impl_gap", StageExecute, true},
		{StageVerify, VerdictFail, "test_gap", StageExecute, true},
		{StageVerify, VerdictFail, "plan_gap", StagePlan, true},
		{StageVerify, VerdictFail, "requirement_gap", StagePlan, true},
		{StageVerify, VerdictFail, "unknown", StageExecute, true},
		{StageDone, VerdictFail, "", "", false},
	}
	for _, c := range cases {
		got, ok := SelectTarget(c.from, c.verdict, c.category)
		if ok != c.ok || got != c.want {
			t.Fatalf("%s/%s/%s: got (%q,%v), want (%q,%v)", c.from, c.verdict, c.category, got, ok, c.want, c.ok)
		}
	}
}

func TestTerminalReason(t *testing.T) {
	if _, ok := TerminalFor(StageDone); !ok {
		t.Fatal("e should map to completed")
	}
	if _, ok := TerminalFor(StageExecute); ok {
		t.Fatal("c must not be terminal")
	}
	for _, r := range []TerminalReason{ReasonCompleted, ReasonAborted, ReasonFailed} {
		if !r.Valid() {
			t.Fatalf("%q should be valid", r)
		}
	}
	if TerminalReason("other").Valid() {
		t.Fatal("other should be invalid")
	}
}

func TestIllegalTransitionEvent(t *testing.T) {
	e := IllegalTransitionEvent(StagePlan, StageDone)
	if e.Type != "stage.illegal_transition" || e.Severity != "error" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

func TestConcurrentAdvanceIsSerialized(t *testing.T) {
	m := NewMachine("task-1")
	var wg sync.WaitGroup
	var success int
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Advance(StagePlan); err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("expected exactly one successful transition, got %d", success)
	}
	if m.Stage() != StagePlan {
		t.Fatalf("stage = %s", m.Stage())
	}
}

func TestTaskID(t *testing.T) {
	m := NewMachine("task-42")
	if m.TaskID() != "task-42" {
		t.Fatalf("TaskID = %q", m.TaskID())
	}
}

func NewMachineAt2(t *testing.T, s Stage) *Machine {
	t.Helper()
	m, err := NewMachineAt("task-1", s)
	if err != nil {
		t.Fatalf("NewMachineAt: %v", err)
	}
	return m
}
