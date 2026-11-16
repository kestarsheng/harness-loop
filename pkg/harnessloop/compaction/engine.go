package compaction

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

var (
	ErrEngineDisabled      = errors.New("compaction: context engine is disabled")
	ErrEngineNotConfigured = errors.New("compaction: context engine slot has no implementation")
)

// 外部压缩引擎（context-compression-engine）选型属于 U16 决策项，在 U16 拍板前
// 本包只提供 mock 适配器（见 engine_adapter.go），不引入任何外部引擎依赖。
//
// 开关语义与其它 runtime 开关一致：默认关闭。
//   - 关闭：engine_fragment 计数恒为 0，L1/L2 单通道路径与事件 schema 零变化。
//   - 打开：仅接入 mock 适配器；产出 engine_fragment 统计标记；适配器失败/超时/输出
//     非法时把 reason 并入 L1 降级，不阻塞任务、不使任务失败。
const (
	EngineNameMock    = "mock"
	EngineNameOff     = "off"
	EngineNameNone    = "none"
	EngineNameUnknown = ""

	// 开关环境变量。对应设计稿中的 compaction.engine.enabled；当前阶段由服务层
	// 读取后注入 EngineAdapterConfig（features.yaml 无独立条目）。
	EngineEnvEnabled   = "COMPACTION_ENGINE_ENABLED"
	EngineEnvName      = "COMPACTION_ENGINE"
	EngineEnvTimeoutMS = "COMPACTION_ENGINE_TIMEOUT_MS"

	DefaultEngineTimeout = 1500 * time.Millisecond
)

// EngineAdapterConfig 是引擎适配器的开关配置。默认值即「关闭 + mock 名称 + 默认超时」，
// 因此在任何调用方不显式构造的情况下都不会产生行为差异。
type EngineAdapterConfig struct {
	Enabled bool
	Name    string
	Timeout time.Duration
}

func DefaultEngineAdapterConfig() EngineAdapterConfig {
	return EngineAdapterConfig{Enabled: false, Name: EngineNameMock, Timeout: DefaultEngineTimeout}
}

func (c EngineAdapterConfig) normalized() EngineAdapterConfig {
	out := c
	if strings.TrimSpace(out.Name) == "" {
		out.Name = EngineNameMock
	}
	out.Name = strings.ToLower(strings.TrimSpace(out.Name))
	if out.Timeout <= 0 {
		out.Timeout = DefaultEngineTimeout
	}
	// Name=off/none 是显式的强制关闭，优先级高于 Enabled。
	if out.Name == EngineNameOff || out.Name == EngineNameNone {
		out.Enabled = false
	}
	return out
}

// ParseEngineAdapterConfig 从注入的取值函数解析开关，便于测试不污染进程环境。
func ParseEngineAdapterConfig(getenv func(string) string) EngineAdapterConfig {
	cfg := DefaultEngineAdapterConfig()
	if getenv == nil {
		return cfg
	}
	cfg.Enabled = parseBool(getenv(EngineEnvEnabled))
	if name := strings.TrimSpace(getenv(EngineEnvName)); name != "" {
		cfg.Name = name
	}
	if raw := strings.TrimSpace(getenv(EngineEnvTimeoutMS)); raw != "" {
		if ms, err := time.ParseDuration(raw + "ms"); err == nil && ms > 0 {
			cfg.Timeout = ms
		}
	}
	return cfg.normalized()
}

func EngineAdapterConfigFromEnv() EngineAdapterConfig {
	return ParseEngineAdapterConfig(os.Getenv)
}

func parseBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

type Fragments struct {
	Lines    []Line
	Messages []string
}

type Compressed struct {
	JSONL []byte
}

type ContextEngine interface {
	Compress(ctx context.Context, fragments Fragments) (Compressed, error)
}

type EngineSlot struct {
	enabled bool
	engine  ContextEngine
}

func NewEngineSlot(enabled bool, engine ContextEngine) EngineSlot {
	return EngineSlot{enabled: enabled, engine: engine}
}

func (s EngineSlot) Enabled() bool { return s.enabled && s.engine != nil }

func (s EngineSlot) Compress(ctx context.Context, fragments Fragments) (Compressed, error) {
	if !s.enabled {
		return Compressed{}, ErrEngineDisabled
	}
	if s.engine == nil {
		return Compressed{}, ErrEngineNotConfigured
	}
	return s.engine.Compress(ctx, fragments)
}
