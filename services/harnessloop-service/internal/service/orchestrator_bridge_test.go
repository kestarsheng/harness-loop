package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/subtask"
)

type autoAcceptor struct {
	seen []string
}

func (a *autoAcceptor) Execute(_ context.Context, _ OrchestratorStageContext, st *subtask.Subtask) (*SubtaskResult, error) {
	a.seen = append(a.seen, st.SubtaskID)
	return &SubtaskResult{AcceptanceID: "acc-" + st.SubtaskID, Conclusion: "done-" + st.SubtaskID}, nil
}

func newTestOrchestratorBridge() (*OrchestratorBridge, *modelEventSink, *autoAcceptor) {
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	executor := &autoAcceptor{}
	return NewOrchestratorBridge(true, emitter, executor), sink, executor
}

const testDAGPlan = `{"dag_id":"dag-1","nodes":[
 {"node_id":"n1","role":"coder","input_contract":{"type":"file_set","paths":["src/a/**"]},"output_contract":{"type":"artifacts","paths":["src/a/**"]}}],
 "edges":[],"parallel_sets":[]}`

// 三个并行节点的计划，用来验证 T-P2-29 的三态（0/3、3/3、2/3 容忍失败）。
func testTriNodePlan(strategy string) string {
	return `{"dag_id":"dag-3","nodes":[
	 {"node_id":"n1","role":"coder","input_contract":{"type":"workspace","paths":["."]},"output_contract":{"type":"files","paths":["out/n1"]}},
	 {"node_id":"n2","role":"coder","input_contract":{"type":"workspace","paths":["."]},"output_contract":{"type":"files","paths":["out/n2"]}},
	 {"node_id":"n3","role":"coder","input_contract":{"type":"workspace","paths":["."]},"output_contract":{"type":"files","paths":["out/n3"]}}],
	 "edges":[],
	 "parallel_sets":[{"set_id":"p1","nodes":["n1","n2","n3"],"failure_strategy":"` + strategy + `","tolerate_ratio":0.5}]}`
}

// scriptedExecutor 按 dag_node_id 决定成功/失败，用来伪造子任务行。
type scriptedExecutor struct {
	fail map[string]bool
}

func (e *scriptedExecutor) Execute(_ context.Context, _ OrchestratorStageContext, st *subtask.Subtask) (*SubtaskResult, error) {
	if e.fail[st.DagNodeID] {
		return nil, &SubtaskExecError{Reason: "scripted failure for " + st.DagNodeID}
	}
	return &SubtaskResult{AcceptanceID: "acc-" + st.SubtaskID, Conclusion: "done-" + st.SubtaskID}, nil
}

func planAndRunDStage(t *testing.T, strategy string, exec SubtaskExecutor) (*orchestrator.AggregationReport, error) {
	t.Helper()
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	bridge := NewOrchestratorBridge(true, emitter, exec)
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "b", AttemptNo: 1}
	if _, err := bridge.PlanBStage(context.Background(), ec, []byte(testTriNodePlan(strategy))); err != nil {
		t.Fatalf("plan: %v", err)
	}
	ec.Stage = "c"
	if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	ec.Stage = "d"
	return bridge.AggregateDStage(context.Background(), ec)
}

