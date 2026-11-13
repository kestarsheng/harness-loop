package modelregistry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var ErrNoConfigPath = errors.New("modelregistry: hot reload requires a config path")

const DefaultReloadInterval = 5 * time.Second

type ReloadResult struct {
	Changed     bool
	Version     int
	ChangedKeys []string
	Event       ModelEvent
	Err         error
}

type HotReloader struct {
	path    string
	current atomic.Pointer[Registry]

	mu       sync.Mutex
	lastMod  time.Time
	lastSize int64
	started  bool

	now   func() time.Time
	onSet func(*Registry)
}

func NewHotReloader(reg *Registry, path string) *HotReloader {
	if reg == nil {
		reg = DefaultRegistry()
	}
	h := &HotReloader{path: path, now: time.Now}
	h.current.Store(reg)
	if path != "" {
		if fi, err := os.Stat(path); err == nil {
			h.lastMod = fi.ModTime()
			h.lastSize = fi.Size()
		}
	}
	return h
}

func (h *HotReloader) SetObserver(fn func(*Registry)) { h.onSet = fn }

func (h *HotReloader) Registry() *Registry { return h.current.Load() }

func (h *HotReloader) Path() string { return h.path }

func (h *HotReloader) Reload(operator string) (ReloadResult, error) {
	if h.path == "" {
		return ReloadResult{}, ErrNoConfigPath
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fi, err := os.Stat(h.path)
	if err != nil {
		return h.failLocked(operator, err), err
	}
	return h.reloadLocked(fi, operator)
}

func (h *HotReloader) ReloadIfChanged(operator string) (ReloadResult, error) {
	if h.path == "" {
		return ReloadResult{}, ErrNoConfigPath
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fi, err := os.Stat(h.path)
	if err != nil {
		return h.failLocked(operator, err), err
	}
	if !h.lastMod.IsZero() && fi.ModTime().Equal(h.lastMod) && fi.Size() == h.lastSize {
		return ReloadResult{Changed: false, Version: h.current.Load().Version}, nil
	}
	return h.reloadLocked(fi, operator)
}

func (h *HotReloader) reloadLocked(fi os.FileInfo, operator string) (ReloadResult, error) {
	raw, err := os.ReadFile(h.path)
	if err != nil {
		h.lastMod = fi.ModTime()
		h.lastSize = fi.Size()
		return h.failLocked(operator, err), err
	}
	next, err := loadBytes(raw)
	if err != nil {
		h.lastMod = fi.ModTime()
		h.lastSize = fi.Size()
		return h.failLocked(operator, err), err
	}
	h.lastMod = fi.ModTime()
	h.lastSize = fi.Size()
	prev := h.current.Load()
	changedKeys := diffRegistries(prev, next)
	h.current.Store(next)
	if h.onSet != nil {
		h.onSet(next)
	}
	result := ReloadResult{
		Changed:     len(changedKeys) > 0,
		Version:     next.Version,
		ChangedKeys: changedKeys,
		Event: ModelEvent{
			Type: EventModelRegistryChanged,
			Payload: map[string]any{
				"operator":     operator,
				"changed_keys": changedKeys,
				"result":       "applied",
				"version":      next.Version,
			},
		},
	}
	return result, nil
}

func (h *HotReloader) failLocked(operator string, err error) ReloadResult {
	return ReloadResult{
		Changed: false,
		Version: h.current.Load().Version,
		Event: ModelEvent{
			Type: EventModelRegistryChanged,
			Payload: map[string]any{
				"operator":     operator,
				"changed_keys": []string{},
				"result":       "failed",
			},
		},
		Err: err,
	}
}

func (h *HotReloader) Start(ctx context.Context, interval time.Duration, operator string) <-chan ReloadResult {
	if interval <= 0 {
		interval = DefaultReloadInterval
	}
	out := make(chan ReloadResult, 8)
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		close(out)
		return out
	}
	h.started = true
	h.mu.Unlock()

	go func() {
		defer close(out)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				res, err := h.ReloadIfChanged(operator)
				if err != nil && res.Event.Type == "" {
					continue
				}
				select {
				case out <- res:
				default:
				}
			}
		}
	}()
	return out
}

