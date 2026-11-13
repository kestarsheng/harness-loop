package memoryfile

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrMemoryFullPinned = errors.New("memoryfile: all topics are pinned, cannot insert")
	ErrTooManyTopics    = errors.New("memoryfile: topic limit exceeded")
)

type Limits struct {
	MaxTopicsProject   int
	MaxTopicsGlobal    int
	MaxBodyChars       int
	MaxConstraintItems int
	MaxFactHistory     int
}

func DefaultLimits() Limits {
	return Limits{
		MaxTopicsProject:   30,
		MaxTopicsGlobal:    50,
		MaxBodyChars:       MaxBodyChars,
		MaxConstraintItems: 10,
		MaxFactHistory:     5,
	}
}

func (l Limits) ForScope(scope string) int {
	if scope == ScopeGlobal {
		return l.MaxTopicsGlobal
	}
	return l.MaxTopicsProject
}

type Eviction struct {
	Key    string
	Reason string
}

type Event struct {
	Type   string
	Key    string
	Detail string
}

type Store struct {
	mu       sync.Mutex
	doc      *Document
	limits   Limits
	evicted  []Eviction
	warnings []Event
	now      func() time.Time
	sink     EventSink
	path     string
}

func NewStore(doc *Document, limits Limits) *Store {
	if doc == nil {
		doc = &Document{SchemaVersion: SchemaVersion, Scope: ScopeProject}
	}
	if limits.MaxTopicsProject <= 0 {
		limits = DefaultLimits()
	}
	return &Store{doc: doc, limits: limits, now: func() time.Time { return time.Now().UTC() }}
}

// WithSink wires an EventSink into the store. Once wired, Get publishes
// memory.lru_promoted whenever it actually moves a topic to the front, and
// Insert publishes memory.evicted as soon as an LRU victim is removed. Callers
// no longer need to forward Writer.EmitPromotion / Store.DrainEvictions by hand.
//
// De-duplication contract:
//   - At most one promoted event is published per actual reorder. Reading a
//     topic that is already at the front publishes nothing.
//   - At most one evicted event is published per key in the same eviction
//     cycle. An eviction cycle ends when the victim leaves the store (Insert)
//     or when pending evictions are drained by Writer.Save; the same key is
//     never re-queued without being re-inserted and evicted again.
//   - An evicted event is NOT a replacement for the deleted topic change
//     produced by Writer.Save's diffTopics. They carry different meanings
//     (LRU removal vs. file diff); consumers must de-duplicate on
//     (topic_key, event_type) and must not drop deleted entries.
func (s *Store) WithSink(sink EventSink) *Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
	return s
}

func (s *Store) setPath(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
}

func (s *Store) emitLocked(e ChangeEvent) {
	if s.sink == nil {
		return
	}
	s.sink.Emit(e)
}

// promoteLocked applies the LRU promotion and publishes it through the single
// event exit. It reports whether key exists, so manual callers can fall back
// only for unknown keys instead of double-publishing an automatic promotion.
func (s *Store) promoteLocked(key string) bool {
	if _, ok := s.doc.TopicByKey(key); !ok {
		return false
	}
	// Long-term memory (global.md) is not LRU: reads never reorder it.
	if s.doc.Scope == ScopeGlobal {
		return true
	}
	if keyIndex(s.doc.Topics, key) <= 0 {
		return true
	}
	s.doc.MoveToFront(key)
	s.emitLocked(newPromotionEvent(s.path, s.doc.Scope, key, 0, s.now()))
	return true
}

func (s *Store) promote(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promoteLocked(key)
}

func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.now = now
	}
}

func (s *Store) Document() *Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doc
}

func (s *Store) Limits() Limits {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limits
}

func (s *Store) Get(key string) (Topic, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	topic, ok := s.doc.TopicByKey(key)
	if !ok {
		return Topic{}, false
	}
	s.promoteLocked(key)
	return topic, true
}

func (s *Store) Insert(topic Topic) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertLocked(topic)
}

