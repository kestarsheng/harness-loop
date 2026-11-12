package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/retry"
	"agent-platform/services/harnessloop-service/internal/repository"
)

// ModelSelectionWriter persists hlp_task.model_selections rows.
type ModelSelectionWriter interface {
	Record(ctx context.Context, row repository.ModelSelectionRow) error
}

// ModelStageContext is the task/stage correlation passed by the runtime.
type ModelStageContext struct {
	TenantID  string
	ProjectID string
	TaskID    string
	Stage     string
	AttemptNo int
}

type ModelStageRequest struct {
	ModelStageContext
	ModelID                 string
	Strategy                string
	RequireToolCall         bool
	RequireStructuredOutput bool
	RequiredContextWindow   int
	TaskBudgetTokens        int64
	EstimatedTokensIn       int64
	EstimatedTokensOut      int64
}

type ModelChoice struct {
	ModelID            string
	Window             int
	ThresholdTokens    int
	MemoryInjectBudget int
	CompactionBudget   int64
	Strategy           string
	ResolvedFrom       string
	IsFallback         bool
	EstimatedCost      float64
}

type modelSnapshot struct {
	reg      *modelregistry.Registry
	fallback *modelregistry.FallbackChain
}

type modelOverrideKey struct {
	taskID string
	stage  string
}

// resolvedFromFallback is the value accepted by the model_selections CHECK
// constraint for a model chosen by the fallback chain.
const resolvedFromFallback = "fallback"

// ModelBridge wires modelregistry.Selector / FallbackChain / HotReloader into
// the runtime. When disabled it returns nil selections and the runtime keeps
// the static StageModel path unchanged.
type ModelBridge struct {
	enabled  bool
	strategy string
	store    ModelSelectionWriter
	emitter  *orchestrator.PublisherEmitter
	now      func() time.Time

	snapshot atomic.Pointer[modelSnapshot]
	hot      *modelregistry.HotReloader

	mu        sync.Mutex
	overrides map[modelOverrideKey]string
	stageFail map[modelOverrideKey]int
}

func NewModelBridge(enabled bool, reg *modelregistry.Registry, strategy string, store ModelSelectionWriter, emitter *orchestrator.PublisherEmitter) *ModelBridge {
	if reg == nil {
		reg = modelregistry.DefaultRegistry()
	}
	if strategy == "" {
		strategy = reg.DefaultStrategy()
	}
	if strategy == "" {
		strategy = modelregistry.StrategyBalanced
	}
	b := &ModelBridge{
		enabled:   enabled,
		strategy:  strategy,
		store:     store,
		emitter:   emitter,
		now:       time.Now,
		overrides: make(map[modelOverrideKey]string),
		stageFail: make(map[modelOverrideKey]int),
	}
	b.install(reg)
	return b
}

func (b *ModelBridge) Enabled() bool { return b != nil && b.enabled }

func (b *ModelBridge) Registry() *modelregistry.Registry {
	if b == nil {
		return nil
	}
	snap := b.snapshot.Load()
	if snap == nil {
		return nil
	}
	return snap.reg
}

func (b *ModelBridge) SetClock(now func() time.Time) {
	if now != nil {
		b.now = now
	}
}

func (b *ModelBridge) install(reg *modelregistry.Registry) {
	b.snapshot.Store(&modelSnapshot{reg: reg, fallback: modelregistry.NewFallbackChain(reg, modelregistry.DefaultFallbackConfig())})
}

// ApplyRegistry swaps the registry snapshot used by later stage selections.
// A task already executing keeps the model it selected for its current stage;
// the new snapshot applies from the next stage selection onward.
func (b *ModelBridge) ApplyRegistry(reg *modelregistry.Registry) {
	if b == nil || reg == nil {
		return
	}
	b.install(reg)
}

