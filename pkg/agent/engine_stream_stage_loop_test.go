package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// stageLoopClient is a scripted streaming client that also records every
// request, so a test can assert which tools the model was offered.
type stageLoopClient struct {
	mu       sync.Mutex
	scripts  [][]LLMStreamChunk
	requests []*LLMRequest
	calls    int
}

func (c *stageLoopClient) Chat(context.Context, *LLMRequest) (*LLMResponse, error) {
	return &LLMResponse{Content: "non-streaming fallback"}, nil
}

func (c *stageLoopClient) ChatStream(_ context.Context, req *LLMRequest) (<-chan LLMStreamChunk, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
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

func (c *stageLoopClient) request(i int) *LLMRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < 0 || i >= len(c.requests) {
		return nil
	}
	return c.requests[i]
}

// stageToolSet is a tool executor that declares the MCP tools the stage adds.
type stageToolSet struct {
	mu    sync.Mutex
	calls []string
}

func (s *stageToolSet) Execute(_ context.Context, toolName string, _ json.RawMessage, _ *ToolSpecificConfig) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, toolName)
	s.mu.Unlock()
	if toolName == stageFinishTool {
		return `{"status":"finished"}`, nil
	}
	return "README content", nil
}

func (s *stageToolSet) ListTools(context.Context) (map[string]any, error) {
	return map[string]any{
		"file_read":     toolDef("file_read"),
		"file_write":    toolDef("file_write"),
		"code_execute":  toolDef("code_execute"),
		stageFinishTool: toolDef(stageFinishTool),
	}, nil
}

func (s *stageToolSet) executed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func newStageLoopEngine(t *testing.T, client LLMClient, tools ToolExecutor, streamToolCalls bool) *Engine {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register(&Agent{
		ID: "main", Name: "main", Instructions: "dispatch",
		Handoffs: []string{"coder"},
	}); err != nil {
		t.Fatalf("register main: %v", err)
	}
	if err := reg.Register(&Agent{ID: "coder", Name: "coder", Instructions: "code"}); err != nil {
		t.Fatalf("register coder: %v", err)
	}
	cfg := DefaultEngineConfig()
	cfg.StreamToolCalls = streamToolCalls
	return NewEngine(reg, client, tools, nil, cfg)
}

