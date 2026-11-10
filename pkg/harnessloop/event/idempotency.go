package event

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrDuplicateEvent = errors.New("duplicate event")

type Idempotency interface {
	Seen(ctx context.Context, eventID string) (bool, error)
	Mark(ctx context.Context, eventID string) error
}

type MemoryIdempotency struct {
	mu   sync.Mutex
	seen map[string]time.Time
	max  int
}

func NewMemoryIdempotency(max int) *MemoryIdempotency {
	if max <= 0 {
		max = 10000
	}
	return &MemoryIdempotency{seen: make(map[string]time.Time, max), max: max}
}

func (m *MemoryIdempotency) Seen(ctx context.Context, eventID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.seen[eventID]
	return ok, nil
}

func (m *MemoryIdempotency) Mark(ctx context.Context, eventID string) error {
	if eventID == "" {
		return errors.New("event id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.seen[eventID]; ok {
		return ErrDuplicateEvent
	}
	if len(m.seen) >= m.max {
		var oldestID string
		var oldest time.Time
		for id, ts := range m.seen {
			if oldestID == "" || ts.Before(oldest) {
				oldestID, oldest = id, ts
			}
		}
		delete(m.seen, oldestID)
	}
	m.seen[eventID] = time.Now()
	return nil
}

func (m *MemoryIdempotency) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.seen)
}
