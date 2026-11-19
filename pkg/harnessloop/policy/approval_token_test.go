package policy

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	agentapproval "agent-platform/pkg/agent/approval"
)

type recordingSink struct {
	mu     sync.Mutex
	tokens []string
}

func (s *recordingSink) RegisterApprovalToken(ctx context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, token)
	return nil
}

func (s *recordingSink) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tokens) == 0 {
		return ""
	}
	return s.tokens[len(s.tokens)-1]
}

func TestApprovalTokenIssueVerify(t *testing.T) {
	issuer, err := NewApprovalTokenIssuer("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewApprovalTokenVerifier("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	args := CanonicalArgsFromJSON([]byte(`{"command":"rm -f scratch/probe.txt"}`))
	digest := ArgumentsDigest(args)

	token, claims, err := issuer.Issue(ApprovalTokenClaims{
		TenantID: "t-1", ProjectID: "p-1", TaskID: "task-1",
		Tool: "run_command", Stage: "c", ArgsDigest: digest, ApprovalID: "ap-1",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if claims.Nonce == "" || claims.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("token claims are not complete: %+v", claims)
	}

	got, err := verifier.Verify(token, ApprovalTokenExpectation{
		TenantID: "t-1", ProjectID: "p-1", TaskID: "task-1",
		Tool: "run_command", ArgsDigest: digest, ApprovalID: "ap-1",
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Nonce != claims.Nonce || got.ApprovalID != "ap-1" {
		t.Fatalf("claims round-trip mismatch: %+v", got)
	}
}

func TestApprovalTokenRejectsWrongBinding(t *testing.T) {
	issuer, _ := NewApprovalTokenIssuer("test-secret")
	verifier, _ := NewApprovalTokenVerifier("test-secret")
	args := CanonicalArgsFromJSON([]byte(`{"command":"rm -f scratch/probe.txt"}`))
	digest := ArgumentsDigest(args)
	token, _, err := issuer.Issue(ApprovalTokenClaims{
		TaskID: "task-1", Tool: "run_command", ArgsDigest: digest, ApprovalID: "ap-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		exp  ApprovalTokenExpectation
		code string
	}{
		{"other task", ApprovalTokenExpectation{TaskID: "task-2", Tool: "run_command", ArgsDigest: digest}, CodeApprovalTokenBinding},
		{"other tool", ApprovalTokenExpectation{TaskID: "task-1", Tool: "write_file", ArgsDigest: digest}, CodeApprovalTokenBinding},
		{"other args", ApprovalTokenExpectation{TaskID: "task-1", Tool: "run_command", ArgsDigest: "deadbeef"}, CodeApprovalTokenBinding},
		{"other approval", ApprovalTokenExpectation{TaskID: "task-1", Tool: "run_command", ArgsDigest: digest, ApprovalID: "ap-2"}, CodeApprovalTokenBinding},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := verifier.Verify(token, c.exp); TokenErrorCode(err) != c.code {
				t.Fatalf("verify error = %v, want code %s", err, c.code)
			}
		})
	}
}

func TestApprovalTokenRejectsTamperedAndExpired(t *testing.T) {
	issuer, _ := NewApprovalTokenIssuer("test-secret")
	verifier, _ := NewApprovalTokenVerifier("test-secret")
	digest := ArgumentsDigest(ToolArgs{Command: []string{"rm", "-f", "x"}})
	token, _, err := issuer.Issue(ApprovalTokenClaims{
		TaskID: "task-1", Tool: "run_command", ArgsDigest: digest, ApprovalID: "ap-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	exp := ApprovalTokenExpectation{TaskID: "task-1", Tool: "run_command", ArgsDigest: digest}

	if _, err := verifier.Verify(token+"x", exp); TokenErrorCode(err) != CodeApprovalTokenInvalid {
		t.Fatalf("tampered token must be invalid, got %v", err)
	}

	issuer.SetClock(func() time.Time { return time.Now().Add(-2 * time.Hour) })
	expired, _, err := issuer.Issue(ApprovalTokenClaims{
		TaskID: "task-1", Tool: "run_command", ArgsDigest: digest, ApprovalID: "ap-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(expired, exp); TokenErrorCode(err) != CodeApprovalTokenExpired {
		t.Fatalf("expired token must be rejected, got %v", err)
	}
}

func TestApprovalTokenEmptySecretFailsClosed(t *testing.T) {
	if _, err := NewApprovalTokenIssuer(""); TokenErrorCode(err) != CodeApprovalTokenKeyMissing {
		t.Fatalf("empty secret must fail closed, got %v", err)
	}
	if _, err := NewApprovalTokenVerifier("  "); TokenErrorCode(err) != CodeApprovalTokenKeyMissing {
		t.Fatalf("empty secret must fail closed, got %v", err)
	}
}

func TestCanonicalArgsDigestParity(t *testing.T) {
	stringForm := CanonicalArgsFromJSON([]byte(`{"command":"rm -f scratch/probe.txt"}`))
	arrayForm := CanonicalArgsFromJSON([]byte(`{"command":["rm","-f","scratch/probe.txt"]}`))
	if ArgumentsDigest(stringForm) != ArgumentsDigest(arrayForm) {
		t.Fatalf("string and array command forms must hash the same: %v vs %v", stringForm.Command, arrayForm.Command)
	}
	pathForm := CanonicalArgsFromJSON([]byte(`{"path":"a.txt","content":"x"}`))
	arrayPath := CanonicalArgsFromJSON([]byte(`{"paths":["a.txt"],"content":"x"}`))
	if ArgumentsDigest(pathForm) != ArgumentsDigest(arrayPath) {
		t.Fatal("path and paths forms must hash the same")
	}
	if len(stringForm.Command) != 3 || stringForm.Command[0] != "rm" {
		t.Fatalf("string command must be split into argv: %v", stringForm.Command)
	}
}

func TestApprovalGateReusesPendingTicket(t *testing.T) {
	manager := agentapproval.NewApprovalFlowManager()
	gate := NewApprovalGate(DefaultApprovalConfig(), manager)
	req := Request{
		Tool: "run_command", Stage: "c", TaskID: "task-1",
		Args: ToolArgs{Command: []string{"rm", "-f", "scratch/probe.txt"}},
	}
	first, err := gate.Begin(context.Background(), req, []string{TagExec})
	if err != nil || first == nil {
		t.Fatalf("begin: %v %v", first, err)
	}
	second, err := gate.Begin(context.Background(), req, []string{TagExec})
	if err != nil || second == nil {
		t.Fatalf("second begin: %v %v", second, err)
	}
	if first.ID != second.ID {
		t.Fatalf("same call must reuse the pending ticket: %s vs %s", first.ID, second.ID)
	}
	if gate.RequestCount() != 1 {
		t.Fatalf("request count = %d, want 1 (no duplicate tickets)", gate.RequestCount())
	}

	// 参数变了就是另一次调用，必须另建工单。
	other := req
	other.Args = ToolArgs{Command: []string{"rm", "-f", "scratch/other.txt"}}
	third, err := gate.Begin(context.Background(), other, []string{TagExec})
	if err != nil || third == nil {
		t.Fatalf("different args begin: %v %v", third, err)
	}
	if third.ArgsDigest == first.ArgsDigest {
		t.Fatalf("different args must produce a different digest: %s", third.ArgsDigest)
	}
	if gate.RequestCount() != 2 {
		t.Fatalf("request count = %d, want 2 (different args need a new ticket)", gate.RequestCount())
	}
}

func TestApprovalGateIssuesTokenOnApproval(t *testing.T) {
	manager := agentapproval.NewApprovalFlowManager()
	gate := NewApprovalGate(DefaultApprovalConfig(), manager)
	issuer, err := NewApprovalTokenIssuer("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	gate.SetTokenDelivery(issuer, sink)

	args := CanonicalArgsFromJSON([]byte(`{"command":"rm -f scratch/probe.txt"}`))
	req := Request{
		TenantID: "t-1", ProjectID: "p-1", TaskID: "task-1",
		Tool: "run_command", Stage: "c", Args: args,
	}
	ticket, err := gate.Begin(context.Background(), req, []string{TagExec})
	if err != nil || ticket == nil {
		t.Fatalf("begin: %v %v", ticket, err)
	}
	done := make(chan ApprovalOutcome, 1)
	go func() { done <- gate.Await(context.Background(), ticket) }()
	waitPending(t, manager, ticket.ID)
	if err := manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
		RequestID: ticket.ID, Decision: agentapproval.StatusApproved, UserID: "reviewer",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	select {
	case out := <-done:
		if !out.Allowed {
			t.Fatalf("approved call must be allowed: %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await timeout")
	}

	token := sink.last()
	if token == "" {
		t.Fatal("approval must deliver a token")
	}
	verifier, _ := NewApprovalTokenVerifier("test-secret")
	claims, err := verifier.Verify(token, ApprovalTokenExpectation{
		TenantID: "t-1", ProjectID: "p-1", TaskID: "task-1",
		Tool: "run_command", ArgsDigest: ArgumentsDigest(args), ApprovalID: ticket.ID,
	})
	if err != nil {
		t.Fatalf("delivered token must verify: %v", err)
	}
	if claims.Nonce == "" {
		t.Fatal("delivered token has no nonce")
	}
}

func TestApprovedCommandBypassesAllowlistButKeepsHardLimits(t *testing.T) {
	engine := NewEngine()
	base := Request{
		Tool: "run_command", Stage: "c", WorkspaceRoot: t.TempDir(),
		Args:         ToolArgs{Command: []string{"rm", "-f", "scratch/probe.txt"}},
		ExecApproved: true,
	}
	if d := engine.SystemDecide(context.Background(), base); !d.Allowed {
		t.Fatalf("approved rm must pass the system layer: %+v", d)
	}

	for _, cmd := range [][]string{
		{"rm", "-rf", "/"},
		{"bash", "-c", "rm -f x"},
		{"rm", "-f", "../outside.txt"},
		{"rm", "-f", "a;b"},
	} {
		req := base
		req.Args = ToolArgs{Command: cmd}
		if d := engine.SystemDecide(context.Background(), req); d.Allowed {
			t.Fatalf("approved call must still reject %v", cmd)
		}
	}

	// 没有批准时白名单照旧生效。
	unapproved := base
	unapproved.ExecApproved = false
	if d := engine.SystemDecide(context.Background(), unapproved); d.Allowed || d.Code != CodeExecDenied {
		t.Fatalf("unapproved rm must be denied by the exec allowlist: %+v", d)
	}
}

func TestCheckApprovedCommandRejectsEmpty(t *testing.T) {
	if err := CheckApprovedCommand(nil); err == nil || !strings.Contains(err.Error(), "empty command") {
		t.Fatalf("empty command must be rejected, got %v", err)
	}
}