func requestToolNames(req *LLMRequest) []string {
	names := make([]string, 0, len(req.Tools))
	for _, def := range req.Tools {
		fn, ok := def["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}

func TestStageLoopRunsToolThenFinishesOnToolCall(t *testing.T) {
	client := &stageLoopClient{scripts: [][]LLMStreamChunk{
		{
			{Content: "先读 README"},
			{ToolCall: &ToolCall{ID: "call-read", Name: "file_read"}, ToolCallIndex: 0},
			{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"path":`)}, ToolCallIndex: 0},
			{ToolCall: &ToolCall{Arguments: json.RawMessage(`"README.md"}`)}, ToolCallIndex: 0},
			{Done: true},
		},
		{
			{ToolCall: &ToolCall{ID: "call-finish", Name: stageFinishTool}, ToolCallIndex: 0},
			{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"attempt_id":"task-1-a-1","stage":"a","summary":"已读取 README"}`)}, ToolCallIndex: 0},
			{Done: true},
		},
	}}
	tools := &stageToolSet{}
	// StreamToolCalls is off on purpose: stage mode must aggregate regardless.
	e := newStageLoopEngine(t, client, tools, false)

	var events []StreamEvent
	res, err := e.RunStream(context.Background(), &ExecutionRequest{
		SessionID:  "s-stage-tool-loop",
		Message:    "stage=a attempt=1",
		EntryAgent: "main",
		StageMode:  true,
		ContextVars: map[string]any{
			"__stage__":              "a",
			StageToolListVar:         "file_read",
			StageRequiresToolCallVar: "true",
		},
	}, func(ev StreamEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if res == nil || res.ContextID == "" {
		t.Fatalf("result must carry a context id: %+v", res)
	}

	// The stage matrix subset is what the model is offered: read only, and the
	// agent's handoffs are gone so no transfer can swallow the stage.
	first := client.request(0)
	names := requestToolNames(first)
	want := map[string]bool{"file_read": true, stageFinishTool: true}
	for _, name := range names {
		if !want[name] {
			t.Fatalf("stage a offered unexpected tool %q (tools=%v)", name, names)
		}
	}
	if !want["file_read"] {
		t.Fatalf("stage a did not offer file_read: %v", names)
	}

	// One real tool execution, then the finish. stage_finish is not executed
	// through the executor: the engine closes the stage itself.
	if got := tools.executed(); len(got) != 1 || got[0] != "file_read" {
		t.Fatalf("executed tools = %v, want [file_read]", got)
	}

	var starts, results []string
	var finishArgs json.RawMessage
	for _, ev := range events {
		switch ev.Type {
		case EventToolStart:
			starts = append(starts, ev.ToolName)
			if ev.ToolName == stageFinishTool {
				finishArgs = ev.ToolArgs
			}
		case EventToolResult:
			results = append(results, ev.ToolName)
		}
	}
	if len(starts) != 2 || starts[0] != "file_read" || starts[1] != stageFinishTool {
		t.Fatalf("tool start order = %v", starts)
	}
	if len(results) != 2 || results[1] != stageFinishTool {
		t.Fatalf("tool result order = %v", results)
	}
	if _, ok := ParseStageFinishProtocol(StageFinishMarker + " " + string(finishArgs)); !ok {
		t.Fatalf("stage_finish args not a valid payload: %s", finishArgs)
	}
}

func TestStageLoopPairsAssistantToolCallWithResult(t *testing.T) {
	client := &stageLoopClient{scripts: [][]LLMStreamChunk{
		{
			{ToolCall: &ToolCall{ID: "call-read", Name: "file_read"}, ToolCallIndex: 0},
			{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"path":"README.md"}`)}, ToolCallIndex: 0},
			{Done: true},
		},
		{
			{ToolCall: &ToolCall{ID: "call-finish", Name: stageFinishTool}, ToolCallIndex: 0},
			{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"attempt_id":"t-a-1","stage":"a","summary":"ok"}`)}, ToolCallIndex: 0},
			{Done: true},
		},
	}}
	store := &captureStore{}
	reg := NewRegistry()
	if err := reg.Register(&Agent{ID: "main", Name: "main", Instructions: "dispatch"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg := DefaultEngineConfig()
	e := NewEngine(reg, client, &stageToolSet{}, store, cfg)

	if _, err := e.RunStream(context.Background(), &ExecutionRequest{
		SessionID: "s-pair", Message: "stage=a attempt=1", EntryAgent: "main", StageMode: true,
		ContextVars: map[string]any{
			"__stage__":      "a",
			StageToolListVar: "file_read",
		},
	}, func(StreamEvent) {}); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	assistantIdx, toolIdx := -1, -1
	for i, msg := range store.last {
		if assistantIdx < 0 && msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			assistantIdx = i
		}
		if toolIdx < 0 && msg.Role == "tool" {
			toolIdx = i
		}
	}
	if assistantIdx < 0 || toolIdx < 0 {
		t.Fatalf("missing pairing messages: %#v", store.last)
	}
	if assistantIdx > toolIdx {
		t.Fatalf("assistant tool call must precede its result (a=%d t=%d)", assistantIdx, toolIdx)
	}
	if got, want := store.last[assistantIdx].ToolCalls[0].ID, store.last[toolIdx].ToolID; got != want {
		t.Fatalf("tool call id %q != tool result id %q", got, want)
	}
}

