package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/services/harnessloop-service/internal/repository"
)

// noTicketGate 模拟「闸门认为这次调用不属于它能扣住的高危类」：Begin 返回
// (nil, nil)。策略层要求审批而闸门不发工单时，调用绝不能放行。
type noTicketGate struct{}

func (noTicketGate) Begin(context.Context, policy.Request, []string) (*policy.ApprovalTicket, error) {
	return nil, nil
}

func (noTicketGate) Await(context.Context, *policy.ApprovalTicket) policy.ApprovalOutcome {
	return policy.ApprovalOutcome{Allowed: true, Reason: "no ticket"}
}

func (noTicketGate) Authorize(context.Context, policy.Request, []string) policy.ApprovalOutcome {
	return policy.ApprovalOutcome{Allowed: true, Reason: "no ticket"}
}

func TestPolicyBridgeApprovalWithoutTicketIsNotAllowed(t *testing.T) {
	engine := policy.NewEngine()
	bridge := NewPolicyBridge(true, engine, nil, nil, noTicketGate{})
	call := policyCall("c", "read_file", t.TempDir())
	call.TaskPolicy = &policy.PolicySet{Rules: []policy.Rule{{
		ID: "task.read.approval", Tool: "read_file", Effect: policy.EffectRequireApproval, RequiresApproval: true,
	}}}

	err := bridge.AuthorizeTool(context.Background(), call)
	if err == nil {
		t.Fatal("require_approval without a ticket released the call: approval became an alias for allow")
	}
	if !strings.Contains(err.Error(), policy.CodeApprovalRequired) {
		t.Fatalf("err = %v, want code %s", err, policy.CodeApprovalRequired)
	}
}

// 装配必须由桥自己做：cmd/main.go 不额外传参，配置也应生效。
func TestPolicyBridgeLoadsLayerConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy_layers.yaml")
	body := `
version: 1
tenants:
  tenant-cfg:
    deny_tools: [write_file]
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("HARNESSLOOP_POLICY_LAYERS_FILE", path)

	writer := &auditRecorder{}
	bridge := NewPolicyBridge(true, policy.NewEngine(), writer, nil, nil)

	allowed := bridge.Preflight(context.Background(), policyCall("c", "read_file", t.TempDir()))
	if !allowed.Allowed {
		t.Fatalf("read_file must stay allowed: %+v", allowed)
	}

	denied := ToolCallInput{
		TenantID: "tenant-cfg", ProjectID: "proj-a", TaskID: "task-cfg",
		Stage: "c", WorkspaceRoot: t.TempDir(), Tool: "write_file", Paths: []string{"a.go"},
	}
	res := bridge.Preflight(context.Background(), denied)
	if res.Allowed || res.Layer != policy.LayerTenant || res.RuleID != "tenant.deny" {
		t.Fatalf("tenant layer from config must deny: %+v", res)
	}

	var found bool
	for _, rec := range writer.records {
		if rec.RuleID == "tenant.deny" {
			found = true
			if rec.PolicySource != "tenant:config" {
				t.Fatalf("audit policy_source = %q, want tenant:config", rec.PolicySource)
			}
			if got := rec.ArgsSummary["policy_source"]; got != "tenant:config" {
				t.Fatalf("audit payload policy_source = %v, want tenant:config", got)
			}
		}
	}
	if !found {
		t.Fatalf("no audit record for the tenant deny: %+v", writer.records)
	}
}

// runtime 从任务快照注入任务层策略集，走的是默认注入目标（生产路径）。
func TestRuntimeInjectsTaskPolicyFromSnapshot(t *testing.T) {
	bridge := NewPolicyBridge(true, policy.NewEngine(), nil, nil, nil)
	snapshot, err := policy.SnapshotWithTaskPolicy([]byte(`{"mode":"enforce"}`), &policy.PolicySet{
		DenyTools: []string{"read_file"},
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	rt := &Runtime{}
	rt.injectTaskPolicy(&repository.TaskRun{TaskID: "task-snap", PolicySnapshot: snapshot})

	res := bridge.Preflight(context.Background(), ToolCallInput{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-snap",
		Stage: "c", WorkspaceRoot: t.TempDir(), Tool: "read_file", Paths: []string{"a.go"},
	})
	if res.Allowed || res.Layer != policy.LayerTask || res.RuleID != "task.deny" {
		t.Fatalf("task layer from snapshot must deny: %+v", res)
	}
}

// 没有 tool_policy 的快照不能让任务层「凭空」生效。
func TestRuntimeIgnoresSnapshotWithoutToolPolicy(t *testing.T) {
	bridge := NewPolicyBridge(true, policy.NewEngine(), nil, nil, nil)
	rt := &Runtime{}
	rt.injectTaskPolicy(&repository.TaskRun{TaskID: "task-plain", PolicySnapshot: []byte(`{"mode":"enforce"}`)})

	res := bridge.Preflight(context.Background(), ToolCallInput{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-plain",
		Stage: "c", WorkspaceRoot: t.TempDir(), Tool: "read_file", Paths: []string{"a.go"},
	})
	if !res.Allowed {
		t.Fatalf("plain snapshot must not create a task layer: %+v", res)
	}
	if strings.HasPrefix(res.RuleID, "task.") {
		t.Fatalf("decision attributed to a task rule that was never configured: %+v", res)
	}
}

// 真实任务里 agent 把命令当整串发过来（"rm -f scratch/probe.txt"）。策略层必须
// 按 MCP 的同一套规则拆成 argv，否则白名单判「可执行名带斜杠」、Classify 判
// 「没有 rm」——审批永远到不了。
func TestToolCallNormalizeSplitsCommandString(t *testing.T) {
	bridge := NewPolicyBridge(true, policy.NewEngine(), nil, nil, nil)
	in := ToolCallInput{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-cmd",
		Stage: "c", WorkspaceRoot: t.TempDir(), Tool: "run_command",
		Arguments: []byte(`{"command":"rm -f scratch/probe.txt"}`),
	}
	normalized := in.normalize()
	if got := strings.Join(normalized.Command, "|"); got != "rm|-f|scratch/probe.txt" {
		t.Fatalf("command argv = %q, want rm|-f|scratch/probe.txt", got)
	}

	res := bridge.Preflight(context.Background(), in)
	if res.RequiresApproval != true || res.Allowed {
		t.Fatalf("high-risk command must reach approval, got %+v", res)
	}
	// 没有闸门时是 E_APPROVAL_UNAVAILABLE（挂起不放行），重点是「已经走到审批」，
	// 不再是 sandbox.exec 的 E_EXEC_DENIED。
	if res.Code != policy.CodeApprovalRequired && res.Code != policy.CodeApprovalUnavailable {
		t.Fatalf("code = %s, want an approval code (not E_EXEC_DENIED)", res.Code)
	}
	if res.RuleID != "policy.approval.high_risk.delete" {
		t.Fatalf("rule = %s, want policy.approval.high_risk.delete", res.RuleID)
	}
}

// 已经是数组形态的 argv 不能被二次拆分。
func TestToolCallNormalizeKeepsArgvArray(t *testing.T) {
	in := ToolCallInput{Arguments: []byte(`{"command":["go","test","./..."]}`)}
	normalized := in.normalize()
	if got := strings.Join(normalized.Command, "|"); got != "go|test|./..." {
		t.Fatalf("argv array = %q", got)
	}
}