// StartHotReload watches path and swaps the registry snapshot on change.
func (b *ModelBridge) StartHotReload(ctx context.Context, path string, interval time.Duration, operator string, ec ModelStageContext) *modelregistry.HotReloader {
	if b == nil || path == "" {
		return nil
	}
	reloader := modelregistry.NewHotReloader(b.Registry(), path)
	reloader.SetObserver(func(reg *modelregistry.Registry) { b.ApplyRegistry(reg) })
	b.hot = reloader
	results := reloader.Start(ctx, interval, operator)
	go func() {
		for res := range results {
			if res.Err != nil {
				b.emit(ec, modelregistry.ModelEvent{
					Type:    modelregistry.EventModelConfigApplied,
					Payload: map[string]any{"result": "failed", "error": res.Err.Error(), "operator": operator},
				})
				continue
			}
			if !res.Changed {
				continue
			}
			b.emit(ec, modelregistry.ModelEvent{
				Type: modelregistry.EventModelRegistryChanged,
				Payload: map[string]any{
					"version": res.Version, "changed_keys": res.ChangedKeys, "operator": operator, "result": "applied",
				},
			})
			b.emit(ec, modelregistry.ModelEvent{
				Type: modelregistry.EventModelConfigApplied,
				Payload: map[string]any{
					"version": res.Version, "changed_keys": res.ChangedKeys, "operator": operator, "result": "applied",
				},
			})
		}
	}()
	return reloader
}

// OnStageEnter is called by the runtime whenever it enters a new stage. It
// resets the per-stage fallback counters so a new stage starts from zero.
func (b *ModelBridge) OnStageEnter(ec ModelStageContext) {
	if !b.Enabled() {
		return
	}
	snap := b.snapshot.Load()
	if snap == nil {
		return
	}
	snap.fallback.ResetStage(ec.Stage)
	b.mu.Lock()
	key := modelOverrideKey{taskID: ec.TaskID, stage: ec.Stage}
	delete(b.overrides, key)
	delete(b.stageFail, key)
	b.mu.Unlock()
	b.emit(ec, modelregistry.ModelEvent{
		Type:    modelregistry.EventModelRecovered,
		Payload: map[string]any{"stage": ec.Stage, "stage_fallbacks": 0, "reset": true},
	})
}

// SelectForStage picks the model for one stage attempt. Selector failure falls
// back to the FallbackChain candidates; successful selection is recorded in
// hlp_task.model_selections with every NOT NULL column populated.
func (b *ModelBridge) SelectForStage(ctx context.Context, req ModelStageRequest) (*ModelChoice, error) {
	if !b.Enabled() {
		return nil, nil
	}
	snap := b.snapshot.Load()
	if snap == nil || snap.reg == nil {
		return nil, fmt.Errorf("model bridge: registry snapshot is unavailable")
	}
	ec := req.ModelStageContext
	strategy := req.Strategy
	if strategy == "" {
		strategy = b.strategy
	}

	if id, ok := b.override(ec.TaskID, ec.Stage); ok {
		if m, exists := snap.reg.Get(id); exists {
			choice := b.choiceOf(m, strategy, resolvedFromFallback, true)
			b.record(ctx, snap.reg, choice, ec, nil)
			return choice, nil
		}
	}

	selector := modelregistry.NewSelector(snap.reg).WithUnavailable(snap.fallback.UnavailableModels)
	if b.store != nil {
		selector = selector.WithRecorder(&selectionRecorder{bridge: b, reg: snap.reg, attemptNo: ec.AttemptNo})
	}
	sel, err := selector.Select(modelregistry.SelectRequest{
		TenantID:                req.TenantID,
		ProjectID:               req.ProjectID,
		TaskID:                  req.TaskID,
		Stage:                   req.Stage,
		ModelID:                 req.ModelID,
		Strategy:                strategy,
		RequireToolCall:         req.RequireToolCall,
		RequireStructuredOutput: req.RequireStructuredOutput,
		RequiredContextWindow:   req.RequiredContextWindow,
		TaskBudgetTokens:        req.TaskBudgetTokens,
		EstimatedTokensIn:       req.EstimatedTokensIn,
		EstimatedTokensOut:      req.EstimatedTokensOut,
	})
	if err != nil || sel == nil {
		candidates := snap.fallback.Candidates("", req.Stage, fallbackRequirement(req))
		if len(candidates) == 0 {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("model bridge: no candidate model for stage %s", req.Stage)
		}
		m := candidates[0]
		choice := b.choiceOf(m, strategy, "fallback", true)
		b.record(ctx, snap.reg, choice, ec, nil)
		b.emit(ec, modelregistry.ModelEvent{
			Type: modelregistry.EventModelFallback,
			Payload: map[string]any{
				"stage": req.Stage, "model_id": m.ID, "resolved_from": "fallback",
				"reason": errorString(err),
			},
		})
		return choice, nil
	}

	choice := &ModelChoice{
		ModelID:            sel.Model.ID,
		Window:             sel.Window,
		ThresholdTokens:    sel.ThresholdTokens,
		MemoryInjectBudget: sel.MemoryInjectBudget,
		CompactionBudget:   sel.CompactionBudget,
		Strategy:           sel.Strategy,
		ResolvedFrom:       sel.ResolvedFrom,
		EstimatedCost:      sel.EstimatedCost,
	}
	b.emit(ec, sel.Event)
	return choice, nil
}

