package compaction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type staticResolver map[string]int

func (s staticResolver) ContextWindow(model string) (int, bool) {
	w, ok := s[model]
	return w, ok
}

func resolver() WindowResolver {
	return staticResolver{
		"qwen-max":   32768,
		"qwen-plus":  131072,
		"qwen-turbo": 8192,
		"gpt-4o":     128000,
	}
}

func TestTriggerUsesRegistryWindow(t *testing.T) {
	tr := NewTrigger(DefaultConfig(), resolver())
	decision := tr.SetModel("qwen-max")
	if decision.Window != 32768 {
		t.Fatalf("window = %d", decision.Window)
	}
	if decision.ThresholdTokens != 26214 {
		t.Fatalf("threshold = %d, want 26214 (32768*0.8)", decision.ThresholdTokens)
	}
	if err := ValidateDecision(decision); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestTriggerRecalculatesAfterModelSwitch(t *testing.T) {
	tr := NewTrigger(DefaultConfig(), resolver())
	small := tr.SetModel("qwen-max")
	large := tr.SetModel("qwen-plus")
	if large.ThresholdTokens <= small.ThresholdTokens {
		t.Fatalf("threshold must grow after switching to a larger window: %d -> %d", small.ThresholdTokens, large.ThresholdTokens)
	}
	if large.ThresholdTokens != 104857 {
		t.Fatalf("threshold = %d, want 104857", large.ThresholdTokens)
	}
	if tr.ThresholdTokens() != large.ThresholdTokens {
		t.Fatal("trigger must keep the recalculated threshold")
	}
}

func TestTriggerFallsBackForUnknownModel(t *testing.T) {
	tr := NewTrigger(DefaultConfig(), resolver())
	decision := tr.SetModel("unknown-model")
	if decision.Window != DefaultFallbackWindow {
		t.Fatalf("window = %d, want fallback %d", decision.Window, DefaultFallbackWindow)
	}
	if decision.ThresholdTokens != 48000 {
		t.Fatalf("threshold = %d, want 48000", decision.ThresholdTokens)
	}
}

func TestTriggerFiresAtThreshold(t *testing.T) {
	tr := NewTrigger(DefaultConfig(), resolver())
	tr.SetModel("qwen-max")

	below := tr.Evaluate(1000)
	if below.Should || below.Reason != "" {
		t.Fatalf("below threshold must not trigger: %+v", below)
	}

	atThreshold := tr.Evaluate(26214)
	if !atThreshold.Should || atThreshold.Reason != ReasonThreshold {
		t.Fatalf("at threshold must trigger: %+v", atThreshold)
	}
	if atThreshold.EstimatedTokens < 26214 {
		t.Fatalf("safety margin must be applied: %d", atThreshold.EstimatedTokens)
	}
}

func TestTriggerSafetyMargin(t *testing.T) {
	if got := ApplySafetyMargin(100, DefaultSafetyMargin); got != 110 {
		t.Fatalf("margin = %d, want 110", got)
	}
	if got := ApplySafetyMargin(0, DefaultSafetyMargin); got != 0 {
		t.Fatalf("zero tokens = %d", got)
	}
	if got := ApplySafetyMargin(100, 0); got != 100 {
		t.Fatalf("no margin = %d", got)
	}
}

func TestTriggerEvaluateText(t *testing.T) {
	tr := NewTrigger(DefaultConfig(), resolver())
	tr.SetModel("qwen-turbo")
	text := strings.Repeat("上下文压缩测试内容。", 2000)
	decision := tr.EvaluateText(text)
	if decision.EstimatedTokens <= 0 {
		t.Fatal("estimate must be positive")
	}
	if !decision.Should {
		t.Fatalf("large text must trigger compaction: %+v", decision)
	}
}

func TestTriggerForceReasons(t *testing.T) {
	tr := NewTrigger(DefaultConfig(), resolver())
	tr.SetModel("qwen-max")

	for _, reason := range []string{ReasonLengthFinish, ReasonManual} {
		decision := tr.Force(reason)
		if !decision.Should || decision.Reason != reason {
			t.Fatalf("force %s -> %+v", reason, decision)
		}
	}
	if got := tr.Force("bogus"); got.Reason != ReasonManual {
		t.Fatalf("unknown force reason must fall back to manual: %q", got.Reason)
	}
}

func TestCompactionBudgetIsIndependent(t *testing.T) {
	tr := NewTrigger(DefaultConfig(), resolver())
	if got := tr.CompactionBudget(100000); got != 10000 {
		t.Fatalf("budget = %d, want 10000 (10%%)", got)
	}
	if got := tr.CompactionBudget(0); got != 0 {
		t.Fatalf("zero budget = %d", got)
	}
	if got := tr.CompactionBudget(1); got != 1 {
		t.Fatalf("tiny budget = %d, want 1", got)
	}
}

func TestConfigNormalization(t *testing.T) {
	cases := []Config{
		{ThresholdRatio: 0, FallbackWindow: 0, SafetyMargin: -1, BudgetRatio: 0},
		{ThresholdRatio: 1.5, FallbackWindow: -1, SafetyMargin: 2, BudgetRatio: 5},
	}
	for _, c := range cases {
		got := c.normalized()
		if got.ThresholdRatio != DefaultThresholdRatio || got.FallbackWindow != DefaultFallbackWindow {
			t.Fatalf("not normalized: %+v", got)
		}
		if got.SafetyMargin != DefaultSafetyMargin || got.BudgetRatio != DefaultBudgetRatio {
			t.Fatalf("not normalized: %+v", got)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compaction.yaml")
	body := "version: 1\nthreshold_ratio: 0.7\nfallback_window: 50000\nsafety_margin: 0.05\nbudget_ratio: 0.2\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ThresholdRatio != 0.7 || cfg.FallbackWindow != 50000 || cfg.BudgetRatio != 0.2 {
		t.Fatalf("config = %+v", cfg)
	}
	if got := thresholdFor(50000, cfg.ThresholdRatio); got != 35000 {
		t.Fatalf("custom ratio threshold = %d", got)
	}

	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing file must fail")
	}
	cfg, err = LoadConfig("")
	if err != nil || cfg.ThresholdRatio != DefaultThresholdRatio {
		t.Fatalf("empty path must return defaults: %+v %v", cfg, err)
	}
}

func TestTriggerEventPayload(t *testing.T) {
	tr := NewTrigger(DefaultConfig(), resolver())
	tr.SetModel("qwen-max")
	decision := tr.Evaluate(30000)
	payload := TriggerEventPayload(decision)
	for _, key := range []string{"model", "window", "threshold_tokens", "reason", "estimated_tokens"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("payload missing %s: %v", key, payload)
		}
	}
	if payload["reason"] != ReasonThreshold {
		t.Fatalf("reason = %v", payload["reason"])
	}
}

