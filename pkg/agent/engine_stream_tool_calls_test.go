package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// scriptedStreamClient replays a fixed list of streaming chunks per call.
// Each ChatStream invocation consumes the next script; the last one repeats.
type scriptedStreamClient struct {
	mu      sync.Mutex
	scripts [][]LLMStreamChunk
	calls   int
}

func (c *scriptedStreamClient) Chat(context.Context, *LLMRequest) (*LLMResponse, error) {
	return &LLMResponse{Content: "non-streaming fallback"}, nil
}

func (c *scriptedStreamClient) ChatStream(context.Context, *LLMRequest) (<-chan LLMStreamChunk, error) {
	c.mu.Lock()
	idx := c.calls
	c.calls++
	c.mu.Unlock()
	if idx >= len(c.scripts) {
		idx = len(c.scripts) - 1
	}

	ch := make(chan LLMStreamChunk, len(c.scripts[idx]))
	for _, chunk := range c.scripts[idx] {
		ch <- chunk
	}
	close(ch)
	return ch, nil
}

func newStreamToolCallEngine(t *testing.T, client LLMClient, tools ToolExecutor, enabled bool) *Engine {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register(&Agent{ID: "exec", Name: "exec", Instructions: "run"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg := DefaultEngineConfig()
	cfg.StreamToolCalls = enabled
	return NewEngine(reg, client, tools, nil, cfg)
}

func callStreaming(t *testing.T, e *Engine) (*LLMResponse, []StreamEvent, error) {
	t.Helper()
	var events []StreamEvent
	resp, err := e.callLLMStreaming(context.Background(), &LLMRequest{}, func(ev StreamEvent) {
		events = append(events, ev)
	}, "exec", "exec", 1)
	return resp, events, err
}

func TestStreamToolCallsAggregatesThreeArgumentFragments(t *testing.T) {
	client := &scriptedStreamClient{scripts: [][]LLMStreamChunk{{
		{Content: "calling "},
		{ToolCall: &ToolCall{ID: "call-1", Name: "read_file"}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"path":"a`)}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`.txt","mode"`)}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`:"read"}`)}, ToolCallIndex: 0},
		{Done: true},
	}}}
	e := newStreamToolCallEngine(t, client, &stubTools{}, true)

	resp, events, err := callStreaming(t, e)
	if err != nil {
		t.Fatalf("callLLMStreaming: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call-1" || tc.Name != "read_file" {
		t.Fatalf("tool call = %+v", tc)
	}
	if got, want := string(tc.Arguments), `{"path":"a.txt","mode":"read"}`; got != want {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
	if !json.Valid(tc.Arguments) {
		t.Fatalf("aggregated arguments are not valid JSON: %q", tc.Arguments)
	}
	if resp.Content != "calling " {
		t.Fatalf("content = %q", resp.Content)
	}
	if len(events) != 1 || events[0].Type != EventToken || events[0].Content != "calling " {
		t.Fatalf("token events changed: %+v", events)
	}
}

func TestStreamToolCallsAggregatesParallelCalls(t *testing.T) {
	client := &scriptedStreamClient{scripts: [][]LLMStreamChunk{{
		{ToolCall: &ToolCall{ID: "call-0", Name: "read_file"}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{ID: "call-1", Name: "search"}, ToolCallIndex: 1},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"path":"a`)}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"query":"weather`)}, ToolCallIndex: 1},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`.txt"}`)}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`"}`)}, ToolCallIndex: 1},
		{Done: true},
	}}}
	e := newStreamToolCallEngine(t, client, &stubTools{}, true)

	resp, _, err := callStreaming(t, e)
	if err != nil {
		t.Fatalf("callLLMStreaming: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call-0" || string(resp.ToolCalls[0].Arguments) != `{"path":"a.txt"}` {
		t.Fatalf("first tool call = %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].ID != "call-1" || string(resp.ToolCalls[1].Arguments) != `{"query":"weather"}` {
		t.Fatalf("second tool call = %+v", resp.ToolCalls[1])
	}
}

func TestStreamToolCallsHandlesOutOfOrderIndices(t *testing.T) {
	client := &scriptedStreamClient{scripts: [][]LLMStreamChunk{{
		{ToolCall: &ToolCall{ID: "call-1", Name: "search"}, ToolCallIndex: 1},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"query":"b"}`)}, ToolCallIndex: 1},
		{ToolCall: &ToolCall{ID: "call-0", Name: "read_file"}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"path":"c"}`)}, ToolCallIndex: 0},
		{Done: true},
	}}}
	e := newStreamToolCallEngine(t, client, &stubTools{}, true)

	resp, _, err := callStreaming(t, e)
	if err != nil {
		t.Fatalf("callLLMStreaming: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call-0" || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("out-of-order result not sorted by index: %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[1].ID != "call-1" || resp.ToolCalls[1].Name != "search" {
		t.Fatalf("out-of-order result not sorted by index: %+v", resp.ToolCalls)
	}
}

func TestStreamToolCallsPlainTextWithoutToolCalls(t *testing.T) {
	client := &scriptedStreamClient{scripts: [][]LLMStreamChunk{{
		{Content: "hello"},
		{Content: " world"},
		{Done: true},
	}}}
	e := newStreamToolCallEngine(t, client, &stubTools{}, true)

	resp, events, err := callStreaming(t, e)
	if err != nil {
		t.Fatalf("callLLMStreaming: %v", err)
	}
	if resp.Content != "hello world" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.ToolCalls != nil {
		t.Fatalf("plain text must not produce tool calls: %+v", resp.ToolCalls)
	}
	if len(events) != 2 || events[0].Content != "hello" || events[1].Content != " world" {
		t.Fatalf("token stream changed: %+v", events)
	}
}

func TestStreamToolCallsDisabledKeepsToolCallsNil(t *testing.T) {
	client := &scriptedStreamClient{scripts: [][]LLMStreamChunk{{
		{Content: "text"},
		{ToolCall: &ToolCall{ID: "call-1", Name: "read_file"}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"path":"a.txt"}`)}, ToolCallIndex: 0},
		{Done: true},
	}}}
	e := newStreamToolCallEngine(t, client, &stubTools{}, false)

	resp, _, err := callStreaming(t, e)
	if err != nil {
		t.Fatalf("callLLMStreaming: %v", err)
	}
	if resp.Content != "text" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.ToolCalls != nil {
		t.Fatalf("disabled switch must keep ToolCalls nil: %+v", resp.ToolCalls)
	}
}

func TestStreamToolCallsRejectsTruncatedArguments(t *testing.T) {
	client := &scriptedStreamClient{scripts: [][]LLMStreamChunk{{
		{ToolCall: &ToolCall{ID: "call-1", Name: "read_file"}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"path":`)}, ToolCallIndex: 0},
		{Done: true},
	}}}
	e := newStreamToolCallEngine(t, client, &stubTools{}, true)

	resp, _, err := callStreaming(t, e)
	if err == nil {
		t.Fatal("truncated arguments must fail instead of reaching the executor")
	}
	if !errors.Is(err, ErrStreamToolCallIncomplete) {
		t.Fatalf("error = %v, want ErrStreamToolCallIncomplete", err)
	}
	if resp != nil {
		t.Fatalf("response must be nil on failure: %+v", resp)
	}
}

