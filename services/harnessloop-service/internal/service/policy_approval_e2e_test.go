package service

// T-P2-31 审批可达 + 四层装配真实验证（e2e）
//
// 与 policy_e2e_test.go 同一套真实链路（真任务行 / 真 runtime / 真 stage runner /
// 真 bridge / 真 PG 审计），这里补的是本期两条主线：
//
//  1. require_approval 在真实任务里可达——策略集不再由测试手工塞进 ToolCallInput：
//     tenant/role 来自仓库里的 pkg/config/harness_loop/policy_layers.yaml，
//     task 来自 task_runs.policy_snapshot，由 runtime 在开跑前注入。
//  2. 四层命中可辨——每条审计都带 policy_source（tenant:config / role:config /
//     task:snapshot / system:engine），能回答「这条决策到底哪一层说的」。
//
// 需要 HARNESS_TEST_PG_DSN；未设置时 t.Skip，不假装通过。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentapproval "agent-platform/pkg/agent/approval"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/services/harnessloop-service/internal/repository"
)

const layersTenant = "tenant-policy-layers"

// taskPolicySnapshot 造一份带任务级工具策略集的 policy_snapshot（存储侧来源）。
func taskPolicySnapshot(t *testing.T, set *policy.PolicySet) []byte {
	t.Helper()
	raw, err := policy.SnapshotWithTaskPolicy([]byte(`{"mode":"enforce"}`), set)
	if err != nil {
		t.Fatalf("build task policy snapshot: %v", err)
	}
	return raw
}

// pendingResolver 在后台把某个任务产生的审批工单按指定决策处理掉。
//
// 用后台轮询而不是「先跑完再批」：AuthorizeTool 是阻塞式的，工单不批任务就一直
// 停在阶段里，只能边跑边批。
type pendingResolver struct {
	mu       sync.Mutex
	resolved int
	stop     chan struct{}
	done     chan struct{}
}

func (p *pendingResolver) Stop() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
}

func (p *pendingResolver) Resolved() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolved
}

func (e *policyE2EEnv) resolvePending(taskID string, decision agentapproval.ApprovalStatus, reason string) *pendingResolver {
	p := &pendingResolver{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
			}
			for _, req := range e.manager.GetPendingRequests("") {
				if req.SessionID != taskID {
					continue
				}
				if err := e.manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
					RequestID: req.ID,
					Decision:  decision,
					UserID:    "auditor",
					Reason:    reason,
				}); err != nil {
					continue
				}
				p.mu.Lock()
				p.resolved++
				p.mu.Unlock()
			}
		}
	}()
	return p
}

// 真实任务：高危命令（不在 exec 白名单）→ require_approval → 工单 → 批准放行。
func TestPolicyApprovalGrantedOnRealTask(t *testing.T) {
	e := newPolicyE2E(t, map[string][]scriptedCall{
		"c": {{Tool: "run_command", Args: `{"command":["rm","-f","scratch/probe.txt"]}`}},
	})
	snapshot := taskPolicySnapshot(t, &policy.PolicySet{Rules: []policy.Rule{{
		ID: "task.approval.exec", Tool: "run_command", Stages: []string{"c"},
		Effect: policy.EffectRequireApproval, RequiresApproval: true,
	}}})

	taskID := fmt.Sprintf("task-approval-ok-%d", time.Now().UnixNano())
	resolver := e.resolvePending(taskID, agentapproval.StatusApproved, "")
	status, runErr := e.runTaskWith(t, runOptions{taskID: taskID, snapshot: snapshot}, policyWorkspace(t))
	resolver.Stop()

	if runErr != nil {
		t.Fatalf("approved task must not fail: %v", runErr)
	}
	if status != string(repository.TaskCompleted) {
		t.Fatalf("approved task status = %s, want completed", status)
	}
	if resolver.Resolved() != 1 {
		t.Fatalf("approval tickets resolved = %d, want 1 (require_approval must reach the gate)", resolver.Resolved())
	}

	rows := e.auditRows(t, taskID)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 (the gated tool call): %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Decision != string(policy.EffectRequireApproval) {
		t.Fatalf("audit decision = %s, want require_approval", row.Decision)
	}
	if row.RuleID != "task.approval.exec" {
		t.Fatalf("audit rule = %s, want task.approval.exec", row.RuleID)
	}
	if row.Layer != string(policy.LayerTask) {
		t.Fatalf("audit layer = %s, want task", row.Layer)
	}
	if row.PolicySource != "task:snapshot" {
		t.Fatalf("audit policy_source = %q, want task:snapshot", row.PolicySource)
	}
	if row.Code != policy.CodeApprovalRequired {
		t.Fatalf("audit code = %s, want %s", row.Code, policy.CodeApprovalRequired)
	}
	t.Logf("approval granted: task=%s audit=%s rule=%s layer=%s source=%s tickets=%d",
		taskID, row.AuditID, row.RuleID, row.Layer, row.PolicySource, resolver.Resolved())

	// 事件面与审计面同 rule_id：审批请求必须能在事件流里被人看见。
	requested := e.waitForEvent(t, taskID, event.ApprovalRequested)
	if requested == nil {
		t.Fatalf("no approval.requested event for task %s", taskID)
	}
	if !strings.Contains(string(requested.Payload), "task.approval.exec") {
		t.Fatalf("approval.requested payload lacks rule id: %s", requested.Payload)
	}
}

