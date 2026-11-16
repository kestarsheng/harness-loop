package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"agent-platform/pkg/config"
)

func newAuditEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	cfg := &config.Config{}
	// Handlers dial lazily; non-empty targets keep constructor validation happy
	// without requiring downstream services in unit tests.
	cfg.Services = config.ServicesConfig{
		Chat:        "127.0.0.1:1",
		Knowledge:   "127.0.0.1:1",
		Memory:      "127.0.0.1:1",
		A2A:         "127.0.0.1:1",
		MCP:         "127.0.0.1:1",
		Harness:     "127.0.0.1:1",
		Agent:       "127.0.0.1:1",
		HarnessLoop: "127.0.0.1:1",
	}
	Setup(engine, cfg)
	return engine
}

func TestStrictTenantAndAuditWiring(t *testing.T) {
	engine := newAuditEngine(t)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/harness-loop/tasks", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant header status = %d, want 401", rec.Code)
	}

	// The SSE monitor stream is excluded from the strict-tenant guard because
	// EventSource cannot set headers. It must not answer 401.
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/harness-loop/monitor/stream", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("monitor stream must stay outside the strict tenant guard")
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v2/harness-loop/audit", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("audit deletion status = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v2/harness-loop/audit", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit query status = %d, want 200", rec.Code)
	}
}

func TestStrictTenantPathsOverride(t *testing.T) {
	t.Setenv("GATEWAY_STRICT_TENANT_PATHS", "/api/v2/harness-loop,/api/v2/memory")
	paths := strictTenantPaths()
	want := []string{"/api/v2/harness-loop", "/api/v2/memory"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}

	t.Setenv("GATEWAY_STRICT_TENANT_PATHS", "")
	if got := strictTenantPaths(); len(got) == 0 || got[0] != "/api/v2/harness-loop" {
		t.Fatalf("default paths = %v", got)
	}
}