func TestStreamToolCallsRejectsArgumentsWithoutName(t *testing.T) {
	client := &scriptedStreamClient{scripts: [][]LLMStreamChunk{{
		{ToolCall: &ToolCall{ID: "call-1"}, ToolCallIndex: 0},
		{ToolCall: &ToolCall{Arguments: json.RawMessage(`{}`)}, ToolCallIndex: 0},
		{Done: true},
	}}}
	e := newStreamToolCallEngine(t, client, &stubTools{}, true)

	_, _, err := callStreaming(t, e)
	if !errors.Is(err, ErrStreamToolCallIncomplete) {
		t.Fatalf("error = %v, want ErrStreamToolCallIncomplete", err)
	}
}

func TestStreamToolCallsPropagatesMidStreamError(t *testing.T) {
	boom := errors.New("stream broke")
	client := &scriptedStreamClient{scripts: [][]LLMStreamChunk{{
		{Content: "partial"},
		{Error: boom},
	}}}
	e := newStreamToolCallEngine(t, client, &stubTools{}, true)

	resp, _, err := callStreaming(t, e)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
	if resp != nil {
		t.Fatalf("response must be nil on stream error: %+v", resp)
	}
}

type recordingTools struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingTools) Execute(_ context.Context, toolName string, _ json.RawMessage, _ *ToolSpecificConfig) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, toolName)
	r.mu.Unlock()
	return "file-content", nil
}

func (r *recordingTools) ListTools(context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestStreamToolCallsLetExistingLoopExecuteToolBranch(t *testing.T) {
	client := &scriptedStreamClient{scripts: [][]LLMStreamChunk{
		{
			{ToolCall: &ToolCall{ID: "call-1", Name: "read_file"}, ToolCallIndex: 0},
			{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"path":"a.txt"}`)}, ToolCallIndex: 0},
			{Done: true},
		},
		{
			{Content: "done"},
			{Done: true},
		},
	}}
	tools := &recordingTools{}
	e := newStreamToolCallEngine(t, client, tools, true)

	var events []StreamEvent
	res, err := e.RunStream(context.Background(), &ExecutionRequest{
		SessionID: "s-stream-tool", Message: "read it", EntryAgent: "exec",
	}, func(ev StreamEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if res == nil || res.Status != AgentStatusCompleted {
		t.Fatalf("result = %+v", res)
	}
	if len(tools.calls) != 1 || tools.calls[0] != "read_file" {
		t.Fatalf("tool branch not executed: %v", tools.calls)
	}

	var started, resulted bool
	for _, ev := range events {
		if ev.Type == EventToolStart && ev.ToolName == "read_file" {
			started = true
		}
		if ev.Type == EventToolResult && ev.ToolName == "read_file" {
			resulted = true
		}
	}
	if !started || !resulted {
		t.Fatalf("missing tool events: %+v", events)
	}
}
