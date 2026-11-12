package subtask

import (
	"errors"
	"fmt"
)

type Status string

const (
	StatusPending        Status = "pending"
	StatusRunning        Status = "running"
	StatusWaitingHandoff Status = "waiting_handoff"
	StatusAccepted       Status = "accepted"
	StatusRejected       Status = "rejected"
	StatusFailed         Status = "failed"
	StatusCancelled      Status = "cancelled"
)

var allStatuses = [7]Status{
	StatusPending, StatusRunning, StatusWaitingHandoff,
	StatusAccepted, StatusRejected, StatusFailed, StatusCancelled,
}

func (s Status) Valid() bool {
	for _, v := range allStatuses {
		if s == v {
			return true
		}
	}
	return false
}

func (s Status) Terminal() bool {
	switch s {
	case StatusAccepted, StatusRejected, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

func ParseStatus(raw string) (Status, error) {
	s := Status(raw)
	if !s.Valid() {
		return "", fmt.Errorf("E_INVALID_SUBTASK_STATUS: %q", raw)
	}
	return s, nil
}

var ErrIllegalSubtaskTransition = errors.New("E_ILLEGAL_SUBTASK_TRANSITION")

func AllowedTransition(from, to Status) bool {
	switch from {
	case StatusPending:
		return to == StatusRunning || to == StatusCancelled
	case StatusRunning:
		return to == StatusRunning || to == StatusWaitingHandoff ||
			to == StatusAccepted || to == StatusRejected ||
			to == StatusFailed || to == StatusCancelled
	case StatusWaitingHandoff:
		return to == StatusRunning || to == StatusCancelled
	}
	return false
}

func Transition(from, to Status, acceptanceID, handoffID string) (Status, error) {
	if !from.Valid() || !to.Valid() {
		return from, fmt.Errorf("E_INVALID_SUBTASK_STATUS: %q -> %q", from, to)
	}
	if !AllowedTransition(from, to) {
		return from, fmt.Errorf("%w: %s -> %s", ErrIllegalSubtaskTransition, from, to)
	}
	if to == StatusAccepted && acceptanceID == "" {
		return from, &AcceptanceRequiredError{}
	}
	if to == StatusWaitingHandoff && handoffID == "" {
		return from, fmt.Errorf("%w: waiting_handoff requires handoff_id", ErrIllegalSubtaskTransition)
	}
	return to, nil
}
