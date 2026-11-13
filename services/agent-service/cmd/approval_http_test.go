package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-platform/pkg/agent/approval"
)

type approvalHTTPData struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
	} `json:"data"`
}

func approvalServer(t *testing.T) (*httptest.Server, *approval.ApprovalFlowManager) {
	t.Helper()
	manager := approval.NewApprovalFlowManager()
	mux := http.NewServeMux()
	registerApprovalRoutes(mux, manager, approval.NewRuleEngine())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, manager
}

func doJSON(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode
}

func createApprovalRequest(t *testing.T, baseURL string) string {
	t.Helper()
	var created approvalHTTPData
	status := doJSON(t, http.MethodPost, baseURL+"/approval/request", map[string]any{
		"type":            "tool_call",
		"priority":        "high",
		"agent_id":        "agent-1",
		"session_id":      "task-1",
		"description":     "high-risk tool deploy_app requires approval",
		"risk_level":      "high",
		"risk_reason":     "policy.high_risk:deploy",
		"auto_approve":    true,
		"timeout_seconds": 30,
	}, &created)
	if status != http.StatusOK || created.Code != 0 || created.Data.RequestID == "" {
		t.Fatalf("create status=%d body=%+v", status, created)
	}
	return created.Data.RequestID
}

func approvalStatus(t *testing.T, baseURL, id string) string {
	t.Helper()
	var payload approvalHTTPData
	status := doJSON(t, http.MethodGet, baseURL+"/approval/status?request_id="+id, nil, &payload)
	if status != http.StatusOK || payload.Code != 0 {
		t.Fatalf("status code=%d body=%+v", status, payload)
	}
	return payload.Data.Status
}

func TestApprovalHTTPRequestApproveContinues(t *testing.T) {
	srv, manager := approvalServer(t)
	id := createApprovalRequest(t, srv.URL)

	if got := approvalStatus(t, srv.URL, id); got != string(approval.StatusPending) {
		t.Fatalf("status after create = %q", got)
	}
	// A high-risk policy approval must never be auto-approved even when the
	// caller asks for it.
	if req, err := manager.GetRequest(id); err != nil || req.Status != approval.StatusPending {
		t.Fatalf("request auto-approval must be ignored: %+v %v", req, err)
	}

	var decided approvalHTTPData
	if status := doJSON(t, http.MethodPost, srv.URL+"/approval/approve", map[string]any{
		"request_id": id, "user_id": "operator-1", "reason": "looks safe",
	}, &decided); status != http.StatusOK || decided.Code != 0 {
		t.Fatalf("approve status=%d body=%+v", status, decided)
	}
	if got := approvalStatus(t, srv.URL, id); got != string(approval.StatusApproved) {
		t.Fatalf("status after approve = %q", got)
	}
}

func TestApprovalHTTPRequestReject(t *testing.T) {
	srv, _ := approvalServer(t)
	id := createApprovalRequest(t, srv.URL)
	var decided approvalHTTPData
	if status := doJSON(t, http.MethodPost, srv.URL+"/approval/reject", map[string]any{
		"request_id": id, "user_id": "operator-1", "reason": "not allowed",
	}, &decided); status != http.StatusOK || decided.Code != 0 {
		t.Fatalf("reject status=%d body=%+v", status, decided)
	}
	if got := approvalStatus(t, srv.URL, id); got != string(approval.StatusRejected) {
		t.Fatalf("status after reject = %q", got)
	}
}

func TestApprovalHTTPStatusValidation(t *testing.T) {
	srv, _ := approvalServer(t)
	if status := doJSON(t, http.MethodGet, srv.URL+"/approval/status", nil, nil); status != http.StatusBadRequest {
		t.Fatalf("missing request_id status = %d", status)
	}
	if status := doJSON(t, http.MethodGet, srv.URL+"/approval/status?request_id=missing", nil, nil); status != http.StatusNotFound {
		t.Fatalf("unknown request status = %d", status)
	}
	if status := doJSON(t, http.MethodGet, srv.URL+"/approval/request", nil, nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d", status)
	}
}
