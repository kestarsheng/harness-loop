package subtask

import (
	"errors"
	"fmt"
	"time"
)

type FailureStrategy string

const (
	StrategyFailFast FailureStrategy = "fail-fast"
	StrategyTolerate FailureStrategy = "tolerate"
	StrategyRetry    FailureStrategy = "retry"
)

func (f FailureStrategy) Valid() bool {
	switch f {
	case StrategyFailFast, StrategyTolerate, StrategyRetry:
		return true
	}
	return false
}

type ArtifactRef struct {
	Path string `json:"path"`
	Hash string `json:"hash,omitempty"`
	Size int64  `json:"size,omitempty"`
}

type Subtask struct {
	SubtaskID          string          `json:"subtask_id"`
	ParentTaskID       string          `json:"parent_task_id"`
	TenantID           string          `json:"tenant_id"`
	ProjectID          string          `json:"project_id"`
	DagID              string          `json:"dag_id"`
	DagNodeID          string          `json:"dag_node_id"`
	Depth              int             `json:"depth"`
	IndexInParent      int             `json:"index_in_parent"`
	ParallelSetID      string          `json:"parallel_set_id,omitempty"`
	Role               string          `json:"role"`
	Status             Status          `json:"status"`
	DependsOn          []string        `json:"depends_on,omitempty"`
	InputContract      map[string]any  `json:"input_contract,omitempty"`
	OutputContract     map[string]any  `json:"output_contract,omitempty"`
	AcceptanceCriteria []string        `json:"acceptance_criteria,omitempty"`
	AcceptanceID       string          `json:"acceptance_id,omitempty"`
	FailureStrategy    FailureStrategy `json:"failure_strategy"`
	AttemptNo          int             `json:"attempt_no"`
	ArtifactRefs       []ArtifactRef   `json:"artifact_refs,omitempty"`
	HandoffID          string          `json:"handoff_id,omitempty"`
	AllocTokens        int64           `json:"alloc_tokens"`
	ConsumedTokens     int64           `json:"consumed_tokens"`
	// TokenSource records whether ConsumedTokens came from the provider's usage
	// block ("actual") or from a local estimate ("estimate").
	TokenSource  string    `json:"token_source,omitempty"`
	ConsumedCost float64   `json:"consumed_cost"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

var ErrInvalidSubtask = errors.New("subtask: invalid subtask")

// Retry errors. A retry re-opens one failed subtask for another attempt; it is
// not a rewind of the state machine, so it has its own guarded entry point
// (RequeueForRetry) instead of an extra edge in AllowedTransition.
var (
	// ErrRetryBudgetExhausted means attempt_no already used the whole budget.
	ErrRetryBudgetExhausted = errors.New("subtask: retry budget exhausted")
	// ErrRetryNotFailed means only a failed subtask may be retried.
	ErrRetryNotFailed = errors.New("subtask: only a failed subtask can be retried")
)

// RetryBudgetLeft reports whether attempts remain under the configured cap.
// attempt_no is the 1-based number of the attempt that produced the current
// status (the planner opens a subtask at 1), so maxAttempts=2 means at most two
// runs in total: attempt 1 may be retried once, attempt 2 is final. It
// deliberately ignores Status: callers decide whether the subtask is in a
// retryable state, and RequeueForRetry enforces it.
func (s *Subtask) RetryBudgetLeft(maxAttempts int) bool {
	return s != nil && maxAttempts > 0 && s.AttemptNo < maxAttempts
}

// RequeueForRetry is the only sanctioned failed→pending migration: it opens the
// next attempt (attempt_no+1) and hands the subtask back to the scheduler as
// pending. It is budget-bounded by construction, so a retry loop can always
// terminate; an exhausted subtask stays failed (terminal) and the parent closes
// through the d-stage gate (T-P2-29).
func (s *Subtask) RequeueForRetry(maxAttempts int) error {
	if s == nil {
		return fmt.Errorf("%w: nil subtask", ErrInvalidSubtask)
	}
	if s.Status != StatusFailed {
		return fmt.Errorf("%w: subtask %s is %s", ErrRetryNotFailed, s.SubtaskID, s.Status)
	}
	if !s.RetryBudgetLeft(maxAttempts) {
		return fmt.Errorf("%w: subtask %s stopped at attempt %d of %d",
			ErrRetryBudgetExhausted, s.SubtaskID, s.AttemptNo, maxAttempts)
	}
	s.AttemptNo++
	s.Status = StatusPending
	s.AcceptanceID = ""
	return nil
}

func (s *Subtask) Validate() error {
	if s.SubtaskID == "" || s.ParentTaskID == "" || s.TenantID == "" || s.ProjectID == "" {
		return fmt.Errorf("%w: subtask_id/parent_task_id/tenant_id/project_id are required", ErrInvalidSubtask)
	}
	if s.DagID == "" || s.DagNodeID == "" {
		return fmt.Errorf("%w: dag_id/dag_node_id are required", ErrInvalidSubtask)
	}
	if s.Depth < 1 || s.Depth > 2 {
		return fmt.Errorf("%w: depth must be 1..2, got %d", ErrInvalidSubtask, s.Depth)
	}
	if s.Role == "" {
		return fmt.Errorf("%w: role is required", ErrInvalidSubtask)
	}
	if !s.Status.Valid() {
		return fmt.Errorf("%w: status %q is invalid", ErrInvalidSubtask, s.Status)
	}
	if s.FailureStrategy == "" {
		s.FailureStrategy = StrategyFailFast
	}
	if !s.FailureStrategy.Valid() {
		return fmt.Errorf("%w: failure_strategy %q is invalid", ErrInvalidSubtask, s.FailureStrategy)
	}
	if s.Status == StatusAccepted && s.AcceptanceID == "" {
		return &AcceptanceRequiredError{SubtaskID: s.SubtaskID}
	}
	if s.Status == StatusWaitingHandoff && s.HandoffID == "" {
		return fmt.Errorf("%w: waiting_handoff requires handoff_id", ErrInvalidSubtask)
	}
	return nil
}

type AcceptanceRequiredError struct {
	SubtaskID string
}

func (e *AcceptanceRequiredError) Error() string {
	return "E_SUBTASK_ACCEPTANCE_REQUIRED: subtask " + e.SubtaskID + " cannot be accepted without an acceptance record"
}
