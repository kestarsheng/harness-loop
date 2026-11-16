package orchestrator

// T-P2-16 · 租户池准入层流控（design 4.13.3 / 4.13.8，REQ-128 / REQ-138 / DFX-R-08）。
//
// 调度层的公平与双配额只回答"下一个 slot 给谁"，不回答"这个新父任务此刻该不该
// 接单"。租户池被 15 分钟级慢任务占满时，新父任务照样被接进来，然后它的第一个
// 子任务一直排队到最慢的那个结束——父任务的首字节延迟被别人的尾部钉死
// （bench/p2/fairness-report.md §3：衰减窗口 = 最长在跑子任务的时长）。
//
// 本层在 TenantFairness 之上加一道准入：租户池还有至少 1 个"有效" slot 才接单，
// 否则给出可预期的排队/拒绝（复用 E_SCHEDULER_LIMIT_EXCEEDED，reason 放在结构化
// 的 details.reason 里，不新增错误码——新增码会破坏前端/CLI 错误字典）。
//
// 两条边界：
//   - 已接单的父任务、已在跑的子任务一律不动（REQ-128 禁止抢占）；
//   - 占用超过 max_subtask_minutes 的 slot 视为"过了衰减窗口"：它们终将被调度层
//     超时收口，于是不再计入准入，窗口因此有上界。调度层原有的"超额在跑、靠自然
//     完成衰减"保持原样，作为兜底。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultMaxSubtaskMinutes is the default upper bound of the decay window: a
// subtask may hold a tenant slot at most this long before the admission layer
// stops counting it. It is deliberately the same 15 minutes as the default
// subtask timeout, but configured independently (T-P2-16 point 3).
const DefaultMaxSubtaskMinutes = 15

// Admission outcomes recorded on every admit/queue/release.
const (
	AdmissionAdmitted = "admitted"
	AdmissionQueued   = "queued"
	AdmissionRejected = "rejected"
	AdmissionReleased = "released"
)

// AdmissionConfig is the normalised admission policy. TenantSlots mirrors
// subtask.max_agents_per_tenant so the two never drift apart.
type AdmissionConfig struct {
	TenantSlots int
	// MaxSubtaskMinutes is the decay window upper bound (T-P2-16 point 3). It is
	// decoupled from the parent TimeoutMinutes: the timeout decides when a
	// subtask fails, this decides when its slot stops blocking new parents.
	MaxSubtaskMinutes int
	// QueueWhenFull: true queues the parent (predictable "retry next tick"),
	// false rejects it outright. Neither path fails a running subtask.
	QueueWhenFull bool
}

// AdmissionConfigFromQuota derives the admission policy from the scheduler
// limits so one policy file still drives every layer.
func AdmissionConfigFromQuota(cfg QuotaConfig) AdmissionConfig {
	cfg.normalize()
	return AdmissionConfig{
		TenantSlots:       cfg.MaxAgentsPerTenant,
		MaxSubtaskMinutes: cfg.MaxSubtaskMinutes,
		QueueWhenFull:     true,
	}
}

func (c AdmissionConfig) normalized() AdmissionConfig {
	def := AdmissionConfigFromQuota(DefaultQuotaConfig())
	if c.TenantSlots <= 0 {
		c.TenantSlots = def.TenantSlots
	}
	if c.MaxSubtaskMinutes <= 0 {
		c.MaxSubtaskMinutes = def.MaxSubtaskMinutes
	}
	return c
}

// window is the decay window derived from the configured minutes.
func (c AdmissionConfig) window() time.Duration {
	return time.Duration(c.MaxSubtaskMinutes) * time.Minute
}

// AdmissionRecord is the structured, observable record of one admission
// decision (T-P2-16 point 4). It is a log/audit record rather than a new event
// type: the platform event dictionary is shared with the dashboard and the CLI,
// so adding subtask.admission_* there would be a wider change than this task.
type AdmissionRecord struct {
	ParentTaskID string    `json:"parent_task_id"`
	Outcome      string    `json:"outcome"`
	Reason       string    `json:"reason,omitempty"`
	SlotsUsed    int       `json:"slots_used"`
	SlotsMax     int       `json:"slots_max"`
	SlotsDecayed int       `json:"slots_decayed"`
	QueueDepth   int       `json:"queue_depth"`
	Position     int       `json:"position,omitempty"`
	RetryAfterMs int64     `json:"retry_after_ms,omitempty"`
	At           time.Time `json:"at"`
}

