package handler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"agent-platform/pkg/config"
	harnesspb "agent-platform/pkg/pb/harnessloop"
	"agent-platform/services/gateway/internal/middleware"
)

type auditFakeLoop struct {
	harnesspb.UnimplementedHarnessLoopServiceServer

	mu    sync.Mutex
	tasks map[string]*harnesspb.TaskView
}

func newAuditFakeLoop() *auditFakeLoop {
	return &auditFakeLoop{tasks: map[string]*harnesspb.TaskView{}}
}

func (s *auditFakeLoop) CreateTask(_ context.Context, req *harnesspb.CreateTaskRequest) (*harnesspb.CreateTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := &harnesspb.TaskView{
		TaskId:        "task-" + req.GetGoal(),
		TenantId:      req.GetTenantId(),
		ProjectId:     req.GetProjectId(),
		Goal:          req.GetGoal(),
		SessionId:     req.GetSessionId(),
		TerminalLabel: req.GetTerminalLabel(),
		Status:        "running",
		CurrentStage:  "a",
	}
	s.tasks[task.TaskId] = task
	return &harnesspb.CreateTaskResponse{Task: task}, nil
}

func (s *auditFakeLoop) ListTasks(_ context.Context, req *harnesspb.ListTasksRequest) (*harnesspb.ListTasksResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &harnesspb.ListTasksResponse{}
	for _, task := range s.tasks {
		if task.TenantId == req.GetTenantId() {
			resp.Tasks = append(resp.Tasks, task)
		}
	}
	return resp, nil
}

