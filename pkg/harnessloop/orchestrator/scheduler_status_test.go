package orchestrator

import (
	"context"
	"errors"
	"testing"

	"agent-platform/pkg/harnessloop/subtask"
)

// statusRecordingSink captures every transition the scheduler reports, so a
// test can prove the DB view (pending→running→accepted/failed) matches events.
type statusRecordingSink struct {
	calls [][2]string // {from, to}
	err   error
}

func (s *statusRecordingSink) StatusChanged(_ context.Context, st *subtask.Subtask, from subtask.Status) error {
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, [2]string{string(from), string(st.Status)})
	return nil
}

func singleSubtask(id string) *subtask.Subtask {
	return testSubtask("task-status", "dag-status", id, 0, 1, "p1", "coder", "out/*.txt")
}

func TestSchedulerWritesBackEveryTransition(t *testing.T) {
	emitter := NewMemoryEmitter()
	s := NewScheduler(DefaultQuotaConfig(), emitter)
	sink := &statusRecordingSink{}
	s.WithStatusSink(sink)

	if err := s.Enqueue([]*subtask.Subtask{singleSubtask("st-w1")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := s.MarkAccepted(context.Background(), "st-w1", "acc-st-w1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	want := [][2]string{{"pending", "running"}, {"running", "accepted"}}
	if len(sink.calls) != len(want) {
		t.Fatalf("sink calls = %v, want %v", sink.calls, want)
	}
	for i := range want {
		if sink.calls[i] != want[i] {
			t.Fatalf("sink call %d = %v, want %v", i, sink.calls[i], want[i])
		}
	}
	if len(emitter.Find(EventSubtaskStarted)) != 1 || len(emitter.Find(EventSubtaskAccepted)) != 1 {
		t.Fatalf("events = %v", emitter.Find(EventSubtaskAccepted))
	}

	// Failure path: the DB must see running→failed too, not only success.
	s2 := NewScheduler(DefaultQuotaConfig(), NewMemoryEmitter())
	sink2 := &statusRecordingSink{}
	s2.WithStatusSink(sink2)
	if err := s2.Enqueue([]*subtask.Subtask{singleSubtask("st-w2")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s2.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := s2.MarkFailed(context.Background(), "st-w2", "subtask_failed"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if len(sink2.calls) != 2 || sink2.calls[1] != [2]string{"running", "failed"} {
		t.Fatalf("failure transitions = %v", sink2.calls)
	}
}

func TestSchedulerRevertsWhenStatusWriteFails(t *testing.T) {
	emitter := NewMemoryEmitter()
	s := NewScheduler(DefaultQuotaConfig(), emitter)
	sinkErr := errors.New("db down")
	s.WithStatusSink(&statusRecordingSink{err: sinkErr})

	if err := s.Enqueue([]*subtask.Subtask{singleSubtask("st-w3")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(context.Background()); !errors.Is(err, sinkErr) {
		t.Fatalf("dispatch error = %v, want the persistence error", err)
	}
	// Nothing was written and nothing was emitted: leaving the item pending lets
	// the next tick retry instead of running a subtask the DB calls pending.
	if len(emitter.Find(EventSubtaskStarted)) != 0 {
		t.Fatalf("no subtask.started may be emitted when the write-back fails")
	}
	item, ok := s.Item("st-w3")
	if !ok || item.Status != subtask.StatusPending {
		t.Fatalf("subtask status = %+v, want pending", item)
	}
	if got := len(s.Running()); got != 0 {
		t.Fatalf("running = %d, want 0 (quota must be released)", got)
	}
	// A second dispatch with a healthy sink must be able to start it.
	s.WithStatusSink(&statusRecordingSink{})
	if _, err := s.Dispatch(context.Background()); err != nil {
		t.Fatalf("retry dispatch: %v", err)
	}
	if len(emitter.Find(EventSubtaskStarted)) != 1 {
		t.Fatalf("a retried dispatch must emit subtask.started")
	}
}

func TestMarkFailedCarriesDiagnostics(t *testing.T) {
	emitter := NewMemoryEmitter()
	s := NewScheduler(DefaultQuotaConfig(), emitter)
	if err := s.Enqueue([]*subtask.Subtask{singleSubtask("st-w4")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	detail := &SubtaskFailureDetail{
		Error:       "agent stream error: boom",
		ResponseLen: 42,
		ModelID:     "qwen-plus",
		DurationMS:  1200,
		AgentStatus: "error",
	}
	if err := s.MarkFailedWithDetail(context.Background(), "st-w4", "subtask_failed", detail); err != nil {
		t.Fatalf("fail: %v", err)
	}
	failed := emitter.Find(EventSubtaskFailed)
	if len(failed) != 1 {
		t.Fatalf("subtask.failed events = %d", len(failed))
	}
	payload := failed[0].Payload
	if payload["error"] != detail.Error || payload["response_len"] != 42 ||
		payload["model_id"] != "qwen-plus" || payload["duration_ms"] != int64(1200) ||
		payload["agent_status"] != "error" {
		t.Fatalf("failure payload = %v", payload)
	}
	if payload["category"] != "subtask_failed" {
		t.Fatalf("category must stay for existing consumers: %v", payload)
	}
}
