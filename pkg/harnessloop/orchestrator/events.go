package orchestrator

import (
	"context"
	"sync"
	"time"
)

// Event types emitted by the orchestration layer (design 4.13.10). The names
// are the platform event dictionary entries; the orchestrator owns the
// vocabulary and emits through the Emitter port so the runtime can adapt it to
// pkg/harnessloop/event without this package importing the event schema.
const (
	EventSubtaskPlanCreated    = "subtask.plan_created"
	EventSubtaskQueued         = "subtask.queued"
	EventSubtaskStarted        = "subtask.started"
	EventSubtaskHandoffWaiting = "subtask.handoff_waiting"
	EventSubtaskAccepted       = "subtask.accepted"
	EventSubtaskRejected       = "subtask.rejected"
	EventSubtaskFailed         = "subtask.failed"
	EventSubtaskCancelled      = "subtask.cancelled"
	EventSubtaskCircuitOpen    = "subtask.circuit_open"
	EventHandoffCreated        = "handoff.created"
	EventHandoffAccepted       = "handoff.accepted"
	EventHandoffRejected       = "handoff.rejected"
	EventAggregationCompleted  = "aggregation.completed"
	EventAggregationConflict   = "aggregation.conflict"
	EventSecurityViolation     = "security.violation"
)

// Severity values mirror the event schema so adapters can map them directly.
const (
	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityError = "error"
)

// Event is the orchestration-facing shape of a platform event. It carries the
// parent/child correlation fields required by REQ-139.
type Event struct {
	Type          string
	Severity      string
	ParentTaskID  string
	SubtaskID     string
	HandoffID     string
	DagID         string
	DagNodeID     string
	ParallelSetID string
	// OldDagID/NewDagID are set on replanning and supersede events so the
	// dashboard can draw "旧 DAG → 新 DAG" without a join (T-P2-10 point 5).
	OldDagID  string
	NewDagID  string
	Payload   map[string]any
	Timestamp time.Time
}

// SubtaskFailureDetail carries the diagnostic fields a subtask failure must
// expose. Without them subtask.failed only says "failed", which is useless for
// triage: the executor already knows the agent status, response size, model and
// duration, so the event carries them instead of forcing a log dig.
type SubtaskFailureDetail struct {
	Error       string
	ResponseLen int
	ModelID     string
	DurationMS  int64
	AgentStatus string
}

// Payload renders the detail in the platform payload shape. Empty strings are
// omitted, but counts are always present: response_len=0 and duration_ms=0 are
// meaningful facts (empty reply / instant failure), not missing data.
func (d *SubtaskFailureDetail) Payload() map[string]any {
	if d == nil {
		return nil
	}
	out := map[string]any{
		"response_len": d.ResponseLen,
		"duration_ms":  d.DurationMS,
	}
	if d.Error != "" {
		out["error"] = d.Error
	}
	if d.ModelID != "" {
		out["model_id"] = d.ModelID
	}
	if d.AgentStatus != "" {
		out["agent_status"] = d.AgentStatus
	}
	return out
}

// Emitter is the outbound port for orchestration events. Implementations may
// forward to pkg/harnessloop/event.Publisher or to a test recorder.
type Emitter interface {
	Emit(ctx context.Context, event Event) error
}

// MemoryEmitter records events in memory. It is used by tests and as a safe
// default when no platform emitter is wired yet.
type MemoryEmitter struct {
	mu     sync.Mutex
	events []Event
}

func NewMemoryEmitter() *MemoryEmitter { return &MemoryEmitter{} }

func (m *MemoryEmitter) Emit(_ context.Context, event Event) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if event.Severity == "" {
		event.Severity = SeverityInfo
	}
	if event.Payload != nil {
		payload := make(map[string]any, len(event.Payload))
		for k, v := range event.Payload {
			payload[k] = v
		}
		event.Payload = payload
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	return nil
}

// Events returns a copy of all recorded events in emission order.
func (m *MemoryEmitter) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out
}

// Find returns events of the given type in emission order.
func (m *MemoryEmitter) Find(eventType string) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

func (m *MemoryEmitter) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

func emit(ctx context.Context, emitter Emitter, event Event) error {
	if emitter == nil {
		return nil
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if event.Severity == "" {
		event.Severity = SeverityInfo
	}
	return emitter.Emit(ctx, event)
}
