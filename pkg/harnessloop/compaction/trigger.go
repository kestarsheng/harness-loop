package compaction

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	eventpkg "agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/llm"
)

const (
	ReasonThreshold    = "threshold"
	ReasonLengthFinish = "length_finish"
	ReasonManual       = "manual"

	DefaultThresholdRatio = 0.80
	DefaultFallbackWindow = 60000
	DefaultSafetyMargin   = 0.10
	DefaultBudgetRatio    = 0.10
)

type Config struct {
	ThresholdRatio float64 `yaml:"threshold_ratio"`
	FallbackWindow int     `yaml:"fallback_window"`
	SafetyMargin   float64 `yaml:"safety_margin"`
	BudgetRatio    float64 `yaml:"budget_ratio"`
}

func DefaultConfig() Config {
	return Config{
		ThresholdRatio: DefaultThresholdRatio,
		FallbackWindow: DefaultFallbackWindow,
		SafetyMargin:   DefaultSafetyMargin,
		BudgetRatio:    DefaultBudgetRatio,
	}
}

func (c Config) normalized() Config {
	out := c
	if out.ThresholdRatio <= 0 || out.ThresholdRatio >= 1 {
		out.ThresholdRatio = DefaultThresholdRatio
	}
	if out.FallbackWindow <= 0 {
		out.FallbackWindow = DefaultFallbackWindow
	}
	if out.SafetyMargin < 0 || out.SafetyMargin > 0.5 {
		out.SafetyMargin = DefaultSafetyMargin
	}
	if out.BudgetRatio <= 0 || out.BudgetRatio > 1 {
		out.BudgetRatio = DefaultBudgetRatio
	}
	return out
}

type fileFormat struct {
	Version int `yaml:"version"`
	Config  `yaml:",inline"`
}

func LoadConfig(path string) (Config, error) {
	if path == "" {
		return DefaultConfig(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return DefaultConfig(), err
	}
	var f fileFormat
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return DefaultConfig(), err
	}
	return f.Config.normalized(), nil
}

type WindowResolver interface {
	ContextWindow(model string) (int, bool)
}

type TriggerDecision struct {
	Should          bool
	Reason          string
	Model           string
	Window          int
	ThresholdTokens int
	EstimatedTokens int
	WithinWindow    bool
}

type Trigger struct {
	cfg      Config
	resolver WindowResolver
	model    string
	window   int
	limit    int
}

func NewTrigger(cfg Config, resolver WindowResolver) *Trigger {
	return &Trigger{cfg: cfg.normalized(), resolver: resolver}
}

func (t *Trigger) Config() Config { return t.cfg }

func (t *Trigger) SetModel(model string) TriggerDecision {
	t.model = model
	t.window = t.cfg.FallbackWindow
	if t.resolver != nil {
		if window, ok := t.resolver.ContextWindow(model); ok && window > 0 {
			t.window = window
		}
	}
	t.limit = thresholdFor(t.window, t.cfg.ThresholdRatio)
	return TriggerDecision{
		Model:           t.model,
		Window:          t.window,
		ThresholdTokens: t.limit,
	}
}

func (t *Trigger) Model() string { return t.model }

func (t *Trigger) Window() int { return t.window }

func (t *Trigger) ThresholdTokens() int { return t.limit }

func (t *Trigger) Evaluate(estimatedTokens int) TriggerDecision {
	estimate := ApplySafetyMargin(estimatedTokens, t.cfg.SafetyMargin)
	decision := TriggerDecision{
		Model:           t.model,
		Window:          t.window,
		ThresholdTokens: t.limit,
		EstimatedTokens: estimate,
		WithinWindow:    t.window <= 0 || estimate < t.window,
	}
	if t.limit > 0 && estimate >= t.limit {
		decision.Should = true
		decision.Reason = ReasonThreshold
	}
	return decision
}

func (t *Trigger) EvaluateText(text string) TriggerDecision {
	return t.Evaluate(llm.EstimateTokens(text))
}

func (t *Trigger) Force(reason string) TriggerDecision {
	switch reason {
	case ReasonLengthFinish, ReasonManual:
	default:
		reason = ReasonManual
	}
	return TriggerDecision{
		Should:          true,
		Reason:          reason,
		Model:           t.model,
		Window:          t.window,
		ThresholdTokens: t.limit,
	}
}

func (t *Trigger) CompactionBudget(taskBudgetTokens int64) int64 {
	if taskBudgetTokens <= 0 {
		return 0
	}
	budget := int64(float64(taskBudgetTokens) * t.cfg.BudgetRatio)
	if budget < 1 {
		return 1
	}
	return budget
}

func thresholdFor(window int, ratio float64) int {
	if window <= 0 {
		return 0
	}
	return int(float64(window) * ratio)
}

func ApplySafetyMargin(tokens int, margin float64) int {
	if tokens <= 0 {
		return 0
	}
	if margin <= 0 {
		return tokens
	}
	return int(float64(tokens) * (1 + margin))
}

func TriggerEventPayload(decision TriggerDecision) map[string]any {
	payload := map[string]any{
		"model":            decision.Model,
		"window":           decision.Window,
		"threshold_tokens": decision.ThresholdTokens,
		"reason":           decision.Reason,
	}
	if decision.EstimatedTokens > 0 {
		payload["estimated_tokens"] = decision.EstimatedTokens
	}
	return payload
}

func ValidateDecision(decision TriggerDecision) error {
	if decision.Should && decision.Reason == "" {
		return fmt.Errorf("compaction: triggered decision without reason")
	}
	if decision.ThresholdTokens <= 0 {
		return fmt.Errorf("compaction: threshold must be positive")
	}
	return nil
}

func (d TriggerDecision) Event(taskID, tenantID, projectID, stage string, seq int64) (*eventpkg.Event, error) {
	payload, err := json.Marshal(TriggerEventPayload(d))
	if err != nil {
		return nil, fmt.Errorf("compaction: encode event payload: %w", err)
	}
	e := &eventpkg.Event{
		EventID:       fmt.Sprintf("%s-compaction-%d", taskID, time.Now().UnixNano()),
		EventType:     eventpkg.CompactionTriggered,
		SchemaVersion: eventpkg.CurrentSchemaVersion,
		TaskID:        taskID,
		Stage:         stage,
		Seq:           seq,
		Timestamp:     time.Now().UTC(),
		Severity:      eventpkg.SeverityInfo,
		TenantID:      tenantID,
		ProjectID:     projectID,
		Payload:       payload,
	}
	if seq > 0 {
		if err := e.Validate(); err != nil {
			return nil, err
		}
	}
	return e, nil
}
