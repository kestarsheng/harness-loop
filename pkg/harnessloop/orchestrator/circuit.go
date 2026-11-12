package orchestrator

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strings"
	"sync"

	"agent-platform/pkg/harnessloop/subtask"
)

// Class-key circuit breaker (REQ-130, design 4.13.3). The key is
// sha1(role | sorted output paths | primary acceptance type) so failures of
// genuinely similar subtasks are pooled without mixing unrelated work.
type classStat struct {
	samples  int
	failures int
	open     bool
}

type CircuitBreaker struct {
	mu      sync.Mutex
	cfg     QuotaConfig
	emitter Emitter
	classes map[string]*classStat
}

func NewCircuitBreaker(cfg QuotaConfig, emitter Emitter) *CircuitBreaker {
	cfg.normalize()
	return &CircuitBreaker{cfg: cfg, emitter: emitter, classes: make(map[string]*classStat)}
}

// ClassKey derives the stable class key for a subtask.
func ClassKey(s *subtask.Subtask) string {
	paths := contractPaths(s.OutputContract)
	sort.Strings(paths)
	primary := ""
	if len(s.AcceptanceCriteria) > 0 {
		primary = s.AcceptanceCriteria[0]
	}
	sum := sha1.Sum([]byte(s.Role + "|" + strings.Join(paths, ",") + "|" + primary))
	return hex.EncodeToString(sum[:])
}

func contractPaths(contract map[string]any) []string {
	if contract == nil {
		return nil
	}
	raw, ok := contract["paths"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// IsOpen reports whether the class has been cut off.
func (c *CircuitBreaker) IsOpen(classKey string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.classes[classKey]
	return ok && st.open
}

// OpenClasses returns all open class keys, sorted for deterministic output.
func (c *CircuitBreaker) OpenClasses() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for key, st := range c.classes {
		if st.open {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Record samples one terminal outcome. It returns true only on the transition
// from closed to open so the caller can emit subtask.circuit_open exactly once.
func (c *CircuitBreaker) Record(ctx context.Context, classKey string, failed bool) (bool, error) {
	c.mu.Lock()
	st, ok := c.classes[classKey]
	if !ok {
		st = &classStat{}
		c.classes[classKey] = st
	}
	if st.open {
		c.mu.Unlock()
		return false, nil
	}
	st.samples++
	if failed {
		st.failures++
	}
	shouldOpen := st.samples >= c.cfg.CircuitMinSamples &&
		float64(st.failures)/float64(st.samples) >= c.cfg.CircuitFailureRatio
	var snapshot classStat
	if shouldOpen {
		st.open = true
		snapshot = *st
	}
	c.mu.Unlock()
	if !shouldOpen {
		return false, nil
	}
	if err := emit(ctx, c.emitter, Event{
		Type:     EventSubtaskCircuitOpen,
		Severity: SeverityWarn,
		Payload: map[string]any{
			"class_key":    classKey,
			"samples":      snapshot.samples,
			"failures":     snapshot.failures,
			"failure_rate": float64(snapshot.failures) / float64(snapshot.samples),
		},
	}); err != nil {
		return true, err
	}
	return true, nil
}

// Reset clears every class. It is called when the parent replans (new dag_id)
// or when an operator runs resume --reset-circuit.
func (c *CircuitBreaker) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.classes = make(map[string]*classStat)
}
