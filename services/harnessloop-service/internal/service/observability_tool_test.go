package service

import (
	"context"
	"errors"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"agent-platform/pkg/harnessloop/tracing"
	agentpb "agent-platform/pkg/pb/agent"
)

type denyGate struct{}

func (denyGate) AuthorizeTool(context.Context, ToolCallInput) error {
	return errors.New("E_POLICY_STAGE_MATRIX: tool not allowed in stage c")
}

func TestStageRunnerEmitsToolSpans(t *testing.T) {
	agent := &stubAgent{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "tool_call", ToolCall: &agentpb.ToolCall{Name: "read_file", Arguments: `{"path":"a.go"}`}},
		{Type: "tool_call", ToolCall: &agentpb.ToolCall{
			Name: "stage_finish", Arguments: `{"attempt_id":"task-1-c-1","stage":"c","summary":"ok"}`,
		}},
	}}
	exp := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	r := NewStageRunner(agent).WithTracer(tracing.NewTracerWithProvider(provider, "stage-test"))

	parentCtx, parent := provider.Tracer("runtime").Start(context.Background(), "stage")
	if _, err := r.Run(parentCtx, baseStageInput()); err != nil {
		t.Fatalf("run: %v", err)
	}
	parent.End()

	spans := exp.GetSpans()
	var tools []tracetest.SpanStub
	for _, span := range spans {
		if span.Name == tracing.SpanTool {
			tools = append(tools, span)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool spans = %d, want 2 (%v)", len(tools), namesOf(spans))
	}
	for _, tool := range tools {
		if tool.Parent.SpanID() != parent.SpanContext().SpanID() {
			t.Fatalf("tool span parent = %s, want stage %s", tool.Parent.SpanID(), parent.SpanContext().SpanID())
		}
	}
	attrs := map[string]string{}
	for _, kv := range tools[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["model_id"] != "qwen-max" {
		t.Fatalf("tool span must carry model_id: %v", attrs)
	}
}

func TestStageRunnerToolSpanRecordsPolicyDeny(t *testing.T) {
	agent := &stubAgent{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "tool_call", ToolCall: &agentpb.ToolCall{Name: "run_command", Arguments: `{"cmd":"rm -rf /"}`}},
	}}
	exp := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	r := NewStageRunner(agent).
		WithTracer(tracing.NewTracerWithProvider(provider, "stage-test")).
		WithToolGate(denyGate{})

	_, _ = r.Run(context.Background(), baseStageInput())

	var found bool
	for _, span := range exp.GetSpans() {
		if span.Name != tracing.SpanTool {
			continue
		}
		found = true
		attrs := map[string]string{}
		for _, kv := range span.Attributes {
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		if attrs["policy_decision"] != "deny" {
			t.Fatalf("denied tool span attrs = %v", attrs)
		}
	}
	if !found {
		t.Fatal("a denied tool call must still produce a span")
	}
}

func namesOf(spans []tracetest.SpanStub) []string {
	out := make([]string, len(spans))
	for i, span := range spans {
		out[i] = span.Name
	}
	return out
}
