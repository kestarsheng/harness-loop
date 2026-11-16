package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// T-P2-05 引擎适配（二期可选，U16 未拍板 → 仅 mock）。
// 本文件只验证三件事：开关默认关闭、打开 mock 产 engine_fragment、失败按 L1 降级。

func engineAdapterHistory() History {
	return History{
		Goal:        "实现压缩引擎适配 T-P2-05",
		Phase:       "impl",
		Done:        []string{"mock 适配器", "开关"},
		Todo:        []string{"等 U16 选型结论"},
		Constraints: []string{"不引入外部引擎依赖"},
		Background:  []string{"U16 未拍板"},
	}
}

func engineAdapterMessages() []ChatMessage {
	msgs := []ChatMessage{
		{Role: "system", Content: strings.Repeat("系统提示。", 200)},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "call-1", Content: strings.Repeat("工具输出。", 400)},
	}
	for i := 0; i < 12; i++ {
		msgs = append(msgs, ChatMessage{Role: "user", Content: strings.Repeat("历史消息内容。", 60)})
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: "收到，继续下一步。"})
	}
	return msgs
}

type engineAdapterStub struct {
	res    EngineResult
	err    error
	delay  time.Duration
	called int
}

func (s *engineAdapterStub) Compact(ctx context.Context, req EngineRequest) (EngineResult, error) {
	s.called++
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return EngineResult{}, ctx.Err()
		}
	}
	return s.res, s.err
}

func engineAdapterEnabledConfig() EngineAdapterConfig {
	cfg := DefaultEngineAdapterConfig()
	cfg.Enabled = true
	cfg.Timeout = 200 * time.Millisecond
	return cfg
}

func TestEngineAdapterSwitchDefaultsOff(t *testing.T) {
	cfg := DefaultEngineAdapterConfig()
	if cfg.Enabled {
		t.Fatal("engine adapter must be disabled by default")
	}
	slot := NewMockEngineSlot(cfg)
	if slot.Enabled() {
		t.Fatal("default slot must be disabled even with mock attached")
	}
	out := RunEngineAdapter(context.Background(), slot, EngineRequest{Messages: engineAdapterMessages(), BudgetTokens: 1200}, engineAdapterHistory(), DefaultDegradeLimits())
	if out.Applied || out.Fragments != 0 || out.Degraded {
		t.Fatalf("closed switch must be a no-op: %+v", out)
	}
	if out.Reason != EngineReasonDisabled {
		t.Fatalf("reason = %q", out.Reason)
	}
}

func TestEngineAdapterSwitchFromEnv(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	if cfg := ParseEngineAdapterConfig(getenv); cfg.Enabled {
		t.Fatal("empty env must not enable the adapter")
	}
	env[EngineEnvEnabled] = "1"
	env[EngineEnvName] = "mock"
	if cfg := ParseEngineAdapterConfig(getenv); !cfg.Enabled || cfg.Name != EngineNameMock {
		t.Fatalf("env switch not honored: %+v", cfg)
	}
	// Name=off 是显式强制关闭，优先级高于 Enabled。
	env[EngineEnvName] = "off"
	if cfg := ParseEngineAdapterConfig(getenv); cfg.Enabled {
		t.Fatal("name=off must force-disable the adapter")
	}
	// 超时可通过环境变量覆盖。
	env[EngineEnvName] = "mock"
	env[EngineEnvTimeoutMS] = "250"
	if cfg := ParseEngineAdapterConfig(getenv); cfg.Timeout != 250*time.Millisecond {
		t.Fatalf("timeout = %v", cfg.Timeout)
	}
}

