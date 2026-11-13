package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentapproval "agent-platform/pkg/agent/approval"
	"agent-platform/pkg/harnessloop/policy"
)

func newApprovalTestServer(t *testing.T) (*httptest.Server, *agentapproval.ApprovalFlowManager) {
	t.Helper()
	manager := agentapproval.NewApprovalFlowManager()
	mux := http.NewServeMux()
	mux.HandleFunc("/approval/request", func(w http.ResponseWriter, r *http.Request) {
		var req agentapproval.ApprovalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Mirror agent-service: high-risk requests never auto-approve.
		req.AutoApprove = false
		created, err := manager.CreateRequest(r.Context(), &req)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"request_id": created.ID, "status": created.Status},
		})
	})
	mux.HandleFunc("/approval/status", func(w http.ResponseWriter, r *http.Request) {
		req, err := manager.GetRequest(r.URL.Query().Get("request_id"))
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"code": -1, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"request_id": req.ID, "status": req.Status},
		})
	})
	mux.HandleFunc("/approval/reject", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RequestID string `json:"request_id"`
			UserID    string `json:"user_id"`
			Reason    string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = manager.SubmitDecision(r.Context(), &agentapproval.ApprovalDecision{
			RequestID: body.RequestID, Decision: agentapproval.StatusRejected,
			UserID: body.UserID, Reason: body.Reason,
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"code": 0})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, manager
}

func newApprovalClientForTest(t *testing.T, address string, timeout time.Duration) *ApprovalClient {
	t.Helper()
	client, err := NewApprovalClient(ApprovalClientConfig{
		Address:      address,
		Timeout:      timeout,
		PollInterval: 5 * time.Millisecond,
		Approval:     policy.DefaultApprovalConfig(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return client
}

func approvalRequest() policy.Request {
	return policy.Request{
		Tool: "write_file", Stage: "c",
		Args:     policy.ToolArgs{Paths: []string{"tmp/old.delete"}},
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Attempt: 1,
	}
}

func waitForPending(t *testing.T, manager *agentapproval.ApprovalFlowManager, timeout time.Duration) *agentapproval.ApprovalRequest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pending := manager.GetPendingRequests(""); len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no pending approval request appeared")
	return nil
}

func TestApprovalClientPendingApproveContinues(t *testing.T) {
	srv, manager := newApprovalTestServer(t)
	client := newApprovalClientForTest(t, srv.URL, 2*time.Second)

	ticket, err := client.Begin(context.Background(), approvalRequest(), []string{policy.TagWrite})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if ticket == nil || ticket.ID == "" || ticket.Kind != policy.HighRiskDelete {
		t.Fatalf("ticket = %+v", ticket)
	}

	go func() {
		pending := waitForPending(t, manager, time.Second)
		_ = manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
			RequestID: pending.ID, Decision: agentapproval.StatusApproved,
			UserID: "operator-1", Reason: "approved by test",
		})
	}()

	out := client.Await(context.Background(), ticket)
	if !out.Allowed || out.Code != "" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestApprovalClientRejectedDoesNotRetry(t *testing.T) {
	srv, manager := newApprovalTestServer(t)
	client := newApprovalClientForTest(t, srv.URL, 2*time.Second)

	ticket, err := client.Begin(context.Background(), approvalRequest(), []string{policy.TagWrite})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	go func() {
		pending := waitForPending(t, manager, time.Second)
		_ = manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
			RequestID: pending.ID, Decision: agentapproval.StatusRejected,
			UserID: "operator-1", Reason: "denied by test",
		})
	}()
	out := client.Await(context.Background(), ticket)
	if out.Allowed || out.Code != policy.CodeApprovalRejected || !errors.Is(out.Err, policy.ErrApprovalRejected) {
		t.Fatalf("outcome = %+v", out)
	}
	if _, err := client.Begin(context.Background(), approvalRequest(), []string{policy.TagWrite}); !errors.Is(err, policy.ErrApprovalRejected) {
		t.Fatalf("a rejected call must not create a new request: %v", err)
	}
}

func TestApprovalClientTimeoutDoesNotAllow(t *testing.T) {
	srv, manager := newApprovalTestServer(t)
	client := newApprovalClientForTest(t, srv.URL, time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	ticket, err := client.Begin(context.Background(), approvalRequest(), []string{policy.TagWrite})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	pending := waitForPending(t, manager, time.Second)
	out := client.Await(context.Background(), ticket)
	if out.Allowed || out.Code != policy.CodeApprovalTimeout {
		t.Fatalf("outcome = %+v", out)
	}
	// The stale remote request must be closed as rejected.
	req, getErr := manager.GetRequest(pending.ID)
	if getErr != nil || req.Status != agentapproval.StatusRejected {
		t.Fatalf("remote request = %+v err=%v", req, getErr)
	}
}

func TestApprovalClientUnavailableHoldsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	client := newApprovalClientForTest(t, srv.URL, time.Second)
	if _, err := client.Begin(context.Background(), approvalRequest(), []string{policy.TagWrite}); !errors.Is(err, policy.ErrApprovalUnavailable) {
		t.Fatalf("unreachable service must be unavailable: %v", err)
	}
}

