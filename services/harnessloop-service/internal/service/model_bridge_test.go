package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/services/harnessloop-service/internal/repository"
)

type selectionStore struct {
	mu   sync.Mutex
	rows []repository.ModelSelectionRow
}

func (s *selectionStore) Record(_ context.Context, row repository.ModelSelectionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
	return nil
}

func (s *selectionStore) snapshot() []repository.ModelSelectionRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]repository.ModelSelectionRow, len(s.rows))
	copy(out, s.rows)
	return out
}

type modelEventSink struct {
	mu     sync.Mutex
	events []*event.Event
}

func (s *modelEventSink) Append(_ context.Context, events []*event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (s *modelEventSink) types() []event.EventType {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]event.EventType, len(s.events))
	for i, ev := range s.events {
		out[i] = ev.EventType
	}
	return out
}

func newTestModelBridge(enabled bool, store ModelSelectionWriter) (*ModelBridge, *modelEventSink) {
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1",
	})
	return NewModelBridge(enabled, modelregistry.DefaultRegistry(), "", store, emitter), sink
}

func TestModelBridgeDisabledKeepsStaticPath(t *testing.T) {
	store := &selectionStore{}
	bridge, sink := newTestModelBridge(false, store)
	if bridge.Enabled() {
		t.Fatal("bridge must be disabled by default")
	}
	choice, err := bridge.SelectForStage(context.Background(), ModelStageRequest{
		ModelStageContext: ModelStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "a", AttemptNo: 1},
	})
	if err != nil || choice != nil {
		t.Fatalf("disabled bridge must return nil,nil: %+v %v", choice, err)
	}
	if len(store.snapshot()) != 0 || len(sink.types()) != 0 {
		t.Fatalf("disabled bridge must not write selections/events: %+v %+v", store.snapshot(), sink.types())
	}
}

func TestModelBridgeSelectsAndRecords(t *testing.T) {
	store := &selectionStore{}
	bridge, sink := newTestModelBridge(true, store)
	choice, err := bridge.SelectForStage(context.Background(), ModelStageRequest{
		ModelStageContext: ModelStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "c", AttemptNo: 2},
		RequireToolCall:   true,
		TaskBudgetTokens:  10000,
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if choice == nil || choice.ModelID == "" || choice.Window <= 0 {
		t.Fatalf("choice = %+v", choice)
	}
	rows := store.snapshot()
	if len(rows) != 1 {
		t.Fatalf("selection rows = %d", len(rows))
	}
	row := rows[0]
	if row.AttemptNo != 2 || row.ContextWindow <= 0 || row.ThresholdTokens <= 0 {
		t.Fatalf("not-null numeric columns missing: %+v", row)
	}
	if row.InputPrice <= 0 || row.OutputPrice <= 0 {
		t.Fatalf("price snapshot missing: %+v", row)
	}
	if row.TenantID != "tenant-a" || row.ProjectID != "proj-a" || row.TaskID != "task-1" || row.Stage != "c" {
		t.Fatalf("identity columns missing: %+v", row)
	}
	if row.Strategy == "" || row.ResolvedFrom == "" || row.ModelID == "" {
		t.Fatalf("strategy/resolved/model missing: %+v", row)
	}
	types := sink.types()
	if len(types) != 1 || types[0] != event.ModelSelected {
		t.Fatalf("events = %v", types)
	}
}

func TestModelBridgeFallsBackWhenSelectionFails(t *testing.T) {
	store := &selectionStore{}
	bridge, sink := newTestModelBridge(true, store)
	if _, err := bridge.SelectForStage(context.Background(), ModelStageRequest{
		ModelStageContext: ModelStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "c", AttemptNo: 1},
	}); err != nil {
		t.Fatalf("initial select: %v", err)
	}

	// An unknown explicit model makes Selector fail; the FallbackChain still
	// yields an available chat model and records resolved_from=fallback.
	choice, err := bridge.SelectForStage(context.Background(), ModelStageRequest{
		ModelStageContext: ModelStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "c", AttemptNo: 2},
		ModelID:           "does-not-exist",
	})
	if err != nil || choice == nil {
		t.Fatalf("fallback select: %+v %v", choice, err)
	}
	if !choice.IsFallback || choice.ResolvedFrom != resolvedFromFallback {
		t.Fatalf("fallback choice = %+v", choice)
	}
	rows := store.snapshot()
	if len(rows) < 2 || !rows[len(rows)-1].IsFallback || rows[len(rows)-1].ResolvedFrom != resolvedFromFallback {
		t.Fatalf("fallback selection not recorded: %+v", rows)
	}
	types := sink.types()
	found := false
	for _, typ := range types {
		if typ == event.ModelFallback {
			found = true
		}
	}
	if !found {
		t.Fatalf("model.fallback event missing: %v", types)
	}
}