// AdmissionSink persists admission records. The platform writes them next to
// the scheduler audit trail; tests use MemoryAdmissionSink. The default sink is
// in-memory so a rejection is never silent.
type AdmissionSink interface {
	Record(ctx context.Context, rec AdmissionRecord) error
}

// MemoryAdmissionSink keeps the last records in memory.
type MemoryAdmissionSink struct {
	mu      sync.Mutex
	records []AdmissionRecord
}

func NewMemoryAdmissionSink() *MemoryAdmissionSink { return &MemoryAdmissionSink{} }

func (m *MemoryAdmissionSink) Record(_ context.Context, rec AdmissionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, rec)
	return nil
}

// Records returns a copy of every recorded decision in arrival order.
func (m *MemoryAdmissionSink) Records() []AdmissionRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AdmissionRecord, len(m.records))
	copy(out, m.records)
	return out
}

// Find returns the records of one outcome in arrival order.
func (m *MemoryAdmissionSink) Find(outcome string) []AdmissionRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AdmissionRecord
	for _, rec := range m.records {
		if rec.Outcome == outcome {
			out = append(out, rec)
		}
	}
	return out
}

// AdmissionError is the rejection returned by Admit. The code is the shared
// E_SCHEDULER_LIMIT_EXCEEDED (no new code: the frontend/CLI error dictionary is
// unchanged) and every discriminator lives in Details, so callers branch on
// details["reason"] instead of parsing the message.
type AdmissionError struct {
	Reason  string
	Detail  string
	Details map[string]any
}

func (e *AdmissionError) Error() string {
	if e.Detail == "" {
		return CodeSchedulerLimitExceeded + ": " + e.Reason
	}
	return CodeSchedulerLimitExceeded + ": " + e.Detail
}

// Code makes AdmissionError satisfy ErrorCode's coder interface so the stable
// code reaches the existing error plumbing unchanged.
func (e *AdmissionError) Code() string { return CodeSchedulerLimitExceeded }

// AdmissionReason extracts details.reason from an admission error. It returns
// "" for any other error.
func AdmissionReason(err error) string {
	var admitErr *AdmissionError
	if errors.As(err, &admitErr) {
		return admitErr.Reason
	}
	return ""
}

// AdmissionDetails extracts the structured details of an admission error.
func AdmissionDetails(err error) map[string]any {
	var admitErr *AdmissionError
	if errors.As(err, &admitErr) {
		return admitErr.Details
	}
	return nil
}

// AdmissionDecision is the outcome of one Admit call. Admitted and Queued are
// never both true; Queued=false with an error means "rejected outright".
type AdmissionDecision struct {
	ParentTaskID string
	Admitted     bool
	Queued       bool
	Reason       string
	Position     int
	SlotsUsed    int
	SlotsMax     int
	SlotsDecayed int
	QueueDepth   int
	RetryAfter   time.Duration
	DecidedAt    time.Time
}

// AdmissionStats are monotonic counters for the admission layer (observability
// without a metrics dependency).
type AdmissionStats struct {
	Admitted        int
	Queued          int
	Rejected        int
	AdmittedByDecay int
}

// Admission gates new parent tasks on the tenant pool. It sits above
// TenantFairness: fairness decides "who gets the next slot among parents that
// are already in", admission decides "whether this parent joins at all".
type Admission struct {
	mu       sync.Mutex
	fair     *TenantFairness
	cfg      AdmissionConfig
	sink     AdmissionSink
	admitted map[string]struct{}
	order    []string
	queue    []string
	stats    AdmissionStats
	now      func() time.Time
}

