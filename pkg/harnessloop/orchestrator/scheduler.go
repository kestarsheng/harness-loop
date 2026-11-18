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
	viaFair      bool
}

// StatusSink persists subtask status transitions. The scheduler owns the state
// machine, so it must be the component that tells persistence about a change;
// letting the caller write back a "final" status hides the intermediate
// pending→running step and makes DB and events disagree.
//
// from is the status the row is expected to hold right now. Implementations
// must reject a write whose current row state does not match it (no rewinds).
type StatusSink interface {
	StatusChanged(ctx context.Context, st *subtask.Subtask, from subtask.Status) error
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
	fair      *TenantFairness
	status    StatusSink
	weight    float64
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

// WithStatusSink attaches persistence for status transitions. A nil sink keeps
// the scheduler purely in-memory (tests, feature-flag off).
func (s *Scheduler) WithStatusSink(sink StatusSink) *Scheduler {
	if s == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = sink
	return s
}

// persistLocked writes one transition before the matching event is emitted. A
// failure leaves DB and events in the same state (nothing written, nothing
// emitted) and the caller restores the in-memory status, so the next dispatch
// can retry instead of running a subtask the database still calls pending.
func (s *Scheduler) persistLocked(ctx context.Context, item *scheduledItem, from subtask.Status) error {
	if s.status == nil || item == nil {
		return nil
	}
	return s.status.StatusChanged(ctx, item.subtask, from)
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

// AttachFairness makes this parent take its tenant slot through the shared WFQ
// allocator instead of its private gate. weight <= 0 keeps the configured
// default. Attaching twice re-registers the weight.
func (s *Scheduler) AttachFairness(fair *TenantFairness, weight float64) error {
	if fair == nil {
		return newError(CodeSchedulerLimitExceeded, "fairness allocator is nil")
	}
	s.mu.Lock()
	parent, tenant := s.parentID, s.tenantID
	s.mu.Unlock()
	if parent == "" {
		return newError(CodeSchedulerLimitExceeded, "enqueue the DAG before attaching fairness")
	}
	if err := fair.Register(parent, tenant, weight); err != nil {
		return err
	}
	s.mu.Lock()
	s.fair = fair
	s.weight = weight
	s.mu.Unlock()
	return nil
}

// Fairness returns the attached WFQ allocator (nil when the scheduler runs on
// its private gate).
func (s *Scheduler) Fairness() *TenantFairness {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fair
}

// ResetCircuit resets every open class after a replan and audits each reset.
// Operator-driven resets go through ResetCircuitClass with an explicit actor.
func (s *Scheduler) ResetCircuit(ctx context.Context, req CircuitResetRequest) ([]string, error) {
	s.mu.Lock()
	if req.ParentTaskID == "" {
		req.ParentTaskID = s.parentID
	}
	if req.TenantID == "" {
		req.TenantID = s.tenantID
	}
	if req.ProjectID == "" {
		req.ProjectID = s.projectID
	}
	if req.DagID == "" {
		req.DagID = s.dagID
	}
	circuit := s.circuit
	s.mu.Unlock()
	return circuit.ResetForReplan(ctx, req)
}

// ResetCircuitClass is the audited manual recovery path for one failure class
// (design 4.13.3: 恢复仅在父任务重新规划或人工 resume --reset-circuit 时发生).
func (s *Scheduler) ResetCircuitClass(ctx context.Context, req CircuitResetRequest) (bool, error) {
	s.mu.Lock()
	if req.ParentTaskID == "" {
		req.ParentTaskID = s.parentID
	}
	if req.TenantID == "" {
		req.TenantID = s.tenantID
	}
	if req.ProjectID == "" {
		req.ProjectID = s.projectID
	}
	if req.DagID == "" {
		req.DagID = s.dagID
	}
	circuit := s.circuit
	s.mu.Unlock()
	return circuit.ResetClass(ctx, req)
}

// WithAuditSink routes circuit resets through the given audit sink. Without a
// sink a reset still happens but leaves no record, so production wiring must
// always supply one (T-P2-09 point 3).
func (s *Scheduler) WithAuditSink(sink SchedulerAuditSink) *Scheduler {
	s.mu.Lock()
	circuit := s.circuit
	s.mu.Unlock()
	circuit.WithAuditSink(sink)
	return s
}

// CircuitStates snapshots every class the breaker has seen.
func (s *Scheduler) CircuitStates() []CircuitState {
	s.mu.Lock()
	circuit := s.circuit
	s.mu.Unlock()
	return circuit.States()
}

// NextRunnable returns the first eligible pending subtask in stable order
// without acquiring quota. The tenant dispatcher uses it to build WFQ
// candidates; calling it alone starts nothing.
func (s *Scheduler) NextRunnable() (*subtask.Subtask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		item := s.items[id]
		if s.eligibleLocked(item) {
			return item.subtask, true
		}
	}
	return nil, false
}

// NoteQueued emits subtask.queued for one blocked subtask. The tenant
// dispatcher calls it when the WFQ picker finds no admissible parent, so
// "everything is stuck" is still visible per parent instead of silently idle.
func (s *Scheduler) NoteQueued(ctx context.Context, subtaskID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[subtaskID]
	if !s.eligibleLocked(item) {
		return nil
	}
	return s.emitQueuedLocked(ctx, item, reason)
}

// DispatchOne starts exactly one named subtask when it is still eligible.
func (s *Scheduler) DispatchOne(ctx context.Context, subtaskID string) (*subtask.Subtask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[subtaskID]
	if !s.eligibleLocked(item) {
		return nil, nil
	}
	started, _, err := s.tryStartLocked(ctx, item)
	if err != nil {
		return nil, err
	}
	s.syncQueueDepthLocked()
	return started, nil
}

// Dispatch runs one scheduling tick and returns the subtasks that entered
// running. Work blocked by quota stays pending and emits subtask.queued with
// its current queue position (REQ-128: queue, never fail).
func (s *Scheduler) Dispatch(ctx context.Context) ([]*subtask.Subtask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	started := make([]*subtask.Subtask, 0, s.cfg.MaxParallelPerParent)
	for _, id := range s.order {
		item := s.items[id]
		if !s.eligibleLocked(item) {
			continue
		}
		next, _, err := s.tryStartLocked(ctx, item)
		if err != nil {
			return started, err
		}
		if next != nil {
			started = append(started, next)
		}
	}
	s.syncQueueDepthLocked()
	return started, nil
}

func (s *Scheduler) eligibleLocked(item *scheduledItem) bool {
	if item == nil || item.running || item.subtask.Status != subtask.StatusPending {
		return false
	}
	if !s.dependenciesAccepted(item.subtask) {
		return false
	}
	return !s.circuit.IsOpen(item.classKey)
}

// tryStartLocked admits one subtask. The double quota (1 slot + predicted
// tokens) is granted or refused together: "占着 slot 等 token" is forbidden.
func (s *Scheduler) tryStartLocked(ctx context.Context, item *scheduledItem) (*subtask.Subtask, bool, error) {
	tokens := s.estimateTokens(item.subtask)
	viaFair := false
	if fair := s.fair; fair != nil {
		ok, reason := fair.Acquire(item.subtask.ParentTaskID, tokens)
		if !ok {
			return nil, true, s.emitQueuedLocked(ctx, item, reason)
		}
		viaFair = true
	} else {
		if !s.quota.CanAcquire(tokens) || !s.gate.Available() {
			reason := AllocDeniedParentSlots
			if s.gate.Available() {
				reason = AllocDeniedParentTokens
			} else {
				reason = AllocDeniedTenantFull
			}
			return nil, true, s.emitQueuedLocked(ctx, item, reason)
		}
		if !s.quota.Acquire(tokens) {
			return nil, true, s.emitQueuedLocked(ctx, item, AllocDeniedParentTokens)
		}
		if !s.gate.TryAcquire() {
			s.quota.Release(tokens)
			return nil, true, s.emitQueuedLocked(ctx, item, AllocDeniedTenantFull)
		}
	}
	next, err := subtask.Transition(item.subtask.Status, subtask.StatusRunning, "", "")
	if err != nil {
		if viaFair {
			s.fair.Release(item.subtask.ParentTaskID, tokens)
		} else {
			s.quota.Release(tokens)
			s.gate.Release()
		}
		return nil, false, err
	}
	prev := item.subtask.Status
	item.subtask.Status = next
	item.running = true
	item.viaFair = viaFair
	item.startedAt = s.now()
	item.deadline = item.startedAt.Add(s.effectiveTimeout())
	item.tokens = tokens
	item.lastQueuePos = 0
	if err := s.persistLocked(ctx, item, prev); err != nil {
		// Nothing was written: put the row back to pending so a later dispatch
		// can start it again instead of running a subtask that is still pending
		// in the database.
		item.subtask.Status = prev
		s.release(item)
		return nil, false, err
	}
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
			"fair":   viaFair,
		},
	}); err != nil {
		return item.subtask, false, err
	}
	return item.subtask, false, nil
}

