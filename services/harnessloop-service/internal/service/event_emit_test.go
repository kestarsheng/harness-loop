package service

// T-P2-32：emit 失败必须「有日志 + 有指标」。过去调用点写成 `_ = publish(...)`，
// 事件丢了连一行日志都没有。这份测试锁住两件事：
//  1. 发布失败会落到 hlp_event_publish_failed_total（按 event_type/reason 分）；
//  2. reportEmitFailure 会打出一条带 task_id/type/reason 的日志。

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/services/harnessloop-service/internal/metrics"
)

type failingPublisher struct{ err error }

func (f failingPublisher) Publish(ctx context.Context, e *event.Event) error { return f.err }

func TestPublishFailureReachesMetrics(t *testing.T) {
	h := metrics.NewHarness()
	pub := event.NewPublisher(failingSink{}, nil, event.NewMemoryBuffer())
	attachEventObserver(pub, h) // Runtime.Run 里做的那一次挂载

	e := &event.Event{
		EventID:       "ev-metric-1",
		EventType:     event.TaskCompleted,
		SchemaVersion: event.CurrentSchemaVersion,
		TaskID:        "task-metric",
		Timestamp:     time.Now().UTC(),
		Severity:      event.SeverityInfo,
		TenantID:      "tenant-a",
		ProjectID:     "proj-a",
		Payload:       []byte(`{"terminal_reason":"completed"}`),
	}
	if err := pub.Publish(context.Background(), e); err == nil {
		t.Fatal("publish must fail while the sink is down")
	}

	v, ok := h.Reg.Find(metrics.MetricEventPublishFailedTotal, map[string]string{
		"event_type": string(event.TaskCompleted), "reason": "sink_error",
	})
	if !ok || v != 1 {
		t.Fatalf("event publish failure counter = %v (found=%v), want 1", v, ok)
	}
	if got := pub.Stats().Buffered; got != 1 {
		t.Fatalf("failed event must be buffered, got %d", got)
	}
}

type failingSink struct{}

func (failingSink) Append(_ context.Context, _ []*event.Event) error {
	return errors.New("pg unavailable")
}

func TestReportEmitFailureLogsContext(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	err := reportEmitFailure("task-log", event.ToolDenied, event.ErrSeqConflict)
	if !errors.Is(err, event.ErrSeqConflict) {
		t.Fatalf("reportEmitFailure must pass the error through, got %v", err)
	}
	out := logged.String()
	for _, want := range []string{"emit failed", "task-log", string(event.ToolDenied), "seq_conflict"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log line missing %q: %q", want, out)
		}
	}
	if err := reportEmitFailure("task-log", event.ToolDenied, nil); err != nil {
		t.Fatalf("nil error must stay nil, got %v", err)
	}
}

// attachEventObserver 必须对 null 发布器 / nil 指标安全（大量测试不带指标面）。
func TestAttachEventObserverIsNilSafe(t *testing.T) {
	attachEventObserver(nil, metrics.NewHarness())
	attachEventObserver(event.NewPublisher(failingSink{}, nil, nil), nil)
	pub := event.NewPublisher(failingSink{}, nil, nil)
	attachEventObserver(pub, metrics.NewHarness())
	attachEventObserver(pub, metrics.NewHarness()) // 幂等
}
