package modelregistry

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const selectorTestRegistry = `
version: 2
models:
  - id: strong
    provider: p
    kind: chat
    context_window: 131072
    max_output_tokens: 8192
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.02
    price_out_per_1k: 0.06
    status: available
    tags: [strong, balanced]
  - id: cheap
    provider: p
    kind: chat
    context_window: 8192
    max_output_tokens: 4096
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.0005
    price_out_per_1k: 0.0015
    status: available
    tags: [cheap]
  - id: notool
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 2048
    tool_call: false
    structured_output: false
    price_in_per_1k: 0.0001
    price_out_per_1k: 0.0002
    status: available
    tags: [cheap]
  - id: embed
    provider: p
    kind: embedding
    context_window: 8192
    max_output_tokens: 1
    embedding_dim: 1024
    price_in_per_1k: 0.0001
    price_out_per_1k: 0
    status: available
defaults:
  task_model: cheap
  strategy: balanced
  fallback: [strong, cheap]
stage_overrides:
  d: strong
`

func loadSelectorTestRegistry(t *testing.T) *Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte(selectorTestRegistry), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := Load(path)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

type memoryRecorder struct {
	records []SelectionRecord
	err     error
}

func (m *memoryRecorder) RecordSelection(record SelectionRecord) error {
	if m.err != nil {
		return m.err
	}
	m.records = append(m.records, record)
	return nil
}

func selectorRequest(strategy string) SelectRequest {
	return SelectRequest{
		TenantID:          "t1",
		ProjectID:         "p1",
		TaskID:            "task-1",
		Stage:             "c",
		Strategy:          strategy,
		RequireToolCall:   true,
		StageBudgetTokens: 8000,
		TaskBudgetTokens:  100000,
	}
}

func TestSelectorStrategiesReproducible(t *testing.T) {
	reg := loadSelectorTestRegistry(t)
	sel := NewSelector(reg)

	cases := []struct {
		strategy string
		want     string
	}{
		{StrategyCostFirst, "cheap"},
		{StrategyQualityFirst, "strong"},
		{StrategyBalanced, "cheap"},
	}
	for _, c := range cases {
		first, err := sel.Select(selectorRequest(c.strategy))
		if err != nil {
			t.Fatalf("%s: %v", c.strategy, err)
		}
		second, err := sel.Select(selectorRequest(c.strategy))
		if err != nil {
			t.Fatalf("%s second: %v", c.strategy, err)
		}
		if first.Model.ID != c.want {
			t.Fatalf("%s chose %s, want %s", c.strategy, first.Model.ID, c.want)
		}
		if first.Model.ID != second.Model.ID {
			t.Fatalf("%s not reproducible: %s vs %s", c.strategy, first.Model.ID, second.Model.ID)
		}
		if first.Strategy != c.strategy || second.Strategy != c.strategy {
			t.Fatalf("%s strategy not recorded", c.strategy)
		}
		if len(first.Candidates) < 2 {
			t.Fatalf("%s candidates missing: %+v", c.strategy, first.Candidates)
		}
		chosenCount := 0
		for _, candidate := range first.Candidates {
			if candidate.Score < 0 || candidate.Score > 1 {
				t.Fatalf("%s score out of range: %+v", c.strategy, candidate)
			}
			if candidate.Chosen {
				chosenCount++
				if candidate.ModelID != first.Model.ID {
					t.Fatalf("%s chosen marker mismatch", c.strategy)
				}
			}
		}
		if chosenCount != 1 {
			t.Fatalf("%s chosen markers = %d", c.strategy, chosenCount)
		}
	}
}

func TestSelectorStageOverrideInheritance(t *testing.T) {
	reg := loadSelectorTestRegistry(t)
	sel := NewSelector(reg)

	req := selectorRequest(StrategyBalanced)
	req.ModelID = "cheap"

	req.Stage = "a"
	inherited, err := sel.Select(req)
	if err != nil {
		t.Fatalf("stage a: %v", err)
	}
	if inherited.Model.ID != "cheap" || inherited.ResolvedFrom != ResolvedFromExplicit {
		t.Fatalf("stage a must inherit task model: %+v", inherited)
	}
	if got := inherited.Event.Payload["stage"]; got != "a" {
		t.Fatalf("event stage = %v", got)
	}

	req.Stage = "d"
	overridden, err := sel.Select(req)
	if err != nil {
		t.Fatalf("stage d: %v", err)
	}
	if overridden.Model.ID != "strong" || overridden.ResolvedFrom != ResolvedFromOverride {
		t.Fatalf("stage d must use override: %+v", overridden)
	}
	if got := overridden.Event.Payload["model_id"]; got != "strong" {
		t.Fatalf("event model_id = %v", got)
	}

	req.Stage = "b"
	inheritedB, err := sel.Select(req)
	if err != nil {
		t.Fatalf("stage b: %v", err)
	}
	if inheritedB.Model.ID != "cheap" {
		t.Fatalf("stage b model = %s", inheritedB.Model.ID)
	}
}