func (s *Scheduler) emitQueuedLocked(ctx context.Context, item *scheduledItem, reason string) error {
	queuePos := s.queuePositionLocked(item)
	if queuePos <= 0 || item.lastQueuePos == queuePos {
		return nil
	}
	item.lastQueuePos = queuePos
	return emit(ctx, s.emitter, Event{
		Type:          EventSubtaskQueued,
		ParentTaskID:  item.subtask.ParentTaskID,
		SubtaskID:     item.subtask.SubtaskID,
		DagID:         item.subtask.DagID,
		DagNodeID:     item.subtask.DagNodeID,
		ParallelSetID: item.subtask.ParallelSetID,
		Payload: map[string]any{
			"position": queuePos,
			"reason":   reason,
		},
	})
}

// queuePositionLocked is the 1-based position of the item inside this parent's
// pending queue. It is stable, so a repeated tick does not re-emit the event.
func (s *Scheduler) queuePositionLocked(item *scheduledItem) int {
	pos := 0
	for _, id := range s.order {
		other := s.items[id]
		if other == nil || other.running || other.subtask.Status != subtask.StatusPending {
			continue
		}
		pos++
		if other == item {
			return pos
		}
	}
	return 0
}

func (s *Scheduler) syncQueueDepthLocked() {
	if s.fair == nil || s.parentID == "" {
		return
	}
	depth := 0
	for _, id := range s.order {
		if item := s.items[id]; item != nil && !item.running && item.subtask.Status == subtask.StatusPending {
			depth++
		}
	}
	s.fair.SetQueueDepth(s.parentID, depth)
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
	prev := item.subtask.Status
	next, err := subtask.Transition(prev, subtask.StatusAccepted, acceptanceID, "")
	if err != nil {
		return err
	}
	prevAcceptanceID := item.subtask.AcceptanceID
	item.subtask.Status = next
	item.subtask.AcceptanceID = acceptanceID
	if err := s.persistLocked(ctx, item, prev); err != nil {
		item.subtask.Status = prev
		item.subtask.AcceptanceID = prevAcceptanceID
		return err
	}
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
	return s.MarkFailedWithDetail(ctx, subtaskID, category, nil)
}

