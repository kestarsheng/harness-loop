package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentapproval "agent-platform/pkg/agent/approval"
	"agent-platform/pkg/harnessloop/policy"
)

// recordingSink 收下 harnessloop 投递的一次性令牌。
type recordingSink struct {
	mu     sync.Mutex
	tokens []string
}

func (s *recordingSink) RegisterApprovalToken(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, token)
	return nil
}

func (s *recordingSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tokens...)
}

// countingServer 是与 newApprovalTestServer 相同的审批桩，额外统计
// /approval/request 次数，用来断言「同一调用只建一次工单」。
func countingServer(t *testing.T) (*httptest.Server, *agentapproval.ApprovalFlowManager, *int64) {
	t.Helper()
	manager := agentapproval.NewApprovalFlowManager()
	requests := new(int64)
	mux := http.NewServeMux()
	mux.HandleFunc("/approval/request", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(requests, 1)
		var req agentapproval.ApprovalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.AutoApprove = false
		created, err := manager.CreateRequest(r.Context(), &req)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"request_id": created.ID, "status": created.Status},
		})
	})
	mux.HandleFunc("/approval/status", func(w http.ResponseWriter, r *http.Request) {
		req, err := manager.GetRequest(r.URL.Query().Get("request_id"))
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": -1, "message": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"request_id": req.ID, "status": req.Status},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, manager, requests
}

func TestApprovalClientReusesPendingTicket(t *testing.T) {
	srv, _, requests := countingServer(t)
	client, err := NewApprovalClient(ApprovalClientConfig{
		Address:      srv.URL,
		Timeout:      2 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Approval:     policy.DefaultApprovalConfig(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	var wg sync.WaitGroup
	tickets := make([]*policy.ApprovalTicket, 4)
	for i := range tickets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tickets[i], _ = client.Begin(context.Background(), approvalRequest(), []string{policy.TagWrite})
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt64(requests); got != 1 {
		t.Fatalf("same call must create exactly one approval request, got %d", got)
	}
	for i := 1; i < len(tickets); i++ {
		if tickets[i] == nil || tickets[0] == nil || tickets[i].ID != tickets[0].ID {
			t.Fatalf("tickets must be shared: %+v", tickets)
		}
	}

	// 不同参数是另一次调用，必须另建一张单。
	other := approvalRequest()
	other.Args = policy.ToolArgs{Paths: []string{"tmp/other.delete"}}
	if _, err := client.Begin(context.Background(), other, []string{policy.TagWrite}); err != nil {
		t.Fatalf("begin other: %v", err)
	}
	if got := atomic.LoadInt64(requests); got != 2 {
		t.Fatalf("different args must create a new request, got %d", got)
	}
}

// 批准后必须签发并投递一枚能绑定这次调用的一次性令牌。
func TestApprovalClientDeliversTokenOnApproval(t *testing.T) {
	srv, manager := newApprovalTestServer(t)
	issuer, err := policy.NewApprovalTokenIssuer("harness-test-secret")
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	sink := &recordingSink{}
	client, err := NewApprovalClient(ApprovalClientConfig{
		Address:      srv.URL,
		Timeout:      2 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Approval:     policy.DefaultApprovalConfig(),
		TokenIssuer:  issuer,
		TokenSink:    sink,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	req := approvalRequest()
	ticket, err := client.Begin(context.Background(), req, []string{policy.TagWrite})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	go func() {
		pending := waitForPending(t, manager, time.Second)
		_ = manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
			RequestID: pending.ID, Decision: agentapproval.StatusApproved,
			UserID: "operator-1", Reason: "approved by test",
		})
	}()

	out := client.Await(context.Background(), ticket)
	if !out.Allowed {
		t.Fatalf("outcome = %+v", out)
	}
	tokens := sink.snapshot()
	if len(tokens) != 1 {
		t.Fatalf("approval must deliver exactly one token, got %d", len(tokens))
	}

	verifier, err := policy.NewApprovalTokenVerifier("harness-test-secret")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	claims, err := verifier.Verify(tokens[0], policy.ApprovalTokenExpectation{
		TenantID:   req.TenantID,
		ProjectID:  req.ProjectID,
		TaskID:     req.TaskID,
		Tool:       req.Tool,
		ArgsDigest: policy.ArgumentsDigest(req.Args),
		ApprovalID: ticket.ID,
	})
	if err != nil {
		t.Fatalf("delivered token must verify against the approved call: %v", err)
	}
	if claims.Nonce == "" {
		t.Fatal("delivered token must carry a one-time nonce")
	}

	// 另一枚令牌只能绑同一次调用：换工具就该绑不上。
	if _, err := verifier.Verify(tokens[0], policy.ApprovalTokenExpectation{
		TaskID: req.TaskID, Tool: "run_command",
		ArgsDigest: policy.ArgumentsDigest(req.Args),
	}); err == nil {
		t.Fatal("token must not bind to a different tool")
	}
}

// 执行面投递失败时不得放行：宁可拒绝，也不能让人以为批准生效了。
func TestApprovalClientFailsClosedWhenTokenDeliveryFails(t *testing.T) {
	srv, manager := newApprovalTestServer(t)
	issuer, _ := policy.NewApprovalTokenIssuer("harness-test-secret")
	client, err := NewApprovalClient(ApprovalClientConfig{
		Address:      srv.URL,
		Timeout:      2 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Approval:     policy.DefaultApprovalConfig(),
		TokenIssuer:  issuer,
		TokenSink:    brokenTokenSink{},
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ticket, err := client.Begin(context.Background(), approvalRequest(), []string{policy.TagWrite})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	go func() {
		pending := waitForPending(t, manager, time.Second)
		_ = manager.SubmitDecision(context.Background(), &agentapproval.ApprovalDecision{
			RequestID: pending.ID, Decision: agentapproval.StatusApproved,
			UserID: "operator-1", Reason: "approved by test",
		})
	}()
	out := client.Await(context.Background(), ticket)
	if out.Allowed || out.Code != policy.CodeApprovalUnavailable {
		t.Fatalf("delivery failure must keep the call closed, got %+v", out)
	}
}

type brokenTokenSink struct{}

func (brokenTokenSink) RegisterApprovalToken(context.Context, string) error {
	return context.DeadlineExceeded
}
