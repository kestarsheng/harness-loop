package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type stubLLM struct {
	resp *LLMResponse
}

func (s *stubLLM) Chat(ctx context.Context, req *LLMRequest) (*LLMResponse, error) {
	return s.resp, nil
}

type stubTools struct{}

func (s *stubTools) Execute(ctx context.Context, toolName string, arguments json.RawMessage, toolConfig *ToolSpecificConfig) (string, error) {
	return "", nil
}

func (s *stubTools) ListTools(ctx context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}

func newStageModeEngine(t *testing.T) *Engine {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register(&Agent{ID: "exec", Name: "exec", Instructions: "run"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return NewEngine(reg, &stubLLM{resp: &LLMResponse{Content: "已完成，无需工具调用"}}, &stubTools{}, nil, DefaultEngineConfig())
}

func TestStageModeWithoutFinishIsNotCompleted(t *testing.T) {
	e := newStageModeEngine(t)
	res, err := e.Run(context.Background(), &ExecutionRequest{
		SessionID:  "s-1",
		Message:    "do work",
		EntryAgent: "exec",
		StageMode:  true,
	})
	if !errors.Is(err, ErrStageNotFinished) {
		t.Fatalf("expected ErrStageNotFinished, got %v", err)
	}
	if res == nil {
		t.Fatal("result must not be nil")
	}
	if res.Status == AgentStatusCompleted {
		t.Fatal("stage mode must not mark the run completed without a stage_finish call")
	}
	if res.Status != AgentStatusError {
		t.Fatalf("status = %s", res.Status)
	}
}

func TestNormalModeCompletesWithoutFinish(t *testing.T) {
	e := newStageModeEngine(t)
	res, err := e.Run(context.Background(), &ExecutionRequest{
		SessionID:  "s-2",
		Message:    "do work",
		EntryAgent: "exec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != AgentStatusCompleted {
		t.Fatalf("status = %s", res.Status)
	}
}

func TestStageModeFlagPropagatesToContext(t *testing.T) {
	e := newStageModeEngine(t)
	res, err := e.Run(context.Background(), &ExecutionRequest{
		SessionID:  "s-3",
		Message:    "do work",
		EntryAgent: "exec",
		StageMode:  true,
	})
	if err == nil {
		t.Fatal("stage mode must fail without finish")
	}
	if res == nil || res.ContextID == "" {
		t.Fatal("result must carry a context id")
	}
}