func TestEngineAdapterOffKeepsZeroFragment(t *testing.T) {
	messages := engineAdapterMessages()
	h := engineAdapterHistory()
	limits := DefaultDegradeLimits()

	// 关闭开关：单通道路径与直接调用 DegradeL1/L2 完全一致。
	out := RunEngineAdapter(context.Background(), NewMockEngineSlot(DefaultEngineAdapterConfig()),
		EngineRequest{Phase: h.Phase, Messages: messages, BudgetTokens: 1200}, h, limits)
	if out.Applied {
		t.Fatal("closed switch must not apply any engine output")
	}
	l1, err := DegradeL1(messages, h, limits)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := DegradeL2(messages, h, limits)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range []DegradeResult{l1, l2} {
		if fragments := CountEngineFragments(result.Lines); fragments != 0 {
			t.Fatalf("engine_fragment count = %d, want 0", fragments)
		}
	}
	if fragments := CountEngineFragments(l1.Lines); fragments != 0 {
		t.Fatalf("engine_fragment must stay 0 when switch is closed, got %d", fragments)
	}
	t.Logf("开关关闭：applied=%v degraded=%v engine_fragment=%d tokens=%d→%d", out.Applied, out.Degraded,
		CountEngineFragments(l1.Lines), l1.TokensBefore, l1.TokensAfter)

	run, err := BuildRun("task-off", "tenant", "project", VariantTemplateV1, TemplateV1, ReasonThreshold,
		l1.TokensBefore, l1.TokensAfter, 1.0, 1.0, false, "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	payload := CompactionEventPayload(run)
	if _, ok := payload["engine_fragment"]; ok {
		t.Fatal("closed switch must not add engine_fragment to the event payload")
	}
}

func TestEngineAdapterMockProducesFragment(t *testing.T) {
	h := engineAdapterHistory()
	messages := engineAdapterMessages()
	before := estimateMessages(messages)
	slot := NewMockEngineSlot(engineAdapterEnabledConfig())
	if !slot.Enabled() {
		t.Fatal("enabled slot must be usable")
	}

	out := RunEngineAdapter(context.Background(), slot,
		EngineRequest{Phase: h.Phase, Messages: messages, BudgetTokens: 1200}, h, DefaultDegradeLimits())
	if !out.Applied || out.Degraded {
		t.Fatalf("mock must apply cleanly: %+v", out)
	}
	if out.Fragments <= 0 {
		t.Fatalf("engine_fragment count = %d, want > 0", out.Fragments)
	}
	if out.Engine != EngineNameMock {
		t.Fatalf("engine = %q", out.Engine)
	}
	if after := estimateMessages(out.Messages); after > 1200 {
		t.Fatalf("tokens_after = %d exceeds budget 1200", after)
	}
	if after := estimateMessages(out.Messages); after >= before {
		t.Fatalf("mock must actually compress: before=%d after=%d", before, after)
	}
	if issues := ValidatePairing(out.Messages); len(issues) > 0 {
		t.Fatalf("mock output must keep tool pairing: %v", issues)
	}
	t.Logf("开关打开(mock)：engine=%s engine_fragment=%d tokens=%d→%d messages=%d→%d", out.Engine,
		out.Fragments, before, estimateMessages(out.Messages), len(messages), len(out.Messages))
	if err := ValidateFiveElements(PlaceholderLines(h)); err != nil {
		t.Fatal(err)
	}

	// engine_fragment 只作为已有统计字段，不新增事件类型。
	run := CompactionRun{TaskID: "t", Variant: VariantTemplateV1, PromptVersion: string(TemplateV1)}.
		WithEngineFragments(out.Engine, out.Fragments)
	payload := CompactionEventPayload(run)
	if payload["engine_fragment"] != out.Fragments {
		t.Fatalf("payload engine_fragment = %v, want %d", payload["engine_fragment"], out.Fragments)
	}
	if payload["engine_name"] != EngineNameMock {
		t.Fatalf("payload engine_name = %v", payload["engine_name"])
	}
}

func TestEngineAdapterFailureDegradesToL1(t *testing.T) {
	h := engineAdapterHistory()
	messages := engineAdapterMessages()
	limits := DefaultDegradeLimits()
	stub := &engineAdapterStub{err: errors.New("engine adapter failed")}
	out := RunEngineAdapter(context.Background(), NewEngineAdapterSlot(engineAdapterEnabledConfig(), stub),
		EngineRequest{Phase: h.Phase, Messages: messages, BudgetTokens: 1200}, h, limits)

	if stub.called != 1 {
		t.Fatalf("adapter calls = %d", stub.called)
	}
	if !out.Applied || !out.Degraded || out.DegradeLevel != DegradeLevelL1 {
		t.Fatalf("failure must fall back to L1: %+v", out)
	}
	if !strings.Contains(out.Reason, DegradeReasonEngineAdapter) || !strings.Contains(out.Reason, EngineReasonFailed) {
		t.Fatalf("reason must merge adapter failure: %q", out.Reason)
	}
	if out.Fragments != 0 {
		t.Fatalf("degraded outcome must not count engine fragments: %d", out.Fragments)
	}
	if err := ValidateDegradeResult(out.Degrade); err != nil {
		t.Fatalf("L1 fallback must stay valid: %v", err)
	}
	if issues := ValidatePairing(out.Messages); len(issues) > 0 {
		t.Fatalf("fallback messages pairing: %v", issues)
	}
	t.Logf("注入适配器失败：level=%s reason=%s engine_fragment=%d 任务未失败(err=nil)", out.DegradeLevel, out.Reason, out.Fragments)
}

func TestEngineAdapterTimeoutDegradesToL1(t *testing.T) {
	h := engineAdapterHistory()
	cfg := engineAdapterEnabledConfig()
	cfg.Timeout = 40 * time.Millisecond
	stub := &engineAdapterStub{delay: 500 * time.Millisecond}
	start := time.Now()
	out := RunEngineAdapter(context.Background(), NewEngineAdapterSlot(cfg, stub),
		EngineRequest{Phase: h.Phase, Messages: engineAdapterMessages(), BudgetTokens: 1200}, h, DefaultDegradeLimits())
	elapsed := time.Since(start)

	if elapsed > 400*time.Millisecond {
		t.Fatalf("adapter timeout must not block the task: elapsed=%v", elapsed)
	}
	if !out.Degraded || out.DegradeLevel != DegradeLevelL1 {
		t.Fatalf("timeout must fall back to L1: %+v", out)
	}
	if !strings.Contains(out.Reason, EngineReasonTimeout) {
		t.Fatalf("reason = %q", out.Reason)
	}
}

func TestEngineAdapterInvalidOutputDegradesToL1(t *testing.T) {
	h := engineAdapterHistory()
	messages := engineAdapterMessages()
	cases := map[string]EngineResult{
		"empty":         {},
		"bad role":      {Messages: []ChatMessage{{Role: "robot", Content: "x"}}},
		"orphan tool":   {Messages: []ChatMessage{{Role: "tool", ToolCallID: "missing", Content: "x"}}},
		"over budget":   {Messages: makeOversizedMessages()},
		"no fragments?": {Messages: []ChatMessage{{Role: "user", Content: "ok"}}},
	}
	for name, res := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &engineAdapterStub{res: res}
			out := RunEngineAdapter(context.Background(), NewEngineAdapterSlot(engineAdapterEnabledConfig(), stub),
				EngineRequest{Phase: h.Phase, Messages: messages, BudgetTokens: 200}, h, DefaultDegradeLimits())
			if name == "no fragments?" {
				// 合法输出：slot 会补正 Fragments=1，不应降级。
				if !out.Applied || out.Degraded || out.Fragments == 0 {
					t.Fatalf("legal output must be applied: %+v", out)
				}
				return
			}
			if !out.Degraded || out.DegradeLevel != DegradeLevelL1 {
				t.Fatalf("invalid output must fall back to L1: %+v", out)
			}
			if !strings.Contains(out.Reason, EngineReasonInvalidOutput) {
				t.Fatalf("reason = %q", out.Reason)
			}
		})
	}
}

