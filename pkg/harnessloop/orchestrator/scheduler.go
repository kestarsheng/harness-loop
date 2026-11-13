package orchestrator

import (
	"context"
	"sort"
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
)

// TimeoutRecord is returned by CheckTimeouts so the propagation layer can
// apply the parallel-set policy.
type TimeoutRecord struct {
	SubtaskID     string
	ParentTaskID  string
	ParallelSetID string
}

type scheduledItem struct {
	subtask      *subtask.Subtask
	classKey     string
	tokens       int64
	startedAt    time.Time
	deadline     time.Time
	lastQueuePos int
	running      bool
}

// Scheduler drives one parent task. It owns quota, dependency gating, queue
// events, timeout enforcement and the class circuit breaker. Execution of the
// subtask itself stays in the runtime.
type Scheduler struct {
	mu        sync.Mutex
	cfg       QuotaConfig
	emitter   Emitter
	gate      *TenantGate
	quota     *ParentQuota
	circuit   *CircuitBreaker
	order     []string
	items     map[string]*scheduledItem
	parentID  string
	tenantID  string
	projectID string
	dagID     string
	remaining time.Duration
	now       func() time.Time
}

func NewScheduler(cfg QuotaConfig, emitter Emitter) *Scheduler {
	return NewSchedulerWithGate(cfg, NewTenantGate(cfg.MaxAgentsPerTenant), emitter)
}

// NewSchedulerWithGate shares one TenantGate across every parent scheduler of
// the same tenant.
func NewSchedulerWithGate(cfg QuotaConfig, gate *TenantGate, emitter Emitter) *Scheduler {
	cfg.normalize()
	if gate == nil {
		gate = NewTenantGate(cfg.MaxAgentsPerTenant)
	}
	return &Scheduler{
		cfg:     cfg,
		emitter: emitter,
		gate:    gate,
		quota:   NewParentQuota(cfg),
		circuit: NewCircuitBreaker(cfg, emitter),
		items:   make(map[string]*scheduledItem),
		now:     time.Now,
	}
}

func (s *Scheduler) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// SetParentRemaining declares how much time budget the parent stage has left.
// A non-positive value means "use the 15 minute default".
func (s *Scheduler) SetParentRemaining(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remaining = d
}

// Enqueue admits the subtasks of one DAG. This is the scheduler-side second
// gate for REQ-127: limits are re-checked so internal callers cannot bypass the
// plan-time validation.
func (s *Scheduler) Enqueue(subtasks []*subtask.Subtask) error {
	if len(subtasks) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	parent, tenant, project, dagID := subtasks[0].ParentTaskID, subtasks[0].TenantID, subtasks[0].ProjectID, subtasks[0].DagID
	if s.parentID == "" {
		s.parentID, s.tenantID, s.projectID, s.dagID = parent, tenant, project, dagID
	} else if s.parentID != parent {
		return newError(CodeCrossTenantDenied, "scheduler already owns parent %s, cannot enqueue %s", s.parentID, parent)
	}

	direct := 0
	for _, st := range subtasks {
		if st == nil {
			return newError(CodeDAGIncomplete, "nil subtask")
		}
		if st.TenantID != tenant || st.ProjectID != project || st.ParentTaskID != parent {
			return newError(CodeCrossTenantDenied, "subtask %s crosses tenant/project/parent boundary", st.SubtaskID)
		}
		if st.Depth < 1 || st.Depth > s.cfg.MaxDepth {
			return newError(CodeSubtaskDepthExceeded, "subtask %s depth %d exceeds max_depth %d", st.SubtaskID, st.Depth, s.cfg.MaxDepth)
		}
		if st.Depth == 1 {
			direct++
		}
		if _, exists := s.items[st.SubtaskID]; exists {
			return newError(CodeDAGIncomplete, "duplicate subtask id %s", st.SubtaskID)
		}
	}
	if direct > s.cfg.MaxDirectChildren {
		return newError(CodeSubtaskLimitExceeded, "direct subtask count %d exceeds limit %d", direct, s.cfg.MaxDirectChildren)
	}

	known := make(map[string]struct{}, len(subtasks))
	for _, st := range subtasks {
		known[st.SubtaskID] = struct{}{}
	}
	for _, st := range subtasks {
		for _, dep := range st.DependsOn {
			if _, ok := known[dep]; !ok && s.items[dep] == nil {
				return newError(CodeDAGIncomplete, "subtask %s depends on unknown subtask %s", st.SubtaskID, dep)
			}
		}
	}

	sort.SliceStable(subtasks, func(i, j int) bool {
		if subtasks[i].ParallelSetID != subtasks[j].ParallelSetID {
			return subtasks[i].ParallelSetID < subtasks[j].ParallelSetID
		}
		return subtasks[i].IndexInParent < subtasks[j].IndexInParent
	})
	for _, st := range subtasks {
		if st.Status == "" {
			st.Status = subtask.StatusPending
		}
		s.items[st.SubtaskID] = &scheduledItem{subtask: st, classKey: ClassKey(st), lastQueuePos: -1}
		s.order = append(s.order, st.SubtaskID)
	}
	return nil
}