// MarkFailedWithDetail records a subtask failure together with the executor's
// diagnostics. The detail is merged into the subtask.failed payload so the
// failure is triageable without reading container logs.
func (s *Scheduler) MarkFailedWithDetail(ctx context.Context, subtaskID, category string, detail *SubtaskFailureDetail) error {
	return s.finish(ctx, subtaskID, subtask.StatusFailed, category, EventSubtaskFailed, detail)
}

// MarkRejected records an exhausted self-verification.
func (s *Scheduler) MarkRejected(ctx context.Context, subtaskID, reason string) error {
	return s.finish(ctx, subtaskID, subtask.StatusRejected, reason, EventSubtaskRejected, nil)
}

func (s *Scheduler) finish(ctx context.Context, subtaskID string, to subtask.Status, reason, eventType string, detail *SubtaskFailureDetail) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.runningItem(subtaskID)
	if err != nil {
		return err
	}
	prev := item.subtask.Status
	next, err := subtask.Transition(prev, to, "", "")
	if err != nil {
		return err
	}
	item.subtask.Status = next
	if err := s.persistLocked(ctx, item, prev); err != nil {
		item.subtask.Status = prev
		return err
	}
	s.release(item)
	if _, err := s.circuit.Record(ctx, item.classKey, true); err != nil {
		return err
	}
	payload := map[string]any{}
	if reason != "" {
		payload["reason"] = reason
		payload["category"] = reason
	}
	for k, v := range detail.Payload() {
		payload[k] = v
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
	prev := item.subtask.Status
	next, err := subtask.Transition(prev, subtask.StatusCancelled, "", "")
	if err != nil {
		return err
	}
	item.subtask.Status = next
	if err := s.persistLocked(ctx, item, prev); err != nil {
		item.subtask.Status = prev
		return err
	}
	if item.running {
		s.release(item)
	}
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
		prev := item.subtask.Status
		next, err := subtask.Transition(prev, subtask.StatusFailed, "", "")
		if err != nil {
			return out, err
		}
		item.subtask.Status = next
		if err := s.persistLocked(ctx, item, prev); err != nil {
			item.subtask.Status = prev
			return out, err
		}
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
			Payload: map[string]any{
				"category":     "timeout",
				"error":        "subtask deadline exceeded",
				"agent_status": "timeout",
			},
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
	if item.viaFair && s.fair != nil {
		s.fair.Release(item.subtask.ParentTaskID, item.tokens)
		item.viaFair = false
		return
	}
	item.viaFair = false
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
	if s.fair != nil && s.parentID != "" {
		used, _ := s.fair.TenantActive()
		for _, row := range s.fair.AllocRows() {
			if row.ParentTaskID == s.parentID {
				return row.SlotsUsed, used
			}
		}
		return 0, used
	}
	return s.quota.UsedSlots(), s.gate.Active()
}

// PendingCount returns the number of still-pending subtasks of this parent.
func (s *Scheduler) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range s.order {
		if item := s.items[id]; item != nil && !item.running && item.subtask.Status == subtask.StatusPending {
			n++
		}
	}
	return n
}