func TestSelectorTieBreakStable(t *testing.T) {
	body := `
version: 1
models:
  - id: aaa
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 2048
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.001
    price_out_per_1k: 0.002
    status: available
  - id: bbb
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 2048
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
	sel := NewSelector(reg)
	req := selectorRequest(StrategyBalanced)

	first, err := sel.Select(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Model.ID != "aaa" {
		t.Fatalf("tie must break by id asc, got %s", first.Model.ID)
	}
	second, err := sel.Select(req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Model.ID != first.Model.ID {
		t.Fatalf("tie break not stable: %s vs %s", first.Model.ID, second.Model.ID)
	}
}

func TestSelectorUnavailableExcluded(t *testing.T) {
	reg := loadSelectorTestRegistry(t)
	sel := NewSelector(reg)

	for _, strategy := range []string{StrategyCostFirst, StrategyQualityFirst, StrategyBalanced} {
		req := selectorRequest(strategy)
		req.Unavailable = map[string]bool{"strong": true}
		res, err := sel.Select(req)
		if err != nil {
			t.Fatalf("%s: %v", strategy, err)
		}
		if res.Model.ID == "strong" {
			t.Fatalf("%s selected unavailable model", strategy)
		}
		for _, candidate := range res.Candidates {
			if candidate.ModelID == "strong" {
				t.Fatalf("%s kept unavailable model in candidates", strategy)
			}
		}
	}
}

func TestSelectorRecordsStageModelTokensCost(t *testing.T) {
	reg := loadSelectorTestRegistry(t)
	recorder := &memoryRecorder{}
	sel := NewSelector(reg).WithRecorder(recorder)

	req := selectorRequest(StrategyBalanced)
	req.ModelID = "cheap"
	req.EstimatedTokensIn = 1000
	req.EstimatedTokensOut = 500
	res, err := sel.Select(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorder.records) != 1 {
		t.Fatalf("records = %d", len(recorder.records))
	}
	record := recorder.records[0]
	if record.Stage != "c" || record.ModelID != "cheap" || record.Strategy != StrategyBalanced {
		t.Fatalf("record identity mismatch: %+v", record)
	}
	if record.TokensIn != 1000 || record.TokensOut != 500 {
		t.Fatalf("record tokens = %d/%d", record.TokensIn, record.TokensOut)
	}
	wantCost := 1000.0 / 1000 * 0.0005 + 500.0 / 1000 * 0.0015
	if math.Abs(record.Cost-wantCost) > 1e-9 {
		t.Fatalf("record cost = %f, want %f", record.Cost, wantCost)
	}
	if math.Abs(res.EstimatedCost-wantCost) > 1e-9 {
		t.Fatalf("selection cost = %f", res.EstimatedCost)
	}
	cheap, _ := reg.Get("cheap")
	if record.Window != cheap.ContextWindow || record.ThresholdTokens != int(float64(cheap.ContextWindow)*DefaultCompactionRatio) {
		t.Fatalf("record window/threshold mismatch: %+v", record)
	}
	if record.CreatedAt.IsZero() || record.SelectionID == "" {
		t.Fatalf("record metadata missing: %+v", record)
	}
}

func TestSelectorCapabilityMismatch(t *testing.T) {
	reg := loadSelectorTestRegistry(t)
	sel := NewSelector(reg)
	req := selectorRequest(StrategyBalanced)
	req.ModelID = "notool"

	_, err := sel.Select(req)
	var capability *CapabilityError
	if !errors.As(err, &capability) {
		t.Fatalf("expected capability error: %v", err)
	}
	if len(capability.Suggested) == 0 {
		t.Fatal("suggested models must not be empty")
	}
	for _, id := range capability.Suggested {
		m, ok := reg.Get(id)
		if !ok || !m.ToolCall {
			t.Fatalf("suggested model %s does not satisfy tool call", id)
		}
	}
	if strings.Contains(strings.Join(capability.Suggested, ","), "notool") {
		t.Fatalf("rejected model must not be suggested: %v", capability.Suggested)
	}
}

func TestSelectorRecordFailureSurfaces(t *testing.T) {
	reg := loadSelectorTestRegistry(t)
	recorder := &memoryRecorder{err: errors.New("db down")}
	sel := NewSelector(reg).WithRecorder(recorder)
	_, err := sel.Select(selectorRequest(StrategyBalanced))
	if !errors.Is(err, ErrSelectionRecord) {
		t.Fatalf("record failure must surface: %v", err)
	}
}