// NewAdmission builds the tenant admission layer. A nil fairness allocator is
// replaced by a private one sized from cfg, so the layer is usable standalone.
func NewAdmission(fair *TenantFairness, cfg AdmissionConfig) *Admission {
	cfg = cfg.normalized()
	if fair == nil {
		fair = NewTenantFairness(FairnessConfig{TenantSlots: cfg.TenantSlots}, nil)
	}
	return &Admission{
		fair:     fair,
		cfg:      cfg,
		sink:     NewMemoryAdmissionSink(),
		admitted: make(map[string]struct{}),
		now:      time.Now,
	}
}

// WithSink routes admission records to sink. A nil sink keeps the in-memory one.
func (a *Admission) WithSink(sink AdmissionSink) *Admission {
	if sink == nil {
		return a
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sink = sink
	return a
}

// SetClock injects the clock for both the admission layer and the fairness
// allocator: the decay window is measured against fairness slot holds, so the
// two must never run on different clocks.
func (a *Admission) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	a.mu.Lock()
	a.now = now
	fair := a.fair
	a.mu.Unlock()
	fair.SetClock(now)
}

func (a *Admission) Config() AdmissionConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// Fairness exposes the shared allocator so callers attach their schedulers to
// the same tenant pool the admission layer is watching.
func (a *Admission) Fairness() *TenantFairness {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fair
}

// Admit accepts one parent task when the tenant pool still has an effective
// slot. Otherwise it returns a decision plus an admission error carrying
// details.reason=tenant_slots_full: queued (default) or rejected, never both.
// Calling it twice for the same parent is idempotent.
func (a *Admission) Admit(ctx context.Context, parentTaskID string) (AdmissionDecision, error) {
	if parentTaskID == "" {
		return AdmissionDecision{}, newError(CodeSchedulerLimitExceeded, "parent_task_id is required for admission")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	window := a.cfg.window()
	used, decayed, max := a.fair.Occupancy(window)
	decision := AdmissionDecision{
		ParentTaskID: parentTaskID,
		SlotsUsed:    used,
		SlotsMax:     max,
		SlotsDecayed: decayed,
		DecidedAt:    a.now().UTC(),
	}
	if _, ok := a.admitted[parentTaskID]; ok {
		decision.Admitted = true
		decision.QueueDepth = len(a.queue)
		return decision, nil
	}

	if used-decayed < max {
		a.admitted[parentTaskID] = struct{}{}
		a.order = append(a.order, parentTaskID)
		a.dropQueuedLocked(parentTaskID)
		a.stats.Admitted++
		if decayed > 0 {
			a.stats.AdmittedByDecay++
		}
		decision.Admitted = true
		decision.QueueDepth = len(a.queue)
		a.recordLocked(ctx, decision, AdmissionAdmitted, "")
		return decision, nil
	}

	decision.Queued = a.cfg.QueueWhenFull
	decision.Reason = AllocDeniedTenantFull
	decision.RetryAfter = a.fair.RetryAfter(window)
	decision.Position = len(a.queue) + 1
	if decision.Queued {
		a.enqueueLocked(parentTaskID)
		a.stats.Queued++
		decision.Position = len(a.queue)
	} else {
		a.stats.Rejected++
		decision.Position = 0
	}
	decision.QueueDepth = len(a.queue)
	outcome := AdmissionRejected
	if decision.Queued {
		outcome = AdmissionQueued
	}
	a.recordLocked(ctx, decision, outcome, decision.Reason)

	return decision, &AdmissionError{
		Reason: decision.Reason,
		Detail: fmt.Sprintf("tenant pool full for parent %s: %d/%d slots used (%d decayed, window %dm)",
			parentTaskID, used, max, decayed, a.cfg.MaxSubtaskMinutes),
		Details: map[string]any{
			"reason":              decision.Reason,
			"parent_task_id":      parentTaskID,
			"tenant_slots":        max,
			"slots_used":          used,
			"slots_decayed":       decayed,
			"queue_depth":         decision.QueueDepth,
			"queued":              decision.Queued,
			"position":            decision.Position,
			"retry_after_ms":      decision.RetryAfter.Milliseconds(),
			"max_subtask_minutes": a.cfg.MaxSubtaskMinutes,
		},
	}
}

// Drain admits queued parents in FIFO order while the pool still has an
// effective slot and returns them so the owner can attach their schedulers.
// This is the "slow task finished → new parent passes automatically" path:
// call it after a release instead of asking the caller to resubmit.
func (a *Admission) Drain(ctx context.Context) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.queue) == 0 {
		return nil
	}
	window := a.cfg.window()
	used, decayed, max := a.fair.Occupancy(window)
	// 一个父任务至少需要 1 个 slot 才能推进，所以放行额度 = 有效空位数量：
	// 空 1 个 slot 就只放 1 个，不做"池子空 1 个、排队 10 个一起进"的挤兑。
	capacity := max - (used - decayed)
	var out []string
	for len(a.queue) > 0 && capacity > 0 {
		parent := a.queue[0]
		a.queue = a.queue[1:]
		if _, ok := a.admitted[parent]; ok {
			continue
		}
		a.admitted[parent] = struct{}{}
		a.order = append(a.order, parent)
		a.stats.Admitted++
		if decayed > 0 {
			a.stats.AdmittedByDecay++
		}
		capacity--
		decision := AdmissionDecision{
			ParentTaskID: parent,
			Admitted:     true,
			SlotsUsed:    used,
			SlotsMax:     max,
			SlotsDecayed: decayed,
			QueueDepth:   len(a.queue),
			DecidedAt:    a.now().UTC(),
		}
		a.recordLocked(ctx, decision, AdmissionAdmitted, "")
		out = append(out, parent)
	}
	return out
}

