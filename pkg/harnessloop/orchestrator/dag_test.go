package orchestrator

import (
	"context"
	"strconv"
	"testing"

	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/pkg/harnessloop/subtask"
)

func validNode(id, role string) PlanNode {
	return PlanNode{
		NodeID: id,
		Role:   role,
		InputContract: Contract{
			Type:  "file_set",
			Paths: []string{"src/**"},
		},
		OutputContract: Contract{
			Type:   "artifacts",
			Paths:  []string{"src/**"},
			Verify: []string{"go build ./..."},
		},
		Acceptance: []AcceptanceSpec{{Type: "build_pass", Expr: "go build ./..."}},
	}
}

func validPlan() *Plan {
	n1 := validNode("n1", "coder")
	n2 := validNode("n2", "coder")
	n3 := validNode("n3", "verifier")
	return &Plan{
		DagID: "dag-1",
		Nodes: []PlanNode{n1, n2, n3},
		Edges: []PlanEdge{{From: "n1", To: "n2"}, {From: "n1", To: "n3"}},
		ParallelSets: []ParallelSet{
			{SetID: "p2", Nodes: []string{"n2", "n3"}, FailureStrategy: subtask.StrategyTolerate, TolerateRatio: 0.2},
		},
	}
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", want)
	}
	if got := ErrorCode(err); got != want {
		t.Fatalf("expected error code %s, got %s (%v)", want, got, err)
	}
}

func TestDAGCompletePlanEmitsPlanCreated(t *testing.T) {
	emitter := NewMemoryEmitter()
	planner := NewPlanner(DefaultPlanConfig(), emitter)
	result, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		Plan:         validPlan(),
	})
	if err != nil {
		t.Fatalf("plan should be accepted: %v", err)
	}
	if len(result.Subtasks) != 3 {
		t.Fatalf("expected 3 subtasks, got %d", len(result.Subtasks))
	}
	if result.Subtasks[1].ParallelSetID != "p2" || result.Subtasks[1].FailureStrategy != subtask.StrategyTolerate {
		t.Fatalf("n2 must inherit parallel set p2/tolerate: %+v", result.Subtasks[1])
	}
	if len(result.Subtasks[0].DependsOn) != 0 || len(result.Subtasks[1].DependsOn) != 1 {
		t.Fatalf("dependency expansion is wrong: %+v", result.Subtasks)
	}

	events := emitter.Find(EventSubtaskPlanCreated)
	if len(events) != 1 {
		t.Fatalf("expected exactly one subtask.plan_created event, got %d", len(events))
	}
	payload := events[0].Payload
	if payload["node_count"] != 3 {
		t.Fatalf("event node_count = %v, want 3", payload["node_count"])
	}
	if payload["parallel_set_count"] != 1 {
		t.Fatalf("event parallel_set_count = %v, want 1", payload["parallel_set_count"])
	}
	if events[0].ParentTaskID != "task-1" || events[0].DagID != "dag-1" {
		t.Fatalf("event correlation fields are missing: %+v", events[0])
	}
}

func TestDAGRejectsCycle(t *testing.T) {
	plan := validPlan()
	plan.Edges = append(plan.Edges, PlanEdge{From: "n2", To: "n1"})
	assertCode(t, plan.Validate(DefaultPlanConfig()), CodeDAGCycle)
}

func TestDAGRejectsUnknownEdgeNode(t *testing.T) {
	plan := validPlan()
	plan.Edges = append(plan.Edges, PlanEdge{From: "n1", To: "missing"})
	assertCode(t, plan.Validate(DefaultPlanConfig()), CodeDAGCycle)
}

func TestDAGRejectsParallelConflict(t *testing.T) {
	plan := validPlan()
	plan.ParallelSets = []ParallelSet{
		{SetID: "p1", Nodes: []string{"n1", "n2"}, FailureStrategy: subtask.StrategyFailFast},
	}
	assertCode(t, plan.Validate(DefaultPlanConfig()), CodeDAGParallelConflict)
}

func TestDAGRejectsTooManyNodes(t *testing.T) {
	plan := &Plan{DagID: "dag-big"}
	for i := 0; i < 21; i++ {
		plan.Nodes = append(plan.Nodes, validNode("n"+strconv.Itoa(i), "coder"))
	}
	assertCode(t, plan.Validate(DefaultPlanConfig()), CodeSubtaskLimitExceeded)
}

func TestDAGRejectsThirdLevel(t *testing.T) {
	root := validNode("n1", "planner")
	child := validNode("c1", "coder")
	grand := validNode("g1", "coder")
	child.Children = []PlanNode{grand}
	root.Children = []PlanNode{child}
	plan := &Plan{DagID: "dag-depth", Nodes: []PlanNode{root}}
	assertCode(t, plan.Validate(DefaultPlanConfig()), CodeSubtaskDepthExceeded)
}

func TestDAGAllowsSecondLevel(t *testing.T) {
	root := validNode("n1", "planner")
	child := validNode("c1", "coder")
	root.Children = []PlanNode{child}
	plan := &Plan{DagID: "dag-depth-2", Nodes: []PlanNode{root}}
	if err := plan.Validate(DefaultPlanConfig()); err != nil {
		t.Fatalf("two-level plan should pass: %v", err)
	}
	subtasks, err := plan.BuildSubtasks("task-1", "tenant-1", "project-1")
	if err != nil {
		t.Fatalf("build subtasks: %v", err)
	}
	if len(subtasks) != 2 || subtasks[0].Depth != 1 || subtasks[1].Depth != 2 {
		t.Fatalf("depth expansion is wrong: %+v", subtasks)
	}
}

