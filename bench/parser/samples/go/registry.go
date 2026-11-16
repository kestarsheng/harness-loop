package benchsample

import (
	"errors"
	"sync"
)

// ErrNotFound is returned when Lookup misses.
var ErrNotFound = errors.New("benchsample: not found")

// Store is a tiny goroutine-safe typed registry.
type Store[T any] struct {
	mu    sync.RWMutex
	items map[string]T
}

// NewStore builds an empty store.
func NewStore[T any]() *Store[T] {
	return &Store[T]{items: make(map[string]T)}
}

// Put inserts value under key.
func (s *Store[T]) Put(key string, value T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = value
}

// Lookup returns the value stored under key.
func (s *Store[T]) Lookup(key string) (T, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if value, ok := s.items[key]; ok {
		return value, nil
	}
	var zero T
	return zero, ErrNotFound
}

// MustLookup panics when the key is missing.
func (s *Store[T]) MustLookup(key string) T {
	value, err := s.Lookup(key)
	if err != nil {
		panic(err)
	}
	return value
}
