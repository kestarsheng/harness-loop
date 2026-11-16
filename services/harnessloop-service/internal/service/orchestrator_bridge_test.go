package service

import (
	"context"
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
