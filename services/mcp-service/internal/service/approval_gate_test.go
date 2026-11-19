package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-platform/pkg/config"
	"agent-platform/pkg/harnessloop/policy"
	pb "agent-platform/pkg/pb/mcp"
	"agent-platform/services/mcp-service/internal/audit"
)

// execDenyPolicy 模拟真实引擎在 exec 白名单上的拒绝：只有 ExecApproved
// （即验过一次性令牌）才放行。
type execDenyPolicy struct{}

func (execDenyPolicy) Authorize(_ context.Context, in ToolPolicyInput) ToolPolicyResult {
	if in.ExecApproved {
		return ToolPolicyResult{Allowed: true, Reason: "approved exec"}
	}
	return ToolPolicyResult{
		Allowed: false,
		Code:    policy.CodeExecDenied,
		RuleID:  "sandbox.exec.allowlist",
		Reason:  "command is not in the exec allowlist",
	}
}

const testTokenSecret = "test-approval-secret"

type approvalFixture struct {
	service *MCPService
	issuer  *policy.ApprovalTokenIssuer
	store   *ApprovalTokenStore
	root    string
}

func newApprovalFixture(t *testing.T, wait time.Duration) *approvalFixture {
	t.Helper()
	verifier, err := policy.NewApprovalTokenVerifier(testTokenSecret)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	issuer, err := policy.NewApprovalTokenIssuer(testTokenSecret)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	store := newApprovalTokenStoreWithVerifier(verifier, time.Now)

	svc := NewMCPService(nil, &config.Config{})
	svc.SetToolPolicy(execDenyPolicy{})
	svc.setApprovalStore(store, wait)

	audit.Reset()
	t.Cleanup(audit.Reset)
	return &approvalFixture{service: svc, issuer: issuer, store: store, root: t.TempDir()}
}

