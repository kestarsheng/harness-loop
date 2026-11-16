package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	SpanTask        = "task"
	SpanStage       = "stage"
	SpanAcceptance  = "acceptance"
	SpanTool        = "tool"
	SpanSubtask     = "subtask"
	SpanHandoff     = "handoff"
	SpanAggregation = "aggregation"
)

// Tracer builds the span hierarchy from design 10.3:
// task -> stage -> acceptance|tool, plus subtask -> tool and handoff (child of
// subtask). Spans never carry message bodies; only identifiers and counters.
type Tracer struct {
	tracer trace.Tracer
}

// NewTracerWithProvider builds a tracer from an explicit provider. Tests use it
// to capture spans with an in-memory exporter instead of touching globals.
func NewTracerWithProvider(provider trace.TracerProvider, name string) *Tracer {
	if name == "" {
		name = "harnessloop"
	}
	return &Tracer{tracer: provider.Tracer(name)}
}

// NewTracer uses the global OTel provider. When tracing is not configured the
// global provider is a no-op and spans stay cheap.
func NewTracer(name string) *Tracer {
	if name == "" {
		name = "harnessloop"
	}
	return &Tracer{tracer: otel.Tracer(name)}
}

// Span is a started span plus its identifiers.
type Span struct {
	ctx   context.Context
	span  trace.Span
	ended bool
}

// Context returns the span context used to start children.
func (s *Span) Context() context.Context {
	if s == nil {
		return context.Background()
	}
	return s.ctx
}