// Release gives the parent's admission back (parent finished, cancelled or
// failed). It never touches running subtasks: their slots are released by the
// scheduler through TenantFairness.
func (a *Admission) Release(ctx context.Context, parentTaskID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.admitted[parentTaskID]; !ok {
		a.dropQueuedLocked(parentTaskID)
		return
	}
	delete(a.admitted, parentTaskID)
	for i, id := range a.order {
		if id == parentTaskID {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	a.dropQueuedLocked(parentTaskID)
	used, decayed, max := a.fair.Occupancy(a.cfg.window())
	a.recordLocked(ctx, AdmissionDecision{
		ParentTaskID: parentTaskID,
		SlotsUsed:    used,
		SlotsMax:     max,
		SlotsDecayed: decayed,
		QueueDepth:   len(a.queue),
		DecidedAt:    a.now().UTC(),
	}, AdmissionReleased, "")
}

// Admitted returns the admitted parent ids in admission order.
func (a *Admission) Admitted() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.order))
	copy(out, a.order)
	return out
}

// Queued returns the waiting parent ids in FIFO order.
func (a *Admission) Queued() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.queue))
	copy(out, a.queue)
	return out
}

// Stats snapshots the admission counters.
func (a *Admission) Stats() AdmissionStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

func (a *Admission) enqueueLocked(parentTaskID string) {
	for _, id := range a.queue {
		if id == parentTaskID {
			return
		}
	}
	a.queue = append(a.queue, parentTaskID)
}

func (a *Admission) dropQueuedLocked(parentTaskID string) {
	for i, id := range a.queue {
		if id == parentTaskID {
			a.queue = append(a.queue[:i], a.queue[i+1:]...)
			return
		}
	}
}

func (a *Admission) recordLocked(ctx context.Context, decision AdmissionDecision, outcome, reason string) {
	sink := a.sink
	if sink == nil {
		return
	}
	rec := AdmissionRecord{
		ParentTaskID: decision.ParentTaskID,
		Outcome:      outcome,
		Reason:       reason,
		SlotsUsed:    decision.SlotsUsed,
		SlotsMax:     decision.SlotsMax,
		SlotsDecayed: decision.SlotsDecayed,
		QueueDepth:   decision.QueueDepth,
		Position:     decision.Position,
		RetryAfterMs: decision.RetryAfter.Milliseconds(),
		At:           decision.DecidedAt,
	}
	// A record is observability, never a reason to fail an admission decision.
	_ = sink.Record(ctx, rec)
}
