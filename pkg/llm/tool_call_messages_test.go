package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func captureChatBody(t *testing.T, cfg Config, req *ChatRequest) map[string]any {
	t.Helper()

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	cfg.BaseURL = srv.URL
	client := NewOpenAIClient(cfg)
	if _, err := client.Chat(context.Background(), req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	return got
}

func toolCallRequests() []Message {
	return []Message{
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "call-1", Type: "function", Function: ToolCallFunction{Name: "read_file", Arguments: `{"path":"a.txt"}`}},
				{ID: "call-2", Type: "function", Function: ToolCallFunction{Name: "write_file", Arguments: `{"path":"b.txt"}`}},
			},
		},
		{Role: "tool", Content: "file contents", ToolCallID: "call-1"},
		{Role: "tool", Content: "written", ToolCallID: "call-2"},
	}
}

func TestChatOmitsToolCallFieldsByDefault(t *testing.T) {
	got := captureChatBody(t, Config{Model: "m", APIKey: "k"}, &ChatRequest{Messages: toolCallRequests()})

	messages, ok := got["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %#v", got["messages"])
	}

	for i, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("message %d is not an object: %#v", i, raw)
		}
		if _, present := msg["tool_calls"]; present {
			t.Errorf("message %d must not carry tool_calls when the switch is off", i)
		}
		if _, present := msg["tool_call_id"]; present {
			t.Errorf("message %d must not carry tool_call_id when the switch is off", i)
		}
	}
}

func TestChatEmitsPairedToolCallFieldsWhenEnabled(t *testing.T) {
	got := captureChatBody(t, Config{Model: "m", APIKey: "k", EmitToolCallMessages: true}, &ChatRequest{Messages: toolCallRequests()})

	messages, ok := got["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %#v", got["messages"])
	}

	assistant, _ := messages[0].(map[string]any)
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("assistant tool_calls = %#v", assistant["tool_calls"])
	}
	if _, present := assistant["index"]; present {
		t.Errorf("request tool_calls must not carry streaming index: %#v", assistant)
	}

	wantIDs := []string{"call-1", "call-2"}
	for i, raw := range calls {
		call, _ := raw.(map[string]any)
		if call["id"] != wantIDs[i] {
			t.Errorf("tool_calls[%d].id = %v, want %s", i, call["id"], wantIDs[i])
		}
		if call["type"] != "function" {
			t.Errorf("tool_calls[%d].type = %v, want function", i, call["type"])
		}
		fn, _ := call["function"].(map[string]any)
		if fn["name"] == "" || fn["arguments"] == "" {
			t.Errorf("tool_calls[%d].function incomplete: %#v", i, fn)
		}
	}

	for i, wantID := range wantIDs {
		msg, _ := messages[i+1].(map[string]any)
		if msg["role"] != "tool" {
			t.Errorf("message %d role = %v, want tool", i+1, msg["role"])
		}
		if msg["tool_call_id"] != wantID {
			t.Errorf("message %d tool_call_id = %v, want %s", i+1, msg["tool_call_id"], wantID)
		}
	}
}

func TestToOpenAIMessageDefaultsCallType(t *testing.T) {
	client := NewOpenAIClient(Config{EmitToolCallMessages: true})
	out := client.toOpenAIMessage(Message{
		Role:      "assistant",
		ToolCalls: []ToolCall{{ID: "call-1", Function: ToolCallFunction{Name: "noop"}}},
	})
	if len(out.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(out.ToolCalls))
	}
	if out.ToolCalls[0].Type != "function" {
		t.Errorf("type = %q, want function", out.ToolCalls[0].Type)
	}
}