// IDs returns the trace and span ids for event/log correlation.
func (s *Span) IDs() (traceID, spanID string) {
	if s == nil {
		return "", ""
	}
	sc := s.span.SpanContext()
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

// End finishes the span. It is safe to call more than once.
func (s *Span) End() {
	if s == nil || s.ended {
		return
	}
	s.ended = true
	s.span.End()
}

func (s *Span) attr(kv ...attribute.KeyValue) {
	s.span.SetAttributes(kv...)
}

// SetAttr adds one attribute after the span started (e.g. the verdict, which is
// only known once acceptance completes).
func (s *Span) SetAttr(key, value string) {
	if s == nil || s.span == nil || value == "" {
		return
	}
	s.span.SetAttributes(attribute.String(key, value))
}

// StartTask opens the root span for one task run.
func (t *Tracer) StartTask(ctx context.Context, tenantID, projectID, taskID string) *Span {
	if t == nil {
		return nil
	}
	ctx, span := t.tracer.Start(ctx, SpanTask)
	s := &Span{ctx: ctx, span: span}
	s.attr(
		attribute.String("tenant_id", tenantID),
		attribute.String("project_id", projectID),
		attribute.String("task_id", taskID),
	)
	return s
}

// StartStage opens a stage span under the task span.
func (t *Tracer) StartStage(parent *Span, stage string, attempt int) *Span {
	if t == nil {
		return nil
	}
	ctx, span := t.tracer.Start(parent.Context(), SpanStage)
	s := &Span{ctx: ctx, span: span}
	s.attr(attribute.String("stage", stage), attribute.Int("attempt", attempt))
	return s
}

// StartAcceptance opens an acceptance span whose parent is the stage span. The
// verifier runs on a separate context so the acceptance span never shares the
// execution span's message history.
func (t *Tracer) StartAcceptance(stage *Span, stageName string, attempt int, verdict string) *Span {
	if t == nil {
		return nil
	}
	ctx := context.Background()
	if stage != nil {
		ctx = trace.ContextWithSpan(context.Background(), trace.SpanFromContext(stage.Context()))
	}
	ctx, span := t.tracer.Start(ctx, SpanAcceptance)
	s := &Span{ctx: ctx, span: span}
	s.attr(
		attribute.String("stage", stageName),
		attribute.Int("attempt", attempt),
		attribute.String("verdict", verdict),
	)
	return s
}

// StartTool opens a tool span. model_id/is_fallback and policy_decision/rule_id
// are recorded on the same span so a trace explains both the model choice and
// a denial.
func (t *Tracer) StartTool(parent *Span, tool string, attrs map[string]string) *Span {
	if t == nil {
		return nil
	}
	ctx, span := t.tracer.Start(parent.Context(), SpanTool)
	s := &Span{ctx: ctx, span: span}
	s.attr(attribute.String("tool", tool))
	for k, v := range attrs {
		if v != "" {
			s.attr(attribute.String(k, v))
		}
	}
	return s
}

// StartToolFromContext opens a tool span from an ambient context, so the tool
// span automatically nests under whichever span that context carries.
func (t *Tracer) StartToolFromContext(ctx context.Context, tool string, attrs map[string]string) *Span {
	if t == nil {
		return nil
	}
	ctx, span := t.tracer.Start(ctx, SpanTool)
	s := &Span{ctx: ctx, span: span}
	s.attr(attribute.String("tool", tool))
	for k, v := range attrs {
		if v != "" {
			s.attr(attribute.String(k, v))
		}
	}
	return s
}

// StartSubtask opens a subtask span. Its parent is the task span, not the stage
// span, so parallel subtasks never nest under each other.
func (t *Tracer) StartSubtask(task *Span, subtaskID, dagID string, depth int, role string) *Span {
	if t == nil {
		return nil
	}
	ctx, span := t.tracer.Start(task.Context(), SpanSubtask)
	s := &Span{ctx: ctx, span: span}
	s.attr(
		attribute.String("subtask_id", subtaskID),
		attribute.String("dag_id", dagID),
		attribute.Int("depth", depth),
		attribute.String("role", role),
	)
	return s
}

// StartSubtaskFromContext opens a subtask span from an ambient context. The
// parent_task_id attribute keeps the link explicit when the parent task span
// lives in a different goroutine (subtask execution never shares its message
// history with the parent, per design 10.3).
func (t *Tracer) StartSubtaskFromContext(ctx context.Context, subtaskID, dagID string, depth int, role, parentTaskID string) *Span {
	if t == nil {
		return nil
	}
	ctx, span := t.tracer.Start(ctx, SpanSubtask)
	s := &Span{ctx: ctx, span: span}
	s.attr(
		attribute.String("subtask_id", subtaskID),
		attribute.String("dag_id", dagID),
		attribute.Int("depth", depth),
		attribute.String("role", role),
		attribute.String("parent_task_id", parentTaskID),
	)
	return s
}

// StartHandoff opens a handoff span under the subtask span.
func (t *Tracer) StartHandoff(subtask *Span, handoffID, fromRole, toRole string) *Span {
	if t == nil {
		return nil
	}
	ctx, span := t.tracer.Start(subtask.Context(), SpanHandoff)
	s := &Span{ctx: ctx, span: span}
	s.attr(
		attribute.String("handoff_id", handoffID),
		attribute.String("from", fromRole),
		attribute.String("to", toRole),
	)
	return s
}

// StartHandoffFromContext opens a handoff span from an ambient context. It is
// used when the handoff record is persisted outside the subtask goroutine.
func (t *Tracer) StartHandoffFromContext(ctx context.Context, handoffID, fromRole, toRole string) *Span {
	if t == nil {
		return nil
	}
	ctx, span := t.tracer.Start(ctx, SpanHandoff)
	s := &Span{ctx: ctx, span: span}
	s.attr(
		attribute.String("handoff_id", handoffID),
		attribute.String("from", fromRole),
		attribute.String("to", toRole),
	)
	return s
}

// StartAggregation opens the aggregation span under the parent stage span.
func (t *Tracer) StartAggregation(stage *Span, items, conflicts int) *Span {
	if t == nil {
		return nil
	}
	ctx, span := t.tracer.Start(stage.Context(), SpanAggregation)
	s := &Span{ctx: ctx, span: span}
	s.attr(attribute.Int("items", items), attribute.Int("conflicts", conflicts))
	return s
}
