package orchestrator

import (
	"context"
	"testing"

	"agent-platform/pkg/harnessloop/subtask"
)

func TestPlanCreatedReportsSourceAndDegradedReason(t *testing.T) {
	plan := &Plan{
		DagID: "dag-src",
		Nodes: []PlanNode{{
			NodeID:         "n1",
			Role:           "coder",
			InputContract:  Contract{Type: "workspace", Paths: []string{"."}},
			OutputContract: Contract{Type: "files", Paths: []string{"."}},
		}},
		ParallelSets: []ParallelSet{{SetID: "p1", Nodes: []string{"n1"}, FailureStrategy: subtask.StrategyTolerate, TolerateRatio: 0.5}},
	}

	t.Run("model", func(t *testing.T) {
		emitter := NewMemoryEmitter()
		planner := NewPlanner(PlanConfig{}, emitter)
		if _, err := planner.Plan(context.Background(), PlanRequest{
			ParentTaskID: "task-src", TenantID: "t", ProjectID: "p", Plan: plan,
		}); err != nil {
			t.Fatalf("plan: %v", err)
		}
		events := emitter.Find(EventSubtaskPlanCreated)
		if len(events) != 1 {
			t.Fatalf("plan_created = %d", len(events))
		}
		if events[0].Payload["source"] != PlanSourceModel {
			t.Fatalf("source = %v, want %q", events[0].Payload["source"], PlanSourceModel)
		}
		if _, ok := events[0].Payload["degraded_reason"]; ok {
			t.Fatalf("a model plan must not carry degraded_reason: %v", events[0].Payload)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		emitter := NewMemoryEmitter()
		planner := NewPlanner(PlanConfig{}, emitter)
		if _, err := planner.Plan(context.Background(), PlanRequest{
			ParentTaskID: "task-src", TenantID: "t", ProjectID: "p", Plan: plan,
			PlanSource: PlanSourceFallback, DegradedReason: "empty_plan_output",
		}); err != nil {
			t.Fatalf("plan: %v", err)
		}
		payload := emitter.Find(EventSubtaskPlanCreated)[0].Payload
		if payload["source"] != PlanSourceFallback {
			t.Fatalf("source = %v, want %q", payload["source"], PlanSourceFallback)
		}
		if payload["degraded_reason"] != "empty_plan_output" {
			t.Fatalf("degraded_reason = %v", payload["degraded_reason"])
		}
	})
}
