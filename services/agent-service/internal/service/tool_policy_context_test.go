package service

import (
	"context"
	"testing"

	"agent-platform/pkg/agent"
)

func TestToolPolicyContextFromVars(t *testing.T) {
	pc := toolPolicyContextFromVars(map[string]any{
		"__stage__":          "c",
		"__project_id__":     "proj-a",
		"__task_id__":        "task-1",
		"__agent_id__":       "agent-1",
		"__workspace_root__": "/ws",
		"__tenant_id__":      "tenant-var",
	}, "tenant-req")
	if pc.Stage != "c" || pc.ProjectID != "proj-a" || pc.TaskID != "task-1" ||
		pc.AgentID != "agent-1" || pc.WorkspaceRoot != "/ws" || pc.TenantID != "tenant-req" {
		t.Fatalf("policy context = %+v", pc)
	}
	if !pc.present() {
		t.Fatal("stage context must be present")
	}
	if empty := toolPolicyContextFromVars(map[string]any{"__stage_mode__": "true"}, "t"); empty.present() {
		t.Fatalf("no stage means no policy context: %+v", empty)
	}
}

func TestWithToolPolicyContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := toolPolicyContextFrom(ctx); ok {
		t.Fatal("empty context must not carry a policy context")
	}
	pc := toolPolicyContext{Stage: "c", TenantID: "t", TaskID: "task-1"}
	got, ok := toolPolicyContextFrom(withToolPolicyContext(ctx, pc))
	if !ok || got.Stage != "c" || got.TaskID != "task-1" {
		t.Fatalf("round trip = %+v ok=%v", got, ok)
	}
}

func TestMergeToolPolicyContextClonesConfig(t *testing.T) {
	original := &agent.ToolSpecificConfig{
		APIKey: "tool-key",
		Extra:  map[string]any{"session_id": "s-1"},
	}
	pc := toolPolicyContext{Stage: "c", TenantID: "t", ProjectID: "p", TaskID: "task-1", AgentID: "a", WorkspaceRoot: "/ws"}
	merged := mergeToolPolicyContext(original, pc)

	if merged == original {
		t.Fatal("merge must return a copy, not mutate the agent config")
	}
	if merged.APIKey != "tool-key" || merged.Extra["session_id"] != "s-1" {
		t.Fatalf("merged config lost fields: %+v", merged)
	}
	raw, ok := merged.Extra["__policy_context__"].(map[string]any)
	if !ok || raw["stage"] != "c" || raw["workspace_root"] != "/ws" {
		t.Fatalf("policy context missing: %+v", merged.Extra)
	}
	if _, leaked := original.Extra["__policy_context__"]; leaked {
		t.Fatal("original config must stay untouched for concurrent runs")
	}

	nilConfig := mergeToolPolicyContext(nil, pc)
	if nilConfig == nil || nilConfig.Extra["__policy_context__"] == nil {
		t.Fatalf("nil config must still carry policy context: %+v", nilConfig)
	}
}
