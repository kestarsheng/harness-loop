package compaction

import (
	"context"
	"errors"
)

var (
	ErrEngineDisabled      = errors.New("compaction: context engine is disabled")
	ErrEngineNotConfigured = errors.New("compaction: context engine slot has no implementation")
)

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