// 真实任务：同样的调用被驳回 → E_APPROVAL_REJECTED，任务不得完成。
func TestPolicyApprovalRejectedOnRealTask(t *testing.T) {
	e := newPolicyE2E(t, map[string][]scriptedCall{
		"c": {{Tool: "run_command", Args: `{"command":["rm","-f","scratch/probe.txt"]}`}},
	})
	snapshot := taskPolicySnapshot(t, &policy.PolicySet{Rules: []policy.Rule{{
		ID: "task.approval.exec", Tool: "run_command", Stages: []string{"c"},
		Effect: policy.EffectRequireApproval, RequiresApproval: true,
	}}})

	taskID := fmt.Sprintf("task-approval-no-%d", time.Now().UnixNano())
	resolver := e.resolvePending(taskID, agentapproval.StatusRejected, "not this time")
	status, runErr := e.runTaskWith(t, runOptions{taskID: taskID, snapshot: snapshot}, policyWorkspace(t))
	resolver.Stop()

	if status == string(repository.TaskCompleted) {
		t.Fatalf("rejected task completed: the gate let the call through")
	}
	var reason string
	if e.lastResult != nil {
		reason = e.lastResult.LastError
	}
	if runErr != nil {
		reason += " | " + runErr.Error()
	}
	if !strings.Contains(reason, policy.CodeApprovalRejected) {
		t.Fatalf("reject reason = %q, want code %s", reason, policy.CodeApprovalRejected)
	}

	rows := e.auditRows(t, taskID)
	if len(rows) != 1 || rows[0].Decision != string(policy.EffectRequireApproval) {
		t.Fatalf("reject-case audit = %+v, want 1 require_approval row", rows)
	}
	if rows[0].PolicySource != "task:snapshot" {
		t.Fatalf("policy_source = %q, want task:snapshot", rows[0].PolicySource)
	}
	t.Logf("approval rejected: task=%s err=%v audit=%s source=%s", taskID, runErr, rows[0].AuditID, rows[0].PolicySource)
}