func diffRegistries(prev, next *Registry) []string {
	if prev == nil || next == nil {
		return []string{"registry"}
	}
	keys := make([]string, 0)
	if prev.Version != next.Version {
		keys = append(keys, "version")
	}
	keys = append(keys, diffDefaults(prev.defaults, next.defaults)...)
	keys = append(keys, diffEmbedding(prev.embedding, next.embedding)...)
	keys = append(keys, diffStageOverrides(prev.stageOverrides, next.stageOverrides)...)
	for id := range next.models {
		if _, ok := prev.models[id]; !ok {
			keys = append(keys, "model."+id)
		}
	}
	for id := range prev.models {
		if _, ok := next.models[id]; !ok {
			keys = append(keys, "model."+id)
		}
	}
	for id, a := range prev.models {
		b, ok := next.models[id]
		if !ok {
			continue
		}
		keys = append(keys, diffModel(id, a, b)...)
	}
	sort.Strings(keys)
	return keys
}

func diffModel(id string, a, b *Model) []string {
	var keys []string
	appendIf := func(changed bool, field string) {
		if changed {
			keys = append(keys, "model."+id+"."+field)
		}
	}
	appendIf(a.Provider != b.Provider, "provider")
	appendIf(a.Kind != b.Kind, "kind")
	appendIf(a.ContextWindow != b.ContextWindow, "context_window")
	appendIf(a.MaxOutputTokens != b.MaxOutputTokens, "max_output_tokens")
	appendIf(a.ToolCall != b.ToolCall, "tool_call")
	appendIf(a.StructuredOutput != b.StructuredOutput, "structured_output")
	appendIf(a.priceIn() != b.priceIn(), "price_in_per_1k")
	appendIf(a.priceOut() != b.priceOut(), "price_out_per_1k")
	appendIf(a.Status != b.Status, "status")
	appendIf(a.EmbeddingDim != b.EmbeddingDim, "embedding_dim")
	appendIf(!equalStrings(a.Tags, b.Tags), "tags")
	appendIf(a.APIKeyRef != b.APIKeyRef, "api_key_ref")
	return keys
}

func diffDefaults(a, b RegistryDefaults) []string {
	var keys []string
	if a.TaskModel != b.TaskModel {
		keys = append(keys, "defaults.task_model")
	}
	if a.Strategy != b.Strategy {
		keys = append(keys, "defaults.strategy")
	}
	if !equalStrings(a.Fallback, b.Fallback) {
		keys = append(keys, "defaults.fallback")
	}
	return keys
}

func diffEmbedding(a, b EmbeddingConfig) []string {
	var keys []string
	if a.MemoryModel != b.MemoryModel {
		keys = append(keys, "embedding.memory_model")
	}
	if a.CodeModel != b.CodeModel {
		keys = append(keys, "embedding.code_model")
	}
	if a.EnforceDim != b.EnforceDim {
		keys = append(keys, "embedding.enforce_dim")
	}
	return keys
}

func diffStageOverrides(a, b map[string]StageOverride) []string {
	var keys []string
	seen := map[string]bool{}
	for stage, ov := range b {
		seen[stage] = true
		old, ok := a[stage]
		if !ok {
			keys = append(keys, "stage_overrides."+stage)
			continue
		}
		if old.Model != ov.Model || old.Strategy != ov.Strategy || !equalStrings(old.Fallbacks, ov.Fallbacks) {
			keys = append(keys, "stage_overrides."+stage)
		}
	}
	for stage := range a {
		if !seen[stage] {
			keys = append(keys, "stage_overrides."+stage)
		}
	}
	return keys
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (h *HotReloader) String() string {
	return fmt.Sprintf("modelregistry.HotReloader(path=%s)", h.path)
}