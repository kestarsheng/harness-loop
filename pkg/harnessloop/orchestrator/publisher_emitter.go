package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/modelregistry"
)

// EventPublisher is the subset of *event.Publisher used by the emitter. Keeping
// it as an interface lets the runtime and tests inject a recorder without
// pulling the persistence layer into this package.
type EventPublisher interface {
	Publish(ctx context.Context, e *event.Event) error
}

// EmitContext carries the tenant/task correlation fields that the platform
// event schema requires. An emitter built for a parent task fills these in so
// orchestration events stay isolated by tenant and parent task (REQ-137/139).
type EmitContext struct {
	TenantID      string
	ProjectID     string
	TaskID        string
	Stage         string
	Attempt       int
	ParentTaskID  string
	SubtaskID     string
	HandoffID     string
	DagID         string
	DagNodeID     string
	ParallelSetID string
}

// PublisherEmitter adapts orchestrator and modelregistry event shapes to
// pkg/harnessloop/event. Seq is deliberately left at zero: the shared
// event.Publisher allocates it per task, so every component writes into one
// monotonic sequence instead of inventing its own.
type PublisherEmitter struct {
	publisher EventPublisher
	base      EmitContext
	now       func() time.Time
}

var emitterSeq atomic.Uint64

func NewPublisherEmitter(publisher EventPublisher, base EmitContext) *PublisherEmitter {
	return &PublisherEmitter{publisher: publisher, base: base, now: time.Now}
}

// For returns a derived emitter bound to another parent task while inheriting
// the base tenant/project. It is safe to create one per task.
func (e *PublisherEmitter) For(ec EmitContext) *PublisherEmitter {
	if e == nil {
		return nil
	}
	merged := e.base
	if ec.TenantID != "" {
		merged.TenantID = ec.TenantID
	}
	if ec.ProjectID != "" {
		merged.ProjectID = ec.ProjectID
	}
	if ec.TaskID != "" {
		merged.TaskID = ec.TaskID
	}
	if ec.ParentTaskID != "" {
		merged.ParentTaskID = ec.ParentTaskID
	}
	out := &PublisherEmitter{publisher: e.publisher, base: merged, now: e.now}

	return out
}

func (e *PublisherEmitter) SetClock(now func() time.Time) {
	if now != nil {
		e.now = now
	}
}

// Emit implements the orchestrator.Emitter port.
func (e *PublisherEmitter) Emit(ctx context.Context, ev Event) error {
	ec := EmitContext{
		TaskID:        firstNonEmpty(ev.ParentTaskID, e.base.TaskID),
		ParentTaskID:  firstNonEmpty(ev.ParentTaskID, e.base.ParentTaskID),
		SubtaskID:     ev.SubtaskID,
		HandoffID:     ev.HandoffID,
		DagID:         ev.DagID,
		DagNodeID:     ev.DagNodeID,
		ParallelSetID: ev.ParallelSetID,
	}
	return e.EmitType(ctx, ec, event.EventType(ev.Type), parseSeverity(ev.Severity), ev.Payload)
}

// EmitModelEvent forwards a modelregistry event. It is used by the model bridge
// so selection/fallback/hot-reload events share the orchestration channel.
func (e *PublisherEmitter) EmitModelEvent(ctx context.Context, ec EmitContext, ev modelregistry.ModelEvent) error {
	severity := event.SeverityInfo
	switch ev.Type {
	case modelregistry.EventModelFallback, modelregistry.EventModelUnavailable, modelregistry.EventModelCapabilityRejected:
		severity = event.SeverityWarn
	case modelregistry.EventModelRecovered:
		severity = event.SeverityInfo
	}
	return e.EmitType(ctx, ec, event.EventType(ev.Type), severity, ev.Payload)
}

