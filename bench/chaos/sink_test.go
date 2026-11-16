package chaos

// 事件 sink 与 publisher 工具：让混沌用例可以观察 kill -9 之后的 seq 续号。

import (
	"context"
	"sync"

	"agent-platform/pkg/harnessloop/event"
)

type seqSink struct {
	mu     sync.Mutex
	events []*event.Event
}

func (s *seqSink) Append(_ context.Context, events []*event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (s *seqSink) seqFor(taskID string) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.events))
	for _, ev := range s.events {
		if ev.TaskID == taskID {
			out = append(out, ev.Seq)
		}
	}
	return out
}

// eventPublisher is a fresh publisher instance: a new process after kill -9
// only knows what it restored from the checkpoint watermark.
func eventPublisher(sink event.Sink) *event.Publisher {
	return event.NewPublisher(sink, event.NewMemoryIdempotency(1024), nil)
}