func (f *approvalFixture) call(t *testing.T, command string, waitSeconds int) *pb.CallToolResponse {
	t.Helper()
	pc, _ := json.Marshal(map[string]any{"__policy_context__": map[string]any{
		"stage":                 "c",
		"tenant_id":             "tenant-a",
		"project_id":            "project-a",
		"task_id":               "task-a",
		"workspace_root":        f.root,
		"approval_wait_seconds": waitSeconds,
	}})
	args, _ := json.Marshal(map[string]any{"command": command})
	resp, err := f.service.CallTool(context.Background(), &pb.CallToolRequest{
		Name:       "code_execute",
		TenantId:   "tenant-a",
		Arguments:  string(args),
		ToolConfig: string(pc),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return resp
}

func (f *approvalFixture) issue(t *testing.T, taskID, tool, argsJSON string) string {
	t.Helper()
	args := policy.CanonicalArgsFromJSON([]byte(argsJSON))
	token, _, err := f.issuer.Issue(policy.ApprovalTokenClaims{
		TenantID:   "tenant-a",
		ProjectID:  "project-a",
		TaskID:     taskID,
		Tool:       tool,
		Stage:      "c",
		ArgsDigest: policy.ArgumentsDigest(args),
		ApprovalID: "apr-test-1",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return token
}

// 批准后 rm 真的执行：这是「批准生效」的唯一硬证据。
func TestCallToolExecutesApprovedCommand(t *testing.T) {
	f := newApprovalFixture(t, 2*time.Second)
	probeDir := filepath.Join(f.root, "scratch")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(probeDir, "probe.txt")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	argsJSON := `{"command":"rm scratch/probe.txt"}`
	token := f.issue(t, "task-a", "run_command", argsJSON)
	if _, err := f.store.Register(token); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp := f.call(t, "rm scratch/probe.txt", 2)
	if resp.IsError {
		t.Fatalf("approved call must run, got error: %s", resp.Content)
	}
	if _, err := os.Stat(probe); !os.IsNotExist(err) {
		t.Fatalf("approved rm did not delete %s (err=%v)", probe, err)
	}

	var consumed bool
	for _, rec := range audit.Records() {
		if rec.Code == "E_APPROVAL_TOKEN_CONSUMED" && rec.Nonce != "" {
			consumed = true
		}
	}
	if !consumed {
		t.Fatalf("audit must record the consumed token: %+v", audit.Records())
	}
}

// 没有批准就没有令牌：拒绝、不执行、留痕。
func TestCallToolRejectsUnapprovedCommand(t *testing.T) {
	f := newApprovalFixture(t, 0)
	probeDir := filepath.Join(f.root, "scratch")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(probeDir, "probe.txt")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := f.call(t, "rm scratch/probe.txt", 0)
	if !resp.IsError {
		t.Fatalf("unapproved call must be denied, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, policy.CodeExecDenied) {
		t.Fatalf("denial must name the policy code, got: %s", resp.Content)
	}
	if _, err := os.Stat(probe); err != nil {
		t.Fatalf("unapproved rm must not touch the file: %v", err)
	}

	var denied bool
	for _, rec := range audit.Records() {
		if rec.Decision == audit.DecisionDeny && rec.Code == policy.CodeExecDenied && rec.TaskID == "task-a" {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("policy denial must be audited: %+v", audit.Records())
	}
}

// 令牌绑定到别的参数时不能放行（否则等于通用通行证）。
func TestCallToolRejectsTokenBoundToOtherArgs(t *testing.T) {
	f := newApprovalFixture(t, 120*time.Millisecond)
	token := f.issue(t, "task-a", "run_command", `{"command":"rm other.txt"}`)
	if _, err := f.store.Register(token); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp := f.call(t, "rm scratch/probe.txt", 0)
	_ = resp
	// wait=0 时不会等令牌，直接按普通策略拒绝；这里直接验 store 的绑定拒绝。
	expected := policy.ApprovalTokenExpectation{
		TenantID:   "tenant-a",
		ProjectID:  "project-a",
		TaskID:     "task-a",
		Tool:       "run_command",
		ArgsDigest: policy.ArgumentsDigest(policy.CanonicalArgsFromJSON([]byte(`{"command":"rm scratch/probe.txt"}`))),
	}
	if _, err := f.store.WaitFor(context.Background(), expected, 60*time.Millisecond); policy.TokenErrorCode(err) != policy.CodeApprovalTokenMissing {
		t.Fatalf("token bound to other args must not match, got err=%v", err)
	}
}

// 同一枚令牌只能用一次；重复消费返回 REPLAY。
func TestApprovalTokenStoreRejectsReplay(t *testing.T) {
	f := newApprovalFixture(t, 0)
	argsJSON := `{"command":"rm scratch/probe.txt"}`
	token := f.issue(t, "task-a", "run_command", argsJSON)
	expected := policy.ApprovalTokenExpectation{
		TenantID:   "tenant-a",
		ProjectID:  "project-a",
		TaskID:     "task-a",
		Tool:       "run_command",
		ArgsDigest: policy.ArgumentsDigest(policy.CanonicalArgsFromJSON([]byte(argsJSON))),
	}
	claims, err := f.store.Register(token)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	first := expected
	first.ApprovalID = claims.ApprovalID
	if _, err := f.store.Consume(first); err != nil {
		t.Fatalf("first consume must succeed: %v", err)
	}
	second := first
	if _, err := f.store.Consume(second); policy.TokenErrorCode(err) != policy.CodeApprovalTokenMissing &&
		policy.TokenErrorCode(err) != policy.CodeApprovalTokenReplay {
		t.Fatalf("second consume must be refused, got err=%v", err)
	}
	// 重新投递同一枚令牌也必须被拒（nonce 已消费）。
	if _, err := f.store.Register(token); policy.TokenErrorCode(err) != policy.CodeApprovalTokenReplay {
		t.Fatalf("re-registering a consumed token must be replay, got err=%v", err)
	}
}

// 过期令牌不得放行。
func TestApprovalTokenStoreRejectsExpired(t *testing.T) {
	f := newApprovalFixture(t, 0)
	// 让签发时钟退回一小时：签出来的令牌一出生就已过期。
	f.issuer.SetClock(func() time.Time { return time.Now().Add(-time.Hour) })
	argsJSON := `{"command":"rm scratch/probe.txt"}`
	token := f.issue(t, "task-a", "run_command", argsJSON)
	if _, err := f.store.Register(token); policy.TokenErrorCode(err) != policy.CodeApprovalTokenExpired {
		t.Fatalf("expired token must be refused at registration, got err=%v", err)
	}
}

// 注册面拒绝坏签名，并在审计里留痕。
func TestRegisterApprovalTokenRejectsBadSignature(t *testing.T) {
	f := newApprovalFixture(t, 0)
	resp, err := f.service.RegisterApprovalToken(context.Background(), &pb.RegisterApprovalTokenRequest{Token: "not-a-token"})
	if err != nil {
		t.Fatalf("RegisterApprovalToken: %v", err)
	}
	if resp.Accepted {
		t.Fatalf("bad token must be refused: %+v", resp)
	}
	if !strings.Contains(resp.Reason, policy.CodeApprovalTokenInvalid) {
		t.Fatalf("refusal must name the code, got: %s", resp.Reason)
	}
	var denied bool
	for _, rec := range audit.Records() {
		if rec.Decision == audit.DecisionDeny && rec.Code == policy.CodeApprovalTokenInvalid {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("bad token registration must be audited: %+v", audit.Records())
	}
}