func TestDAGRejectsNotSplittable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PlanNode)
	}{
		{"approval point", func(n *PlanNode) { n.RequiresApproval = true }},
		{"consistency group", func(n *PlanNode) { n.OutputContract.ConsistencyGroup = "iface-a" }},
		{"explicit non-splittable", func(n *PlanNode) { flag := false; n.Splittable = &flag }},
		{"single file deterministic", func(n *PlanNode) {
			n.OutputContract.Paths = []string{"src/a/x.go"}
			n.Acceptance = []AcceptanceSpec{{Type: "build_pass", Expr: "go build ./src/a"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := validNode("n1", "coder")
			tc.mutate(&node)
			plan := &Plan{DagID: "dag-ns", Nodes: []PlanNode{node}}
			assertCode(t, plan.Validate(DefaultPlanConfig()), CodeSubtaskNotSplittable)
		})
	}
}

func TestDAGRejectsMissingPropagation(t *testing.T) {
	plan := validPlan()
	plan.ParallelSets = []ParallelSet{{SetID: "p1", Nodes: []string{"n2", "n3"}}}
	assertCode(t, plan.Validate(DefaultPlanConfig()), CodePropagationUndeclared)
}

func TestDAGRejectsMissingEdges(t *testing.T) {
	plan := validPlan()
	plan.Edges = nil
	plan.ParallelSets = nil
	assertCode(t, plan.Validate(DefaultPlanConfig()), CodeDAGIncomplete)
}

func TestDAGRejectsIncompleteContract(t *testing.T) {
	node := validNode("n1", "coder")
	node.OutputContract.Paths = nil
	plan := &Plan{DagID: "dag-contract", Nodes: []PlanNode{node}}
	assertCode(t, plan.Validate(DefaultPlanConfig()), CodeContractIncomplete)
}

func TestDAGRejectsContractOutsideSandbox(t *testing.T) {
	node := validNode("n1", "coder")
	node.OutputContract.Paths = []string{"../outside/x.go"}
	plan := &Plan{DagID: "dag-sandbox", Nodes: []PlanNode{node}}
	assertCode(t, plan.Validate(DefaultPlanConfig()), CodeContractIncomplete)
}

func TestDAGRejectsUndeclaredRole(t *testing.T) {
	node := validNode("n1", "")
	plan := &Plan{DagID: "dag-role", Nodes: []PlanNode{node}}
	assertCode(t, plan.Validate(DefaultPlanConfig()), subtask.CodeAgentRoleUndeclared)
}

type fakeCatalog struct {
	roles []string
	tools []string
}

func (f fakeCatalog) AgentRoles(context.Context, string) ([]string, error) { return f.roles, nil }
func (f fakeCatalog) AgentTools(context.Context, string) ([]string, error) { return f.tools, nil }

type fakeTags struct{}

func (fakeTags) ToolPermissionTags(_ context.Context, tool string) ([]string, error) {
	if tool == "read_file" || tool == "write_file" {
		return []string{policy.TagRead}, nil
	}
	return nil, nil
}

func TestDAGReusesRoleToolRegistry(t *testing.T) {
	cfg := DefaultPlanConfig()
	cfg.Roles = subtask.NewRegistry(fakeCatalog{roles: []string{"coder"}, tools: []string{"read_file", "write_file"}}, policy.DefaultMatrix(), fakeTags{})

	planner := NewPlanner(cfg, nil)
	allowed := validNode("n1", "coder")
	allowed.AgentID = "agent-1"
	allowed.Tools = []string{"read_file"}
	if _, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: "task-1", TenantID: "tenant-1", ProjectID: "project-1",
		Stage: "b", Plan: &Plan{DagID: "dag-role-ok", Nodes: []PlanNode{allowed}},
	}); err != nil {
		t.Fatalf("read_file should be allowed in stage b: %v", err)
	}

	denied := validNode("n1", "coder")
	denied.AgentID = "agent-1"
	denied.Tools = []string{"write_file"}
	_, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: "task-1", TenantID: "tenant-1", ProjectID: "project-1",
		Stage: "b", Plan: &Plan{DagID: "dag-role-denied", Nodes: []PlanNode{denied}},
	})
	assertCode(t, err, subtask.CodeToolNotPermitted)

	badRole := validNode("n1", "verifier")
	badRole.AgentID = "agent-1"
	_, err = planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: "task-1", TenantID: "tenant-1", ProjectID: "project-1",
		Stage: "b", Plan: &Plan{DagID: "dag-role-bad", Nodes: []PlanNode{badRole}},
	})
	assertCode(t, err, subtask.CodeAgentRoleUndeclared)
}

func TestDAGSuggestedLevels(t *testing.T) {
	levels, err := validPlan().SuggestedLevels()
	if err != nil {
		t.Fatalf("levels: %v", err)
	}
	if levels["n1"] != 1 || levels["n2"] != 2 || levels["n3"] != 2 {
		t.Fatalf("unexpected levels: %v", levels)
	}
}

func TestDAGParsePlanRejectsEmpty(t *testing.T) {
	_, err := ParsePlan([]byte("  "))
	assertCode(t, err, CodeDAGIncomplete)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
