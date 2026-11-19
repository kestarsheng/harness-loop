package orchestrator

import (
	"context"
	"testing"

	"agent-platform/pkg/harnessloop/subtask"
)

// retryScheduler builds a scheduler with one runnable subtask and a recording
// emitter, then starts it so the item is in the state a failure leaves behind.
func retryScheduler(t *testing.T, id string, attemptNo int) (*Scheduler, *MemoryEmitter, *subtask.Subtask) {
	t.Helper()
	emitter := NewMemoryEmitter()
	s := NewScheduler(DefaultQuotaConfig(), emitter)
	st := testSubtask("task-retry", "dag-retry", id, 0, 1, "ps-retry", "coder", "out/*.txt")
	st.AttemptNo = attemptNo
	if err := s.Enqueue([]*subtask.Subtask{st}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	started, err := s.Dispatch(context.Background())
	if err != nil || len(started) != 1 {
		t.Fatalf("dispatch: started=%d err=%v", len(started), err)
	}
	return s, emitter, st
}

// subtaskEvents lists the event types emitted for one subtask, in order.
func subtaskEvents(emitter *MemoryEmitter, subtaskID string) []string {
	var out []string
	for _, ev := range emitter.Events() {
		if ev.SubtaskID == subtaskID {
			out = append(out, ev.Type)
		}
	}
	return out
}

// 失败→重跑→accepted：重试必须真的回到 pending、被重新调度，并以 accepted 收口，
// 且事件序列能把"失败—重试—重跑—验收"四步讲清楚。
func TestSchedulerRetryRerunsAndAccepts(t *testing.T) {
	ctx := context.Background()
	s, emitter, st := retryScheduler(t, "st-retry", 1)
	if err := s.MarkFailed(ctx, st.SubtaskID, "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	requeued, err := s.RetrySubtask(ctx, st.SubtaskID, "test")
	if err != nil || !requeued {
		t.Fatalf("retry: requeued=%v err=%v", requeued, err)
	}
	item, _ := s.Item(st.SubtaskID)
	if item.Status != subtask.StatusPending || item.AttemptNo != 2 {
		t.Fatalf("after retry: status=%s attempt_no=%d, want pending/2", item.Status, item.AttemptNo)
	}

	started, err := s.Dispatch(ctx)
	if err != nil || len(started) != 1 || started[0].SubtaskID != st.SubtaskID {
		t.Fatalf("the requeued subtask must be dispatched again: started=%d err=%v", len(started), err)
	}
	if err := s.MarkAccepted(ctx, st.SubtaskID, "acc-retry"); err != nil {
		t.Fatalf("accept: %v", err)
	}

	item, _ = s.Item(st.SubtaskID)
	if item.Status != subtask.StatusAccepted || item.AttemptNo != 2 {
		t.Fatalf("final: status=%s attempt_no=%d, want accepted/2", item.Status, item.AttemptNo)
	}
	want := []string{
		EventSubtaskStarted,
		EventSubtaskFailed,
		EventSubtaskRetryScheduled,
		EventSubtaskStarted,
		EventSubtaskAccepted,
	}
	got := subtaskEvents(emitter, st.SubtaskID)
	if len(got) != len(want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event sequence = %v, want %v", got, want)
		}
	}
	// retry.scheduled 必须把预算写进载荷，否则排障时看不出还剩几次。
	retryEvents := emitter.Find(EventSubtaskRetryScheduled)
	if len(retryEvents) != 1 {
		t.Fatalf("retry.scheduled events = %d, want 1", len(retryEvents))
	}
	if retryEvents[0].Payload["attempt_no"] != 2 || retryEvents[0].Payload["max_attempts"] != 2 {
		t.Fatalf("retry payload = %+v", retryEvents[0].Payload)
	}
}

// 超限：预算用尽后子任务仍是 failed，不得再被调度；父任务只能靠 d 阶段闸门收口。
func TestSchedulerRetryStopsAtAttemptCap(t *testing.T) {
	ctx := context.Background()
	s, emitter, st := retryScheduler(t, "st-cap", 2) // RetryMaxAttempts = 2
	if err := s.MarkFailed(ctx, st.SubtaskID, "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	requeued, err := s.RetrySubtask(ctx, st.SubtaskID, "test")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if requeued {
		t.Fatalf("an exhausted budget must not requeue")
	}
	item, _ := s.Item(st.SubtaskID)
	if item.Status != subtask.StatusFailed || item.AttemptNo != 2 {
		t.Fatalf("status=%s attempt_no=%d, want failed/2", item.Status, item.AttemptNo)
	}
	started, err := s.Dispatch(ctx)
	if err != nil || len(started) != 0 {
		t.Fatalf("an exhausted subtask must not run again: started=%d err=%v", len(started), err)
	}
	if n := len(emitter.Find(EventSubtaskRetryExhausted)); n != 1 {
		t.Fatalf("retry.exhausted events = %d, want 1 (all=%v)", n, subtaskEvents(emitter, st.SubtaskID))
	}
	if n := len(emitter.Find(EventSubtaskRetryScheduled)); n != 0 {
		t.Fatalf("an exhausted budget must not schedule a retry")
	}
}

// 只有 failed 能重试：accepted 的终态不会因为一次 retry 调用被拉回队列。
func TestSchedulerRetryIgnoresNonFailed(t *testing.T) {
	ctx := context.Background()
	s, _, st := retryScheduler(t, "st-acc", 1)
	if err := s.MarkAccepted(ctx, st.SubtaskID, "acc-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	requeued, err := s.RetrySubtask(ctx, st.SubtaskID, "test")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if requeued {
		t.Fatalf("accepted must not be retried")
	}
	if item, _ := s.Item(st.SubtaskID); item.Status != subtask.StatusAccepted {
		t.Fatalf("status = %s, want accepted", item.Status)
	}
}

// ActionRetry 之前只有测试引用；这里证明它已经接到执行侧：Apply 之后同一 tick
// 就能重新调度，并再次以 accepted 收口。
func TestPropagationActionRetryReachesScheduler(t *testing.T) {
	ctx := context.Background()
	emitter := NewMemoryEmitter()
	s := NewScheduler(DefaultQuotaConfig(), emitter)
	st := testSubtask("task-retry", "dag-retry", "st-prop", 0, 1, "ps-retry", "coder", "out/*.txt")
	st.FailureStrategy = subtask.StrategyRetry
	if err := s.Enqueue([]*subtask.Subtask{st}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := s.MarkFailed(ctx, st.SubtaskID, "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	propagator := NewPropagator(DefaultQuotaConfig())
	result, err := propagator.Propagate(PropagationRequest{
		ParentTaskID: "task-retry",
		Sets:         []ParallelSet{{SetID: "ps-retry", Nodes: []string{"st-prop"}, FailureStrategy: subtask.StrategyRetry}},
		Subtasks:     []*subtask.Subtask{st},
		Failed:       []FailedSubtask{{SubtaskID: st.SubtaskID, Category: "error"}},
	})
	if err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if result.Decisions[0].Action != ActionRetry {
		t.Fatalf("decision = %+v, want retry", result.Decisions[0])
	}
	if err := propagator.Apply(ctx, s, result); err != nil {
		t.Fatalf("apply: %v", err)
	}
	item, _ := s.Item(st.SubtaskID)
	if item.Status != subtask.StatusPending {
		t.Fatalf("Apply(ActionRetry) must leave the subtask pending, got %s", item.Status)
	}
	started, err := s.Dispatch(ctx)
	if err != nil || len(started) != 1 {
		t.Fatalf("dispatch after retry: started=%d err=%v", len(started), err)
	}
	if err := s.MarkAccepted(ctx, st.SubtaskID, "acc-2"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if n := len(emitter.Find(EventSubtaskRetryScheduled)); n != 1 {
		t.Fatalf("retry.scheduled events = %d, want 1", n)
	}
}

// 预算耗尽时 Apply(ActionRetry) 不能凭空造出重跑：子任务保持 failed，
// 这样上层看到的终态和事件与库里一致。
func TestPropagationApplyRetryRespectsCap(t *testing.T) {
	ctx := context.Background()
	emitter := NewMemoryEmitter()
	s := NewScheduler(DefaultQuotaConfig(), emitter)
	st := testSubtask("task-retry", "dag-retry", "st-prop-cap", 0, 1, "ps-retry", "coder", "out/*.txt")
	st.FailureStrategy = subtask.StrategyRetry
	if err := s.Enqueue([]*subtask.Subtask{st}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := s.MarkFailed(ctx, st.SubtaskID, "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	item, _ := s.Item(st.SubtaskID)
	item.AttemptNo = 2 // 预算已用尽：即使手里有一份 ActionRetry 决策也不能重跑

	result := &PropagationResult{Decisions: []PropagationDecision{{
		Action:        ActionRetry,
		ParallelSetID: "ps-retry",
		Strategy:      subtask.StrategyRetry,
		Retry:         []string{st.SubtaskID},
	}}}
	if err := NewPropagator(DefaultQuotaConfig()).Apply(ctx, s, result); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if item, _ := s.Item(st.SubtaskID); item.Status != subtask.StatusFailed {
		t.Fatalf("status = %s, want failed", item.Status)
	}
	if n := len(emitter.Find(EventSubtaskRetryExhausted)); n != 1 {
		t.Fatalf("retry.exhausted events = %d, want 1", n)
	}
}
