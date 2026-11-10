package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	c := Default()
	if err := c.Validate(); err == nil {
		t.Fatal("default config without dsn must be invalid")
	}
	c.Postgres.DSN = "postgres://localhost/db"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if c.ListenAddress() != ":50008" {
		t.Fatalf("address = %s", c.ListenAddress())
	}
}

func TestLoadFromFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
server:
  address: ":50999"
postgres:
  dsn: "postgres://file/db"
agent:
  target: "agent:50002"
  entry_agent: "exec"
  stage_model: "qwen-max"
harness:
  max_steps: 10
  graceful_timeout_seconds: 3
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ListenAddress() != ":50999" || c.Postgres.DSN != "postgres://file/db" {
		t.Fatalf("file config not applied: %+v", c)
	}
	if c.Harness.MaxSteps != 10 || c.Agent.StageModel != "qwen-max" {
		t.Fatalf("harness fields not applied: %+v", c)
	}

	t.Setenv("HARNESSLOOP_PG_DSN", "postgres://env/db")
	t.Setenv("HARNESSLOOP_ADDRESS", ":51000")
	t.Setenv("HARNESSLOOP_AGENT_TARGET", "env-agent:50002")
	c, err = Load(path)
	if err != nil {
		t.Fatalf("load with env: %v", err)
	}
	if c.Postgres.DSN != "postgres://env/db" || c.ListenAddress() != ":51000" || c.Agent.Target != "env-agent:50002" {
		t.Fatalf("env override not applied: %+v", c)
	}
}

func TestLocalAddress(t *testing.T) {
	c := Default()
	c.Server.Local = true
	c.Server.Address = ":50008"
	if got := normalizeAddress(c.Server.Address, true); got != "127.0.0.1:50008" {
		t.Fatalf("local address = %s", got)
	}
	if got := normalizeAddress("0.0.0.0:50008", true); got != "0.0.0.0:50008" {
		t.Fatalf("explicit host must be preserved: %s", got)
	}
	if got := normalizeAddress("", false); got != ":50008" {
		t.Fatalf("empty address fallback = %s", got)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing config file must fail")
	}
	if _, err := Load(""); err != nil {
		t.Fatalf("empty path must use defaults: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []func(*Config){
		func(c *Config) { c.Agent.Target = "" },
		func(c *Config) { c.Agent.EntryAgent = "" },
		func(c *Config) { c.Agent.StageModel = "" },
		func(c *Config) { c.Harness.MaxSteps = 0 },
		func(c *Config) { c.Harness.GracefulTimeoutSeconds = 0 },
	}
	for _, mut := range cases {
		c := Default()
		c.Postgres.DSN = "postgres://localhost/db"
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
}

func TestStageTimeoutFallback(t *testing.T) {
	c := Default()
	c.Agent.TimeoutSeconds = 0
	if c.StageTimeout() != 600 {
		t.Fatalf("fallback timeout = %d", c.StageTimeout())
	}
	c.Agent.TimeoutSeconds = 42
	if c.StageTimeout() != 42 {
		t.Fatalf("timeout = %d", c.StageTimeout())
	}
}