func TestModelBridgeStageReset(t *testing.T) {
	store := &selectionStore{}
	bridge, _ := newTestModelBridge(true, store)
	ec := ModelStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "c", AttemptNo: 1}
	if _, err := bridge.SelectForStage(context.Background(), ModelStageRequest{ModelStageContext: ec}); err != nil {
		t.Fatalf("select: %v", err)
	}
	primary := ""
	for _, m := range bridge.snapshot.Load().reg.Available() {
		if m.Kind == modelregistry.KindChat {
			primary = m.ID
			break
		}
	}
	if primary == "" {
		t.Fatal("no chat candidate")
	}
	bridge.ObserveStageFailure(ec, primary, modelregistry.FailureError)
	if bridge.StageFallbacks("task-1", "c") == 0 {
		t.Fatal("stage fallback counter must increment on failure")
	}
	bridge.OnStageEnter(ModelStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "c", AttemptNo: 2})
	if bridge.StageFallbacks("task-1", "c") != 0 {
		t.Fatalf("entering a stage must reset fallbacks, got %d", bridge.StageFallbacks("task-1", "c"))
	}
}


func TestModelBridgeHotReloadSwapsSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	writeRegistryFile(t, path, 1, 4096)
	store := &selectionStore{}
	reg, err := modelregistry.Load(path)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{TenantID: "tenant-a", ProjectID: "proj-a"})
	bridge := NewModelBridge(true, reg, "", store, emitter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.StartHotReload(ctx, path, 10*time.Millisecond, "tester", ModelStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "model-registry"})

	writeRegistryFile(t, path, 2, 8192)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bridge.Registry() != nil && bridge.Registry().Version == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if bridge.Registry().Version != 2 {
		t.Fatalf("registry snapshot was not swapped, version=%d", bridge.Registry().Version)
	}
	if m, ok := bridge.Registry().Get("test-model"); !ok || m.ContextWindow != 8192 {
		t.Fatalf("new config not applied: %+v", m)
	}
}

func writeRegistryFile(t *testing.T, path string, version, window int) {
	t.Helper()
	raw := []byte("version: " + itoa(version) + "\n" +
		"models:\n" +
		"  - id: test-model\n" +
		"    provider: test\n" +
		"    kind: chat\n" +
		"    context_window: " + itoa(window) + "\n" +
		"    max_output_tokens: 1024\n" +
		"    tool_call: true\n" +
		"    structured_output: true\n" +
		"    price_in_per_1k: 0.01\n" +
		"    price_out_per_1k: 0.02\n" +
		"    status: available\n")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
func TestModelBridgeRuntimeUsesSelection(t *testing.T) {
	store := &selectionStore{}
	bridge, sink := newTestModelBridge(true, store)
	stages := &stubStages{}
	rt, _, _, _ := newTestRuntime(stages)
	rt.Models = bridge

	if _, err := rt.Run(context.Background(), runtimeInput()); err != nil {
		t.Fatalf("run: %v", err)
	}
	rows := store.snapshot()
	if len(rows) < 4 {
		t.Fatalf("expected one selection per stage, got %d", len(rows))
	}
	stages.mu.Lock()
	inputs := append([]StageRunInput(nil), stages.inputs...)
	stages.mu.Unlock()
	if len(inputs) < 4 {
		t.Fatalf("stage calls = %d", len(inputs))
	}
	for i := 0; i < 4; i++ {
		if inputs[i].StageModel != rows[i].ModelID {
			t.Fatalf("stage %d model=%s, recorded=%s", i, inputs[i].StageModel, rows[i].ModelID)
		}
		if inputs[i].ModelWindow != rows[i].ContextWindow {
			t.Fatalf("stage %d window=%d, recorded=%d", i, inputs[i].ModelWindow, rows[i].ContextWindow)
		}
	}
	found := false
	for _, typ := range sink.types() {
		if typ == event.ModelSelected {
			found = true
		}
	}
	if !found {
		t.Fatalf("model.selected event missing: %v", sink.types())
	}
}