// T-P2-29 三态：3/3 accepted → complete；0/3 accepted → 不得 complete；
// tolerate 下 2/3 成功 → complete 但带 failed_subtasks。
func TestOrchestratorBridgeDStageClosureThreeStates(t *testing.T) {
	t.Run("all accepted closes", func(t *testing.T) {
		report, err := planAndRunDStage(t, "tolerate", &scriptedExecutor{})
		if err != nil {
			t.Fatalf("3/3 accepted must close: %v", err)
		}
		if !report.Complete || report.Summary.Accepted != 3 || report.CompletedWithFailures {
			t.Fatalf("unexpected report: %+v", report)
		}
		if len(report.FailedSubtasks) != 0 {
			t.Fatalf("a clean run must not list failed subtasks: %+v", report.FailedSubtasks)
		}
	})

	t.Run("zero accepted blocks", func(t *testing.T) {
		exec := &scriptedExecutor{fail: map[string]bool{"n1": true, "n2": true, "n3": true}}
		report, err := planAndRunDStage(t, "tolerate", exec)
		if err == nil {
			t.Fatalf("0/3 accepted must not close the d stage")
		}
		if orchestrator.ErrorCode(err) != orchestrator.CodeAggregationNoAccepted {
			t.Fatalf("error code = %q, want %q (err=%v)", orchestrator.ErrorCode(err), orchestrator.CodeAggregationNoAccepted, err)
		}
		if report == nil || report.Complete {
			t.Fatalf("report must be incomplete: %+v", report)
		}
		if report.Summary.Failed != 3 || len(report.FailedSubtasks) != 3 {
			t.Fatalf("every failed child must be listed: summary=%+v failed=%+v", report.Summary, report.FailedSubtasks)
		}
	})

	t.Run("tolerated failure completes with failures", func(t *testing.T) {
		exec := &scriptedExecutor{fail: map[string]bool{"n3": true}}
		report, err := planAndRunDStage(t, "tolerate", exec)
		if err != nil {
			t.Fatalf("a tolerated failure must still aggregate: %v", err)
		}
		if !report.Complete || !report.CompletedWithFailures {
			t.Fatalf("expected complete with failures: complete=%v with_failures=%v", report.Complete, report.CompletedWithFailures)
		}
		if report.Summary.Accepted != 2 || report.Summary.Failed != 1 {
			t.Fatalf("unexpected summary: %+v", report.Summary)
		}
		if len(report.FailedSubtasks) != 1 || report.FailedSubtasks[0].DagNodeID != "n3" {
			t.Fatalf("failed_subtasks must name n3: %+v", report.FailedSubtasks)
		}
	})

	// 需求 1：running/pending 子任务 → 不得返回 complete。
	t.Run("open subtasks block", func(t *testing.T) {
		sink := &modelEventSink{}
		pub := event.NewPublisher(sink, nil, nil)
		emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
		// 不挂 executor：子任务停在 running，正是"没收口"的场景。
		bridge := NewOrchestratorBridge(true, emitter, nil)
		ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "b", AttemptNo: 1}
		if _, err := bridge.PlanBStage(context.Background(), ec, []byte(testTriNodePlan("tolerate"))); err != nil {
			t.Fatalf("plan: %v", err)
		}
		ec.Stage = "c"
		if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		ec.Stage = "d"
		report, err := bridge.AggregateDStage(context.Background(), ec)
		if orchestrator.ErrorCode(err) != orchestrator.CodeAggregationNotTerminal {
			t.Fatalf("error code = %q, want %q (err=%v)", orchestrator.ErrorCode(err), orchestrator.CodeAggregationNotTerminal, err)
		}
		if report.Complete || report.Summary.NonTerminal != 3 {
			t.Fatalf("open subtasks must block completion: %+v", report)
		}
	})
}

// 没有规划过 DAG（b 阶段没走到 / 计划被拒）不是"子任务没收口"，必须能被上层识别。
func TestOrchestratorBridgeDStageWithoutPlan(t *testing.T) {
	bridge, _, _ := newTestOrchestratorBridge()
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-none", Stage: "d"}
	_, err := bridge.AggregateDStage(context.Background(), ec)
	if !errors.Is(err, ErrNoDagPlanned) {
		t.Fatalf("err = %v, want ErrNoDagPlanned", err)
	}
}

func TestOrchestratorBridgePlansDispatchesAndAggregates(t *testing.T) {
	bridge, sink, executor := newTestOrchestratorBridge()
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "b", AttemptNo: 1}
	plan, err := bridge.PlanBStage(context.Background(), ec, []byte(testDAGPlan))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan == nil || len(plan.Subtasks) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	planned, started := 0, 0
	for _, typ := range sink.types() {
		switch typ {
		case event.SubtaskPlanCreated:
			planned++
		case event.SubtaskStarted:
			started++
		}
	}
	if planned != 1 {
		t.Fatalf("subtask.plan_created events = %d, all=%v", planned, sink.types())
	}

	ec.Stage = "c"
	if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
		t.Fatalf("dispatch 1: %v", err)
	}
	if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	if len(executor.seen) != 1 {
		t.Fatalf("executed subtasks = %v", executor.seen)
	}
	for _, typ := range sink.types() {
		if typ == event.SubtaskStarted {
			started++
		}
	}
	if started == 0 {
		t.Fatalf("subtask.started event missing: %v", sink.types())
	}
	ec.Stage = "d"
	report, err := bridge.AggregateDStage(context.Background(), ec)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if report == nil || report.Summary.Accepted != 1 || report.DagID != "dag-1" {
		t.Fatalf("report = %+v", report)
	}
	found := false
	for _, typ := range sink.types() {
		if typ == event.AggregationCompleted {
			found = true
		}
	}
	if !found {
		t.Fatalf("aggregation.completed event missing: %v", sink.types())
	}
}

