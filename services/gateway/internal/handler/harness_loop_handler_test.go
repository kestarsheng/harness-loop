package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"agent-platform/pkg/config"
	"agent-platform/services/gateway/internal/middleware"
)

func newRouterWithHarnessLoop(t *testing.T) (*gin.Engine, *HarnessLoopHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	h := NewHarnessLoopHandler(cfg)

	engine := gin.New()
	api := engine.Group("/api/v2")
	api.Use(middleware.Tenant())
	hl := api.Group("/harness-loop")
	hl.Use(middleware.StrictTenant())
	hl.GET("/tasks", h.ListTasks)
	hl.POST("/tasks", h.CreateTask)
	hl.GET("/tasks/:id", h.GetTask)
	hl.POST("/tasks/:id/pause", h.PauseTask)
	hl.POST("/tasks/:id/abort", h.AbortTask)
	hl.GET("/events", h.ListEvents)
	hl.GET("/checkpoints", h.ListCheckpoints)
	return engine, h
}

func TestHarnessLoopRoutes(t *testing.T) {
	engine, h := newRouterWithHarnessLoop(t)
	defer h.Close()

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"list tasks", http.MethodGet, "/api/v2/harness-loop/tasks"},
		{"get task", http.MethodGet, "/api/v2/harness-loop/tasks/t1"},
		{"create task", http.MethodPost, "/api/v2/harness-loop/tasks"},
		{"pause", http.MethodPost, "/api/v2/harness-loop/tasks/t1/pause"},
		{"abort", http.MethodPost, "/api/v2/harness-loop/tasks/t1/abort"},
		{"events", http.MethodGet, "/api/v2/harness-loop/events"},
		{"checkpoints", http.MethodGet, "/api/v2/harness-loop/checkpoints"},
	}
	for _, c := range cases {
		t.Run(c.name+" without tenant", func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if body["code"] != "E_TENANT_REQUIRED" {
				t.Fatalf("code = %v", body["code"])
			}
			for _, key := range []string{"code", "message", "details", "trace_id"} {
				if _, ok := body[key]; !ok {
					t.Fatalf("envelope missing %s: %v", key, body)
				}
			}
		})
	}
}

func TestHarnessLoopConfiguredServiceUnavailable(t *testing.T) {
	engine, h := newRouterWithHarnessLoop(t)
	defer h.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v2/harness-loop/tasks", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "E_SERVICE_UNAVAILABLE" {
		t.Fatalf("code = %v", body["code"])
	}
}

func TestPauseRequiresConfirmation(t *testing.T) {
	engine, h := newRouterWithHarnessLoop(t)
	defer h.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/harness-loop/tasks/t1/pause", strings.NewReader(`{"confirm":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-a")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "E_CONFIRMATION_REQUIRED" {
		t.Fatalf("code = %v", body["code"])
	}
}

func TestCreateTaskRejectsMissingFields(t *testing.T) {
	engine, h := newRouterWithHarnessLoop(t)
	defer h.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/harness-loop/tasks", strings.NewReader(`{"goal":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-a")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestMapStatus(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{status.Error(codes.NotFound, "missing"), http.StatusNotFound, "E_NOT_FOUND"},
		{status.Error(codes.InvalidArgument, "bad"), http.StatusBadRequest, "E_INVALID_ARGUMENT"},
		{status.Error(codes.Aborted, "version conflict"), http.StatusConflict, "E_VERSION_CONFLICT"},
		{status.Error(codes.FailedPrecondition, "E_ILLEGAL_TRANSITION: a->e"), http.StatusConflict, "E_ILLEGAL_TRANSITION"},
		{status.Error(codes.FailedPrecondition, "E_ACCEPTANCE_CONTEXT_INCOMPLETE: missing artifacts"), http.StatusUnprocessableEntity, "E_ACCEPTANCE_CONTEXT_INCOMPLETE"},
		{status.Error(codes.FailedPrecondition, "E_AGGREGATION_MISSING"), http.StatusUnprocessableEntity, "E_AGGREGATION_MISSING"},
		{status.Error(codes.FailedPrecondition, "E_FSM_DISABLED"), http.StatusServiceUnavailable, "E_FSM_DISABLED"},
		{status.Error(codes.FailedPrecondition, "other"), http.StatusPreconditionFailed, "E_PRECONDITION_FAILED"},
		{status.Error(codes.Unavailable, "down"), http.StatusServiceUnavailable, "E_SERVICE_UNAVAILABLE"},
		{status.Error(codes.DeadlineExceeded, "slow"), http.StatusGatewayTimeout, "E_TIMEOUT"},
		{status.Error(codes.Internal, "boom"), http.StatusInternalServerError, "E_INTERNAL"},
		{status.Error(codes.PermissionDenied, "no"), http.StatusForbidden, "E_FORBIDDEN"},
		{status.Error(codes.Unauthenticated, "who"), http.StatusUnauthorized, "E_UNAUTHENTICATED"},
	}
	for _, c := range cases {
		gotStatus, gotCode := mapStatus(c.err)
		if gotStatus != c.status || gotCode != c.code {
			t.Fatalf("%v -> (%d,%s), want (%d,%s)", c.err, gotStatus, gotCode, c.status, c.code)
		}
	}

	if gotStatus, gotCode := mapStatus(assertErr{}); gotStatus != http.StatusInternalServerError || gotCode != "E_INTERNAL" {
		t.Fatalf("plain error -> (%d,%s)", gotStatus, gotCode)
	}
}

type assertErr struct{}

func (assertErr) Error() string { return "plain" }

func TestDescribeTarget(t *testing.T) {
	if got := describeTarget(nil); !strings.Contains(got, "not configured") {
		t.Fatalf("nil config -> %s", got)
	}
	cfg := &config.Config{}
	cfg.Services.HarnessLoop = "harnessloop-service:50008"
	if got := describeTarget(cfg); got != "harnessloop-service:50008" {
		t.Fatalf("target = %s", got)
	}
}
