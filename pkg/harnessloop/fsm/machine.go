package fsm

import (
	"fmt"
	"sync"
)

type Stage string

const (
	StageRequirement Stage = "a"
	StagePlan        Stage = "b"
	StageExecute     Stage = "c"
	StageVerify      Stage = "d"
	StageDone        Stage = "e"
)

const (
	ErrInvalidStage      = "E_INVALID_STAGE"
	ErrIllegalTransition = "E_ILLEGAL_TRANSITION"
)

var allStages = [5]Stage{StageRequirement, StagePlan, StageExecute, StageVerify, StageDone}

func ParseStage(raw string) (Stage, error) {
	s := Stage(raw)
	if !s.Valid() {
		return "", &Error{Code: ErrInvalidStage, Msg: fmt.Sprintf("stage %q is not one of a,b,c,d,e", raw)}
	}
	return s, nil
}

func (s Stage) Valid() bool {
	for _, v := range allStages {
		if s == v {
			return true
		}
	}
	return false
}

func (s Stage) IsTerminal() bool { return s == StageDone }

func (s Stage) String() string { return string(s) }

type Error struct {
	Code   string
	From   Stage
	To     Stage
	Detail string
	Msg    string
}

func (e *Error) Error() string {
	if e.From != "" || e.To != "" {
		return fmt.Sprintf("%s: %s (%s -> %s)", e.Code, e.Msg, e.From, e.To)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

type PendingEvent struct {
	Type     string
	Severity string
	From     Stage
	To       Stage
}

type Decision struct {
	Transition Transition
	From       Stage
	To         Stage
	Events     []PendingEvent
}

type Machine struct {
	mu        sync.Mutex
	taskID    string
	stage     Stage
	transport int
}

func NewMachine(taskID string) *Machine {
	return &Machine{taskID: taskID, stage: StageRequirement}
}

func NewMachineAt(taskID string, stage Stage) (*Machine, error) {
	if !stage.Valid() {
		return nil, &Error{Code: ErrInvalidStage, Msg: fmt.Sprintf("cannot resume at stage %q", stage)}
	}
	return &Machine{taskID: taskID, stage: stage}, nil
}

func (m *Machine) TaskID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.taskID
}

func (m *Machine) Stage() Stage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stage
}

func (m *Machine) Terminal() bool {
	return m.Stage().IsTerminal()
}

func (m *Machine) Advance(to Stage) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !to.Valid() {
		return Decision{}, &Error{Code: ErrInvalidStage, From: m.stage, To: to, Msg: "target stage is not a,b,c,d,e"}
	}
	t, ok := transitionTo(m.stage, to)
	if !ok {
		return Decision{From: m.stage, To: to}, &Error{
			Code:   ErrIllegalTransition,
			From:   m.stage,
			To:     to,
			Detail: "transition is not in the whitelist T1-T8",
			Msg:    "illegal stage transition",
		}
	}

	from := m.stage
	m.stage = to
	m.transport++

	return Decision{
		Transition: t,
		From:       from,
		To:         to,
		Events:     eventsFor(t),
	}, nil
}

func eventsFor(t Transition) []PendingEvent {
	out := make([]PendingEvent, 0, 4)
	if t.Trigger == TriggerAcceptancePass {
		out = append(out, PendingEvent{Type: "acceptance.passed", Severity: "info", From: t.From, To: t.To})
	} else {
		out = append(out, PendingEvent{Type: "acceptance.failed", Severity: "warn", From: t.From, To: t.To})
	}
	out = append(out, PendingEvent{Type: "stage.exited", Severity: "info", From: t.From, To: t.To})
	if t.To != StageDone {
		out = append(out, PendingEvent{Type: "stage.entered", Severity: "info", From: t.From, To: t.To})
	} else {
		out = append(out, PendingEvent{Type: "task.completed", Severity: "info", From: t.From, To: t.To})
	}
	return out
}

func IllegalTransitionEvent(from, to Stage) PendingEvent {
	return PendingEvent{Type: "stage.illegal_transition", Severity: "error", From: from, To: to}
}
