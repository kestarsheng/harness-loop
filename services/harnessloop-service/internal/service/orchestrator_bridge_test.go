package service

import (
	"context"
	"testing"

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
