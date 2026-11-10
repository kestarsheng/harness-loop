package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func resetOpts() { opts = Options{} }

func TestResolveServerPrecedence(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)

	if got := resolveServer(fileConfig{}); got != "127.0.0.1:50008" {
		t.Fatalf("default = %s", got)
	}
	if got := resolveServer(fileConfig{Server: "file:50008"}); got != "file:50008" {
		t.Fatalf("file = %s", got)
	}
	t.Setenv("HARNESS_SERVER", "env:50008")
	if got := resolveServer(fileConfig{Server: "file:50008"}); got != "env:50008" {
		t.Fatalf("env = %s", got)
	}
	opts.Server = "flag:50008"
	if got := resolveServer(fileConfig{Server: "file:50008"}); got != "flag:50008" {
		t.Fatalf("flag = %s", got)
	}
}

func TestResolveTenant(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)

	if _, err := resolveTenant(fileConfig{}); err == nil {
		t.Fatal("missing tenant must fail")
	}
	if got, err := resolveTenant(fileConfig{Tenant: "file-tenant"}); err != nil || got != "file-tenant" {
		t.Fatalf("file tenant: %s %v", got, err)
	}
	t.Setenv("HARNESS_TENANT", "env-tenant")
	if got, _ := resolveTenant(fileConfig{Tenant: "file-tenant"}); got != "env-tenant" {
		t.Fatalf("env tenant = %s", got)
	}
	opts.Tenant = "flag-tenant"
	if got, _ := resolveTenant(fileConfig{Tenant: "file-tenant"}); got != "flag-tenant" {
		t.Fatalf("flag tenant = %s", got)
	}
}

func TestLoadFileConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "server: file:50008\ntenant: file-tenant\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadFileConfig(path)
	if cfg.Server != "file:50008" || cfg.Tenant != "file-tenant" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg := loadFileConfig(filepath.Join(dir, "missing.yaml")); cfg.Server != "" || cfg.Tenant != "" {
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
