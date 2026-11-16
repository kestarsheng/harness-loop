package service

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"agent-platform/pkg/harnessloop/tracing"
	"agent-platform/services/harnessloop-service/internal/metrics"
)

func TestRuntimeEmitsDFXM01MetricsAndSpans(t *testing.T) {
	rt, _, _, _ := newTestRuntime(&stubStages{})

	harness := metrics.NewHarness()
	exp := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	rt.Metrics = harness
	rt.Tracer = tracing.NewTracerWithProvider(provider, "harnessloop-test")

	if _, err := rt.Run(context.Background(), runtimeInput()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got, ok := harness.Reg.Find(metrics.MetricTasksTotal, map[string]string{"status": "completed"}); !ok || got != 1 {
		t.Fatalf("tasks_total{completed} = %v (ok=%v)", got, ok)
	}
	var acceptancePass float64
	for _, sample := range harness.Reg.Snapshot() {
		if sample.Name == metrics.MetricAcceptanceTotal && sample.Labels["verdict"] == "pass" {
			acceptancePass += sample.Value
		}
	}
	if acceptancePass < 4 {
		t.Fatalf("acceptance pass counter = %v, want at least one per stage", acceptancePass)
	}
	var stageSamples int
	for _, sample := range harness.Reg.Snapshot() {
		if sample.Name == metrics.MetricStageDurationSeconds && sample.Value > 0 {
			stageSamples++
		}
	}
	if stageSamples == 0 {
		t.Fatal("stage duration histogram never observed")
	}

	spans := exp.GetSpans()
	counts := map[string]int{}
	for _, span := range spans {
		counts[span.Name]++
	}
	if counts[tracing.SpanTask] != 1 {
		t.Fatalf("task spans = %d, want 1 (%v)", counts[tracing.SpanTask], counts)
	}
	if counts[tracing.SpanStage] < 4 {
		t.Fatalf("stage spans = %d, want >= 4", counts[tracing.SpanStage])
	}
	if counts[tracing.SpanAcceptance] < 4 {
		t.Fatalf("acceptance spans = %d, want >= 4", counts[tracing.SpanAcceptance])
	}

	// Every acceptance span must hang off a stage span, and every stage span
	// off the single task span.
	var taskID string
	for _, span := range spans {
		if span.Name == tracing.SpanTask {
			taskID = span.SpanContext.SpanID().String()
		}
	}
	stageIDs := map[string]bool{}
	for _, span := range spans {
		if span.Name == tracing.SpanStage {
			if span.Parent.SpanID().String() != taskID {
				t.Fatalf("stage span parent = %s, want task %s", span.Parent.SpanID(), taskID)
			}
			stageIDs[span.SpanContext.SpanID().String()] = true
		}
	}
	for _, span := range spans {
		if span.Name == tracing.SpanAcceptance && !stageIDs[span.Parent.SpanID().String()] {
			t.Fatalf("acceptance span parent %s is not a stage span", span.Parent.SpanID())
		}
	}
}
