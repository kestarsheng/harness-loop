package modelregistry

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

const fallbackWindowRegistry = `
version: 1
models:
  - id: mid
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 8192
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.004
    price_out_per_1k: 0.012
    status: available
  - id: large
    provider: p
    kind: chat
    context_window: 131072
    max_output_tokens: 8192
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.02
    price_out_per_1k: 0.06
    status: available
defaults:
  task_model: mid
  strategy: balanced
  fallback: [large]
`

func loadFallbackWindowRegistry(t *testing.T) *Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte(fallbackWindowRegistry), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := Load(path)
	if err != nil {
		t.Fatalf("load fallback registry: %v", err)
	}
	return reg
}

func TestFallbackRateLimitSwitchesAndRecalculates(t *testing.T) {
	reg := loadFallbackWindowRegistry(t)
	chain := NewFallbackChain(reg, DefaultFallbackConfig())
	var calls []string
	call := func(ctx context.Context, modelID string) CallOutcome {
		calls = append(calls, modelID)
		if modelID == "mid" {
			return CallOutcome{Kind: FailureRateLimit, Err: errors.New("429 rate limited"), TokensIn: 10}
		}
		return CallOutcome{TokensIn: 100, TokensOut: 20}
	}
	budget := &Budget{LimitTokens: 100000}
	res, err := chain.Execute(context.Background(), FallbackRequest{
		TaskID: "task-f",
		Stage:  "c",
		Primary: "mid",
		Requirement: FallbackRequirement{
			RequireToolCall: true,
		},
		Budget:           budget,
		TaskBudgetTokens: 100000,
	}, call)
	if err != nil {
		t.Fatalf("fallback run: %v", err)
	}
	if !res.Switched || res.Model == nil || res.Model.ID != "large" {
		t.Fatalf("must switch to large: %+v", res)
	}
	if res.Trigger != TriggerRateLimit || res.AttemptNo != 1 {
		t.Fatalf("trigger/attempt = %s/%d", res.Trigger, res.AttemptNo)
	}
	if len(calls) != 2 || calls[0] != "mid" || calls[1] != "large" {
		t.Fatalf("calls = %v", calls)
	}
	wantThreshold := int(math.Floor(float64(131072) * DefaultCompactionRatio))
	oldThreshold := int(math.Floor(float64(32768) * DefaultCompactionRatio))
	if res.ThresholdTokens != wantThreshold {
		t.Fatalf("threshold = %d, want %d", res.ThresholdTokens, wantThreshold)
	}
	if res.ThresholdTokens <= oldThreshold {
		t.Fatalf("threshold did not move forward: %d", res.ThresholdTokens)
	}
	if res.MemoryInjectBudget <= 0 || res.CompactionBudget <= 0 {
		t.Fatalf("window budget not recalculated: %+v", res)
	}
	if budget.UsedTokens != 130 {
		t.Fatalf("budget tokens = %d", budget.UsedTokens)
	}
	var switched, fallback, applied bool
	for _, event := range res.Events {
		switch event.Type {
		case EventModelSwitched:
			switched = true
			if event.Payload["from"] != "mid" || event.Payload["to"] != "large" {
				t.Fatalf("switch payload = %v", event.Payload)
			}
		case EventModelFallback:
			fallback = true
			if event.Payload["trigger"] != TriggerRateLimit {
				t.Fatalf("fallback payload = %v", event.Payload)
			}
		case EventModelConfigApplied:
			applied = true
		}
	}
	if !switched || !fallback || !applied {
		t.Fatalf("events missing: switched=%v fallback=%v applied=%v", switched, fallback, applied)
	}
}

func TestFallbackStopsAfterMaxPerStage(t *testing.T) {
	reg := loadFallbackWindowRegistry(t)
	cfg := DefaultFallbackConfig()
	cfg.CandidatesPerStage = 1
	chain := NewFallbackChain(reg, cfg)
	callCounts := map[string]int{}
	call := func(ctx context.Context, modelID string) CallOutcome {
		callCounts[modelID]++
		return CallOutcome{Kind: FailureRateLimit, Err: errors.New("429")}
	}
	req := FallbackRequest{
		TaskID:      "task-f",
		Stage:       "c",
		Primary:     "mid",
		Requirement: FallbackRequirement{RequireToolCall: true},
	}
	for i := 0; i < 4; i++ {
		res, err := chain.Execute(context.Background(), req, call)
		if i < 3 {
			if err == nil {
				t.Fatalf("run %d must fail", i)
			}
			if res.StageFallbacks != i+1 {
				t.Fatalf("run %d stage fallbacks = %d", i, res.StageFallbacks)
			}
			continue
		}
		if !res.Exhausted || err == nil {
			t.Fatalf("fourth run must stop without calling: %+v %v", res, err)
		}
		var fallbackErr *FallbackError
		if !errors.As(err, &fallbackErr) || fallbackErr.Code != CodeFallbackExhausted {
			t.Fatalf("error = %v", err)
		}
	}
	if callCounts["mid"] != 4 {
		t.Fatalf("primary calls = %d, want 4", callCounts["mid"])
	}
	if callCounts["large"] != 3 {
		t.Fatalf("fallback calls = %d, want 3", callCounts["large"])
	}
	if chain.StageFallbacks("c") != 3 {
		t.Fatalf("stage fallbacks = %d", chain.StageFallbacks("c"))
	}
}