func TestValidateDecision(t *testing.T) {
	if err := ValidateDecision(TriggerDecision{Should: true, ThresholdTokens: 100}); err == nil {
		t.Fatal("triggered decision without reason must fail")
	}
	if err := ValidateDecision(TriggerDecision{Should: false, ThresholdTokens: 0}); err == nil {
		t.Fatal("zero threshold must fail")
	}
	if err := ValidateDecision(TriggerDecision{Should: true, Reason: ReasonManual, ThresholdTokens: 1}); err != nil {
		t.Fatalf("valid decision rejected: %v", err)
	}
}

func TestTriggerDecisionEvent(t *testing.T) {
	tr := NewTrigger(DefaultConfig(), resolver())
	tr.SetModel("qwen-max")
	decision := tr.Evaluate(30000)

	ev, err := decision.Event("task-1", "tenant-a", "proj-a", "c", 1)
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if ev.EventType != "compaction.triggered" || ev.Severity != "info" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.Stage != "c" || ev.TenantID != "tenant-a" || ev.TaskID != "task-1" {
		t.Fatalf("event metadata = %+v", ev)
	}
	if !strings.Contains(string(ev.Payload), `"threshold_tokens"`) {
		t.Fatalf("payload = %s", ev.Payload)
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("event must be valid: %v", err)
	}
}