func (s *Store) insertLocked(topic Topic) error {
	if !ValidTopicKey(topic.Key) {
		return fmt.Errorf("%w: %q", ErrInvalidTopicKey, topic.Key)
	}
	if !topic.Meta.Category.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidMeta, topic.Meta.Category)
	}
	if strings.TrimSpace(topic.Title) == "" {
		return fmt.Errorf("%w: title is required", ErrInvalidMeta)
	}
	if len([]rune(topic.Body)) > s.limits.MaxBodyChars {
		return fmt.Errorf("%w: topic %q has %d chars", ErrBodyTooLarge, topic.Key, len([]rune(topic.Body)))
	}

	existing, exists := s.doc.TopicByKey(topic.Key)
	if exists {
		merged := topic
		merged.Body = mergeBody(topic.Meta.Category, existing.Body, topic.Body, s.limits)
		if len([]rune(merged.Body)) > s.limits.MaxBodyChars {
			merged.Body = truncateRunes(merged.Body, s.limits.MaxBodyChars)
		}
		if merged.Body == existing.Body && merged.Title == existing.Title {
			merged.Meta.UpdatedAt = existing.Meta.UpdatedAt
			merged.Meta.ContentHash = existing.Meta.ContentHash
		} else {
			if merged.Meta.UpdatedAt.IsZero() {
				merged.Meta.UpdatedAt = s.now()
			}
			merged.Meta.ContentHash = ComputeContentHash(merged.Body)
		}
		s.doc.UpsertTopic(merged, s.doc.Scope != ScopeGlobal)
		return nil
	}

	if topic.Meta.UpdatedAt.IsZero() {
		topic.Meta.UpdatedAt = s.now()
	}
	if topic.Meta.ContentHash == "" {
		topic.Meta.ContentHash = ComputeContentHash(topic.Body)
	}

	max := s.limits.ForScope(s.doc.Scope)
	softLimit := s.doc.Scope == ScopeGlobal

	if len(s.doc.Topics) >= max {
		if softLimit {
			s.warnings = append(s.warnings, Event{Type: "memory.capacity_warning", Key: topic.Key, Detail: fmt.Sprintf("global memory has %d topics (soft limit %d)", len(s.doc.Topics), max)})
			s.doc.UpsertTopic(topic, false)
			return nil
		}
		victim, ok := s.findVictim()
		if !ok {
			return fmt.Errorf("%w: %d topics all pinned", ErrMemoryFullPinned, len(s.doc.Topics))
		}
		s.doc.RemoveTopic(victim.Key)
		ev := Eviction{Key: victim.Key, Reason: "lru"}
		if s.sink != nil {
			// Publish immediately and keep the key out of the pending queue so
			// Writer.Save and manual DrainEvictions cannot publish it again in
			// the same eviction cycle.
			s.emitLocked(newEvictionEvent(s.path, s.doc.Scope, ev, s.now()))
		} else {
			s.evicted = append(s.evicted, ev)
		}
	}
	s.doc.UpsertTopic(topic, s.doc.Scope != ScopeGlobal)
	return nil
}

func (s *Store) findVictim() (Topic, bool) {
	for i := len(s.doc.Topics) - 1; i >= 0; i-- {
		if !s.doc.Topics[i].Meta.Pinned {
			return s.doc.Topics[i], true
		}
	}
	return Topic{}, false
}

func (s *Store) Remove(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doc.RemoveTopic(key)
}

func (s *Store) Touch(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.doc.TopicByKey(key); !ok {
		return false
	}
	if s.doc.Scope != ScopeGlobal {
		s.doc.MoveToFront(key)
	}
	return true
}

// DrainEvictions returns and clears the evictions that have not been published
// yet. When a sink is wired with WithSink, Insert publishes evicted events
// immediately and does not enqueue them, so this returns an empty slice and the
// same key is never published twice in one eviction cycle.
func (s *Store) DrainEvictions() []Eviction {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.evicted
	s.evicted = nil
	return out
}

func (s *Store) DrainWarnings() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.warnings
	s.warnings = nil
	return out
}

func (s *Store) Render() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doc.UpdatedAt.IsZero() {
		s.doc.UpdatedAt = s.now()
	}
	s.doc.OrderVersion++
	return s.doc.Render()
}

func mergeBody(category Category, oldBody, newBody string, limits Limits) string {
	switch category {
	case CategoryLongTermConstraint:
		return mergeList(oldBody, newBody, limits.MaxConstraintItems)
	case CategoryProjectFact:
		return mergeFact(oldBody, newBody, limits.MaxFactHistory)
	default:
		return strings.TrimSpace(newBody)
	}
}

func mergeList(oldBody, newBody string, maxItems int) string {
	seen := map[string]bool{}
	var items []string
	for _, line := range append(splitItems(oldBody), splitItems(newBody)...) {
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		items = append(items, line)
	}
	if maxItems > 0 && len(items) > maxItems {
		items = items[len(items)-maxItems:]
	}
	formatted := make([]string, 0, len(items))
	for _, item := range items {
		formatted = append(formatted, "- "+item)
	}
	return strings.Join(formatted, "\n")
}

func splitItems(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func mergeFact(oldBody, newBody string, maxHistory int) string {
	newBody = strings.TrimSpace(newBody)
	oldBody = strings.TrimSpace(oldBody)
	if oldBody == "" || oldBody == newBody {
		return newBody
	}
	history := splitItems(oldBody)
	if maxHistory > 0 && len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
	}
	var buf strings.Builder
	buf.WriteString(newBody)
	if len(history) > 0 {
		buf.WriteString("\n\n> 历史要点：\n")
		for _, h := range history {
			if strings.Contains(newBody, h) {
				continue
			}
			buf.WriteString("> - ")
			buf.WriteString(h)
			buf.WriteString("\n")
		}
	}
	return strings.TrimSpace(buf.String())
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
