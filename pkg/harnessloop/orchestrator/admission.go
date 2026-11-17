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
//
// 二次确认（gate 同源）：衰减只是记账口径，不等于闸门真的有空位——慢任务可能
// 还在真实占着 slot。所以 Admit/Drain 在衰减快判定放行之后，必须再读一次与调度
// 层共用的同一个 TenantGate：真实可用量为 0 就照旧返回 E_SCHEDULER_LIMIT_EXCEEDED
// + reason=tenant_slots_full（+ 估得出时带 retry_after_ms；估不出就缺省，缺省
// ≠ 立刻可重试，见 AdmissionError），绝不"放行后换个地方排队"。

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
	// RetryAfterMs 是衰减窗口的估算值；0 表示「估算不出来」，不是「立刻可重试」
	// （与 AdmissionError.details["retry_after_ms"] 同一套契约，见 AdmissionError）。
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
//
// retry_after_ms 契约：
//   - 有值 = 对"最早那个仍占着 slot 的父任务何时滑出衰减窗口"的估算（毫秒）；
//   - 缺省 = **无法估算**，不是"立刻可以重试"。不能估算的原因至少有两种：准入层
//     看不到任何 hold（slot 是被调度层/gate 直接占住的），或衰减窗口配置为 0。
//     这时按 0 处理会让调用方立刻重试，把拒绝变成热循环。
//
// 所以调用方一律用指数退避（1s 起、上限 30s、带抖动）兜底，把 retry_after_ms 当
// "至少等这么久"的下界而不是精确时刻：
//
//	delay := exponentialBackoff(attempt)      // 缺省时的默认路径
//	if ms, ok := details["retry_after_ms"]; ok { delay = max(delay, ms) }
//
// 这条契约只约束本类型；E_SCHEDULER_LIMIT_EXCEEDED 的其它产生点不受影响。
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
	// DeniedByGate counts the decisions where the decayed accounting said "there
	// is room" but the shared tenant gate had no free slot left. A non-zero value
	// means the decay window is no longer a proxy for real capacity: the slow
	// tasks outlived it while still holding their slots.
	DeniedByGate int
}

// poolSnapshot is one reading of the tenant pool, taken while deciding. used /
// decayed / max come from the decayed accounting; gateAvailable is the gate's
// real headroom when it was re-read after the fast path, and negative means the
// gate was not consulted (the decayed accounting already said "full").
type poolSnapshot struct {
	used          int
	decayed       int
	max           int
	gateAvailable int
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
//
// 两段判定（T-P2-16 二次确认）：
//  1. 快路径看衰减记账（used-decayed < max）：过了窗口的 slot 不再挡新父任务；
//  2. 慢路径再读同一个 TenantGate 的真实可用量：衰减只是记账口径，慢任务可能
//     仍真实占着 gate。gate 没空位时必须拒，否则"准入放行"只是把父任务换个地方
//     排队，首字节延迟照样被别人的尾部钉死。
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
		// 第二确认：TenantActive 读的是 admission 与调度层共用的那个 gate 实例，
		// 所以"这里读到有空位"与"调度层能立刻拿到 slot"指的是同一件事。
		liveUsed, liveMax := a.fair.TenantActive()
		if liveMax-liveUsed > 0 {
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
		a.stats.DeniedByGate++
		return a.fullLocked(ctx, parentTaskID, decision, poolSnapshot{
			used:          liveUsed,
			decayed:       decayed,
			max:           liveMax,
			gateAvailable: 0,
		})
	}

	return a.fullLocked(ctx, parentTaskID, decision, poolSnapshot{
		used:          used,
		decayed:       decayed,
		max:           max,
		gateAvailable: -1,
	})
}

// fullLocked is the single "pool is full" exit of Admit: queue or reject, record
// the decision, and return the shared E_SCHEDULER_LIMIT_EXCEEDED error with
// details.reason=tenant_slots_full. Both the decayed fast path and the gate
// second confirmation go through here so the contract cannot drift.
func (a *Admission) fullLocked(ctx context.Context, parentTaskID string, decision AdmissionDecision, snap poolSnapshot) (AdmissionDecision, error) {
	decision.SlotsUsed = snap.used
	decision.SlotsMax = snap.max
	decision.SlotsDecayed = snap.decayed
	decision.Queued = a.cfg.QueueWhenFull
	decision.Reason = AllocDeniedTenantFull
	decision.RetryAfter = a.fair.RetryAfter(a.cfg.window())
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

	detail := fmt.Sprintf("tenant pool full for parent %s: %d/%d slots used (%d decayed, window %dm)",
		parentTaskID, snap.used, snap.max, snap.decayed, a.cfg.MaxSubtaskMinutes)
	if snap.gateAvailable >= 0 {
		detail = fmt.Sprintf("tenant pool full for parent %s: gate has no free slot (%d/%d used, %d decayed, window %dm)",
			parentTaskID, snap.used, snap.max, snap.decayed, a.cfg.MaxSubtaskMinutes)
	}
	details := map[string]any{
		"reason":              decision.Reason,
		"parent_task_id":      parentTaskID,
		"tenant_slots":        snap.max,
		"slots_used":          snap.used,
		"slots_decayed":       snap.decayed,
		"queue_depth":         decision.QueueDepth,
		"queued":              decision.Queued,
		"position":            decision.Position,
		"max_subtask_minutes": a.cfg.MaxSubtaskMinutes,
	}
	setRetryAfter(details, decision.RetryAfter)
	if snap.gateAvailable >= 0 {
		// 只在真的读过 gate 时才暴露这一项，否则"没查"会被误读成"没空位"。
		details["gate_available"] = snap.gateAvailable
	}
	return decision, &AdmissionError{
		Reason:  decision.Reason,
		Detail:  detail,
		Details: details,
	}
}

// setRetryAfter writes retry_after_ms only when a real estimate exists.
//
// RetryAfter<=0 means "估算不出来"（没有可观测的 hold，或衰减窗口为 0），此时
// 字段整个不出现。写 0 会被读成"立刻可重试"，调用方一顿重试就把拒绝变成了热循环。
func setRetryAfter(details map[string]any, d time.Duration) {
	if d <= 0 {
		return
	}
	details["retry_after_ms"] = d.Milliseconds()
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
	// 与 Admit 同一条二次确认：衰减记账放行的额度不能超过 gate 的真实空位，
	// 否则排队父任务会绕过闸门被"放行"进池子。
	if liveUsed, liveMax := a.fair.TenantActive(); liveMax-liveUsed < capacity {
		capacity = liveMax - liveUsed
	}
	if capacity < 0 {
		capacity = 0
	}
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
