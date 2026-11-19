package service

import (
	"context"
	"testing"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/subtask"
)

// flakyExecutor fails the first N calls of a node and succeeds afterwards, so a
// retry is observable as "the same subtask ran twice with different outcomes".
type flakyExecutor struct {
	failures map[string]int
	seen     map[string]int
}

func newFlakyExecutor(failures map[string]int) *flakyExecutor {
	return &flakyExecutor{failures: failures, seen: make(map[string]int)}
}

func (e *flakyExecutor) Execute(_ context.Context, _ OrchestratorStageContext, st *subtask.Subtask) (*SubtaskResult, error) {
	e.seen[st.DagNodeID]++
	if e.seen[st.DagNodeID] <= e.failures[st.DagNodeID] {
		return nil, &SubtaskExecError{Reason: "attempt " + st.SubtaskID + " failed"}
	}
	return &SubtaskResult{AcceptanceID: "acc-" + st.SubtaskID, Conclusion: "done-" + st.SubtaskID}, nil
}

const retrySingleNodePlan = `{"dag_id":"dag-retry","nodes":[
 {"node_id":"n1","role":"coder","input_contract":{"type":"workspace","paths":["."]},"output_contract":{"type":"files","paths":["out/n1"]}}],
 "edges":[],
 "parallel_sets":[{"set_id":"p1","nodes":["n1"],"failure_strategy":"tolerate","tolerate_ratio":0.5}]}`

func newRetryBridge(t *testing.T, exec SubtaskExecutor) (*OrchestratorBridge, *modelEventSink, *orchestrator.PlanResult) {
	t.Helper()
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	bridge := NewOrchestratorBridge(true, emitter, exec)
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-retry", Stage: "b", AttemptNo: 1}
	plan, err := bridge.PlanBStage(context.Background(), ec, []byte(retrySingleNodePlan))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return bridge, sink, plan
}

// countEvents counts how many events of one type reached the publisher.
func countEvents(sink *modelEventSink, typ event.EventType) int {
	n := 0
	for _, got := range sink.types() {
		if got == typ {
			n++
		}
	}
	return n
}

func retryStageContext(stage string) OrchestratorStageContext {
	return OrchestratorStageContext{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-retry", Stage: stage, AttemptNo: 1,
	}
}

// 失败→重跑→accepted：d 阶段闸门把父任务打回 c 之后，之前 failed 的子任务必须
// 按剩余预算重新执行并 accepted，d 阶段才可能收口。
func TestBridgeRetriesFailedSubtaskAfterRollback(t *testing.T) {
	ctx := context.Background()
	exec := newFlakyExecutor(map[string]int{"n1": 1}) // 第一次失败，重跑成功
	bridge, sink, _ := newRetryBridge(t, exec)

	if _, err := bridge.DispatchCStage(ctx, retryStageContext("c")); err != nil {
		t.Fatalf("dispatch 1: %v", err)
	}
	report, err := bridge.AggregateDStage(ctx, retryStageContext("d"))
	if err == nil {
		t.Fatalf("0/1 accepted must not close the d stage: %+v", report)
	}

	// d→c 回退后的重入：沙箱里同一个 DAG、同一个子任务，failed 必须被重新拉起。
	if _, err := bridge.DispatchCStage(ctx, retryStageContext("c")); err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	report, err = bridge.AggregateDStage(ctx, retryStageContext("d"))
	if err != nil {
		t.Fatalf("the retried subtask was accepted, so d must close: %v", err)
	}
	if !report.Complete || report.Summary.Accepted != 1 || report.Summary.Failed != 0 {
		t.Fatalf("report = %+v", report)
	}
	if got := exec.seen["n1"]; got != 2 {
		t.Fatalf("n1 executed %d times, want 2 (original + retry)", got)
	}

	// 事件序列必须能把这件事讲明白：failed → retry.scheduled → started → accepted。
	var seq []event.EventType
	for _, typ := range sink.types() {
		switch typ {
		case event.SubtaskFailed, event.SubtaskStarted, event.RetryScheduled, event.SubtaskAccepted:
			seq = append(seq, typ)
		}
	}
	want := []event.EventType{event.SubtaskStarted, event.SubtaskFailed, event.RetryScheduled, event.SubtaskStarted, event.SubtaskAccepted}
	if len(seq) != len(want) {
		t.Fatalf("event sequence = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("event sequence = %v, want %v", seq, want)
		}
	}
	if n := countEvents(sink, event.RetryScheduled); n != 1 {
		t.Fatalf("retry.scheduled events = %d, want 1", n)
	}
}

// 超限：预算用尽后子任务停在 failed，不再重跑，父任务只能按 T-P2-29 由 d 阶段
// 闸门拦下（这里断言闸门始终不通过 + retry.exhausted 出现一次）。
func TestBridgeRetryExhaustionKeepsFailed(t *testing.T) {
	ctx := context.Background()
	exec := newFlakyExecutor(map[string]int{"n1": 100}) // 永远失败
	bridge, sink, _ := newRetryBridge(t, exec)

	// attempt_no 从 1 起算：RetryMaxAttempts=2 表示最多两次真实执行。
	for round := 0; round < 3; round++ {
		if _, err := bridge.DispatchCStage(ctx, retryStageContext("c")); err != nil {
			t.Fatalf("dispatch round %d: %v", round, err)
		}
		if _, err := bridge.AggregateDStage(ctx, retryStageContext("d")); err == nil {
			t.Fatalf("round %d: a failed subtask must never close the d stage", round)
		}
	}

	maxAttempts := orchestrator.DefaultQuotaConfig().RetryMaxAttempts
	if got := exec.seen["n1"]; got != maxAttempts {
		t.Fatalf("n1 executed %d times, want %d (attempt budget)", got, maxAttempts)
	}
	if n := countEvents(sink, event.SubtaskFailed); n != maxAttempts {
		t.Fatalf("subtask.failed events = %d, want %d", n, maxAttempts)
	}
	if n := countEvents(sink, event.RetryScheduled); n != maxAttempts-1 {
		t.Fatalf("retry.scheduled events = %d, want %d", n, maxAttempts-1)
	}
	if n := countEvents(sink, event.RetryExhausted); n != 1 {
		t.Fatalf("retry.exhausted events = %d, want 1 (all=%v)", n, sink.types())
	}
	// 超限后不得出现 accepted：父任务靠闸门收口，而不是被"重试过"洗白。
	if n := countEvents(sink, event.SubtaskAccepted); n != 0 {
		t.Fatalf("subtask.accepted events = %d, want 0", n)
	}
}
