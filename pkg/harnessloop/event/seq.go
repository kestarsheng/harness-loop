package event

import (
	"errors"
	"sync"
)

var ErrSeqRegression = errors.New("seq regression: watermark is behind the current allocation")

type SeqAllocator struct {
	mu           sync.Mutex
	last         int64
	persisted    int64
	persistEvery int
}

func NewSeqAllocator(watermark int64) *SeqAllocator {
	if watermark < 0 {
		watermark = 0
	}
	return &SeqAllocator{last: watermark, persisted: watermark, persistEvery: 100}
}

func (a *SeqAllocator) Next() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.last++
	return a.last
}

func (a *SeqAllocator) Last() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

func (a *SeqAllocator) Restore(watermark int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if watermark < a.last {
		return ErrSeqRegression
	}
	a.last = watermark
	return nil
}

func (a *SeqAllocator) NeedsPersist() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last-a.persisted >= int64(a.persistEvery)
}

func (a *SeqAllocator) PersistedTo(seq int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq > a.persisted {
		a.persisted = seq
	}
}

func (a *SeqAllocator) Persisted() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.persisted
}

func (a *SeqAllocator) SetPersistEvery(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n > 0 {
		a.persistEvery = n
	}
}
