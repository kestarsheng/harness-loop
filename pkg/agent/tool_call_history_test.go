package agent

import (
	"encoding/json"
	"testing"
)

func TestAddAssistantToolCallMessagePairsWithToolResult(t *testing.T) {
	ctx := NewExecutionContext("session-1")
	ctx.AddMessage("user", "inspect the workspace")
	ctx.AddAssistantToolCallMessage("reading files", []ToolCall{
		{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
		{ID: "call-2", Name: "write_file", Arguments: json.RawMessage(`{"path":"b.txt"}`)},
	})
	ctx.AddToolMessage("read_file", "call-1", "contents")
	ctx.AddToolMessage("write_file", "call-2", "written")

	if len(ctx.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(ctx.Messages))
	}

	assistant := ctx.Messages[1]
	if assistant.Role != "assistant" || assistant.Content != "reading files" {
		t.Fatalf("assistant message = %#v", assistant)
	}
	if len(assistant.ToolCalls) != 2 {
		t.Fatalf("assistant tool calls = %d, want 2", len(assistant.ToolCalls))
	}
	if assistant.ToolCalls[0].ID != "call-1" || assistant.ToolCalls[0].Name != "read_file" {
		t.Errorf("first tool call = %#v", assistant.ToolCalls[0])
	}
	if assistant.ToolCalls[0].Arguments != `{"path":"a.txt"}` {
		t.Errorf("first tool call arguments = %q", assistant.ToolCalls[0].Arguments)
	}

	calls := map[string]bool{}
	results := map[string]bool{}
	for _, msg := range ctx.Messages {
		for _, call := range msg.ToolCalls {
			calls[call.ID] = true
		}
		if msg.Role == "tool" {
			results[msg.ToolID] = true
		}
	}
	for id := range calls {
		if !results[id] {
			t.Errorf("tool call %s has no matching tool result", id)
		}
	}
	for id := range results {
		if !calls[id] {
			t.Errorf("tool result %s has no matching tool call", id)
		}
	}
}

func TestAddAssistantToolCallMessageWithoutCalls(t *testing.T) {
	ctx := NewExecutionContext("session-1")
	ctx.AddAssistantToolCallMessage("plain answer", nil)

	if len(ctx.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(ctx.Messages))
	}
	if ctx.Messages[0].Role != "assistant" || ctx.Messages[0].Content != "plain answer" {
		t.Errorf("message = %#v", ctx.Messages[0])
	}
	if len(ctx.Messages[0].ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %#v", ctx.Messages[0].ToolCalls)
	}
}

func TestCheckpointRoundTripKeepsToolCalls(t *testing.T) {
	original := []Message{
		{Role: "user", Content: "go"},
		{
			Role:    "assistant",
			Content: "working",
			ToolCalls: []MessageToolCall{
				{ID: "call-1", Name: "read_file", Arguments: `{"path":"a.txt"}`},
			},
		},
		{Role: "tool", Name: "read_file", ToolID: "call-1", Content: "contents"},
	}

	restored := checkpointMessagesToAgent(messagesToCheckpoint(original))
	if len(restored) != len(original) {
		t.Fatalf("message count = %d, want %d", len(restored), len(original))
	}
	if len(restored[1].ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(restored[1].ToolCalls))
	}
	if restored[1].ToolCalls[0] != original[1].ToolCalls[0] {
		t.Errorf("tool call = %#v, want %#v", restored[1].ToolCalls[0], original[1].ToolCalls[0])
	}
	if restored[2].ToolID != "call-1" {
		t.Errorf("tool message id = %q, want call-1", restored[2].ToolID)
	}
}