// ResetCircuit is called when the parent replans (new dag_id) or an operator
// explicitly resumes the class.
func (s *Scheduler) ResetCircuit() {
	s.circuit.Reset()
}

// Dispatch runs one scheduling tick and returns the subtasks that entered
// running. Work blocked by quota stays pending and emits subtask.queued with
// its current queue position (REQ-128: queue, never fail).
func (s *Scheduler) Dispatch(ctx context.Context) ([]*subtask.Subtask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	started := make([]*subtask.Subtask, 0, s.cfg.MaxParallelPerParent)
	queuePos := 0
	for _, id := range s.order {
		item := s.items[id]
		if item == nil || item.running || item.subtask.Status != subtask.StatusPending {
			continue
		}
		if !s.dependenciesAccepted(item.subtask) {
			continue
		}
		if s.circuit.IsOpen(item.classKey) {
			continue
		}
		tokens := s.estimateTokens(item.subtask)
		if !s.quota.CanAcquire(tokens) || !s.gate.Available() {
			queuePos++
			if item.lastQueuePos != queuePos {
				item.lastQueuePos = queuePos
				if err := emit(ctx, s.emitter, Event{
					Type:          EventSubtaskQueued,
					ParentTaskID:  item.subtask.ParentTaskID,
					SubtaskID:     item.subtask.SubtaskID,
					DagID:         item.subtask.DagID,
					DagNodeID:     item.subtask.DagNodeID,
					ParallelSetID: item.subtask.ParallelSetID,
					Payload:       map[string]any{"position": queuePos},
				}); err != nil {
					return started, err
				}
			}
			continue
		}
		if !s.quota.Acquire(tokens) {
			continue
		}
		if !s.gate.TryAcquire() {
			s.quota.Release(tokens)
			continue
		}
		next, err := subtask.Transition(item.subtask.Status, subtask.StatusRunning, "", "")
		if err != nil {
			s.quota.Release(tokens)
			s.gate.Release()
			return started, err
		}
		item.subtask.Status = next
		item.running = true
		item.startedAt = now
		item.deadline = now.Add(s.effectiveTimeout())
		item.tokens = tokens
		item.lastQueuePos = 0
		started = append(started, item.subtask)
		if err := emit(ctx, s.emitter, Event{
			Type:          EventSubtaskStarted,
			ParentTaskID:  item.subtask.ParentTaskID,
			SubtaskID:     item.subtask.SubtaskID,
			DagID:         item.subtask.DagID,
			DagNodeID:     item.subtask.DagNodeID,
			ParallelSetID: item.subtask.ParallelSetID,
			Payload: map[string]any{
				"role":   item.subtask.Role,
				"tokens": tokens,
			},
		}); err != nil {
			return started, err
		}
	}
	return started, nil
}

