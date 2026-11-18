package event

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu     sync.Mutex
	events []*Event
	fail   bool
	calls  int
	// allocate 为 true 时模拟事件库：给 Seq==0 的事件分配 per-task 递增的 seq
	// 并回填到原对象上（真实实现是 repository.EventRepo.Append 的 RETURNING）。
	allocate bool
	seqs     map[string]int64
	err      error
}

func (s *fakeSink) Append(ctx context.Context, events []*Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return s.err
	}
	if s.fail {
		return errors.New("sink unavailable")
	}
	if s.allocate {
		if s.seqs == nil {
			s.seqs = map[string]int64{}
		}
		for _, e := range events {
			if e.Seq <= 0 {
				s.seqs[e.TaskID]++
				e.Seq = s.seqs[e.TaskID]
			}
		}
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

// 分配权在存储端：发布器不得给事件编号，只读取回填值维护水位镜像。
func TestPublisherDelegatesSeqToStore(t *testing.T) {
	sink := &fakeSink{allocate: true}
	p := NewPublisher(sink, NewMemoryIdempotency(100), nil)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		e := mkEvent("ev-"+string(rune('a'+i)), "task-1", 0)
		if err := p.Publish(ctx, e); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		if e.Seq != int64(i) {
			t.Fatalf("seq = %d, want %d (store-assigned)", e.Seq, i)
		}
	}
	if sink.count() != 5 {
		t.Fatalf("sink count = %d", sink.count())
	}
	if got := p.SeqWatermark("task-1"); got != 5 {
		t.Fatalf("watermark = %d, want 5 (backfilled from store)", got)
	}
	if got := p.Stats().SeqBackfilled; got != 5 {
		t.Fatalf("backfilled stat = %d", got)
	}
}

// 存储端不回填 seq（例如内存 sink）时，发布器也绝不能自己 +1 —— 那正是双写者。
func TestPublisherNeverInventsSeq(t *testing.T) {
	sink := &fakeSink{}
	p := NewPublisher(sink, NewMemoryIdempotency(100), nil)
	e := mkEvent("ev-noseq", "task-1", 0)
	if err := p.Publish(context.Background(), e); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if e.Seq != 0 {
		t.Fatalf("publisher must not assign seq, got %d", e.Seq)
	}
	if p.SeqWatermark("task-1") != 0 {
		t.Fatal("watermark must stay 0 when the store did not assign")
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

func TestPublisherRestoreSeqOnlyMirrorsWatermark(t *testing.T) {
	sink := &fakeSink{}
	p := NewPublisher(sink, NewMemoryIdempotency(100), nil)
	p.RestoreSeq("task-1", 10)
	if got := p.SeqWatermark("task-1"); got != 10 {
		t.Fatalf("watermark = %d, want 10", got)
	}
	e := mkEvent("ev-4", "task-1", 0)
	if err := p.Publish(context.Background(), e); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Checkpoint 水位只用来判定漂移；seq 仍然由库分配。
	if e.Seq != 0 {
		t.Fatalf("seq = %d, want 0 (store assigns)", e.Seq)
	}
	if p.SeqWatermark("task-1") != 10 {
		t.Fatal("watermark must keep the restored value")
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

type recordingObserver struct {
	n      int
	typ    string
	reason string
}

func (o *recordingObserver) EventPublishFailed(eventType, reason string) {
	o.n++
	o.typ, o.reason = eventType, reason
}

// 失败必须可见：日志 + 计数 + 观测钩子，三者缺一不可。
func TestPublisherFailureIsVisible(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	sink := &fakeSink{err: fmt.Errorf("db down: %w", ErrSeqConflict)}
	obs := &recordingObserver{}
	buf := NewMemoryBuffer()
	p := NewPublisher(sink, nil, buf)
	p.SetObserver(obs)

	err := p.Publish(context.Background(), mkEvent("ev-fail", "task-1", 0))
	if !errors.Is(err, ErrSeqConflict) {
		t.Fatalf("err = %v, want ErrSeqConflict", err)
	}
	stats := p.Stats()
	if stats.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", stats.Failed)
	}
	if stats.Buffered != 1 || p.Pending() != 1 {
		t.Fatalf("failed event must be buffered, pending=%d buffered=%d", p.Pending(), stats.Buffered)
	}
	if obs.n != 1 || obs.reason != "seq_conflict" || obs.typ != string(StageTransition) {
		t.Fatalf("observer = %+v, want one publish failure with reason=seq_conflict", obs)
	}
	out := logged.String()
	if !strings.Contains(out, "publish failed") || !strings.Contains(out, "ev-fail") || !strings.Contains(out, "seq_conflict") {
		t.Fatalf("failure log missing context: %q", out)
	}
}

// 连缓冲都写不进去，才算真丢；这种必须单独计数并显式记日志。
func TestPublisherCountsLostEventsWhenBufferFails(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	p := NewPublisher(&fakeSink{fail: true}, nil, failingBuffer{})
	if err := p.Publish(context.Background(), mkEvent("ev-lost", "task-1", 0)); err == nil {
		t.Fatal("publish must report the failure")
	}
	stats := p.Stats()
	if stats.BufferedLost != 1 || stats.Buffered != 0 {
		t.Fatalf("stats = %+v, want one lost event", stats)
	}
	if !strings.Contains(logged.String(), "event lost") {
		t.Fatalf("lost event must be logged: %q", logged.String())
	}
}

type failingBuffer struct{}

func (failingBuffer) Add(e *Event) error       { return errors.New("disk full") }
func (failingBuffer) Drain() ([]*Event, error) { return nil, nil }
func (failingBuffer) Len() int                 { return 0 }

func TestFailureReason(t *testing.T) {
	cases := map[string]error{
		"seq_conflict": ErrSeqConflict,
		"canceled":     context.Canceled,
		"timeout":      context.DeadlineExceeded,
		"sink_error":   errors.New("boom"),
	}
	for want, err := range cases {
		if got := FailureReason(err); got != want {
			t.Fatalf("FailureReason(%v) = %s, want %s", err, got, want)
		}
	}
}
