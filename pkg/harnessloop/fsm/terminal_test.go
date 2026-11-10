package fsm

import "testing"

func TestTerminalMatrix(t *testing.T) {
	cases := []struct {
		stage    Stage
		terminal bool
		reason   TerminalReason
		ok       bool
	}{
		{StageRequirement, false, "", false},
		{StagePlan, false, "", false},
		{StageExecute, false, "", false},
		{StageVerify, false, "", false},
		{StageDone, true, ReasonCompleted, true},
	}
	for _, c := range cases {
		if got := c.stage.IsTerminal(); got != c.terminal {
			t.Fatalf("%s terminal=%v want %v", c.stage, got, c.terminal)
		}
		reason, ok := TerminalFor(c.stage)
		if ok != c.ok || reason != c.reason {
			t.Fatalf("%s -> (%q,%v), want (%q,%v)", c.stage, reason, ok, c.reason, c.ok)
		}
	}
}

func TestTerminalReasonSet(t *testing.T) {
	reasons := []TerminalReason{ReasonCompleted, ReasonAborted, ReasonFailed}
	seen := map[TerminalReason]bool{}
	for _, r := range reasons {
		if !r.Valid() {
			t.Fatalf("%q must be valid", r)
		}
		if seen[r] {
			t.Fatalf("duplicate terminal reason %q", r)
		}
		seen[r] = true
	}
	for _, bad := range []TerminalReason{"", "done", "cancelled"} {
		if bad.Valid() {
			t.Fatalf("%q must be invalid", bad)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("terminal reasons must be exactly three, got %d", len(seen))
	}
}

func TestTerminalOnlyReachableFromVerify(t *testing.T) {
	for _, from := range []Stage{StageRequirement, StagePlan, StageExecute} {
		m, err := NewMachineAt("task-1", from)
		if err != nil {
			t.Fatalf("NewMachineAt: %v", err)
		}
		if _, err := m.Advance(StageDone); err == nil {
			t.Fatalf("%s must not reach e directly", from)
		}
	}
	m, err := NewMachineAt("task-1", StageVerify)
	if err != nil {
		t.Fatalf("NewMachineAt: %v", err)
	}
	if _, err := m.Advance(StageDone); err != nil {
		t.Fatalf("d->e must be legal: %v", err)
	}
	if !m.Terminal() {
		t.Fatal("machine must be terminal after d->e")
	}
	if _, ok := TerminalFor(m.Stage()); !ok {
		t.Fatal("terminal stage must map to a reason")
	}
}
