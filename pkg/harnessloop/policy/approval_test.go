package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentapproval "agent-platform/pkg/agent/approval"
)

func TestApprovalConfigLoad(t *testing.T) {
	path := filepath.Join("..", "..", "config", "harness_loop", "approval.yaml")
	cfg, err := LoadApprovalConfig(path)
	if err != nil {
		t.Fatalf("load approval config: %v", err)
	}
	if !cfg.Enabled || cfg.TimeoutSeconds != 300 || cfg.CostThreshold != 0.10 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.TimeoutAutoApprove {
		t.Fatal("timeout_auto_approve must be false")
	}
	for name, on := range map[string]bool{
		"delete":        cfg.HighRisk.Delete,
		"network_write": cfg.HighRisk.NetworkWrite,
		"deploy":        cfg.HighRisk.Deploy,
		"cost":          cfg.HighRisk.Cost,
	} {
		if !on {
			t.Fatalf("high risk %s must be enabled", name)
		}
	}
}

func TestApprovalConfigRejectsTimeoutAutoApprove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.yaml")
	body := "version: 1\napproval:\n  timeout_seconds: 300\n  timeout_auto_approve: true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApprovalConfig(path)
	if err == nil || !strings.Contains(err.Error(), "timeout_auto_approve") {
		t.Fatalf("timeout_auto_approve must be rejected, got %v", err)
	}
}

func TestApprovalHighRiskClassification(t *testing.T) {
	gate := NewApprovalGate(DefaultApprovalConfig(), nil)
	cases := []struct {
		name string
		tool string
		tags []string
		args ToolArgs
		kind HighRiskKind
	}{
		{"deploy tool", "deploy_app", []string{TagExec}, ToolArgs{}, HighRiskDeploy},
		{"kubectl apply", "run_command", []string{TagExec}, ToolArgs{Command: []string{"kubectl", "apply"}}, HighRiskDeploy},
		{"network write", "http_fetch", []string{TagNetwork}, ToolArgs{NetworkMethod: "POST"}, HighRiskNetworkWrite},
		{"network read", "http_fetch", []string{TagNetwork}, ToolArgs{NetworkMethod: "GET"}, HighRiskNone},
		{"delete", "delete", []string{TagWrite}, ToolArgs{Paths: []string{"a.go"}}, HighRiskDelete},
		{"large cost", "write_file", []string{TagWrite}, ToolArgs{EstimatedCost: 0.2}, HighRiskCost},
		{"read only", "read_file", []string{TagRead}, ToolArgs{}, HighRiskNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, ruleID := gate.Classify(c.tool, c.tags, c.args)
			if kind != c.kind {
				t.Fatalf("kind = %q, want %q", kind, c.kind)
			}
			if c.kind != HighRiskNone && !strings.HasPrefix(ruleID, "policy.approval.high_risk.") {
				t.Fatalf("unexpected rule id %q", ruleID)
			}
		})
	}
}

func TestApprovalDeployPendingAndStatus(t *testing.T) {
	manager := agentapproval.NewApprovalFlowManager()
	gate := NewApprovalGate(DefaultApprovalConfig(), manager)
	req := Request{Tool: "deploy_app", Stage: "c", TaskID: "task-1", TenantID: "t-1"}

	ticket, err := gate.Begin(context.Background(), req, []string{TagExec})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if ticket == nil || ticket.Kind != HighRiskDeploy {
		t.Fatalf("deploy call must require approval: %+v", ticket)
	}
	st, err := gate.Status(context.Background(), ticket.ID)
	if err != nil || st != agentapproval.StatusPending {
		t.Fatalf("status = %s, err = %v, want pending", st, err)
	}

	done := make(chan ApprovalOutcome, 1)
	go func() { done <- gate.Await(context.Background(), ticket) }()

	waitPending(t, manager, ticket.ID)
	if err := manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
		RequestID: ticket.ID,
		Decision:  agentapproval.StatusApproved,
		UserID:    "reviewer",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	select {
	case out := <-done:
		if !out.Allowed || out.Status != agentapproval.StatusApproved {
			t.Fatalf("approved call must be allowed: %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await approval timeout")
	}
}

func TestApprovalRejectedNotRetried(t *testing.T) {
	manager := agentapproval.NewApprovalFlowManager()
	gate := NewApprovalGate(DefaultApprovalConfig(), manager)
	req := Request{Tool: "delete", Stage: "c", TaskID: "task-2", Args: ToolArgs{Paths: []string{"a.go"}}}

	ticket, err := gate.Begin(context.Background(), req, []string{TagWrite})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	done := make(chan ApprovalOutcome, 1)
	go func() { done <- gate.Await(context.Background(), ticket) }()

	waitPending(t, manager, ticket.ID)
	if err := manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
		RequestID: ticket.ID,
		Decision:  agentapproval.StatusRejected,
		UserID:    "reviewer",
		Reason:    "not allowed",
	}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	out := <-done
	if out.Allowed || out.Code != CodeApprovalRejected {
		t.Fatalf("rejected call must fail: %+v", out)
	}
	if gate.RequestCount() != 1 {
		t.Fatalf("request count = %d, want 1", gate.RequestCount())
	}

	if _, err := gate.Begin(context.Background(), req, []string{TagWrite}); !errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("rejected call must not retry, got %v", err)
	}
	if gate.RequestCount() != 1 {
		t.Fatalf("rejected retry must not create a new request, count = %d", gate.RequestCount())
	}
}

