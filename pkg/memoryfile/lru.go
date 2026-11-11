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
	s.doc.MoveToFront(key)
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
		s.doc.UpsertTopic(merged, true)
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
			s.doc.UpsertTopic(topic, true)
			return nil
		}
		victim, ok := s.findVictim()
		if !ok {
			return fmt.Errorf("%w: %d topics all pinned", ErrMemoryFullPinned, len(s.doc.Topics))
		}
		s.doc.RemoveTopic(victim.Key)
		s.evicted = append(s.evicted, Eviction{Key: victim.Key, Reason: "lru"})
	}
	s.doc.UpsertTopic(topic, true)
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
	s.doc.MoveToFront(key)
	return true
}

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