func makeOversizedMessages() []ChatMessage {
	msgs := []ChatMessage{{Role: "system", Content: "keep"}}
	for i := 0; i < 40; i++ {
		msgs = append(msgs, ChatMessage{Role: "user", Content: strings.Repeat("超长内容。", 200)})
	}
	return msgs
}

func TestEngineAdapterEventSchemaUnchanged(t *testing.T) {
	// 关闭 → payload 键集合与既有字段完全一致（无 engine_* 键）。
	off, err := BuildRun("task-schema-off", "tenant", "project", VariantTemplateV1, TemplateV1, ReasonThreshold,
		1000, 300, 1.0, 1.0, false, "", "ref.jsonl", 7)
	if err != nil {
		t.Fatal(err)
	}
	payload := CompactionEventPayload(off)
	for _, key := range []string{"trigger_reason", "tokens_before", "tokens_after", "ratio", "duration_ms", "degraded", "strategy_id", "prompt_version", "output_ref"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("off payload missing %s", key)
		}
	}
	if len(payload) != 9 {
		t.Fatalf("off payload must not grow: %v", payload)
	}
	// 打开 → 仅追加统计字段，事件类型仍是 compaction.triggered。
	on := off.WithEngineFragments(EngineNameMock, 1)
	onPayload := CompactionEventPayload(on)
	if len(onPayload) != len(payload)+2 {
		t.Fatalf("on payload = %v", onPayload)
	}
	if onPayload["engine_fragment"] != 1 {
		t.Fatalf("engine_fragment = %v", onPayload["engine_fragment"])
	}
}

func TestEngineAdapterReservedFragmentLineStillRejected(t *testing.T) {
	// 边界：engine_fragment 只做统计标记，phase one 的 JSONL 行类型仍被拒绝。
	lines := PlaceholderLines(engineAdapterHistory())
	fragmented := append(append([]Line{}, lines...),
		Line{Type: LineEngineFragment, Phase: "impl", Summary: "mock", SourceRefs: []string{"engine:mock"}})
	if err := ValidateFiveElements(fragmented); !errors.Is(err, ErrInvalidJSONL) {
		t.Fatalf("engine_fragment line must stay rejected in phase one: %v", err)
	}
	if got := CountEngineFragments(fragmented); got != 1 {
		t.Fatalf("count = %d", got)
	}
}
