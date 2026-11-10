package fsm

type TerminalReason string

const (
	ReasonCompleted TerminalReason = "completed"
	ReasonAborted   TerminalReason = "aborted"
	ReasonFailed    TerminalReason = "failed"
)

func (r TerminalReason) Valid() bool {
	return r == ReasonCompleted || r == ReasonAborted || r == ReasonFailed
}

func IsTerminalStage(s Stage) bool { return s == StageDone }

func TerminalFor(s Stage) (TerminalReason, bool) {
	if s == StageDone {
		return ReasonCompleted, true
	}
	return "", false
}
