package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Address string `yaml:"address"`
		Local   bool   `yaml:"local"`
	} `yaml:"server"`
	Postgres struct {
		DSN         string `yaml:"dsn"`
		MaxConns    int32  `yaml:"max_conns"`
		SlowQueryMs int    `yaml:"slow_query_ms"`
	} `yaml:"postgres"`
	Agent struct {
		Target         string `yaml:"target"`
		EntryAgent     string `yaml:"entry_agent"`
		StageModel     string `yaml:"stage_model"`
		TimeoutSeconds int    `yaml:"timeout_seconds"`
	} `yaml:"agent"`
	Harness struct {
		Enabled                bool `yaml:"enabled"`
		AutoStart              bool `yaml:"auto_start"`
		MaxSteps               int  `yaml:"max_steps"`
		SilentThresholdSeconds int  `yaml:"silent_threshold_seconds"`
		GracefulTimeoutSeconds int  `yaml:"graceful_timeout_seconds"`
	} `yaml:"harness"`
	Policy struct {
		MatrixFile    string `yaml:"matrix_file"`
		ExecAllowFile string `yaml:"exec_allow_file"`
		ApprovalFile  string `yaml:"approval_file"`
	} `yaml:"policy"`
	Approval struct {
		Address        string `yaml:"addr"`
		TimeoutSeconds int    `yaml:"timeout_seconds"`
		PollMs         int    `yaml:"poll_ms"`
	} `yaml:"approval"`
	Models struct {
		File string `yaml:"file"`
	} `yaml:"models"`
	FeatureFlags struct {
		File string `yaml:"file"`
	} `yaml:"feature_flags"`
	Memory struct {
		Addr        string `yaml:"addr"`
		HarnessHome string `yaml:"harness_home"`
	} `yaml:"memory"`
	Retry struct {
		File string `yaml:"file"`
	} `yaml:"retry"`
}

func Default() Config {
	var c Config
	c.Server.Address = ":50008"
	c.Agent.Target = "127.0.0.1:50002"
	c.Agent.EntryAgent = "default"
	c.Agent.StageModel = "qwen-plus"
	c.Agent.TimeoutSeconds = 600
	c.Harness.Enabled = false
	c.Harness.AutoStart = true
	c.Harness.MaxSteps = 200
	c.Harness.SilentThresholdSeconds = 120
	c.Harness.GracefulTimeoutSeconds = 15
	c.Policy.MatrixFile = "config/harness_loop/stage_matrix.yaml"
	c.Policy.ExecAllowFile = "config/harness_loop/exec_allow.yaml"
	c.Policy.ApprovalFile = "config/harness_loop/approval.yaml"
	c.Approval.Address = ""
	c.Approval.TimeoutSeconds = 300
	c.Approval.PollMs = 200
	c.Models.File = "config/harness_loop/models.yaml"
	c.FeatureFlags.File = "config/harness_loop/features.yaml"
	c.Retry.File = "config/harness_loop/retry.yaml"
	return c
}

func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return c, fmt.Errorf("harnessloop config: %w", err)
		}
	}
	if dsn := os.Getenv("HARNESSLOOP_PG_DSN"); dsn != "" {
		c.Postgres.DSN = dsn
	}
	if addr := os.Getenv("HARNESSLOOP_ADDRESS"); addr != "" {
		c.Server.Address = addr
	}
	if target := os.Getenv("HARNESSLOOP_AGENT_TARGET"); target != "" {
		c.Agent.Target = target
	}
	if addr := os.Getenv("HARNESSLOOP_MEMORY_ADDR"); addr != "" {
		c.Memory.Addr = addr
	}
	if home := os.Getenv("HARNESSLOOP_HARNESS_HOME"); home != "" {
		c.Memory.HarnessHome = home
	}
	if addr := os.Getenv("HARNESSLOOP_APPROVAL_ADDR"); addr != "" {
		c.Approval.Address = addr
	}
	if timeout := os.Getenv("HARNESSLOOP_APPROVAL_TIMEOUT_SECONDS"); timeout != "" {
		if v, convErr := strconv.Atoi(timeout); convErr == nil && v > 0 {
			c.Approval.TimeoutSeconds = v
		}
	}
	if poll := os.Getenv("HARNESSLOOP_APPROVAL_POLL_MS"); poll != "" {
		if v, convErr := strconv.Atoi(poll); convErr == nil && v > 0 {
			c.Approval.PollMs = v
		}
	}
	c.Server.Address = normalizeAddress(c.Server.Address, c.Server.Local)
	return c, nil
}

func normalizeAddress(addr string, local bool) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = ":50008"
	}
	if !local {
		return addr
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

func (c Config) ListenAddress() string { return c.Server.Address }

func (c Config) Validate() error {
	if c.Postgres.DSN == "" {
		return fmt.Errorf("harnessloop config: postgres.dsn is required")
	}
	if c.Agent.Target == "" {
		return fmt.Errorf("harnessloop config: agent.target is required")
	}
	if c.Agent.EntryAgent == "" {
		return fmt.Errorf("harnessloop config: agent.entry_agent is required")
	}
	if c.Agent.StageModel == "" {
		return fmt.Errorf("harnessloop config: agent.stage_model is required")
	}
	if c.Harness.MaxSteps <= 0 {
		return fmt.Errorf("harnessloop config: harness.max_steps must be positive")
	}
	if c.Harness.GracefulTimeoutSeconds <= 0 {
		return fmt.Errorf("harnessloop config: harness.graceful_timeout_seconds must be positive")
	}
	if c.Approval.TimeoutSeconds <= 0 {
		return fmt.Errorf("harnessloop config: approval.timeout_seconds must be positive")
	}
	if c.Approval.PollMs <= 0 {
		return fmt.Errorf("harnessloop config: approval.poll_ms must be positive")
	}
	return nil
}

func (c Config) StageTimeout() int {
	if c.Agent.TimeoutSeconds <= 0 {
		return 600
	}
	return c.Agent.TimeoutSeconds
}
