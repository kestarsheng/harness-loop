package subtask

import (
	"errors"
	"testing"
)

// 一次重试必须消耗一次预算，且只有 failed 能进入这条路径：重试是"再跑一次"，
// 不是把任何状态随手改回 pending。
func TestRequeueForRetryConsumesBudget(t *testing.T) {
	s := &Subtask{SubtaskID: "st-1", Status: StatusFailed, AttemptNo: 1}
	if !s.RetryBudgetLeft(2) {
		t.Fatalf("attempt 1 of 2 must have a retry left")
	}
	if err := s.RequeueForRetry(2); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if s.Status != StatusPending || s.AttemptNo != 2 {
		t.Fatalf("status=%s attempt_no=%d, want pending/2", s.Status, s.AttemptNo)
	}
	if err := s.RequeueForRetry(2); !errors.Is(err, ErrRetryNotFailed) {
		t.Fatalf("a pending subtask must not be requeued again: %v", err)
	}
}

// 上限是硬约束：预算用尽后必须报 ErrRetryBudgetExhausted 并保持 failed，
// 这样调用方（scheduler/propagation）不可能构造出无限重试。
func TestRequeueForRetryStopsAtCap(t *testing.T) {
	s := &Subtask{SubtaskID: "st-2", Status: StatusFailed, AttemptNo: 2}
	if s.RetryBudgetLeft(2) {
		t.Fatalf("attempt_no=2 with max=2 must be exhausted")
	}
	if err := s.RequeueForRetry(2); !errors.Is(err, ErrRetryBudgetExhausted) {
		t.Fatalf("err = %v, want ErrRetryBudgetExhausted", err)
	}
	if s.Status != StatusFailed || s.AttemptNo != 2 {
		t.Fatalf("an exhausted retry must not change the subtask: %s/%d", s.Status, s.AttemptNo)
	}
}

// 终态不可随意回退：新增的重试入口不能顺手把通用迁移打开。
// AllowedTransition / Transition 对 failed 仍无出边，只有 RequeueForRetry 能出去。
func TestRetryDoesNotOpenGenericTerminalRewind(t *testing.T) {
	if AllowedTransition(StatusFailed, StatusPending) {
		t.Fatalf("failed -> pending must stay illegal in the generic graph")
	}
	if _, err := Transition(StatusFailed, StatusPending, "", ""); !errors.Is(err, ErrIllegalSubtaskTransition) {
		t.Fatalf("Transition(failed->pending) err = %v, want ErrIllegalSubtaskTransition", err)
	}
	for _, to := range allStatuses {
		if AllowedTransition(StatusCancelled, to) || AllowedTransition(StatusAccepted, to) {
			t.Fatalf("%s must keep no outgoing edge", to)
		}
	}
}
