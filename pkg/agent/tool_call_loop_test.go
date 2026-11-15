package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type captureStore struct {
	last []Message
}

func (s *captureStore) Save(_ context.Context, execCtx *ExecutionContext) error {
	s.last = append([]Message(nil), execCtx.Messages...)
	return nil
}

func (s *captureStore) Get(context.Context, string) (*ExecutionContext, error) { return nil, nil }
func (s *captureStore) Delete(context.Context, string) error                   { return nil }
func (s *captureStore) ListBySession(context.Context, string) ([]*ExecutionContext, error) {
	return nil, nil
}
func (s *captureStore) ListRecent(context.Context, int) ([]*ExecutionContext, error) { return nil, nil }
func (s *captureStore) CleanupOld(context.Context, time.Duration) error              { return nil }

// sequenceLLM returns a tool call on the first step and a plain answer after,
// which lets the loop produce one assistant tool-call turn and one tool result.
type sequenceLLM struct {
	step int
}

func (s *sequenceLLM) Chat(context.Context, *LLMRequest) (*LLMResponse, error) {
	s.step++
	if s.step == 1 {
		return &LLMResponse{
			Content: "reading the file",
			ToolCalls: []ToolCall{
				{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
			},
		}, nil
	}
	return &LLMResponse{Content: "done"}, nil
}

func TestExecuteLoopRecordsAssistantToolCallsBeforeResults(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&Agent{ID: "exec", Name: "exec", Instructions: "run"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := &captureStore{}
	e := NewEngine(reg, &sequenceLLM{}, &stubTools{}, store, DefaultEngineConfig())

	if _, err := e.Run(context.Background(), &ExecutionRequest{
		SessionID: "session-tool", Message: "go", EntryAgent: "exec",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	var assistantIdx, toolIdx = -1, -1
	for i, msg := range store.last {
		if assistantIdx == -1 && msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			assistantIdx = i
		}
		if toolIdx == -1 && msg.Role == "tool" {
			toolIdx = i
		}
	}
	if assistantIdx == -1 {
		t.Fatalf("no assistant tool-call message recorded: %#v", store.last)
	}
	if toolIdx == -1 {
		t.Fatalf("no tool result message recorded: %#v", store.last)
	}
	if assistantIdx > toolIdx {
		t.Errorf("assistant tool-call message must precede its tool result (assistant=%d tool=%d)", assistantIdx, toolIdx)
	}
	if store.last[assistantIdx].ToolCalls[0].ID != store.last[toolIdx].ToolID {
		t.Errorf("tool call id %q != tool result id %q",
			store.last[assistantIdx].ToolCalls[0].ID, store.last[toolIdx].ToolID)
	}
}
