package retry

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrRetriesExhausted         = errors.New("retries exhausted")
	ErrAcceptanceFailsExhausted = errors.New("acceptance failures exhausted")
)

type Counters struct {
	RetryCount          int
	AcceptanceFailCount int
}

type Decision struct {
	Allowed    bool
	Terminal   bool
	RetryIndex int
	MaxRetries int
	Backoff    time.Duration
	Reason     string
}

type Budget struct {
	mu       sync.Mutex
	policy   Policy
	counters Counters
}

func NewBudget(p Policy) *Budget { return &Budget{policy: p} }

func (b *Budget) Policy() Policy {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.policy
}

func (b *Budget) OnStageEnter() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters.RetryCount = 0
}

func (b *Budget) Counters() Counters {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counters
}

func (b *Budget) Restore(c Counters) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters = c
}

func (b *Budget) RecordError(c Category, jitter JitterFunc) Decision {
	b.mu.Lock()
	defer b.mu.Unlock()

	maxRetries := b.policy.MaxRetriesFor(c)
	if c == Quota || c == ContentFilter || maxRetries == 0 {
		return Decision{
			Allowed:    false,
			Terminal:   true,
			RetryIndex: b.counters.RetryCount,
			MaxRetries: maxRetries,
			Backoff:    0,
			Reason:     b.policy.ExclusionReason(c),
		}
	}
	if b.counters.RetryCount >= maxRetries {
		return Decision{
			Allowed:    false,
			Terminal:   true,
			RetryIndex: b.counters.RetryCount + 1,
			MaxRetries: maxRetries,
			Backoff:    0,
			Reason:     ErrRetriesExhausted.Error(),
		}
	}

	b.counters.RetryCount++
	idx := b.counters.RetryCount
	return Decision{
		Allowed:    true,
		RetryIndex: idx,
		MaxRetries: maxRetries,
		Backoff:    b.policy.BackoffFor(c, idx, jitter),
		Reason:     "retry scheduled",
	}
}

func (b *Budget) RecordAcceptanceFailure() Decision {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.counters.AcceptanceFailCount++
	if b.counters.AcceptanceFailCount >= b.policy.AcceptanceMaxFails {
		return Decision{
			Allowed:    false,
			Terminal:   true,
			RetryIndex: b.counters.AcceptanceFailCount,
			MaxRetries: b.policy.AcceptanceMaxFails,
			Reason:     ErrAcceptanceFailsExhausted.Error(),
		}
	}
	return Decision{
		Allowed:    true,
		RetryIndex: b.counters.AcceptanceFailCount,
		MaxRetries: b.policy.AcceptanceMaxFails,
		Reason:     "rollback scheduled",
	}
}

func (b *Budget) RemainingRetries(c Category) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	left := b.policy.MaxRetriesFor(c) - b.counters.RetryCount
	if left < 0 {
		return 0
	}
	return left
}
