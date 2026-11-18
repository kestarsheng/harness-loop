package policy

import (
	"context"
	"testing"
)

// exec 白名单不能抢在 Classify 之前一票否决：高危但留在工作区内的命令
// 必须走到审批，而不是直接 E_EXEC_DENIED。
func TestResolverHighRiskExecReachesApproval(t *testing.T) {
	r := NewResolver(nil)
	root := t.TempDir()

	cases := []struct {
		name     string
		command  []string
		wantKind HighRiskKind
	}{
		{name: "delete command", command: []string{"rm", "-f", "scratch/probe.txt"}, wantKind: HighRiskDelete},
		{name: "deploy command", command: []string{"kubectl", "apply", "-f", "deploy.yaml"}, wantKind: HighRiskDeploy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := r.Decide(context.Background(), Request{
				Tool: "run_command", Stage: "c", WorkspaceRoot: root,
				Args: ToolArgs{Command: tc.command},
			})
			if d.Allowed {
				t.Fatalf("high-risk exec must not be allowed inline: %+v", d)
			}
			if d.Effect != EffectRequireApproval || !d.RequiresApproval {
				t.Fatalf("effect = %q requires_approval=%v, want require_approval (%+v)", d.Effect, d.RequiresApproval, d)
			}
			if d.Code != CodeApprovalRequired {
				t.Fatalf("code = %s, want %s", d.Code, CodeApprovalRequired)
			}
			if d.Layer != LayerSystem {
				t.Fatalf("layer = %s, want system (exec allowlist is a system constraint)", d.Layer)
			}
			if want := "policy.approval.high_risk." + string(tc.wantKind); d.RuleID != want {
				t.Fatalf("rule = %s, want %s", d.RuleID, want)
			}
			if d.PolicySource != "system:engine" {
				t.Fatalf("policy_source = %q", d.PolicySource)
			}
		})
	}
}

// 审批只对「工作区内还能收场」的命令开放：绝对路径 / 上跳 / 元字符仍硬拒。
func TestResolverEscapingCommandStaysDenied(t *testing.T) {
	r := NewResolver(nil)
	root := t.TempDir()

	cases := [][]string{
		{"rm", "-rf", "/"},
		{"rm", "-rf", "../outside"},
		{"deploy.sh", "--target=../../prod"},
		{"rm", "-f", "a;b"},
	}
	for _, command := range cases {
		t.Run(command[0], func(t *testing.T) {
			d := r.Decide(context.Background(), Request{
				Tool: "run_command", Stage: "c", WorkspaceRoot: root,
				Args: ToolArgs{Command: command},
			})
			if d.Allowed || d.Effect != EffectDeny {
				t.Fatalf("escaping command must be denied: %+v", d)
			}
			if d.Code != CodeExecDenied {
				t.Fatalf("code = %s, want %s (%+v)", d.Code, CodeExecDenied, d)
			}
		})
	}
}

// 不在白名单又不属于高危类的命令，仍然按原来的 E_EXEC_DENIED 拒掉。
func TestResolverLowRiskUnlistedCommandStaysDenied(t *testing.T) {
	r := NewResolver(nil)
	d := r.Decide(context.Background(), Request{
		Tool: "run_command", Stage: "c", WorkspaceRoot: t.TempDir(),
		Args: ToolArgs{Command: []string{"curl", "example.com"}},
	})
	if d.Allowed || d.Code != CodeExecDenied {
		t.Fatalf("low-risk unlisted command must stay E_EXEC_DENIED: %+v", d)
	}
}

// 租户层显式拒绝必须压过「高危转审批」：审批不能绕过 tenant deny。
func TestResolverTenantDenyBeatsExecApproval(t *testing.T) {
	r := NewResolver(nil)
	r.SetLayerStore(NewLayerStore())
	d := r.Decide(context.Background(), Request{
		Tool: "run_command", Stage: "c", WorkspaceRoot: t.TempDir(),
		Args:    ToolArgs{Command: []string{"rm", "-f", "scratch/probe.txt"}},
		Tenant:  &PolicySet{DenyTools: []string{"run_command"}},
		TenantID: "t-1",
	})
	if d.Allowed || d.Effect != EffectDeny {
		t.Fatalf("tenant deny must win over exec approval: %+v", d)
	}
	if d.Layer != LayerTenant || d.RuleID != "tenant.deny" || d.PolicySource != "tenant:inline" {
		t.Fatalf("deny must be attributed to the tenant layer: %+v", d)
	}
}

// 分类逻辑只有一份：gate.Classify 与 resolver 用的是同一套判定。
func TestClassifySharedByGateAndResolver(t *testing.T) {
	gate := NewApprovalGate(DefaultApprovalConfig(), nil)
	args := ToolArgs{Command: []string{"rm", "-f", "scratch/probe.txt"}}
	gateKind, gateRule := gate.Classify("run_command", []string{TagExec}, args)

	resolverKind, resolverRule := NewHighRiskClassifier(DefaultApprovalConfig()).Classify("run_command", []string{TagExec}, args)
	if gateKind != resolverKind || gateRule != resolverRule {
		t.Fatalf("gate/resolver classification diverged: %q/%q vs %q/%q", gateKind, gateRule, resolverKind, resolverRule)
	}
	if gateKind != HighRiskDelete {
		t.Fatalf("kind = %q, want delete", gateKind)
	}
}

// write 标签工具的删除判定不能因为这次改动被放宽成「任何带命令的都算」。
func TestClassifyDeleteNeedsWriteTagOrCommand(t *testing.T) {
	c := NewHighRiskClassifier(DefaultApprovalConfig())
	if kind, _ := c.Classify("write_file", []string{TagWrite}, ToolArgs{Paths: []string{"a.go"}}); kind != HighRiskNone {
		t.Fatalf("plain write must not be delete class: %q", kind)
	}
	if kind, _ := c.Classify("delete", []string{TagWrite}, ToolArgs{Paths: []string{"a.go"}}); kind != HighRiskDelete {
		t.Fatalf("delete tool must be delete class: %q", kind)
	}
	if kind, _ := c.Classify("grep", []string{TagRead}, ToolArgs{Paths: []string{"a.go"}}); kind != HighRiskNone {
		t.Fatalf("read tool must not be high risk: %q", kind)
	}
}
