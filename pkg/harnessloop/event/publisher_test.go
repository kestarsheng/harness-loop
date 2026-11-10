package event

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu     sync.Mutex
	events []*Event
	fail   bool
	calls  int
}

func (s *fakeSink) Append(ctx context.Context, events []*Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail {
		return errors.New("sink unavailable")
	}
	s.events = append(s.events, events...)
	return nil
}

func (s *fakeSink) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

func (s *fakeSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func mkEvent(id, taskID string, seq int64) *Event {
	return &Event{
		EventID:       id,
		EventType:     StageTransition,
		SchemaVersion: CurrentSchemaVersion,
		TaskID:        taskID,
		Seq:           seq,
		Timestamp:     time.Now(),
		Severity:      SeverityInfo,
		TenantID:      "tenant-a",
		ProjectID:     "proj-a",
		Payload:       []byte(`{"from":"a","to":"b"}`),
	}
}

func TestPublisherAssignsMonotonicSeq(t *testing.T) {
	sink := &fakeSink{}
	p := NewPublisher(sink, NewMemoryIdempotency(100), nil)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		e := mkEvent("ev-"+string(rune('a'+i)), "task-1", 0)
		if err := p.Publish(ctx, e); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		if e.Seq != int64(i) {
			t.Fatalf("seq = %d, want %d", e.Seq, i)
		}
	}
	if sink.count() != 5 {
		t.Fatalf("sink count = %d", sink.count())
	}
}

func TestPublisherDeduplicates(t *testing.T) {
	sink := &fakeSink{}
	p := NewPublisher(sink, NewMemoryIdempotency(100), nil)
	ctx := context.Background()

	e := mkEvent("dup-1", "task-1", 1)
	for i := 0; i < 3; i++ {
		if err := p.Publish(ctx, e); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if sink.count() != 1 {
		t.Fatalf("duplicate must be written once, got %d", sink.count())
	}
}

func TestPublisherBuffersOnFailureAndFlush(t *testing.T) {
	sink := &fakeSink{fail: true}
	buf := NewMemoryBuffer()
	p := NewPublisher(sink, NewMemoryIdempotency(100), buf)
	ctx := context.Background()

	if err := p.Publish(ctx, mkEvent("ev-1", "task-1", 1)); err == nil {
		t.Fatal("publish must report sink failure")
	}
	if p.Pending() != 1 {
		t.Fatalf("pending = %d", p.Pending())
	}

	sink.setFail(false)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if p.Pending() != 0 {
		t.Fatalf("pending after flush = %d", p.Pending())
	}
	if sink.count() != 1 {
		t.Fatalf("sink count = %d", sink.count())
	}

	if err := p.Flush(ctx); err != nil {
		t.Fatalf("empty flush: %v", err)
	}
}

func TestPublisherFlushKeepsEventsOnFailure(t *testing.T) {
	sink := &fakeSink{fail: true}
	buf := NewMemoryBuffer()
	p := NewPublisher(sink, NewMemoryIdempotency(100), buf)
	ctx := context.Background()

	_ = p.Publish(ctx, mkEvent("ev-2", "task-1", 1))
	if err := p.Flush(ctx); err == nil {
		t.Fatal("flush must fail while sink is down")
	}
	if p.Pending() != 1 {
		t.Fatalf("pending must be restored, got %d", p.Pending())
	}
}

func TestPublisherRejectsInvalidEvent(t *testing.T) {
	sink := &fakeSink{}
	p := NewPublisher(sink, nil, nil)
	e := mkEvent("ev-3", "task-1", 1)
	e.Severity = Severity("fatal")
	if err := p.Publish(context.Background(), e); err == nil {
		t.Fatal("invalid event must be rejected")
	}
	if sink.count() != 0 {
		t.Fatal("invalid event must not reach the sink")
	}
}

func TestPublisherRestoreSeq(t *testing.T) {
	sink := &fakeSink{}
	p := NewPublisher(sink, NewMemoryIdempotency(100), nil)
	p.RestoreSeq("task-1", 10)
	e := mkEvent("ev-4", "task-1", 0)
	if err := p.Publish(context.Background(), e); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if e.Seq != 11 {
		t.Fatalf("seq = %d, want 11", e.Seq)
	}
}

func TestJSONLBufferRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buffer.jsonl")
	buf := NewJSONLBuffer(path)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		if err := buf.Add(mkEvent("buf-"+string(rune('a'+i)), "task-1", int64(i))); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if buf.Len() != 3 {
		t.Fatalf("len = %d", buf.Len())
	}
	out, err := buf.Drain()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(out) != 3 || out[0].EventID != "buf-b" {
		t.Fatalf("unexpected drain: %v", out)
	}
	if buf.Len() != 0 {
		t.Fatalf("buffer must be empty after drain, len=%d", buf.Len())
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("buffer file missing: %v", err)
	}

	sink := &fakeSink{}
	p := NewPublisher(sink, nil, NewJSONLBuffer(filepath.Join(t.TempDir(), "none.jsonl")))
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("flush empty: %v", err)
	}
}