func TestOrchestratorBridgeDisabledIsNoop(t *testing.T) {
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	bridge := NewOrchestratorBridge(false, emitter, nil)
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "b"}
	if res, err := bridge.PlanBStage(context.Background(), ec, []byte(testDAGPlan)); res != nil || err != nil {
		t.Fatalf("disabled bridge must be a noop: %+v %v", res, err)
	}
	if len(sink.types()) != 0 {
		t.Fatalf("disabled bridge emitted %v", sink.types())
	}
}

// 时钟用同包 stage_runner_test.go 里已有的 fakeClock：准入层的衰减窗口按 slot
// hold 的年龄算，Admission.SetClock 会把时钟一并交给 fairness，所以年龄可控。

// TestOrchestratorBridgeSharesTenantGateWithAdmission 是生产桥的核心回归：
// 同租户的父任务必须共用一把 TenantGate，所以"前一父任务占满池子"会真的挡住
// 后一父任务——准入层看到的占用与调度层取 slot 的地方是同一本账。
// （如果 bridge 还在用 NewScheduler(DefaultQuotaConfig(), …)，每个父任务会拿到
// 自己的一把 20-slot 空闸门，这个用例必失败。）
//
// 第二段故意越过衰减窗口：衰减记账会放行，但二次确认读到的仍是同一把 gate，
// 空位为 0，所以照样拒绝——这正是"衰减不能凭空造 slot"的桥上层证据。
func TestOrchestratorBridgeSharesTenantGateWithAdmission(t *testing.T) {
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	// 不挂 executor：子任务停在 running，slot 不会被归还，池子因此一直占满。
	quota := orchestrator.DefaultQuotaConfig()
	quota.MaxAgentsPerTenant = 1
	quota.MaxParallelPerParent = 1
	bridge := NewOrchestratorBridge(true, emitter, nil).withQuota(quota)
	clock := &fakeClock{t: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	pool, _ := bridge.poolFor("tenant-a")
	pool.admission.SetClock(clock.Now)

	first := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "b", AttemptNo: 1}
	if _, err := bridge.PlanBStage(context.Background(), first, []byte(testDAGPlan)); err != nil {
		t.Fatalf("first parent must be admitted into an empty pool: %v", err)
	}
	first.Stage = "c"
	running, err := bridge.DispatchCStage(context.Background(), first)
	if err != nil {
		t.Fatalf("dispatch first parent: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("first parent started %d subtasks, want the single tenant slot", len(running))
	}

	second := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-b", TaskID: "task-2", Stage: "b", AttemptNo: 1}
	_, err = bridge.PlanBStage(context.Background(), second, []byte(testDAGPlan))
	if err == nil {
		t.Fatalf("a second parent of the same tenant must not be admitted into a full pool")
	}
	if got := orchestrator.ErrorCode(err); got != orchestrator.CodeSchedulerLimitExceeded {
		t.Fatalf("admission error code = %q, want %q", got, orchestrator.CodeSchedulerLimitExceeded)
	}
	if got := orchestrator.AdmissionReason(err); got != orchestrator.AllocDeniedTenantFull {
		t.Fatalf("admission reason = %q, want %q", got, orchestrator.AllocDeniedTenantFull)
	}
	if details := orchestrator.AdmissionDetails(err); details["queued"] != false {
		t.Fatalf("the bridge rejects instead of queueing, details = %v", details)
	}

	// 越过衰减窗口：记账放行，闸门仍满 → 二次确认必须拒绝，并暴露 gate_available=0。
	clock.Advance(16 * time.Minute)
	_, err = bridge.PlanBStage(context.Background(), second, []byte(testDAGPlan))
	if orchestrator.AdmissionReason(err) != orchestrator.AllocDeniedTenantFull {
		t.Fatalf("a decayed accounting must be re-confirmed against the shared gate, got %v", err)
	}
	details := orchestrator.AdmissionDetails(err)
	if details["gate_available"] != 0 {
		t.Fatalf("admission details = %v, want gate_available=0 (the gate was really re-read)", details)
	}
	if details["slots_decayed"] != 1 || details["slots_used"] != 1 {
		t.Fatalf("admission details = %v, want 1 decayed hold still occupying the gate", details)
	}

	// 换一个租户：池子是按租户隔离的，不该被 tenant-a 的占用连坐。
	other := OrchestratorStageContext{TenantID: "tenant-b", ProjectID: "proj-c", TaskID: "task-3", Stage: "b", AttemptNo: 1}
	if _, err := bridge.PlanBStage(context.Background(), other, []byte(testDAGPlan)); err != nil {
		t.Fatalf("another tenant must have its own pool: %v", err)
	}
}