func TestFallbackBudgetCountsFallbackCalls(t *testing.T) {
	reg := loadFallbackWindowRegistry(t)
	chain := NewFallbackChain(reg, DefaultFallbackConfig())
	call := func(ctx context.Context, modelID string) CallOutcome {
		if modelID == "mid" {
			return CallOutcome{Kind: FailureRateLimit, Err: errors.New("429"), TokensIn: 100}
		}
		return CallOutcome{TokensIn: 200, TokensOut: 50}
	}
	budget := &Budget{LimitTokens: 1000}
	res, err := chain.Execute(context.Background(), FallbackRequest{
		TaskID:  "task-f",
		Stage:   "c",
		Primary: "mid",
		Budget:  budget,
	}, call)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Switched {
		t.Fatal("must switch")
	}
	if budget.UsedTokens != 350 {
		t.Fatalf("fallback tokens not counted: %d", budget.UsedTokens)
	}
	if budget.UsedTokens > budget.LimitTokens {
		t.Fatalf("budget exceeded: %d > %d", budget.UsedTokens, budget.LimitTokens)
	}
	wantCost := 200.0/1000*0.02 + 50.0/1000*0.06 + 100.0/1000*0.004
	if math.Abs(budget.UsedCost-wantCost) > 1e-9 {
		t.Fatalf("cost = %f, want %f", budget.UsedCost, wantCost)
	}
}

func TestFallbackBudgetExceededStopsChain(t *testing.T) {
	reg := loadFallbackWindowRegistry(t)
	chain := NewFallbackChain(reg, DefaultFallbackConfig())
	calls := 0
	call := func(ctx context.Context, modelID string) CallOutcome {
		calls++
		if modelID == "mid" {
			return CallOutcome{Kind: FailureRateLimit, Err: errors.New("429"), TokensIn: 100}
		}
		return CallOutcome{TokensIn: 200}
	}
	budget := &Budget{LimitTokens: 150}
	res, err := chain.Execute(context.Background(), FallbackRequest{
		TaskID:  "task-f",
		Stage:   "c",
		Primary: "mid",
		Budget:  budget,
	}, call)
	if err == nil || !res.BudgetExceeded {
		t.Fatalf("budget must stop chain: %+v %v", res, err)
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, must stop after exceeded call", calls)
	}
}

func TestFallbackConsecutiveTimeoutsTrigger(t *testing.T) {
	reg := loadFallbackWindowRegistry(t)
	chain := NewFallbackChain(reg, DefaultFallbackConfig())
	call := func(ctx context.Context, modelID string) CallOutcome {
		if modelID == "mid" {
			return CallOutcome{Kind: FailureTimeout, Err: errors.New("timeout")}
		}
		return CallOutcome{TokensIn: 10}
	}
	req := FallbackRequest{TaskID: "task-f", Stage: "c", Primary: "mid"}
	for i := 0; i < 2; i++ {
		res, err := chain.Execute(context.Background(), req, call)
		if err == nil || res.Switched {
			t.Fatalf("run %d must not fallback yet: %+v %v", i, res, err)
		}
	}
	res, err := chain.Execute(context.Background(), req, call)
	if err != nil {
		t.Fatalf("third timeout must fallback: %v", err)
	}
	if !res.Switched || res.Trigger != TriggerTimeout {
		t.Fatalf("third timeout did not fallback: %+v", res)
	}
}

func TestFallbackUnavailableAfterFiveFailuresAndRecovery(t *testing.T) {
	reg := loadSelectorTestRegistry(t)
	chain := NewFallbackChain(reg, DefaultFallbackConfig())
	for i := 0; i < 5; i++ {
		events := chain.ObserveFailure("strong", FailureError)
		if i == 4 && len(events) == 0 {
			t.Fatal("fifth failure must emit unavailable event")
		}
	}
	if !chain.UnavailableModels()["strong"] {
		t.Fatalf("strong must be unavailable: %v", chain.UnavailableModels())
	}

	sel := NewSelector(reg).WithUnavailable(chain.UnavailableModels)
	req := selectorRequest(StrategyBalanced)
	for _, strategy := range []string{StrategyCostFirst, StrategyQualityFirst, StrategyBalanced} {
		req.Strategy = strategy
		res, err := sel.Select(req)
		if err != nil {
			t.Fatalf("%s: %v", strategy, err)
		}
		if res.Model.ID == "strong" {
			t.Fatalf("%s must not select unavailable model", strategy)
		}
	}

	events := chain.Probe("strong", true)
	if len(events) != 1 || events[0].Type != EventModelRecovered {
		t.Fatalf("recovery events = %+v", events)
	}
	if len(chain.UnavailableModels()) != 0 {
		t.Fatalf("model must recover: %v", chain.UnavailableModels())
	}
	req.Strategy = StrategyQualityFirst
	res, err := sel.Select(req)
	if err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if res.Model.ID != "strong" {
		t.Fatalf("recovered strong must be selectable, got %s", res.Model.ID)
	}
}