func (s *auditFakeLoop) get(tenantID, taskID string) (*harnesspb.TaskView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok || task.TenantId != tenantID {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	return task, nil
}

func (s *auditFakeLoop) PauseTask(_ context.Context, req *harnesspb.TaskCommand) (*harnesspb.TaskView, error) {
	return s.get(req.GetTenantId(), req.GetTaskId())
}

func (s *auditFakeLoop) ResumeTask(_ context.Context, req *harnesspb.ResumeTaskRequest) (*harnesspb.TaskView, error) {
	return s.get(req.GetTenantId(), req.GetTaskId())
}

func (s *auditFakeLoop) AbortTask(_ context.Context, req *harnesspb.TaskCommand) (*harnesspb.TaskView, error) {
	return s.get(req.GetTenantId(), req.GetTaskId())
}

type auditTestEnv struct {
	engine *gin.Engine
	audit  *AuditLog
	fake   *auditFakeLoop
}

func newAuditTestEnv(t *testing.T) *auditTestEnv {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	fake := newAuditFakeLoop()
	harnesspb.RegisterHarnessLoopServiceServer(grpcServer, fake)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	cfg := &config.Config{}
	cfg.Services.HarnessLoop = lis.Addr().String()
	h := NewHarnessLoopHandler(cfg)
	t.Cleanup(h.Close)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api/v2")
	api.Use(middleware.Tenant())
	api.Use(middleware.StrictTenantForPaths(nil))
	api.Use(middleware.Audit(h.Audit(), AuditRules()))

	loop := api.Group("/harness-loop")
	loop.POST("/tasks", h.CreateTask)
	loop.GET("/tasks", h.ListTasks)
	loop.GET("/tasks/:id", h.GetTask)
	loop.POST("/tasks/:id/pause", h.PauseTask)
	loop.POST("/tasks/:id/resume", h.ResumeTask)
	loop.POST("/tasks/:id/abort", h.AbortTask)
	loop.POST("/checkpoints/restore", h.RestoreCheckpoint)
	loop.GET("/audit", h.ListAudit)
	loop.DELETE("/audit", h.DeleteAudit)

	// Stand-ins for downstream services: audit only needs a terminal status.
	api.DELETE("/memory/:id", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	api.DELETE("/knowledge/documents/:id", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	api.PUT("/memory/forgetting/config", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	return &auditTestEnv{engine: engine, audit: h.Audit(), fake: fake}
}

func (e *auditTestEnv) do(t *testing.T, method, path, tenant, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	e.engine.ServeHTTP(rec, req)
	return rec
}

func TestAuditRecordsLifecycleOperations(t *testing.T) {
	env := newAuditTestEnv(t)
	headers := map[string]string{"X-User-ID": "alice", "X-Trace-ID": "trace-1"}

	if rec := env.do(t, http.MethodPost, "/api/v2/harness-loop/tasks", "tenant-a", `{"goal":"t1","workspace_root":"/w"}`, headers); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v2/harness-loop/tasks/t1/pause", `{"confirm":true,"reason":"ops"}`},
		{http.MethodPost, "/api/v2/harness-loop/tasks/t1/resume", `{}`},
		{http.MethodPost, "/api/v2/harness-loop/tasks/t1/abort", `{"confirm":true,"reason":"ops"}`},
		{http.MethodPost, "/api/v2/harness-loop/checkpoints/restore", `{"task_id":"t1"}`},
		{http.MethodDelete, "/api/v2/memory/m1", `{"confirm":true}`},
		{http.MethodDelete, "/api/v2/knowledge/documents/d1", `{"confirm":true}`},
		{http.MethodPut, "/api/v2/memory/forgetting/config", `{"confirm":true}`},
	}
	for _, c := range cases {
		env.do(t, c.method, c.path, "tenant-a", c.body, headers)
	}

	rec := env.do(t, http.MethodGet, "/api/v2/harness-loop/audit?limit=50", "tenant-a", "", headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit query status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []AuditEntry `json:"entries"`
		Total   int          `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	wantActions := map[string]bool{
		AuditActionStart: false, AuditActionPause: false, AuditActionResume: false,
		AuditActionAbort: false, AuditActionRebuild: false, AuditActionDelete: false,
		AuditActionConfigChange: false,
	}
	for _, entry := range body.Entries {
		if _, ok := wantActions[entry.Action]; !ok {
			continue
		}
		wantActions[entry.Action] = true
		if entry.Operator != "alice" {
			t.Fatalf("operator = %q", entry.Operator)
		}
		if entry.TenantID != "tenant-a" {
			t.Fatalf("tenant = %q", entry.TenantID)
		}
		if entry.Time.IsZero() {
			t.Fatal("audit entry must carry a timestamp")
		}
		if entry.SourceIP == "" {
			t.Fatal("audit entry must carry the source ip")
		}
		if entry.Object == "" {
			t.Fatal("audit entry must carry the object")
		}
	}
	for action, seen := range wantActions {
		if !seen {
			t.Fatalf("action %s was not audited: %+v", action, body.Entries)
		}
	}
	if body.Total < len(cases)+1 {
		t.Fatalf("total = %d, want at least %d", body.Total, len(cases)+1)
	}
}

func TestAuditRejectsDeletion(t *testing.T) {
	env := newAuditTestEnv(t)

	rec := env.do(t, http.MethodDelete, "/api/v2/harness-loop/audit", "tenant-a", "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["code"] != "E_AUDIT_IMMUTABLE" {
		t.Fatalf("code = %v", body["code"])
	}

	entries := env.audit.List("tenant-a", AuditActionDelete, "", 10)
	if len(entries) != 1 || entries[0].Result != "denied" {
		t.Fatalf("deletion attempt must be audited as denied: %+v", entries)
	}
}

func TestAuditConfirmFalseIsRejected(t *testing.T) {
	env := newAuditTestEnv(t)
	env.do(t, http.MethodPost, "/api/v2/harness-loop/tasks", "tenant-a", `{"goal":"t1","workspace_root":"/w"}`, nil)

	rec := env.do(t, http.MethodPost, "/api/v2/harness-loop/tasks/t1/pause", "tenant-a", `{"confirm":false}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "E_CONFIRMATION_REQUIRED") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	entries := env.audit.List("tenant-a", AuditActionPause, "", 10)
	if len(entries) != 1 || entries[0].Result != "denied" || entries[0].Confirmed != "false" {
		t.Fatalf("rejected confirmation must be audited: %+v", entries)
	}
}

func TestAuditCrossTenantIsolation(t *testing.T) {
	env := newAuditTestEnv(t)
	env.do(t, http.MethodPost, "/api/v2/harness-loop/tasks", "tenant-a", `{"goal":"t1","workspace_root":"/w"}`, nil)

	// Cross-tenant write attempt: the upstream answers NotFound, the gateway
	// records it as a denied operation for the caller's tenant.
	rec := env.do(t, http.MethodPost, "/api/v2/harness-loop/tasks/t1/pause", "tenant-b", `{"confirm":true}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant pause status = %d, want 404", rec.Code)
	}
	if entries := env.audit.List("tenant-b", AuditActionPause, "", 10); len(entries) != 1 || entries[0].Result != "denied" {
		t.Fatalf("cross-tenant attempt must be audited as denied: %+v", entries)
	}

	// Cross-tenant reads return an empty result instead of leaking other tenants.
	rec = env.do(t, http.MethodGet, "/api/v2/harness-loop/tasks", "tenant-b", "", nil)
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(list.Tasks) != 0 {
		t.Fatalf("cross-tenant list must be empty: %s", rec.Body.String())
	}
	if entries := env.audit.List("tenant-b", AuditActionStart, "", 10); len(entries) != 0 {
		t.Fatalf("tenant-b must not see tenant-a audit entries: %+v", entries)
	}
}

func TestAuditLogAppendOnlyAndScoped(t *testing.T) {
	log := NewAuditLog(4)
	for i := 0; i < 6; i++ {
		log.Record(middleware.AuditRecord{TenantID: "t", Action: AuditActionStart, Result: "success", Object: "task:x"})
	}
	if got := log.Count("t"); got != 4 {
		t.Fatalf("ring capacity not enforced: %d", got)
	}
	if got := log.Count("other"); got != 0 {
		t.Fatalf("tenant scoping broken: %d", got)
	}
}

func TestStrictTenantPathsConfigurable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	engine.Use(middleware.StrictTenantForPaths(nil))
	engine.GET("/api/v2/harness-loop/tasks", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.GET("/api/v2/harness-loop/monitor/stream", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.GET("/api/v2/memory/legacy", func(c *gin.Context) { c.Status(http.StatusOK) })

	cases := []struct {
		path string
		want int
	}{
		{"/api/v2/harness-loop/tasks", http.StatusUnauthorized},
		{"/api/v2/harness-loop/monitor/stream", http.StatusOK},
		{"/api/v2/memory/legacy", http.StatusOK},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		engine.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s status = %d, want %d", c.path, rec.Code, c.want)
		}
	}
}
