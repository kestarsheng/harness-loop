package config

import (
	"fmt"
	"net"
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

// SetLocalOnly rewrites the bind address to loopback while keeping the port.
//
// 旧实现把 `-local` 写死成 127.0.0.1:50008：端口矩阵里 50008 不是 harnessloop
// 的端口，本地单机起栈时它要么抢到别人的端口，要么自己 bind 失败后以 exit 1
// 退场——两者都会表现为"服务秒退"。改成"只改主机、保留端口"后，`-local` 与
// HARNESSLOOP_ADDRESS / 配置文件里的端口不再互相打架。
func (c *Config) SetLocalOnly() {
	c.Server.Address = loopbackAddress(c.Server.Address)
	c.Server.Local = true
}

// loopbackAddress keeps the port and forces the host to 127.0.0.1.
func loopbackAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = ":50008"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return net.JoinHostPort("127.0.0.1", port)
}

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

// LLMEnvConfig is the optional compaction-summarizer configuration carried in
// the environment (T-P2-36). It is not part of the yaml file on purpose: the key
// must never be committed, and the swarm stacks inject it per-run.
type LLMEnvConfig struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Configured reports whether a summarizer endpoint was requested at all.
func (c LLMEnvConfig) Configured() bool { return strings.TrimSpace(c.BaseURL) != "" }

// LoadLLMEnv reads HARNESSLOOP_LLM_{BASE_URL,API_KEY,MODEL}.
//
// 全空 = 功能关闭（compaction 走 L1 降级），合法；但只要给了 base_url，缺另外
// 两个就是配置错误，并且必须把缺的变量名写出来。旧代码不校验：它用一个缺 key
// 的 client 继续启动，故障被推到第一次摘要调用，操作者看到的是"进程秒退 +
// stderr 一行"，正是 T-P2-36 的静默 exit(1)。
func LoadLLMEnv(getenv func(string) string) (LLMEnvConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := LLMEnvConfig{
		BaseURL: strings.TrimSpace(getenv("HARNESSLOOP_LLM_BASE_URL")),
		APIKey:  strings.TrimSpace(getenv("HARNESSLOOP_LLM_API_KEY")),
		Model:   strings.TrimSpace(getenv("HARNESSLOOP_LLM_MODEL")),
	}
	if cfg.BaseURL == "" {
		if cfg.APIKey != "" || cfg.Model != "" {
			return cfg, fmt.Errorf("harnessloop llm: HARNESSLOOP_LLM_API_KEY or HARNESSLOOP_LLM_MODEL is set but HARNESSLOOP_LLM_BASE_URL is empty")
		}
		return cfg, nil
	}
	missing := make([]string, 0, 2)
	if cfg.APIKey == "" {
		missing = append(missing, "HARNESSLOOP_LLM_API_KEY")
	}
	if cfg.Model == "" {
		missing = append(missing, "HARNESSLOOP_LLM_MODEL")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("harnessloop llm: HARNESSLOOP_LLM_BASE_URL is set but %s is empty",
			strings.Join(missing, " and "))
	}
	return cfg, nil
}
