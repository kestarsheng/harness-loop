package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"agent-platform/internal/cli/tui"
)

func resetOpts() { opts = Options{} }

func TestResolveServerPrecedence(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)
	t.Setenv("HARNESS_SERVER", "")

	if got := resolveServer(fileConfig{}); got != DefaultServerAddr {
		t.Fatalf("default = %s", got)
	}
	if DefaultServerAddr != "127.0.0.1:50013" {
		t.Fatalf("default server must target harnessloop-service: %s", DefaultServerAddr)
	}
	if got := resolveServer(fileConfig{Server: "file:50013"}); got != "file:50013" {
		t.Fatalf("file = %s", got)
	}
	t.Setenv("HARNESS_SERVER", "env:50013")
	if got := resolveServer(fileConfig{Server: "file:50013"}); got != "env:50013" {
		t.Fatalf("env = %s", got)
	}
	opts.Server = "flag:50013"
	if got := resolveServer(fileConfig{Server: "file:50013"}); got != "flag:50013" {
		t.Fatalf("flag = %s", got)
	}
}

func TestResolveTenantPrecedence(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)
	t.Setenv("HARNESS_TENANT", "")

	if got := resolveTenant(fileConfig{}); got != DefaultTenant {
		t.Fatalf("default tenant = %s", got)
	}
	if DefaultTenant != "default" {
		t.Fatalf("default tenant must be default: %s", DefaultTenant)
	}
	if got := resolveTenant(fileConfig{Tenant: "file-tenant"}); got != "file-tenant" {
		t.Fatalf("file tenant: %s", got)
	}
	t.Setenv("HARNESS_TENANT", "env-tenant")
	if got := resolveTenant(fileConfig{Tenant: "file-tenant"}); got != "env-tenant" {
		t.Fatalf("env tenant = %s", got)
	}
	opts.Tenant = "flag-tenant"
	if got := resolveTenant(fileConfig{Tenant: "file-tenant"}); got != "flag-tenant" {
		t.Fatalf("flag tenant = %s", got)
	}
}

func TestResolveWebBaseURLPrecedence(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)
	t.Setenv("HARNESS_WEB_BASE_URL", "")

	if got := resolveWebBaseURL(fileConfig{}); got != tui.DefaultWebBaseURL {
		t.Fatalf("default web base = %s", got)
	}
	if got := resolveWebBaseURL(fileConfig{WebBaseURL: "http://file:5174"}); got != "http://file:5174" {
		t.Fatalf("file web base = %s", got)
	}
	t.Setenv("HARNESS_WEB_BASE_URL", "http://env:5174")
	if got := resolveWebBaseURL(fileConfig{WebBaseURL: "http://file:5174"}); got != "http://env:5174" {
		t.Fatalf("env web base = %s", got)
	}
	opts.WebBaseURL = "http://flag:5174"
	if got := resolveWebBaseURL(fileConfig{WebBaseURL: "http://file:5174"}); got != "http://flag:5174" {
		t.Fatalf("flag web base = %s", got)
	}
}

func TestLoadFileConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "server: file:50013\ntenant: file-tenant\nweb_base_url: http://localhost:9000\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadFileConfig(path)
	if cfg.Server != "file:50013" || cfg.Tenant != "file-tenant" || cfg.WebBaseURL != "http://localhost:9000" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg := loadFileConfig(filepath.Join(dir, "missing.yaml")); cfg.Server != "" || cfg.Tenant != "" || cfg.WebBaseURL != "" {
		t.Fatalf("missing config must be empty: %+v", cfg)
	}
}

func TestExitErrorCode(t *testing.T) {
	err := &ExitError{Code: 3}
	if err.Error() == "" {
		t.Fatal("error string must not be empty")
	}
	if err.Code != 3 {
		t.Fatalf("code = %d", err.Code)
	}
}

func TestIsTerminal(t *testing.T) {
	for _, s := range []string{"completed", "aborted", "failed"} {
		if !isTerminal(s) {
			t.Fatalf("%s must be terminal", s)
		}
	}
	for _, s := range []string{"running", "paused", ""} {
		if isTerminal(s) {
			t.Fatalf("%s must not be terminal", s)
		}
	}
}

func TestDescribeError(t *testing.T) {
	env := describeError(errString("boom"))
	if env.OK || env.Error == nil || env.Error.Message == "" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	env = describeError(errString("E_TENANT_REQUIRED: missing"))
	if env.Error.Code != "E_TENANT_REQUIRED" {
		t.Fatalf("code = %s", env.Error.Code)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
