package fsm

import (
	"fmt"
	"sync"
	"time"
)

const (
	ErrInvalidEntryReason     = "E_INVALID_ENTRY_REASON"
	ErrAttemptOnTerminalStage = "E_ATTEMPT_ON_TERMINAL_STAGE"
	ErrAcceptanceBeforeFinish = "E_ACCEPTANCE_BEFORE_FINISH"
	ErrInvalidAttemptNo       = "E_INVALID_ATTEMPT_NO"
)

type EntryReason string

const (
	EntryInitial          EntryReason = "initial"
	EntryAcceptanceFailed EntryReason = "acceptance_failed"
	EntryRetry            EntryReason = "retry"
	EntryResume           EntryReason = "resume"
)

var allEntryReasons = [4]EntryReason{EntryInitial, EntryAcceptanceFailed, EntryRetry, EntryResume}

func (r EntryReason) Valid() bool {
	for _, v := range allEntryReasons {
		if r == v {
			return true
		}
	}
	return false
}

func ParseEntryReason(raw string) (EntryReason, error) {
	r := EntryReason(raw)
	if !r.Valid() {
		return "", &Error{Code: ErrInvalidEntryReason, Msg: fmt.Sprintf("entry_reason %q must be initial|acceptance_failed|retry|resume", raw)}
	}
	return r, nil
}

type Attempt struct {
	AttemptID           string
	TaskID              string
	Stage               Stage
	AttemptNo           int
	EntryReason         EntryReason
	StartedAt           time.Time
	EndedAt             time.Time
	FinishReceived      bool
	AcceptanceID        string
	RetryCount          int
	AcceptanceFailCount int
}

func NewAttempt(taskID string, stage Stage, attemptNo int, reason EntryReason, now time.Time) (Attempt, error) {
	if !stage.Valid() {
		return Attempt{}, &Error{Code: ErrInvalidStage, Msg: fmt.Sprintf("stage %q is invalid", stage)}
	}
	if stage == StageDone {
		return Attempt{}, &Error{Code: ErrAttemptOnTerminalStage, Msg: "stage e never records an attempt"}
	}
	if attemptNo < 1 {
		return Attempt{}, &Error{Code: ErrInvalidAttemptNo, Msg: fmt.Sprintf("attempt_no %d must be >= 1", attemptNo)}
	}
	if !reason.Valid() {
		return Attempt{}, &Error{Code: ErrInvalidEntryReason, Msg: fmt.Sprintf("entry_reason %q is invalid", reason)}
	}
	return Attempt{
		AttemptID:   AttemptIDFor(taskID, stage, attemptNo),
		TaskID:      taskID,
		Stage:       stage,
		AttemptNo:   attemptNo,
		EntryReason: reason,
		StartedAt:   now,
	}, nil
}

func AttemptIDFor(taskID string, stage Stage, attemptNo int) string {
	return fmt.Sprintf("%s-%s-%d", taskID, stage, attemptNo)
}

func (a *Attempt) Finish(now time.Time) error {
	if a.FinishReceived {
		return nil
	}
	a.FinishReceived = true
	a.EndedAt = now
	return nil
}

func (a *Attempt) AttachAcceptance(acceptanceID string) error {
	if !a.FinishReceived {
		return &Error{Code: ErrAcceptanceBeforeFinish, Msg: "acceptance_id requires finish_received=true"}
	}
	if acceptanceID == "" {
		return &Error{Code: ErrAcceptanceBeforeFinish, Msg: "acceptance_id must not be empty"}
	}
	a.AcceptanceID = acceptanceID
	return nil
}

func (a Attempt) Completed() bool { return a.FinishReceived && !a.EndedAt.IsZero() }

type AttemptTracker struct {
	mu   sync.Mutex
	next map[Stage]int
}

func NewAttemptTracker(attemptNoByStage map[string]int) *AttemptTracker {
	t := &AttemptTracker{next: make(map[Stage]int, 4)}
	for _, s := range allStages[:4] {
		last := attemptNoByStage[string(s)]
		if last < 0 {
			last = 0
		}
		t.next[s] = last + 1
	}
	return t
}

func (t *AttemptTracker) NextNo(stage Stage) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.next[stage]
}

func (t *AttemptTracker) Enter(taskID string, stage Stage, reason EntryReason, now time.Time) (Attempt, error) {
	if stage == StageDone {
		return Attempt{}, &Error{Code: ErrAttemptOnTerminalStage, Msg: "stage e never records an attempt"}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	no := t.next[stage]
	a, err := NewAttempt(taskID, stage, no, reason, now)
	if err != nil {
		return Attempt{}, err
	}
	t.next[stage] = no + 1
	return a, nil
}

func (t *AttemptTracker) Snapshot() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.next))
	for k, v := range t.next {
		out[string(k)] = v - 1
	}
	return out
}