func TestStageLoopRejectsFinishWithoutAnyToolCall(t *testing.T) {
	textFinish := `分析完成。` + StageFinishMarker + ` {"attempt_id":"t-a-1","stage":"a","summary":"没有工具"}`
	client := &stageLoopClient{scripts: [][]LLMStreamChunk{{
		{Content: textFinish},
		{Done: true},
	}}}
	e := newStageLoopEngine(t, client, &stageToolSet{}, true)

	var events []StreamEvent
	_, err := e.RunStream(context.Background(), &ExecutionRequest{
		SessionID: "s-no-tool", Message: "stage=a attempt=1", EntryAgent: "main", StageMode: true,
		ContextVars: map[string]any{
			"__stage__":              "a",
			StageToolListVar:         "file_read",
			StageRequiresToolCallVar: "true",
		},
	}, func(ev StreamEvent) {
		events = append(events, ev)
	})
	if !errors.Is(err, ErrStageToolCallRequired) {
		t.Fatalf("error = %v, want ErrStageToolCallRequired", err)
	}

	// It must re-prompt before failing, and the re-prompt has to mention tools.
	nudges := 0
	for _, ev := range events {
		if ev.Type == EventError && ev.Error == ErrStageToolCallRequired.Error() {
			nudges++
		}
	}
	if nudges == 0 {
		t.Fatalf("no visible error event for the rejected finish: %+v", events)
	}
	if calls := len(client.requests); calls != stageNudgeLimit+1 {
		t.Fatalf("model calls = %d, want %d (initial + %d nudges)", calls, stageNudgeLimit+1, stageNudgeLimit)
	}
	last := client.request(len(client.requests) - 1)
	if last == nil {
		t.Fatal("missing last request")
	}
	body := messageText(last.Messages)
	if !strings.Contains(body, "还没有调用任何工具") {
		t.Fatalf("nudge message missing from the retry prompt: %q", body)
	}
}

func TestStageLoopAcceptsFinishWithoutToolCallWhenNotRequired(t *testing.T) {
	textFinish := `分析完成。` + StageFinishMarker + ` {"attempt_id":"t-a-1","stage":"a","summary":"没有工具"}`
	client := &stageLoopClient{scripts: [][]LLMStreamChunk{{
		{Content: textFinish},
		{Done: true},
	}}}
	e := newStageLoopEngine(t, client, &stageToolSet{}, true)

	if _, err := e.RunStream(context.Background(), &ExecutionRequest{
		SessionID: "s-optional", Message: "stage=a attempt=1", EntryAgent: "main", StageMode: true,
		ContextVars: map[string]any{"__stage__": "a", StageToolListVar: "file_read"},
	}, func(StreamEvent) {}); err != nil {
		t.Fatalf("finish without a mandatory tool call must stay allowed: %v", err)
	}
}

func TestStageLoopRejectsMalformedFinishPayload(t *testing.T) {
	client := &stageLoopClient{scripts: [][]LLMStreamChunk{
		{
			{ToolCall: &ToolCall{ID: "call-finish", Name: stageFinishTool}, ToolCallIndex: 0},
			{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"stage":"a"}`)}, ToolCallIndex: 0},
			{Done: true},
		},
		{
			{ToolCall: &ToolCall{ID: "call-finish-2", Name: stageFinishTool}, ToolCallIndex: 0},
			{ToolCall: &ToolCall{Arguments: json.RawMessage(`{"attempt_id":"t-a-1","stage":"a","summary":"ok"}`)}, ToolCallIndex: 0},
			{Done: true},
		},
	}}
	e := newStageLoopEngine(t, client, &stageToolSet{}, true)

	var results []string
	if _, err := e.RunStream(context.Background(), &ExecutionRequest{
		SessionID: "s-bad-finish", Message: "stage=a attempt=1", EntryAgent: "main", StageMode: true,
		ContextVars: map[string]any{"__stage__": "a", StageToolListVar: "file_read"},
	}, func(ev StreamEvent) {
		if ev.Type == EventToolResult && ev.ToolName == stageFinishTool {
			results = append(results, ev.ToolResult)
		}
	}); err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("stage_finish results = %v", results)
	}
	if !strings.Contains(results[0], "不合法") {
		t.Fatalf("malformed payload must be reported back to the model: %v", results)
	}
	if results[1] != `{"status":"finished"}` {
		t.Fatalf("valid retry not accepted: %v", results)
	}
}

func messageText(messages []Message) string {
	var sb strings.Builder
	for _, m := range messages {
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}
