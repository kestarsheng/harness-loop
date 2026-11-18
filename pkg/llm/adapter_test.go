package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestChatNormalizesFinishReasonAndToolCall(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{
				"message":{
					"role":"assistant",
					"content":"hi",
					"tool_calls":[{"id":"call-1","function":{"name":"read_file","arguments":"{}"}}]
				},
				"finish_reason":"function_call"
			}],
			"usage":{"prompt_tokens":3,"total_tokens":7}
		}`))
	}))
	defer srv.Close()

	client := NewOpenAIClient(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	resp, err := client.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", resp.FinishReason)
	}
	if resp.Content != "hi" || resp.TotalTokens != 7 || resp.PromptTokens != 3 {
		t.Errorf("response = %#v", resp)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Type != "function" {
		t.Errorf("tool call type = %q, want function", resp.ToolCalls[0].Type)
	}
	if resp.Cost == 0 {
		t.Error("cost must still be calculated")
	}
}

func TestNormalizeFinishReason(t *testing.T) {
	cases := map[string]string{
		"stop":          "stop",
		"end_turn":      "stop",
		"max_tokens":    "length",
		"function_call": "tool_calls",
		"safety":        "content_filter",
		"weird":         "weird",
		"":              "",
	}
	for in, want := range cases {
		if got := normalizeFinishReason(in); got != want {
			t.Errorf("normalizeFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChatStreamNormalizesDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		frames := []string{
			`data: {"choices":[{"delta":{"content":"he"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read_file","arguments":"{}"}}]}}]}`,
			`data: [DONE]`,
		}
		for _, f := range frames {
			_, _ = fmt.Fprintf(w, "%s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := NewOpenAIClient(Config{BaseURL: srv.URL, Model: "m"})
	ch, err := client.ChatStream(context.Background(), &ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var content string
	var sawCall, done bool
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream chunk error: %v", chunk.Error)
		}
		content += chunk.Content
		if chunk.ToolCall != nil {
			sawCall = true
			if chunk.ToolCall.ID != "call-1" || chunk.ToolCall.Function.Name != "read_file" {
				t.Errorf("tool call = %#v", chunk.ToolCall)
			}
			if chunk.ToolCall.Type != "function" {
				t.Errorf("tool call type = %q, want function", chunk.ToolCall.Type)
			}
			if chunk.ToolCallIndex != 0 {
				t.Errorf("tool call index = %d, want 0", chunk.ToolCallIndex)
			}
		}
		if chunk.Done {
			done = true
		}
	}
	if content != "he" || !sawCall || !done {
		t.Errorf("content=%q sawCall=%v done=%v", content, sawCall, done)
	}
}

func TestChatStreamParsesUsageFrame(t *testing.T) {
	var requestBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		frames := []string{
			`data: {"choices":[{"delta":{"content":"hi"}}]}`,
			// The usage frame carries no choices; it must not be dropped.
			`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			`data: [DONE]`,
		}
		for _, f := range frames {
			_, _ = fmt.Fprintf(w, "%s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := NewOpenAIClient(Config{BaseURL: srv.URL, Model: "m"})
	ch, err := client.ChatStream(context.Background(), &ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var usage *Usage
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream chunk error: %v", chunk.Error)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if usage == nil {
		t.Fatal("usage frame was dropped")
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 3 || usage.TotalTokens != 10 {
		t.Errorf("usage = %+v", usage)
	}
	opts, _ := requestBody["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v, want true", opts["include_usage"])
	}
}

func TestChatClassifiesTimeoutAndCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	client := NewOpenAIClient(Config{BaseURL: srv.URL, Model: "m"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.Chat(ctx, &ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if !IsKind(err, ErrorKindTimeout) {
		t.Fatalf("timeout err = %v, want kind timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout must preserve context.DeadlineExceeded: %v", err)
	}

	canceledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	_, err = client.Chat(canceledCtx, &ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if !IsKind(err, ErrorKindCanceled) {
		t.Fatalf("cancel err = %v, want kind canceled", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel must preserve context.Canceled: %v", err)
	}
}

func TestChatClassifiesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit","code":"rate_limit_exceeded"}}`))
	}))
	defer srv.Close()

	client := NewOpenAIClient(Config{BaseURL: srv.URL, Model: "m"})
	_, err := client.Chat(context.Background(), &ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if !IsKind(err, ErrorKindRateLimit) {
		t.Fatalf("err = %v, want rate_limit", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests || apiErr.ProviderCode != "rate_limit_exceeded" {
		t.Errorf("api error = %#v", apiErr)
	}
	if apiErr.Message != "slow down" {
		t.Errorf("message = %q", apiErr.Message)
	}
}

func TestEmbedBatchPreservesOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		// Return in reverse index order to prove reordering by index.
		_, _ = w.Write([]byte(`{"data":[
			{"embedding":[2,2],"index":1},
			{"embedding":[1,1],"index":0}
		]}`))
	}))
	defer srv.Close()

	client := NewOpenAIClient(Config{BaseURL: srv.URL})
	got, err := client.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(got) != 2 || got[0][0] != 1 || got[1][0] != 2 {
		t.Errorf("embeddings = %#v", got)
	}
}
