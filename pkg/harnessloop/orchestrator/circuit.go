package orchestrator

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
)

// Class-key circuit breaker (REQ-130, design 4.13.3). The key is
// sha1(role | sorted output paths | primary acceptance type) so failures of
// genuinely similar subtasks are pooled without mixing unrelated work.
//
// T-P2-09 补三件事：
//  1. 参数可调（failure_ratio / min_samples / 滚动 sample_window）；
//  2. 恢复**只允许人工复位**：没有任何超时自动闭合的路径，避免"熔断自己好了"
//     把同类失败再放进来一遍；
//  3. 每次复位写审计（操作人 + 原因 + 来源 = operator | dag_replan），
//     没有操作人的复位请求直接拒绝。

// CircuitResetSource distinguishes an operator reset from the automatic reset
// that follows a successful replan (design 4.13.3: 仅在新 dag_id 或人工复位时恢复).
type CircuitResetSource string

const (
	CircuitResetSourceOperator CircuitResetSource = "operator"
	CircuitResetSourceReplan   CircuitResetSource = "dag_replan"
)

const (
	AuditActionCircuitReset = "subtask.circuit_reset"
)

// SchedulerAudit is one 调度层审计记录。字段对齐 hlp_event.audit（design 5.1.5），
// 由运行时适配器落库；本包只产出记录，不直接写表。
type SchedulerAudit struct {
	AuditID      string             `json:"audit_id"`
	At           time.Time          `json:"at"`
	TenantID     string             `json:"tenant_id"`
	ProjectID    string             `json:"project_id"`
	ParentTaskID string             `json:"parent_task_id"`
	DagID        string             `json:"dag_id,omitempty"`
	ClassKey     string             `json:"class_key,omitempty"`
	Actor        string             `json:"actor"`
	Action       string             `json:"action"`
	Source       CircuitResetSource `json:"source"`
	Reason       string             `json:"reason"`
	Result       string             `json:"result"`
	Detail       map[string]any     `json:"detail,omitempty"`
}

// SchedulerAuditSink persists scheduler audit records. The platform writes them
// into hlp_event.audit; tests use MemorySchedulerAudit.
type SchedulerAuditSink interface {
	Write(ctx context.Context, entry SchedulerAudit) error
}

// MemorySchedulerAudit is the in-memory sink used by tests and as a safe
// default before the PostgreSQL audit table is wired.
type MemorySchedulerAudit struct {
	mu      sync.Mutex
	entries []SchedulerAudit
	next    int64
}

func NewMemorySchedulerAudit() *MemorySchedulerAudit { return &MemorySchedulerAudit{} }

func (s *MemorySchedulerAudit) Write(_ context.Context, entry SchedulerAudit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	if entry.AuditID == "" {
		entry.AuditID = fmt.Sprintf("sched-audit-%d", s.next)
	}
	if entry.Detail != nil {
		detail := make(map[string]any, len(entry.Detail))
		for k, v := range entry.Detail {
			detail[k] = v
		}
		entry.Detail = detail
	}
	s.entries = append(s.entries, entry)
	return nil
}

func (s *MemorySchedulerAudit) Entries() []SchedulerAudit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SchedulerAudit, len(s.entries))
	copy(out, s.entries)
	return out
}

// FindByAction returns audit entries of one action in write order.
func (s *MemorySchedulerAudit) FindByAction(action string) []SchedulerAudit {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SchedulerAudit
	for _, entry := range s.entries {
		if entry.Action == action {
			out = append(out, entry)
		}
	}
	return out
}

// CircuitState is the read-only breaker state used by tests, the alloc view and
// the `resume --reset-circuit` CLI prompt.
type CircuitState struct {
	ClassKey    string
	Samples     int
	Failures    int
	Open        bool
	OpenedAt    time.Time
	ResetCount  int
	LastResetBy string
}

type classStat struct {
	samples  int
	failures int
	open     bool
	recent   []bool
	openedAt time.Time
	resets   int
	lastBy   string
}

// CircuitResetRequest is an audited, manual reset. Actor is mandatory for an
// operator reset: an unattributed reset erases the evidence of a failure class.
type CircuitResetRequest struct {
	ClassKey     string
	Actor        string
	Reason       string
	Source       CircuitResetSource
	TenantID     string
	ProjectID    string
	ParentTaskID string
	DagID        string
}

type CircuitBreaker struct {
	mu      sync.Mutex
	cfg     QuotaConfig
	emitter Emitter
	audit   SchedulerAuditSink
	classes map[string]*classStat
	now     func() time.Time
}

func NewCircuitBreaker(cfg QuotaConfig, emitter Emitter) *CircuitBreaker {
	cfg.normalize()
	return &CircuitBreaker{cfg: cfg, emitter: emitter, classes: make(map[string]*classStat), now: time.Now}
}

