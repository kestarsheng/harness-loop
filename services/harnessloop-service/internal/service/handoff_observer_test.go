package service

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/tracing"
	"agent-platform/services/harnessloop-service/internal/metrics"
)

func TestHandoffObserverRecordsMetricAndSpan(t *testing.T) {
	harness := metrics.NewHarness()
	exp := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))

	obs := HandoffObservability{
		Metrics: harness,
		Tracer:  tracing.NewTracerWithProvider(provider, "handoff-test"),
	}
	obs.HandoffTransition(context.Background(), &orchestrator.HandoffRecord{
		HandoffID: "ho-1", FromRole: "planner", ToRole: "executor",
		Status: orchestrator.HandoffCreated,
	})
	obs.HandoffTransition(context.Background(), &orchestrator.HandoffRecord{
		HandoffID: "ho-1", FromRole: "planner", ToRole: "executor",
		Status: orchestrator.HandoffRejected,
	})

	requested, _ := harness.Reg.Find(metrics.MetricHandoffTotal, map[string]string{
		"status": "requested", "from_role": "planner", "to_role": "executor",
	})
	rejected, _ := harness.Reg.Find(metrics.MetricHandoffTotal, map[string]string{
		"status": "rejected", "from_role": "planner", "to_role": "executor",
	})
	if requested != 1 || rejected != 1 {
		t.Fatalf("handoff counters = requested:%v rejected:%v", requested, rejected)
	}

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("handoff spans = %d, want 2", len(spans))
	}
	for _, span := range spans {
		if span.Name != tracing.SpanHandoff {
			t.Fatalf("span name = %q, want handoff", span.Name)
		}
		attrs := map[string]string{}
		for _, kv := range span.Attributes {
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		if attrs["handoff_id"] != "ho-1" || attrs["from"] != "planner" || attrs["to"] != "executor" {
			t.Fatalf("handoff span attrs = %v", attrs)
		}
	}
}