func TestApprovalTimeoutDoesNotAllow(t *testing.T) {
	manager := agentapproval.NewApprovalFlowManager()
	cfg := DefaultApprovalConfig()
	cfg.TimeoutSeconds = 1
	gate := NewApprovalGate(cfg, manager)
	gate.SetPollInterval(time.Millisecond)

	var fake = time.Now()
	gate.SetClock(func() time.Time {
		fake = fake.Add(time.Second)
		return fake
	})

	req := Request{Tool: "deploy_app", Stage: "c", TaskID: "task-3"}
	ticket, err := gate.Begin(context.Background(), req, []string{TagExec})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	out := gate.Await(context.Background(), ticket)
	if out.Allowed {
		t.Fatalf("timeout must not allow the call: %+v", out)
	}
	if out.Code != CodeApprovalTimeout || out.Status != agentapproval.StatusTimeout {
		t.Fatalf("timeout outcome = %+v", out)
	}
	status, err := manager.GetRequest(ticket.ID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if status.Status != agentapproval.StatusTimeout {
		t.Fatalf("request status = %s, want timeout", status.Status)
	}
}

func TestApprovalServiceUnavailable(t *testing.T) {
	gate := NewApprovalGate(DefaultApprovalConfig(), nil)
	alerted := 0
	gate.SetAlertHook(func(string) { alerted++ })

	out := gate.Authorize(context.Background(), Request{Tool: "deploy_app", Stage: "c"}, []string{TagExec})
	if out.Allowed || out.Code != CodeApprovalUnavailable || out.Status != agentapproval.StatusPending {
		t.Fatalf("unavailable approval must stay pending: %+v", out)
	}
	if alerted != 1 {
		t.Fatalf("unavailable approval must alert once, got %d", alerted)
	}

	ok := gate.Authorize(context.Background(), Request{Tool: "read_file", Stage: "c"}, []string{TagRead})
	if !ok.Allowed || ok.Required {
		t.Fatalf("non high-risk call must not depend on approval service: %+v", ok)
	}
}

func TestApprovalCancelledOnContext(t *testing.T) {
	manager := agentapproval.NewApprovalFlowManager()
	gate := NewApprovalGate(DefaultApprovalConfig(), manager)
	ctx, cancel := context.WithCancel(context.Background())

	req := Request{Tool: "deploy_app", Stage: "c", TaskID: "task-4"}
	ticket, err := gate.Begin(ctx, req, []string{TagExec})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	done := make(chan ApprovalOutcome, 1)
	go func() { done <- gate.Await(ctx, ticket) }()
	waitPending(t, manager, ticket.ID)
	cancel()

	select {
	case out := <-done:
		if out.Allowed || out.Code != CodeApprovalCancelled {
			t.Fatalf("cancelled approval outcome = %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await cancellation timeout")
	}
}

func waitPending(t *testing.T, manager *agentapproval.ApprovalFlowManager, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, err := manager.GetRequest(id)
		if err == nil && req.Status == agentapproval.StatusPending {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("approval %s never became pending", id)
}