func TestFallbackEmptyChainFailsFast(t *testing.T) {
	body := `
version: 1
models:
  - id: only
    provider: p
    kind: chat
    context_window: 8192
    max_output_tokens: 1024
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.001
    price_out_per_1k: 0.002
    status: available
`
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	chain := NewFallbackChain(reg, DefaultFallbackConfig())
	calls := 0
	call := func(ctx context.Context, modelID string) CallOutcome {
		calls++
		return CallOutcome{Kind: FailureRateLimit, Err: errors.New("429")}
	}
	res, err := chain.Execute(context.Background(), FallbackRequest{
		TaskID: "task-f", Stage: "c", Primary: "only",
	}, call)
	if err == nil || !res.Exhausted {
		t.Fatal("empty fallback list must fail")
	}
	var fallbackErr *FallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr.Code != CodeFallbackUnavailable {
		t.Fatalf("error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, must not wait on empty chain", calls)
	}
}

func TestFallbackRuntimeSwitchRules(t *testing.T) {
	unapproved := ValidateSwitch(SwitchCheck{From: "mid", To: "large", Reason: SwitchReasonManualUpgrade})
	var fallbackErr *FallbackError
	if !errors.As(unapproved, &fallbackErr) || fallbackErr.Code != CodeSwitchForbidden {
		t.Fatalf("unapproved upgrade must be rejected: %v", unapproved)
	}
	if err := ValidateSwitch(SwitchCheck{From: "mid", To: "large", Reason: SwitchReasonUnavailable}); err != nil {
		t.Fatalf("fallback switch must be allowed: %v", err)
	}
	if err := ValidateSwitch(SwitchCheck{From: "mid", To: "large", Reason: SwitchReasonManualUpgrade, Approved: true, ApprovalID: "ap-1"}); err != nil {
		t.Fatalf("approved upgrade must be allowed: %v", err)
	}
	if err := ValidateSwitch(SwitchCheck{From: "mid", To: "large", Reason: "random"}); err == nil {
		t.Fatal("unknown reason must be rejected")
	}
}

func TestFallbackHotReloadKeepsSnapshotOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	body := `
version: 1
models:
  - id: mid
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 8192
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.004
    price_out_per_1k: 0.012
    status: available
defaults:
  task_model: mid
stage_overrides:
  d: mid
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	reloader := NewHotReloader(reg, path)
	if _, err := reloader.ReloadIfChanged("test"); err != nil {
		t.Fatalf("unchanged reload: %v", err)
	}

	broken := body + "\n  - id: broken\n    provider: p\n"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := reloader.Reload("test")
	if err == nil {
		t.Fatal("broken config must fail")
	}
	if len(reloader.Registry().IDs()) != 1 || reloader.Registry().IDs()[0] != "mid" {
		t.Fatalf("old snapshot must be kept: %v", reloader.Registry().IDs())
	}
	if res.Event.Payload["result"] != "failed" {
		t.Fatalf("reload failure event = %+v", res.Event)
	}

	updated := `
version: 2
models:
  - id: mid
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 8192
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.005
    price_out_per_1k: 0.012
    status: available
defaults:
  task_model: mid
stage_overrides:
  d: mid
`
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = reloader.ReloadIfChanged("operator-1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !res.Changed || res.Version != 2 {
		t.Fatalf("reload must apply: %+v", res)
	}
	if res.Event.Payload["operator"] != "operator-1" || res.Event.Payload["result"] != "applied" {
		t.Fatalf("reload event = %+v", res.Event)
	}
	keys := res.ChangedKeys
	foundVersion, foundPrice := false, false
	for _, key := range keys {
		if key == "version" {
			foundVersion = true
		}
		if key == "model.mid.price_in_per_1k" {
			foundPrice = true
		}
	}
	if !foundVersion || !foundPrice {
		t.Fatalf("changed keys = %v", keys)
	}
	if got, _ := reloader.Registry().Get("mid"); got.priceIn() != 0.005 {
		t.Fatalf("new snapshot not active: %f", got.priceIn())
	}
	badEmbedding := "version: 1\nmodels:\n  - id: mid\n    provider: p\n    kind: chat\n    context_window: 32768\n    max_output_tokens: 8192\n    tool_call: true\n    structured_output: true\n    price_in_per_1k: 0.005\n    price_out_per_1k: 0.012\n    status: available\n  - id: emb\n    provider: p\n    kind: embedding\n    context_window: 8192\n    max_output_tokens: 1\n    embedding_dim: 1024\n    price_in_per_1k: 0.0001\n    price_out_per_1k: 0\n    status: available\nembedding:\n  enforce_dim: 1536\n"
	if err := os.WriteFile(path, []byte(badEmbedding), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reloader.Reload("operator-2"); err == nil {
		t.Fatal("embedding dim mismatch must reject the whole snapshot")
	}
}