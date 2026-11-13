package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-platform/pkg/config"
	"agent-platform/pkg/harnessloop/policy"
	pb "agent-platform/pkg/pb/mcp"
	"agent-platform/services/mcp-service/internal/model"
)

type stubToolPolicy struct {
	result ToolPolicyResult
	seen   ToolPolicyInput
}

func (s *stubToolPolicy) Authorize(_ context.Context, in ToolPolicyInput) ToolPolicyResult {
	s.seen = in
	return s.result
}

type recorderExecutor struct {
	called bool
}

func (r *recorderExecutor) Execute(_ context.Context, _ map[string]interface{}) (string, error) {
	r.called = true
	return "executed", nil
}

func (r *recorderExecutor) ExecuteWithConfig(_ context.Context, _ map[string]interface{}, _ map[string]interface{}) (string, error) {
	r.called = true
	return "executed", nil
}

func TestCallToolDeniedByPolicyNeverExecutes(t *testing.T) {
	svc := NewMCPService(nil, config.LoadDefault())
	rec := &recorderExecutor{}
	svc.tools["capture_tool"] = model.Tool{Name: "capture_tool", PermissionTags: []string{"fs.write"}}
	svc.toolExec["capture_tool"] = rec
	policyStub := &stubToolPolicy{result: ToolPolicyResult{Allowed: false, Code: policy.CodeToolDenied, Reason: "stage denies write"}}
	svc.SetToolPolicy(policyStub)

	toolConfig, _ := json.Marshal(map[string]any{
		"extra": map[string]any{
			"__policy_context__": map[string]any{"stage": "a", "workspace_root": t.TempDir()},
		},
	})
	resp, err := svc.CallTool(context.Background(), &pb.CallToolRequest{
		Name:       "capture_tool",
		Arguments:  `{"path":"out/a.txt"}`,
		ToolConfig: string(toolConfig),
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if resp == nil || !resp.IsError || !strings.Contains(resp.Content, policy.CodeToolDenied) {
		t.Fatalf("response = %+v", resp)
	}
	if rec.called {
		t.Fatal("denied tool must never reach its executor")
	}
	if policyStub.seen.Stage != "a" || policyStub.seen.Tool != "capture_tool" || len(policyStub.seen.Paths) != 1 {
		t.Fatalf("policy input = %+v", policyStub.seen)
	}
}

func TestCallToolAllowedByPolicyExecutes(t *testing.T) {
	svc := NewMCPService(nil, config.LoadDefault())
	rec := &recorderExecutor{}
	svc.tools["capture_tool"] = model.Tool{Name: "capture_tool", PermissionTags: []string{"fs.read"}}
	svc.toolExec["capture_tool"] = rec
	svc.SetToolPolicy(&stubToolPolicy{result: ToolPolicyResult{Allowed: true}})

	resp, err := svc.CallTool(context.Background(), &pb.CallToolRequest{Name: "capture_tool", Arguments: `{}`})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if resp == nil || resp.IsError {
		t.Fatalf("response = %+v", resp)
	}
	if !rec.called {
		t.Fatal("allowed tool must execute")
	}
}

func TestCallToolWithoutPolicyKeepsLegacyPath(t *testing.T) {
	svc := NewMCPService(nil, config.LoadDefault())
	rec := &recorderExecutor{}
	svc.tools["capture_tool"] = model.Tool{Name: "capture_tool", PermissionTags: []string{"fs.write"}}
	svc.toolExec["capture_tool"] = rec

	resp, err := svc.CallTool(context.Background(), &pb.CallToolRequest{Name: "capture_tool", Arguments: `{}`})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if resp == nil || resp.IsError {
		t.Fatalf("response = %+v", resp)
	}
	if !rec.called {
		t.Fatal("disabled policy must keep executing tools")
	}
}

func TestHarnessToolPolicyStageMatrix(t *testing.T) {
	gate := NewHarnessToolPolicy(policy.NewEngine())
	root := t.TempDir()
	ctx := context.Background()

	if res := gate.Authorize(ctx, ToolPolicyInput{
		Tool: "file_write", Stage: "a", WorkspaceRoot: root,
		Paths: []string{"out/a.txt"}, Tags: []string{"fs.write"},
	}); res.Allowed || res.Code != policy.CodeToolDenied {
		t.Fatalf("write in stage a must be denied: %+v", res)
	}

	if res := gate.Authorize(ctx, ToolPolicyInput{
		Tool: "file_write", Stage: "c", WorkspaceRoot: root,
		Paths: []string{"out/a.txt"}, Tags: []string{"fs.write"},
	}); !res.Allowed {
		t.Fatalf("write in stage c must be allowed: %+v", res)
	}

	if res := gate.Authorize(ctx, ToolPolicyInput{
		Tool: "file_write", Stage: "c", WorkspaceRoot: root,
		Paths: []string{"../escape.txt"}, Tags: []string{"fs.write"},
	}); res.Allowed || res.Code != policy.CodePathEscape {
		t.Fatalf("workspace escape must be denied: %+v", res)
	}

	if res := gate.Authorize(ctx, ToolPolicyInput{
		Tool: "web_search", Stage: "c", Tags: []string{"net.read"},
	}); res.Allowed || res.Code != policy.CodeToolDenied {
		t.Fatalf("network tool must be denied: %+v", res)
	}

	if res := gate.Authorize(ctx, ToolPolicyInput{
		Tool: "calculator", Stage: "a", Tags: []string{"compute"},
	}); !res.Allowed {
		t.Fatalf("compute tool must be allowed: %+v", res)
	}

	if res := gate.Authorize(ctx, ToolPolicyInput{Tool: "file_read", Stage: "z"}); res.Allowed || res.Code != policy.CodeUnknownStage {
		t.Fatalf("unknown stage must be denied: %+v", res)
	}

	if res := gate.Authorize(ctx, ToolPolicyInput{Tool: "file_write", Tags: []string{"fs.write"}}); !res.Allowed {
		t.Fatalf("non-stage caller must keep legacy behaviour: %+v", res)
	}
}

func TestPolicyContextFromToolConfig(t *testing.T) {
	pc := policyContextFromToolConfig(`{"extra":{"__policy_context__":{"stage":"c","tenant_id":"t","project_id":"p","task_id":"task","agent_id":"a","workspace_root":"/ws"}}}`)
	if pc.Stage != "c" || pc.TenantID != "t" || pc.ProjectID != "p" || pc.TaskID != "task" || pc.AgentID != "a" || pc.WorkspaceRoot != "/ws" {
		t.Fatalf("nested policy context = %+v", pc)
	}
	top := policyContextFromToolConfig(`{"__policy_context__":{"stage":"b"}}`)
	if top.Stage != "b" {
		t.Fatalf("top-level policy context = %+v", top)
	}
	if bad := policyContextFromToolConfig(`not json`); bad.Stage != "" {
		t.Fatalf("invalid tool config must be empty: %+v", bad)
	}
}