func (s *Scheduler) dependenciesAccepted(st *subtask.Subtask) bool {
	for _, dep := range st.DependsOn {
		item := s.items[dep]
		if item == nil || item.subtask.Status != subtask.StatusAccepted {
			return false
		}
	}
	return true
}

func (s *Scheduler) effectiveTimeout() time.Duration {
	timeout := time.Duration(s.cfg.TimeoutMinutes) * time.Minute
	if s.remaining > 0 && s.remaining < timeout {
		return s.remaining
	}
	return timeout
}

func (s *Scheduler) estimateTokens(st *subtask.Subtask) int64 {
	tokens := s.cfg.DefaultSubtaskTokens
	if st.OutputContract != nil {
		if raw, ok := st.OutputContract["estimated_tokens"]; ok {
			switch v := raw.(type) {
			case int:
				tokens = int64(v)
			case int64:
				tokens = v
			case float64:
				tokens = int64(v)
			}
		}
	}
	if tokens <= 0 {
		tokens = s.cfg.DefaultSubtaskTokens
	}
	if remaining := s.quota.RemainingTokens(); remaining > 0 {
		if half := remaining / 2; tokens > half {
			tokens = half
		}
	} else {
		tokens = 0
	}
	return tokens
}

// MarkAccepted records a subtask-level acceptance. An empty acceptance id is
// rejected by the subtask state machine (E_SUBTASK_ACCEPTANCE_REQUIRED).
func (s *Scheduler) MarkAccepted(ctx context.Context, subtaskID, acceptanceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.runningItem(subtaskID)
	if err != nil {
		return err
	}
	next, err := subtask.Transition(item.subtask.Status, subtask.StatusAccepted, acceptanceID, "")
	if err != nil {
		return err
	}
	item.subtask.Status = next
	item.subtask.AcceptanceID = acceptanceID
	s.release(item)
	if _, err := s.circuit.Record(ctx, item.classKey, false); err != nil {
		return err
	}
	return emit(ctx, s.emitter, Event{
		Type:          EventSubtaskAccepted,
		ParentTaskID:  item.subtask.ParentTaskID,
		SubtaskID:     item.subtask.SubtaskID,
		DagID:         item.subtask.DagID,
		DagNodeID:     item.subtask.DagNodeID,
		ParallelSetID: item.subtask.ParallelSetID,
		Payload:       map[string]any{"acceptance_id": acceptanceID},
	})
}

// MarkFailed records a subtask failure and samples the circuit breaker.
func (s *Scheduler) MarkFailed(ctx context.Context, subtaskID, category string) error {
	return s.finish(ctx, subtaskID, subtask.StatusFailed, category, EventSubtaskFailed)
}

// MarkRejected records an exhausted self-verification.
func (s *Scheduler) MarkRejected(ctx context.Context, subtaskID, reason string) error {
	return s.finish(ctx, subtaskID, subtask.StatusRejected, reason, EventSubtaskRejected)
}

func (s *Scheduler) finish(ctx context.Context, subtaskID string, to subtask.Status, reason, eventType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.runningItem(subtaskID)
	if err != nil {
		return err
	}
	next, err := subtask.Transition(item.subtask.Status, to, "", "")
	if err != nil {
		return err
	}
	item.subtask.Status = next
	s.release(item)
	if _, err := s.circuit.Record(ctx, item.classKey, true); err != nil {
		return err
	}
	payload := map[string]any{}
	if reason != "" {
		payload["reason"] = reason
		payload["category"] = reason
	}
	return emit(ctx, s.emitter, Event{
		Type:          eventType,
		Severity:      SeverityWarn,
		ParentTaskID:  item.subtask.ParentTaskID,
		SubtaskID:     item.subtask.SubtaskID,
		DagID:         item.subtask.DagID,
		DagNodeID:     item.subtask.DagNodeID,
		ParallelSetID: item.subtask.ParallelSetID,
		Payload:       payload,
	})
}