// EmitType is the generic adapter used for policy/approval events which are
// produced by the runtime layer rather than by orchestrator or modelregistry.
func (e *PublisherEmitter) EmitType(ctx context.Context, ec EmitContext, typ event.EventType, severity event.Severity, payload map[string]any) error {
	if e == nil || e.publisher == nil {
		return nil
	}
	if !event.IsKnownType(typ) {
		return fmt.Errorf("publisher emitter: event type %q is not registered", typ)
	}
	merged := e.base
	mergeContext(&merged, ec)
	if merged.TenantID == "" || merged.ProjectID == "" || merged.TaskID == "" {
		return fmt.Errorf("publisher emitter: tenant_id/project_id/task_id are required for %s", typ)
	}
	if severity == "" || !severity.Valid() {
		severity = event.SeverityInfo
	}
	body := make(map[string]any, len(payload)+5)
	for k, v := range payload {
		body[k] = v
	}
	putIfAbsent(body, "subtask_id", merged.SubtaskID)
	putIfAbsent(body, "handoff_id", merged.HandoffID)
	putIfAbsent(body, "dag_id", merged.DagID)
	putIfAbsent(body, "dag_node_id", merged.DagNodeID)
	putIfAbsent(body, "parallel_set_id", merged.ParallelSetID)

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("publisher emitter: marshal payload: %w", err)
	}
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	ts := now().UTC()
	ev := &event.Event{
		EventID:       fmt.Sprintf("%s-%s-%d-%d", merged.TaskID, typ, ts.UnixNano(), emitterSeq.Add(1)),
		EventType:     typ,
		SchemaVersion: event.CurrentSchemaVersion,
		TaskID:        merged.TaskID,
		Stage:         merged.Stage,
		Attempt:       merged.Attempt,
		Timestamp:     ts,
		Severity:      severity,
		TenantID:      merged.TenantID,
		ProjectID:     merged.ProjectID,
		Payload:       raw,
		ParentTaskID:  merged.ParentTaskID,
		SubtaskID:     merged.SubtaskID,
		HandoffID:     merged.HandoffID,
		DagID:         merged.DagID,
		DagNodeID:     merged.DagNodeID,
	}
	return e.publisher.Publish(ctx, ev)
}

// EmitOrchestratorEvent is a convenience wrapper for callers outside this
// package that build orchestrator.Event values directly.
func (e *PublisherEmitter) EmitOrchestratorEvent(ctx context.Context, ec EmitContext, ev Event) error {
	derived := e.For(ec)
	return derived.Emit(ctx, ev)
}

func mergeContext(dst *EmitContext, src EmitContext) {
	if src.TenantID != "" {
		dst.TenantID = src.TenantID
	}
	if src.ProjectID != "" {
		dst.ProjectID = src.ProjectID
	}
	if src.TaskID != "" {
		dst.TaskID = src.TaskID
	}
	if src.ParentTaskID != "" {
		dst.ParentTaskID = src.ParentTaskID
	}
	if src.Stage != "" {
		dst.Stage = src.Stage
	}
	if src.Attempt > 0 {
		dst.Attempt = src.Attempt
	}
	if src.SubtaskID != "" {
		dst.SubtaskID = src.SubtaskID
	}
	if src.HandoffID != "" {
		dst.HandoffID = src.HandoffID
	}
	if src.DagID != "" {
		dst.DagID = src.DagID
	}
	if src.DagNodeID != "" {
		dst.DagNodeID = src.DagNodeID
	}
	if src.ParallelSetID != "" {
		dst.ParallelSetID = src.ParallelSetID
	}
}

func putIfAbsent(m map[string]any, key, value string) {
	if value == "" {
		return
	}
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}

func parseSeverity(raw string) event.Severity {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(event.SeverityDebug):
		return event.SeverityDebug
	case string(event.SeverityWarn):
		return event.SeverityWarn
	case string(event.SeverityError):
		return event.SeverityError
	case string(event.SeverityInfo):
		return event.SeverityInfo
	}
	return event.SeverityInfo
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}