// 四层命中：同一进程、同一套生产链路上，四个层各自都能被判出来并且来源可辨。
func TestPolicyFourLayersDistinguishable(t *testing.T) {
	cases := []struct {
		name       string
		scripts    map[string][]scriptedCall
		opts       runOptions
		wantLayer  policy.Layer
		wantRule   string
		wantSource string
		wantCode   string
	}{
		{
			name:       "tenant layer denies from config",
			scripts:    map[string][]scriptedCall{"c": {{Tool: "write_file", Args: `{"path":"notes.txt"}`}}},
			wantLayer:  policy.LayerTenant,
			wantRule:   "tenant.deny",
			wantSource: "tenant:config",
			wantCode:   policy.CodeToolDenied,
		},
		{
			name:       "role layer denies from config",
			scripts:    map[string][]scriptedCall{"b": {{Tool: "list_dir", Args: `{"path":"docs"}`}}},
			opts:       runOptions{entryAgent: "policy-probe-agent"},
			wantLayer:  policy.LayerRole,
			wantRule:   "role.deny",
			wantSource: "role:config",
			wantCode:   policy.CodeToolDenied,
		},
		{
			name:       "task layer denies from snapshot",
			scripts:    map[string][]scriptedCall{"a": {{Tool: "read_file", Args: `{"path":"docs/requirements.md"}`}}},
			opts:       runOptions{snapshot: nil}, // 下面按用例填
			wantLayer:  policy.LayerTask,
			wantRule:   "task.deny",
			wantSource: "task:snapshot",
			wantCode:   policy.CodeToolDenied,
		},
		{
			name:       "system layer denies unregistered tool",
			scripts:    map[string][]scriptedCall{"a": {{Tool: "shell_exec", Args: `{"command":["whoami"]}`}}},
			wantLayer:  policy.LayerSystem,
			wantRule:   "registry.unknown",
			wantSource: "system:engine",
			wantCode:   policy.CodeUnknownTool,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPolicyE2E(t, tc.scripts)
			opts := tc.opts
			opts.taskID = fmt.Sprintf("task-layers-%s-%d", strings.ReplaceAll(tc.name, " ", "-"), time.Now().UnixNano())
			opts.tenantID = layersTenant
			if tc.wantLayer == policy.LayerTask {
				opts.snapshot = taskPolicySnapshot(t, &policy.PolicySet{DenyTools: []string{"read_file"}})
			}

			status, _ := e.runTaskWith(t, opts, policyWorkspace(t))
			if status == string(repository.TaskCompleted) {
				t.Fatalf("denied tool call still completed the task")
			}

			rows := e.auditRowsIn(t, layersTenant, opts.taskID)
			if len(rows) != 1 {
				t.Fatalf("audit rows = %d, want 1: %+v", len(rows), rows)
			}
			row := rows[0]
			if row.Decision != string(policy.EffectDeny) {
				t.Fatalf("decision = %s, want deny", row.Decision)
			}
			if row.Layer != string(tc.wantLayer) {
				t.Fatalf("layer = %s, want %s (rule=%s)", row.Layer, tc.wantLayer, row.RuleID)
			}
			if row.RuleID != tc.wantRule {
				t.Fatalf("rule = %s, want %s", row.RuleID, tc.wantRule)
			}
			if row.PolicySource != tc.wantSource {
				t.Fatalf("policy_source = %q, want %q", row.PolicySource, tc.wantSource)
			}
			if row.Code != tc.wantCode {
				t.Fatalf("code = %s, want %s", row.Code, tc.wantCode)
			}
			t.Logf("layer hit: layer=%s rule=%s source=%s code=%s audit=%s",
				row.Layer, row.RuleID, row.PolicySource, row.Code, row.AuditID)
		})
	}
}

// 高危命令在真实任务里的审批来源：exec 白名单不再抢在 Classify 之前拒掉它，
// 决策落到 system 层但 effect=require_approval，并且真能建出工单。
func TestPolicyHighRiskExecReachesApprovalOnRealTask(t *testing.T) {
	e := newPolicyE2E(t, map[string][]scriptedCall{
		"c": {{Tool: "run_command", Args: `{"command":["rm","-rf","scratch/out"]}`}},
	})
	taskID := fmt.Sprintf("task-exec-approval-%d", time.Now().UnixNano())
	resolver := e.resolvePending(taskID, agentapproval.StatusApproved, "")
	status, runErr := e.runTaskWith(t, runOptions{taskID: taskID}, policyWorkspace(t))
	resolver.Stop()

	if runErr != nil {
		t.Fatalf("approved high-risk exec must be released: %v", runErr)
	}
	if status != string(repository.TaskCompleted) {
		t.Fatalf("status = %s, want completed", status)
	}
	if resolver.Resolved() != 1 {
		t.Fatalf("approval tickets = %d, want 1 (exec allowlist must defer to Classify)", resolver.Resolved())
	}

	rows := e.auditRows(t, taskID)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Decision != string(policy.EffectRequireApproval) || row.Code != policy.CodeApprovalRequired {
		t.Fatalf("audit = %+v, want require_approval / %s", row, policy.CodeApprovalRequired)
	}
	if row.RuleID != "policy.approval.high_risk.delete" {
		t.Fatalf("rule = %s, want policy.approval.high_risk.delete", row.RuleID)
	}
	if row.PolicySource != "system:engine" {
		t.Fatalf("policy_source = %q, want system:engine", row.PolicySource)
	}
	t.Logf("exec→approval: task=%s rule=%s source=%s tickets=%d", taskID, row.RuleID, row.PolicySource, resolver.Resolved())
}