// MarkCancelled cancels a pending or running subtask (fail-fast / parent
// rollback). Already terminal subtasks are left untouched.
func (s *Scheduler) MarkCancelled(ctx context.Context, subtaskID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[subtaskID]
	if item == nil {
		return newError(CodeSubtaskNotFound, "unknown subtask %s", subtaskID)
	}
	if item.subtask.Status.Terminal() {
		return nil
	}
	next, err := subtask.Transition(item.subtask.Status, subtask.StatusCancelled, "", "")
	if err != nil {
		return err
	}
	if item.running {
		s.release(item)
	}
	item.subtask.Status = next
	return emit(ctx, s.emitter, Event{
		Type:          EventSubtaskCancelled,
		Severity:      SeverityWarn,
		ParentTaskID:  item.subtask.ParentTaskID,
		SubtaskID:     item.subtask.SubtaskID,
		DagID:         item.subtask.DagID,
		DagNodeID:     item.subtask.DagNodeID,
		ParallelSetID: item.subtask.ParallelSetID,
		Payload:       map[string]any{"reason": reason},
	})
}

// CheckTimeouts fails every running subtask whose deadline has passed
// (REQ-129) and returns them for propagation handling.
func (s *Scheduler) CheckTimeouts(ctx context.Context, now time.Time) ([]TimeoutRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TimeoutRecord
	for _, id := range s.order {
		item := s.items[id]
		if item == nil || !item.running || item.deadline.IsZero() {
			continue
		}
		if now.Before(item.deadline) {
			continue
		}
		next, err := subtask.Transition(item.subtask.Status, subtask.StatusFailed, "", "")
		if err != nil {
			return out, err
		}
		item.subtask.Status = next
		s.release(item)
		if _, err := s.circuit.Record(ctx, item.classKey, true); err != nil {
			return out, err
		}
		out = append(out, TimeoutRecord{
			SubtaskID:     item.subtask.SubtaskID,
			ParentTaskID:  item.subtask.ParentTaskID,
			ParallelSetID: item.subtask.ParallelSetID,
		})
		if err := emit(ctx, s.emitter, Event{
			Type:          EventSubtaskFailed,
			Severity:      SeverityWarn,
			ParentTaskID:  item.subtask.ParentTaskID,
			SubtaskID:     item.subtask.SubtaskID,
			DagID:         item.subtask.DagID,
			DagNodeID:     item.subtask.DagNodeID,
			ParallelSetID: item.subtask.ParallelSetID,
			Payload:       map[string]any{"category": "timeout"},
		}); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *Scheduler) runningItem(subtaskID string) (*scheduledItem, error) {
	item := s.items[subtaskID]
	if item == nil {
		return nil, newError(CodeSubtaskNotFound, "unknown subtask %s", subtaskID)
	}
	if !item.running {
		return nil, newError(CodeSubtaskNotRunning, "subtask %s is not running", subtaskID)
	}
	return item, nil
}

func (s *Scheduler) release(item *scheduledItem) {
	if !item.running {
		return
	}
	item.running = false
	s.quota.Release(item.tokens)
	s.gate.Release()
}

// Running returns running subtasks in stable order.
func (s *Scheduler) Running() []*subtask.Subtask {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*subtask.Subtask
	for _, id := range s.order {
		item := s.items[id]
		if item != nil && item.running {
			out = append(out, item.subtask)
		}
	}
	return out
}

// Pending returns non-terminal, non-running subtasks in stable order.
func (s *Scheduler) Pending() []*subtask.Subtask {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*subtask.Subtask
	for _, id := range s.order {
		item := s.items[id]
		if item != nil && !item.running && item.subtask.Status == subtask.StatusPending {
			out = append(out, item.subtask)
		}
	}
	return out
}

// Item returns the scheduler's view of one subtask.
func (s *Scheduler) Item(subtaskID string) (*subtask.Subtask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[subtaskID]
	if item == nil {
		return nil, false
	}
	return item.subtask, true
}

// ActiveSlots reports the parent and tenant slot usage (observability for
// scheduler.alloc, REQ-138).
func (s *Scheduler) ActiveSlots() (parent, tenant int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quota.UsedSlots(), s.gate.Active()
}
