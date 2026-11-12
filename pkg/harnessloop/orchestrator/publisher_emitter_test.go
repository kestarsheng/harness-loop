package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/modelregistry"
)

type recordingSink struct {
	mu     sync.Mutex
	events []*event.Event
}

func (s *recordingSink) Append(_ context.Context, events []*event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (s *recordingSink) snapshot() []*event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*event.Event, len(s.events))
	copy(out, s.events)
	return out
}

func newEmitterForTest() (*PublisherEmitter, *recordingSink) {
	sink := &recordingSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := NewPublisherEmitter(pub, EmitContext{TenantID: "tenant-a", ProjectID: "proj-a"})
	emitter.SetClock(func() time.Time { return time.Unix(1700000000, 0) })
	return emitter, sink
}

func TestPublisherEmitterMapsOrchestratorEvent(t *testing.T) {
	emitter, sink := newEmitterForTest()
	err := emitter.Emit(context.Background(), Event{
		Type:          EventSubtaskPlanCreated,
		Severity:      SeverityInfo,
		ParentTaskID:  "task-1",
		SubtaskID:     "st-1",
		DagID:         "dag-1",
		DagNodeID:     "n1",
		ParallelSetID: "p1",
		Payload:       map[string]any{"nodes": 2},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("events = %d", len(got))
	}
	ev := got[0]
	if ev.EventType != event.SubtaskPlanCreated {
		t.Fatalf("type = %s", ev.EventType)
	}
	if ev.TaskID != "task-1" || ev.TenantID != "tenant-a" || ev.ProjectID != "proj-a" {
		t.Fatalf("isolation fields = %+v", ev)
	}
	if ev.SubtaskID != "st-1" || ev.DagID != "dag-1" || ev.DagNodeID != "n1" {
		t.Fatalf("correlation fields = %+v", ev)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["nodes"].(float64) != 2 || payload["parallel_set_id"] != "p1" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestPublisherEmitterAllocatesSeqThroughPublisher(t *testing.T) {
	emitter, sink := newEmitterForTest()
	types := []string{EventSubtaskQueued, EventSubtaskStarted, EventAggregationCompleted}
	for _, typ := range types {
		if err := emitter.Emit(context.Background(), Event{Type: typ, ParentTaskID: "task-1", Payload: map[string]any{"k": typ}}); err != nil {
			t.Fatalf("emit %s: %v", typ, err)
		}
	}
	events := sink.snapshot()
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestPublisherEmitterModelEvent(t *testing.T) {
	emitter, sink := newEmitterForTest()
	err := emitter.EmitModelEvent(context.Background(), EmitContext{TaskID: "task-1", Stage: "c", Attempt: 2}, modelregistry.ModelEvent{
		Type:    modelregistry.EventModelSelected,
		Payload: map[string]any{"model_id": "qwen-max", "tenant_id": "tenant-a", "project_id": "proj-a"},
	})
	if err != nil {
		t.Fatalf("emit model: %v", err)
	}
	ev := sink.snapshot()[0]
	if ev.EventType != event.ModelSelected || ev.Stage != "c" || ev.Attempt != 2 {
		t.Fatalf("model event = %+v", ev)
	}
}

func TestPublisherEmitterRejectsUnknownType(t *testing.T) {
	emitter, _ := newEmitterForTest()
	err := emitter.EmitType(context.Background(), EmitContext{TaskID: "task-1"}, event.EventType("subtask.unknown"), event.SeverityInfo, nil)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown type must be rejected, got %v", err)
	}
}

func TestPublisherEmitterTenantIsolation(t *testing.T) {
	sink := &recordingSink{}
	pub := event.NewPublisher(sink, nil, nil)
	base := NewPublisherEmitter(pub, EmitContext{TenantID: "tenant-a", ProjectID: "proj-a"})
	taskA := base.For(EmitContext{TaskID: "task-a"})
	taskB := base.For(EmitContext{TenantID: "tenant-b", ProjectID: "proj-b", TaskID: "task-b"})
	_ = taskA.EmitType(context.Background(), EmitContext{}, event.SubtaskStarted, event.SeverityInfo, map[string]any{"x": 1})
	_ = taskB.EmitType(context.Background(), EmitContext{}, event.SubtaskStarted, event.SeverityInfo, map[string]any{"x": 2})
	events := sink.snapshot()
	if events[0].TenantID != "tenant-a" || events[0].TaskID != "task-a" {
		t.Fatalf("task-a event = %+v", events[0])
	}
	if events[1].TenantID != "tenant-b" || events[1].TaskID != "task-b" {
		t.Fatalf("task-b event = %+v", events[1])
	}
	if events[0].Seq != 1 || events[1].Seq != 1 {
		t.Fatalf("seq must be per task, got %d,%d", events[0].Seq, events[1].Seq)
	}
}