// WithAuditSink wires the audit sink. A nil sink keeps the breaker usable in
// tests, but production wiring must supply one: without it a reset is silent.
func (c *CircuitBreaker) WithAuditSink(sink SchedulerAuditSink) *CircuitBreaker {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.audit = sink
	return c
}

func (c *CircuitBreaker) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
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

// State returns the breaker state for one class (zero value when unknown).
func (c *CircuitBreaker) State(classKey string) CircuitState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.classes[classKey]
	if !ok {
		return CircuitState{ClassKey: classKey}
	}
	return CircuitState{
		ClassKey:    classKey,
		Samples:     st.samples,
		Failures:    st.failures,
		Open:        st.open,
		OpenedAt:    st.openedAt,
		ResetCount:  st.resets,
		LastResetBy: st.lastBy,
	}
}

// States returns every known class sorted by key (dashboard/CLI view).
func (c *CircuitBreaker) States() []CircuitState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CircuitState, 0, len(c.classes))
	for key, st := range c.classes {
		out = append(out, CircuitState{
			ClassKey:    key,
			Samples:     st.samples,
			Failures:    st.failures,
			Open:        st.open,
			OpenedAt:    st.openedAt,
			ResetCount:  st.resets,
			LastResetBy: st.lastBy,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClassKey < out[j].ClassKey })
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
	st.recent = append(st.recent, failed)
	if window := c.cfg.CircuitSampleWindow; window > 0 && len(st.recent) > window {
		st.recent = st.recent[len(st.recent)-window:]
	}
	st.failures = countTrue(st.recent)
	shouldOpen := len(st.recent) >= c.cfg.CircuitMinSamples &&
		float64(st.failures)/float64(len(st.recent)) >= c.cfg.CircuitFailureRatio
	var snapshot classStat
	if shouldOpen {
		st.open = true
		st.openedAt = c.now().UTC()
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
			"failure_rate": float64(snapshot.failures) / float64(len(snapshot.recent)),
			"window":       len(snapshot.recent),
			"manual_reset": true,
		},
	}); err != nil {
		return true, err
	}
	return true, nil
}

func countTrue(samples []bool) int {
	n := 0
	for _, v := range samples {
		if v {
			n++
		}
	}
	return n
}

// ResetClass performs the only supported recovery path for one class: an
// audited manual reset. An operator reset requires an actor; a replan reset is
// attributed to the orchestrator by the caller (ResetForReplan).
//
// It returns true when a previously open class was actually closed.
func (c *CircuitBreaker) ResetClass(ctx context.Context, req CircuitResetRequest) (bool, error) {
	if strings.TrimSpace(req.ClassKey) == "" {
		return false, newError(CodeCircuitResetDenied, "class_key is required for a circuit reset")
	}
	source := req.Source
	if source == "" {
		source = CircuitResetSourceOperator
	}
	if source == CircuitResetSourceOperator && strings.TrimSpace(req.Actor) == "" {
		return false, newError(CodeCircuitResetDenied,
			"a manual circuit reset requires an actor (resume --reset-circuit --actor)")
	}
	if strings.TrimSpace(req.Reason) == "" {
		return false, newError(CodeCircuitResetDenied, "a circuit reset requires a reason")
	}

	c.mu.Lock()
	st, ok := c.classes[req.ClassKey]
	wasOpen := ok && st.open
	var samples, failures int
	if ok {
		samples, failures = st.samples, st.failures
	}
	if wasOpen {
		st.open = false
		st.recent = nil
		st.samples = 0
		st.failures = 0
		st.resets++
		st.lastBy = req.Actor
	}
	audit := c.audit
	c.mu.Unlock()

	if !wasOpen {
		return false, nil
	}
	result := "reset"
	if audit != nil {
		entry := SchedulerAudit{
			At:           c.now().UTC(),
			TenantID:     req.TenantID,
			ProjectID:    req.ProjectID,
			ParentTaskID: req.ParentTaskID,
			DagID:        req.DagID,
			ClassKey:     req.ClassKey,
			Actor:        req.Actor,
			Action:       AuditActionCircuitReset,
			Source:       source,
			Reason:       req.Reason,
			Result:       result,
			Detail: map[string]any{
				"samples":  samples,
				"failures": failures,
			},
		}
		if err := audit.Write(ctx, entry); err != nil {
			return true, err
		}
	}
	return true, nil
}

// ResetForReplan resets every open class after a successful replan (new dag_id)
// and records one audit entry per class with source=dag_replan.
func (c *CircuitBreaker) ResetForReplan(ctx context.Context, req CircuitResetRequest) ([]string, error) {
	open := c.OpenClasses()
	for _, key := range open {
		scoped := req
		scoped.ClassKey = key
		scoped.Source = CircuitResetSourceReplan
		if scoped.Actor == "" {
			scoped.Actor = "orchestrator"
		}
		if _, err := c.ResetClass(ctx, scoped); err != nil {
			return nil, err
		}
	}
	return open, nil
}
