package orchestrator

import (
	"fmt"

	"agent-platform/pkg/harnessloop/subtask"
)

// Stable error codes for the multi-agent orchestration layer (design 4.13).
// Role/tool errors are deliberately reused from pkg/harnessloop/subtask so the
// whole platform has exactly one permission contract (T-P1-22).
const (
	CodeDAGIncomplete          = "E_DAG_INCOMPLETE"
	CodeDAGCycle               = "E_DAG_CYCLE"
	CodeDAGParallelConflict    = "E_DAG_PARALLEL_CONFLICT"
	CodeContractIncomplete     = "E_CONTRACT_INCOMPLETE"
	CodeSubtaskLimitExceeded   = "E_SUBTASK_LIMIT_EXCEEDED"
	CodeSubtaskDepthExceeded   = "E_SUBTASK_DEPTH_EXCEEDED"
	CodeSubtaskNotSplittable   = "E_SUBTASK_NOT_SPLITTABLE"
	CodePropagationUndeclared  = "E_PROPAGATION_UNDECLARED"
	CodeHandoffIncomplete      = "E_HANDOFF_INCOMPLETE"
	CodeCrossTenantDenied      = "E_CROSS_TENANT_DENIED"
	CodeAggregationMissing     = "E_AGGREGATION_MISSING"
	CodeAggregationIncomplete  = "E_AGGREGATION_INCOMPLETE"
	CodeHandoffHistoryForbid   = "E_HANDOFF_HISTORY_FORBIDDEN"
	CodeSubtaskNotAccepted     = "E_SUBTASK_ACCEPTANCE_REQUIRED"
	CodeSubtaskNotFound        = "E_SUBTASK_NOT_FOUND"
	CodeSubtaskNotRunning      = "E_SUBTASK_NOT_RUNNING"
	CodePropagationIncomplete  = "E_PROPAGATION_INCOMPLETE"
	CodeSchedulerLimitExceeded = "E_SCHEDULER_LIMIT_EXCEEDED"
)

func newError(code, format string, args ...any) error {
	return &subtask.CodedError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// ErrorCode extracts the stable code from an orchestration error. Any error
// that is not a coded error is reported as the empty string.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if coded, ok := err.(*subtask.CodedError); ok {
		return coded.Code
	}
	type coder interface{ Code() string }
	if c, ok := err.(coder); ok {
		return c.Code()
	}
	return ""
}