func TestPolicyBridgeAuthorizeApprovalContinues(t *testing.T) {
	srv, manager := newApprovalTestServer(t)
	client := newApprovalClientForTest(t, srv.URL, 2*time.Second)
	bridge := NewPolicyBridge(true, policy.NewEngine(), nil, nil, client)

	in := ToolCallInput{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1",
		Stage: "c", Attempt: 1, WorkspaceRoot: t.TempDir(), AgentID: "agent-1", Tool: "write_file",
		Paths: []string{"tmp/old.delete"},
		RolePolicy: &policy.PolicySet{Rules: []policy.Rule{{
			ID: "role.require", Tool: "write_file", Effect: policy.EffectRequireApproval,
		}}},
	}
	go func() {
		pending := waitForPending(t, manager, time.Second)
		_ = manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
			RequestID: pending.ID, Decision: agentapproval.StatusApproved,
			UserID: "operator-1",
		})
	}()
	if err := bridge.AuthorizeTool(context.Background(), in); err != nil {
		t.Fatalf("approved high-risk call must continue: %v", err)
	}
}

func TestPolicyBridgeAuthorizeApprovalRejectedStops(t *testing.T) {
	srv, manager := newApprovalTestServer(t)
	client := newApprovalClientForTest(t, srv.URL, 2*time.Second)
	bridge := NewPolicyBridge(true, policy.NewEngine(), nil, nil, client)

	in := ToolCallInput{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1",
		Stage: "c", Attempt: 1, WorkspaceRoot: t.TempDir(), AgentID: "agent-1", Tool: "write_file",
		Paths: []string{"tmp/old.delete"},
		RolePolicy: &policy.PolicySet{Rules: []policy.Rule{{
			ID: "role.require", Tool: "write_file", Effect: policy.EffectRequireApproval,
		}}},
	}
	go func() {
		pending := waitForPending(t, manager, time.Second)
		_ = manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
			RequestID: pending.ID, Decision: agentapproval.StatusRejected,
			UserID: "operator-1", Reason: "denied",
		})
	}()
	err := bridge.AuthorizeTool(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), policy.CodeApprovalRejected) {
		t.Fatalf("rejected high-risk call must not continue: %v", err)
	}
}

func TestPolicyBridgePreflightReturnsPendingApproval(t *testing.T) {
	srv, _ := newApprovalTestServer(t)
	client := newApprovalClientForTest(t, srv.URL, 2*time.Second)
	bridge := NewPolicyBridge(true, policy.NewEngine(), nil, nil, client)

	in := ToolCallInput{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1",
		Stage: "c", Attempt: 1, WorkspaceRoot: t.TempDir(), AgentID: "agent-1", Tool: "write_file",
		Paths: []string{"tmp/old.delete"},
		RolePolicy: &policy.PolicySet{Rules: []policy.Rule{{
			ID: "role.require", Tool: "write_file", Effect: policy.EffectRequireApproval,
		}}},
	}
	res := bridge.Preflight(context.Background(), in)
	if res.Allowed || !res.RequiresApproval || res.ApprovalID == "" {
		t.Fatalf("preflight must create a pending approval: %+v", res)
	}
}

func TestPolicyBridgeWithoutApprovalGateStaysPending(t *testing.T) {
	bridge := NewPolicyBridge(true, policy.NewEngine(), nil, nil, nil)
	in := ToolCallInput{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1",
		Stage: "c", Attempt: 1, WorkspaceRoot: t.TempDir(), Tool: "write_file", Paths: []string{"tmp/old.delete"},
		RolePolicy: &policy.PolicySet{Rules: []policy.Rule{{
			ID: "role.require", Tool: "write_file", Effect: policy.EffectRequireApproval,
		}}},
	}
	res := bridge.Preflight(context.Background(), in)
	if res.Allowed || !res.RequiresApproval || res.Code != policy.CodeApprovalUnavailable {
		t.Fatalf("missing gate must stay pending: %+v", res)
	}
	if err := bridge.AuthorizeTool(context.Background(), in); err == nil {
		t.Fatal("missing gate must not authorize")
	}
}