// ObserveStageFailure feeds a stage failure into the FallbackChain and records
// the next candidate for this task/stage. The runtime retries the same stage
// with the override on its next loop iteration.
func (b *ModelBridge) ObserveStageFailure(ec ModelStageContext, modelID string, kind modelregistry.FailureKind) string {
	if !b.Enabled() || modelID == "" {
		return ""
	}
	snap := b.snapshot.Load()
	if snap == nil {
		return ""
	}
	for _, ev := range snap.fallback.ObserveFailure(modelID, kind) {
		b.emit(ec, ev)
	}
	key := modelOverrideKey{taskID: ec.TaskID, stage: ec.Stage}
	b.mu.Lock()
	b.stageFail[key]++
	b.mu.Unlock()
	candidates := snap.fallback.Candidates(modelID, ec.Stage, modelregistry.FallbackRequirement{})
	if len(candidates) == 0 {
		return ""
	}
	next := candidates[0].ID
	b.mu.Lock()
	b.overrides[key] = next
	b.mu.Unlock()
	b.emit(ec, modelregistry.ModelEvent{
		Type: modelregistry.EventModelFallback,
		Payload: map[string]any{
			"stage": ec.Stage, "from": modelID, "model_id": next, "trigger": string(kind),
		},
	})
	return next
}

func (b *ModelBridge) StageFallbacks(taskID, stage string) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stageFail[modelOverrideKey{taskID: taskID, stage: stage}]
}

func (b *ModelBridge) override(taskID, stage string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.overrides[modelOverrideKey{taskID: taskID, stage: stage}]
	return id, ok
}

func (b *ModelBridge) choiceOf(m *modelregistry.Model, strategy, resolvedFrom string, fallback bool) *ModelChoice {
	if strategy == "" {
		strategy = modelregistry.StrategyBalanced
	}
	return &ModelChoice{
		ModelID:         m.ID,
		Window:          m.ContextWindow,
		ThresholdTokens: int(float64(m.ContextWindow) * modelregistry.DefaultCompactionRatio),
		Strategy:        strategy,
		ResolvedFrom:    resolvedFrom,
		IsFallback:      fallback,
	}
}

