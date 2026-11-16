package tracing

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newTestTracer() (*Tracer, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	return &Tracer{tracer: tp.Tracer("harnessloop-test")}, exp
}

func attrsOf(t *testing.T, spans []tracetest.SpanStub, name string) map[string]string {
	t.Helper()
	for _, s := range spans {
		if s.Name != name {
			continue
		}
		out := map[string]string{}
		for _, kv := range s.Attributes {
			out[string(kv.Key)] = kv.Value.Emit()
		}
		out["__span_id"] = s.SpanContext.SpanID().String()
		out["__parent_id"] = s.Parent.SpanID().String()
		return out
	}
	t.Fatalf("span %q not found in %v", name, names(spans))
	return nil
}

func names(spans []tracetest.SpanStub) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}

func TestTraceTaskStageAcceptanceToolHierarchy(t *testing.T) {
	tr, exp := newTestTracer()

	task := tr.StartTask(context.Background(), "tenant-a", "proj-a", "task-1")
	stage := tr.StartStage(task, "d", 2)
	acceptance := tr.StartAcceptance(stage, "d", 2, "pass")
	tool := tr.StartTool(stage, "run_command", map[string]string{"policy_decision": "allow", "rule_id": "r-1", "model_id": "m-1", "is_fallback": "false"})
	tool.End()
	acceptance.End()
	stage.End()
	task.End()

	spans := exp.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("spans = %v", names(spans))
	}
	taskAttrs := attrsOf(t, spans, SpanTask)
	stageAttrs := attrsOf(t, spans, SpanStage)
	acceptanceAttrs := attrsOf(t, spans, SpanAcceptance)
	toolAttrs := attrsOf(t, spans, SpanTool)

	if stageAttrs["__parent_id"] != taskAttrs["__span_id"] {
		t.Fatalf("stage parent mismatch")
	}
	if acceptanceAttrs["__parent_id"] != stageAttrs["__span_id"] {
		t.Fatalf("acceptance parent must be the stage span")
	}
	if toolAttrs["__parent_id"] != stageAttrs["__span_id"] {
		t.Fatalf("tool parent must be the stage span")
	}
	if acceptanceAttrs["verdict"] != "pass" || acceptanceAttrs["attempt"] != "2" {
		t.Fatalf("acceptance attrs = %v", acceptanceAttrs)
	}
	if toolAttrs["policy_decision"] != "allow" || toolAttrs["rule_id"] != "r-1" || toolAttrs["model_id"] != "m-1" {
		t.Fatalf("tool attrs = %v", toolAttrs)
	}
}

func TestTraceSubtaskAndHandoffHierarchy(t *testing.T) {
	tr, exp := newTestTracer()

	task := tr.StartTask(context.Background(), "tenant-a", "proj-a", "task-1")
	sub1 := tr.StartSubtask(task, "sub-1", "dag-1", 1, "executor")
	sub2 := tr.StartSubtask(task, "sub-2", "dag-1", 1, "verifier")
	handoff := tr.StartHandoff(sub1, "ho-1", "executor", "verifier")
	tool := tr.StartTool(sub2, "read_file", nil)
	tool.End()
	handoff.End()
	sub2.End()
	sub1.End()
	task.End()

	spans := exp.GetSpans()
	taskAttrs := attrsOf(t, spans, SpanTask)
	sub1Attrs := subtaskAttrs(t, spans, "sub-1")
	handoffAttrs := attrsOf(t, spans, SpanHandoff)

	if sub1Attrs["__parent_id"] != taskAttrs["__span_id"] {
		t.Fatal("subtask parent must be the task span")
	}
	if sub1Attrs["role"] != "executor" || sub1Attrs["depth"] != "1" || sub1Attrs["dag_id"] != "dag-1" {
		t.Fatalf("subtask attrs = %v", sub1Attrs)
	}
	if handoffAttrs["__parent_id"] != sub1Attrs["__span_id"] {
		t.Fatal("handoff parent must be the subtask span")
	}
	if handoffAttrs["from"] != "executor" || handoffAttrs["to"] != "verifier" {
		t.Fatalf("handoff attrs = %v", handoffAttrs)
	}

	// Two subtasks under the same task must share one trace id.
	var traceIDs = map[string]bool{}
	for _, s := range spans {
		if s.Name == SpanSubtask {
			traceIDs[s.SpanContext.TraceID().String()] = true
		}
	}
	if len(traceIDs) != 1 {
		t.Fatalf("subtasks must share the task trace id: %v", traceIDs)
	}
}

func TestTraceAggregationUnderStage(t *testing.T) {
	tr, exp := newTestTracer()

	task := tr.StartTask(context.Background(), "tenant-a", "proj-a", "task-1")
	stage := tr.StartStage(task, "d", 1)
	agg := tr.StartAggregation(stage, 4, 1)
	agg.End()
	stage.End()
	task.End()

	spans := exp.GetSpans()
	stageAttrs := attrsOf(t, spans, SpanStage)
	aggAttrs := attrsOf(t, spans, SpanAggregation)
	if aggAttrs["__parent_id"] != stageAttrs["__span_id"] {
		t.Fatal("aggregation parent must be the stage span")
	}
	if aggAttrs["items"] != "4" || aggAttrs["conflicts"] != "1" {
		t.Fatalf("aggregation attrs = %v", aggAttrs)
	}
}

func TestTraceIDsForEventCorrelation(t *testing.T) {
	tr, _ := newTestTracer()
	task := tr.StartTask(context.Background(), "t", "p", "task-1")
	defer task.End()
	traceID, spanID := task.IDs()
	if traceID == "" || spanID == "" {
		t.Fatalf("trace ids missing: %q %q", traceID, spanID)
	}
}

func TestNilTracerIsSafe(t *testing.T) {
	var tr *Tracer
	span := tr.StartTask(context.Background(), "t", "p", "x")
	span.End()
	span = tr.StartStage(nil, "a", 1)
	span.End()
	if traceID, spanID := span.IDs(); traceID != "" || spanID != "" {
		t.Fatalf("nil span must not produce ids")
	}
}

func subtaskAttrs(t *testing.T, spans []tracetest.SpanStub, subtaskID string) map[string]string {
	t.Helper()
	for _, s := range spans {
		if s.Name != SpanSubtask {
			continue
		}
		out := map[string]string{}
		for _, kv := range s.Attributes {
			out[string(kv.Key)] = kv.Value.Emit()
		}
		if out["subtask_id"] != subtaskID {
			continue
		}
		out["__span_id"] = s.SpanContext.SpanID().String()
		out["__parent_id"] = s.Parent.SpanID().String()
		return out
	}
	t.Fatalf("subtask %s not found", subtaskID)
	return nil
}
