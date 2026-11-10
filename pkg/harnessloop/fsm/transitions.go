package fsm

type Trigger string

const (
	TriggerAcceptancePass Trigger = "acceptance_pass"
	TriggerAcceptanceFail Trigger = "acceptance_fail"
)

type Transition struct {
	ID      string
	From    Stage
	To      Stage
	Trigger Trigger
}

var transitions = [8]Transition{
	{ID: "T1", From: StageRequirement, To: StagePlan, Trigger: TriggerAcceptancePass},
	{ID: "T2", From: StagePlan, To: StageRequirement, Trigger: TriggerAcceptanceFail},
	{ID: "T3", From: StagePlan, To: StageExecute, Trigger: TriggerAcceptancePass},
	{ID: "T4", From: StageExecute, To: StagePlan, Trigger: TriggerAcceptanceFail},
	{ID: "T5", From: StageExecute, To: StageVerify, Trigger: TriggerAcceptancePass},
	{ID: "T6", From: StageVerify, To: StageExecute, Trigger: TriggerAcceptanceFail},
	{ID: "T7", From: StageVerify, To: StagePlan, Trigger: TriggerAcceptanceFail},
	{ID: "T8", From: StageVerify, To: StageDone, Trigger: TriggerAcceptancePass},
}

func AllTransitions() []Transition {
	out := make([]Transition, len(transitions))
	copy(out, transitions[:])
	return out
}

func TransitionByID(id string) (Transition, bool) {
	for _, t := range transitions {
		if t.ID == id {
			return t, true
		}
	}
	return Transition{}, false
}

func LegalTargets(from Stage) []Stage {
	out := make([]Stage, 0, 3)
	for _, t := range transitions {
		if t.From == from {
			out = append(out, t.To)
		}
	}
	return out
}

func transitionTo(from, to Stage) (Transition, bool) {
	for _, t := range transitions {
		if t.From == from && t.To == to {
			return t, true
		}
	}
	return Transition{}, false
}

type Verdict string

const (
	VerdictPass Verdict = "pass"
	VerdictFail Verdict = "fail"
)

func SelectTarget(from Stage, verdict Verdict, category string) (Stage, bool) {
	switch from {
	case StageRequirement:
		if verdict == VerdictPass {
			return StagePlan, true
		}
		return "", false
	case StagePlan:
		if verdict == VerdictPass {
			return StageExecute, true
		}
		return StageRequirement, true
	case StageExecute:
		if verdict == VerdictPass {
			return StageVerify, true
		}
		return StagePlan, true
	case StageVerify:
		if verdict == VerdictPass {
			return StageDone, true
		}
		switch category {
		case "plan_gap", "requirement_gap":
			return StagePlan, true
		case "impl_gap", "test_gap", "":
			return StageExecute, true
		default:
			return StageExecute, true
		}
	default:
		return "", false
	}
}

func IsLegalTarget(from, to Stage) bool {
	_, ok := transitionTo(from, to)
	return ok
}

func (t Trigger) Valid() bool {
	return t == TriggerAcceptancePass || t == TriggerAcceptanceFail
}