func (b *ModelBridge) record(ctx context.Context, reg *modelregistry.Registry, choice *ModelChoice, ec ModelStageContext, candidates []modelregistry.CandidateScore) {
	if b.store == nil || choice == nil {
		return
	}
	inPrice, outPrice := modelPrices(reg, choice.ModelID)
	raw, _ := json.Marshal(candidates)
	attempt := ec.AttemptNo
	if attempt <= 0 {
		attempt = 1
	}
	row := repository.ModelSelectionRow{
		SelectionID:     fmt.Sprintf("sel-%s-%s-%d-%d", ec.TaskID, ec.Stage, attempt, b.now().UnixNano()),
		TenantID:        ec.TenantID,
		ProjectID:       ec.ProjectID,
		TaskID:          ec.TaskID,
		Stage:           ec.Stage,
		AttemptNo:       attempt,
		ModelID:         choice.ModelID,
		Strategy:        choice.Strategy,
		ResolvedFrom:    choice.ResolvedFrom,
		Candidates:      raw,
		ContextWindow:   choice.Window,
		ThresholdTokens: int64(choice.ThresholdTokens),
		InputPrice:      inPrice,
		OutputPrice:     outPrice,
		IsFallback:      choice.IsFallback,
	}
	// Selection statistics must never break task execution: a persistence
	// failure is reported through the event channel instead of failing the
	// selection that already succeeded.
	if err := b.store.Record(ctx, row); err != nil {
		b.emit(ec, modelregistry.ModelEvent{
			Type:    modelregistry.EventModelConfigApplied,
			Payload: map[string]any{"result": "selection_record_failed", "error": err.Error(), "stage": ec.Stage},
		})
	}
}

func (b *ModelBridge) emit(ec ModelStageContext, ev modelregistry.ModelEvent) {
	if b == nil || b.emitter == nil || ev.Type == "" {
		return
	}
	ectx := orchestrator.EmitContext{
		TenantID: ec.TenantID, ProjectID: ec.ProjectID, TaskID: ec.TaskID,
		Stage: ec.Stage, Attempt: ec.AttemptNo,
	}
	_ = b.emitter.EmitModelEvent(context.Background(), ectx, ev)
}

type selectionRecorder struct {
	bridge    *ModelBridge
	reg       *modelregistry.Registry
	attemptNo int
}

func (r *selectionRecorder) RecordSelection(rec modelregistry.SelectionRecord) error {
	choice := &ModelChoice{
		ModelID:         rec.ModelID,
		Window:          rec.Window,
		ThresholdTokens: rec.ThresholdTokens,
		Strategy:        rec.Strategy,
		ResolvedFrom:    rec.ResolvedFrom,
	}
	ec := ModelStageContext{
		TenantID: rec.TenantID, ProjectID: rec.ProjectID, TaskID: rec.TaskID,
		Stage: rec.Stage, AttemptNo: r.attemptNo,
	}
	r.bridge.record(context.Background(), r.reg, choice, ec, rec.Candidates)
	return nil
}

func fallbackRequirement(req ModelStageRequest) modelregistry.FallbackRequirement {
	return modelregistry.FallbackRequirement{
		RequireToolCall:         req.RequireToolCall,
		RequireStructuredOutput: req.RequireStructuredOutput,
		RequiredContextWindow:   req.RequiredContextWindow,
	}
}

func modelPrices(reg *modelregistry.Registry, modelID string) (float64, float64) {
	if reg == nil {
		return 0, 0
	}
	m, ok := reg.Get(modelID)
	if !ok || m == nil {
		return 0, 0
	}
	var in, out float64
	if m.PriceInPer1K != nil {
		in = *m.PriceInPer1K
	}
	if m.PriceOutPer1K != nil {
		out = *m.PriceOutPer1K
	}
	return in, out
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func modelFailureKind(category retry.Category) modelregistry.FailureKind {
	switch category {
	case retry.Timeout:
		return modelregistry.FailureTimeout
	case retry.Quota:
		return modelregistry.FailureQuota
	case retry.Network, retry.Runtime:
		return modelregistry.FailureRateLimit
	default:
		return modelregistry.FailureError
	}
}
