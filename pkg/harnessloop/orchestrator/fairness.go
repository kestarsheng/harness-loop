package orchestrator

// T-P2-09 · 公平调度（WFQ）与预算硬上限（design 4.13.8，REQ-138）。
//
// 本文件解决两个问题：
//
//  1. 同租户多父任务并发时，FIFO 会让"先提交的大任务"长期吃满 20 个 Agent
//     slot，其他父任务排队饿死。这里用加权公平队列（WFQ）按 virtual_time 轮转，
//     权重可配（subtask.fairness.weight，默认 1.0，租户策略最低 0.5）。
//  2. 仅靠"下一个给谁"还不够，必须给每个父任务一个硬上限：
//     parent_used_slots <= ceil(tenant_slots * parent_share)，
//     parent_used_tokens <= parent_token_share。硬上限优先于 WFQ。
//
// 双配额（1 slot + 预估 token 同时获取）仍由 ParentQuota 负责：
// WFQ 决定"下一个给谁"，ParentQuota 决定"能不能给"，TenantFairness 决定
// "该父任务是否已经吃超了租户份额"。配额不足时只跳过该候选，不阻塞其他父任务，
// 也绝不抢占已在运行的子任务（REQ-128 明确禁止抢占）。

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
)

// SchedulerAllocViewName is the queryable occupancy view required by T-P2-09
// point 4. The in-memory rows are returned by TenantFairness.AllocRows; the DDL
// for the persisted view (same name, same columns) is AllocViewSQL.
const SchedulerAllocViewName = "hlp_scheduler_alloc"

// FairCandidate is one schedulable subtask offered to the WFQ picker. Only
// eligibility is asserted by the caller (dependencies met, class circuit closed
// and the parent's own double quota available).
type FairCandidate struct {
	ParentTaskID  string
	SubtaskID     string
	ParallelSetID string
	IndexInParent int
	Tokens        int64
}

// FairnessConfig is the normalised WFQ configuration. TenantSlots mirrors
// subtask.max_agents_per_tenant so the two never drift apart.
type FairnessConfig struct {
	Weight       float64 `yaml:"weight"`
	ParentShare  float64 `yaml:"parent_share"`
	TenantSlots  int     `yaml:"tenant_slots"`
	TenantTokens int64   `yaml:"tenant_tokens"`
	MinWeight    float64 `yaml:"min_weight"`
	MaxWeight    float64 `yaml:"max_weight"`
}

// FairnessConfigFromQuota derives the WFQ configuration from the scheduler
// limits, so a single policy file drives both layers.
func FairnessConfigFromQuota(cfg QuotaConfig) FairnessConfig {
	cfg.normalize()
	return FairnessConfig{
		Weight:       cfg.Fairness.Weight,
		ParentShare:  cfg.Fairness.ParentShare,
		TenantSlots:  cfg.MaxAgentsPerTenant,
		TenantTokens: cfg.Fairness.TenantTokens,
		MinWeight:    cfg.Fairness.MinWeight,
		MaxWeight:    cfg.Fairness.MaxWeight,
	}
}

func (c FairnessConfig) normalized() FairnessConfig {
	def := FairnessConfigFromQuota(DefaultQuotaConfig())
	if c.Weight <= 0 {
		c.Weight = def.Weight
	}
	if c.TenantSlots <= 0 {
		c.TenantSlots = def.TenantSlots
	}
	if c.MinWeight <= 0 {
		c.MinWeight = def.MinWeight
	}
	if c.MaxWeight <= 0 || c.MaxWeight < c.MinWeight {
		c.MaxWeight = def.MaxWeight
	}
	if c.ParentShare < 0 || c.ParentShare > 1 {
		c.ParentShare = 0
	}
	if c.TenantTokens < 0 {
		c.TenantTokens = 0
	}
	if c.Weight < c.MinWeight {
		c.Weight = c.MinWeight
	}
	if c.Weight > c.MaxWeight {
		c.Weight = c.MaxWeight
	}
	return c
}

// SchedulerAllocRow is one row of hlp_scheduler_alloc: "谁在吃配额"必须可见
// （REQ-138 验收条件）。JSON tags follow the view column names.
type SchedulerAllocRow struct {
	TenantID     string    `json:"tenant_id"`
	ParentTaskID string    `json:"parent_task_id"`
	Weight       float64   `json:"weight"`
	Share        float64   `json:"share"`
	SlotsUsed    int       `json:"slots_used"`
	SlotsCap     int       `json:"slots_cap"`
	TokensUsed   int64     `json:"tokens_alloc"`
	TokensCap    int64     `json:"tokens_cap"`
	VirtualTime  float64   `json:"virtual_time"`
	QueueDepth   int       `json:"queue_depth"`
	AllocCount   int       `json:"alloc_count"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type fairParent struct {
	id        string
	tenantID  string
	weight    float64
	virtual   float64
	slots     int
	tokens    int64
	queue     int
	allocs    int
	updatedAt time.Time
	// holds records when each held slot was acquired (oldest first), so the
	// admission layer can age out slots that outlive the decay window (T-P2-16).
	holds []time.Time
}

// TenantFairness is the tenant-wide allocator: one instance per tenant, shared
// by every parent scheduler of that tenant (the WFQ counterpart of TenantGate).
type TenantFairness struct {
	mu      sync.Mutex
	cfg     FairnessConfig
	gate    *TenantGate
	parents map[string]*fairParent
	order   []string
	now     func() time.Time
}

func NewTenantFairness(cfg FairnessConfig, gate *TenantGate) *TenantFairness {
	cfg = cfg.normalized()
	if gate == nil {
		gate = NewTenantGate(cfg.TenantSlots)
	}
	return &TenantFairness{
		cfg:     cfg,
		gate:    gate,
		parents: make(map[string]*fairParent),
		now:     time.Now,
	}
}

func (f *TenantFairness) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

func (f *TenantFairness) Config() FairnessConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

// Register adds (or updates) a parent task. Parents of one fairness instance
// must share a tenant: a mismatch is a cross-tenant scheduling request and is
// rejected rather than silently merged (REQ-137).
func (f *TenantFairness) Register(parentTaskID, tenantID string, weight float64) error {
	if parentTaskID == "" {
		return newError(CodeSchedulerLimitExceeded, "parent_task_id is required for fairness registration")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.parents[parentTaskID]; ok {
		if tenantID != "" && existing.tenantID != "" && existing.tenantID != tenantID {
			return newError(CodeCrossTenantDenied, "parent %s belongs to tenant %s, not %s",
				parentTaskID, existing.tenantID, tenantID)
		}
		if weight > 0 {
			existing.weight = f.clampWeight(weight)
		}
		existing.updatedAt = f.now().UTC()
		return nil
	}
	if tenantID != "" {
		for _, other := range f.parents {
			if other.tenantID != "" && other.tenantID != tenantID {
				return newError(CodeCrossTenantDenied,
					"tenant %s cannot share a fairness queue with tenant %s", tenantID, other.tenantID)
			}
		}
	}
	if weight <= 0 {
		weight = f.cfg.Weight
	}
	f.parents[parentTaskID] = &fairParent{
		id:        parentTaskID,
		tenantID:  tenantID,
		weight:    f.clampWeight(weight),
		updatedAt: f.now().UTC(),
	}
	f.order = append(f.order, parentTaskID)
	return nil
}

func (f *TenantFairness) clampWeight(weight float64) float64 {
	if weight <= 0 {
		weight = f.cfg.Weight
	}
	if weight < f.cfg.MinWeight {
		weight = f.cfg.MinWeight
	}
	if weight > f.cfg.MaxWeight {
		weight = f.cfg.MaxWeight
	}
	return weight
}

// SetWeight adjusts a parent's WFQ weight (tenant policy may only move inside
// [MinWeight, MaxWeight]).
func (f *TenantFairness) SetWeight(parentTaskID string, weight float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent, ok := f.parents[parentTaskID]
	if !ok {
		return newError(CodeSubtaskNotFound, "unknown parent %s in fairness queue", parentTaskID)
	}
	parent.weight = f.clampWeight(weight)
	parent.updatedAt = f.now().UTC()
	return nil
}

// SetQueueDepth records how many subtasks of the parent are waiting. It is a
// view-only counter; scheduling decisions never read it.
func (f *TenantFairness) SetQueueDepth(parentTaskID string, depth int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent, ok := f.parents[parentTaskID]
	if !ok {
		return
	}
	if depth < 0 {
		depth = 0
	}
	parent.queue = depth
	parent.updatedAt = f.now().UTC()
}

// Share returns the parent's slot share. A configured parent_share wins;
// otherwise the dynamic 1/N rule from design 4.13.8 applies.
func (f *TenantFairness) Share(parentTaskID string) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shareLocked(parentTaskID)
}

func (f *TenantFairness) shareLocked(parentTaskID string) float64 {
	if f.cfg.ParentShare > 0 {
		return f.cfg.ParentShare
	}
	if len(f.parents) == 0 {
		return 1
	}
	if _, ok := f.parents[parentTaskID]; !ok {
		return 0
	}
	return 1 / float64(len(f.parents))
}

// SlotsCap is ceil(tenant_slots * share): the hard cap that beats WFQ.
func (f *TenantFairness) SlotsCap(parentTaskID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.slotsCapLocked(parentTaskID)
}

func (f *TenantFairness) slotsCapLocked(parentTaskID string) int {
	share := f.shareLocked(parentTaskID)
	if share <= 0 {
		return 0
	}
	cap := int(math.Ceil(float64(f.cfg.TenantSlots) * share))
	if cap < 1 {
		cap = 1
	}
	return cap
}

// TokensCap is the parent's share of the tenant token ceiling. Zero means "no
// tenant-level token cap"; the per-parent ParentQuota still applies.
func (f *TenantFairness) TokensCap(parentTaskID string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokensCapLocked(parentTaskID)
}

func (f *TenantFairness) tokensCapLocked(parentTaskID string) int64 {
	if f.cfg.TenantTokens <= 0 {
		return 0
	}
	share := f.shareLocked(parentTaskID)
	if share <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(f.cfg.TenantTokens) * share))
}

// allocDenied reasons are stable strings so the queue event and the report can
// distinguish "tenant pool full" from "this parent already hit its hard cap".
const (
	AllocDeniedTenantFull   = "tenant_slots_full"
	AllocDeniedParentSlots  = "parent_slot_cap"
	AllocDeniedParentTokens = "parent_token_cap"
	AllocDeniedUnknown      = "unknown_parent"
)

// CanAcquire reports whether the parent may take one slot plus tokens without
// breaching either the tenant pool or its own hard cap.
func (f *TenantFairness) CanAcquire(parentTaskID string, tokens int64) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canAcquireLocked(parentTaskID, tokens)
}

func (f *TenantFairness) canAcquireLocked(parentTaskID string, tokens int64) (bool, string) {
	parent, ok := f.parents[parentTaskID]
	if !ok {
		return false, AllocDeniedUnknown
	}
	if parent.slots+1 > f.slotsCapLocked(parentTaskID) {
		return false, AllocDeniedParentSlots
	}
	if cap := f.tokensCapLocked(parentTaskID); cap > 0 && tokens > 0 && parent.tokens+tokens > cap {
		return false, AllocDeniedParentTokens
	}
	if f.gate.Active() >= f.gate.Max() {
		return false, AllocDeniedTenantFull
	}
	return true, ""
}

// Acquire takes one tenant slot and the predicted tokens for the parent. Both
// are granted or neither: "占着 slot 等 token" is exactly the starvation this
// layer exists to prevent.
//
// Acquire also charges the WFQ accounting (virtual_time += 1/weight, alloc
// counter) so Pick never has to remember what it just handed out.
func (f *TenantFairness) Acquire(parentTaskID string, tokens int64) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok, reason := f.canAcquireLocked(parentTaskID, tokens)
	if !ok {
		return false, reason
	}
	if !f.gate.TryAcquire() {
		return false, AllocDeniedTenantFull
	}
	parent := f.parents[parentTaskID]
	parent.slots++
	parent.holds = append(parent.holds, f.now().UTC())
	if tokens > 0 {
		parent.tokens += tokens
	}
	parent.virtual += 1 / parent.weight
	parent.allocs++
	parent.updatedAt = f.now().UTC()
	return true, ""
}

func (f *TenantFairness) Release(parentTaskID string, tokens int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent, ok := f.parents[parentTaskID]
	if !ok {
		return
	}
	if parent.slots > 0 {
		parent.slots--
	}
	if len(parent.holds) > 0 {
		parent.holds = append(parent.holds[:0], parent.holds[1:]...)
	}
	if tokens > 0 {
		parent.tokens -= tokens
		if parent.tokens < 0 {
			parent.tokens = 0
		}
	}
	parent.updatedAt = f.now().UTC()
	f.gate.Release()
}

// Pick selects the next candidate by WFQ: minimum virtual_time across the
// parents that can actually take work, then stable (parallel_set, index) order
// inside the winner.
//
// Pick is a pure decision: the winner's virtual_time is charged by Acquire, so
// "decide" and "account" never diverge.
//
// A parent whose hard cap is hit is skipped, never allowed to block the queue
// (no head-of-line blocking).
func (f *TenantFairness) Pick(candidates []FairCandidate) (FairCandidate, bool) {
	if len(candidates) == 0 {
		return FairCandidate{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	type group struct {
		parent string
		weight float64
		vtime  float64
		items  []FairCandidate
	}
	groups := make(map[string]*group)
	var order []string
	for _, candidate := range candidates {
		parent, ok := f.parents[candidate.ParentTaskID]
		if !ok {
			continue
		}
		if ok, _ := f.canAcquireLocked(candidate.ParentTaskID, candidate.Tokens); !ok {
			continue
		}
		g, exists := groups[candidate.ParentTaskID]
		if !exists {
			g = &group{parent: candidate.ParentTaskID, weight: parent.weight, vtime: parent.virtual}
			groups[candidate.ParentTaskID] = g
			order = append(order, candidate.ParentTaskID)
		}
		g.items = append(g.items, candidate)
	}
	if len(order) == 0 {
		return FairCandidate{}, false
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := groups[order[i]], groups[order[j]]
		if a.vtime != b.vtime {
			return a.vtime < b.vtime
		}
		return a.parent < b.parent
	})
	winner := groups[order[0]]
	sort.SliceStable(winner.items, func(i, j int) bool {
		a, b := winner.items[i], winner.items[j]
		if a.ParallelSetID != b.ParallelSetID {
			return a.ParallelSetID < b.ParallelSetID
		}
		if a.IndexInParent != b.IndexInParent {
			return a.IndexInParent < b.IndexInParent
		}
		return a.SubtaskID < b.SubtaskID
	})
	parent := f.parents[winner.parent]
	parent.updatedAt = f.now().UTC()
	return winner.items[0], true
}

// AllocRows snapshots hlp_scheduler_alloc: one row per registered parent, in
// registration order so the dashboard bars never jump between ticks.
func (f *TenantFairness) AllocRows() []SchedulerAllocRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SchedulerAllocRow, 0, len(f.order))
	for _, id := range f.order {
		parent, ok := f.parents[id]
		if !ok {
			continue
		}
		out = append(out, SchedulerAllocRow{
			TenantID:     parent.tenantID,
			ParentTaskID: parent.id,
			Weight:       parent.weight,
			Share:        f.shareLocked(parent.id),
			SlotsUsed:    parent.slots,
			SlotsCap:     f.slotsCapLocked(parent.id),
			TokensUsed:   parent.tokens,
			TokensCap:    f.tokensCapLocked(parent.id),
			VirtualTime:  parent.virtual,
			QueueDepth:   parent.queue,
			AllocCount:   parent.allocs,
			UpdatedAt:    parent.updatedAt,
		})
	}
	return out
}

// TenantActive returns the tenant-wide slot usage (observability only).
func (f *TenantFairness) TenantActive() (used, max int) {
	return f.gate.Active(), f.gate.Max()
}

// Occupancy reports the tenant pool as the admission layer must see it (T-P2-16):
// used is the raw slot count, decayed is how many of those holds are already
// older than the decay window, and max is the pool ceiling. A slot past the
// window no longer counts against admission—the scheduler will fail it at its
// timeout and release it, so letting it pin new parents forever would be the
// unbounded window this task exists to remove. The slot itself is never
// preempted (REQ-128): decay only changes admission accounting.
func (f *TenantFairness) Occupancy(window time.Duration) (used, decayed, max int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	used = f.gate.Active()
	max = f.gate.Max()
	if window <= 0 {
		return used, 0, max
	}
	now := f.now()
	for _, parent := range f.parents {
		for _, hold := range parent.holds {
			if now.Sub(hold) >= window {
				decayed++
			}
		}
	}
	if decayed > used {
		decayed = used
	}
	return used, decayed, max
}

// RetryAfter is how long until the oldest still-counted slot crosses the decay
// window: the predictable "come back in N" value for a queued parent. Zero when
// nothing is holding the pool.
func (f *TenantFairness) RetryAfter(window time.Duration) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if window <= 0 {
		return 0
	}
	now := f.now()
	var oldest time.Duration
	found := false
	for _, parent := range f.parents {
		for _, hold := range parent.holds {
			age := now.Sub(hold)
			if age >= window {
				continue
			}
			if !found || age > oldest {
				oldest = age
				found = true
			}
		}
	}
	if !found {
		return 0
	}
	return window - oldest
}

// TenantDispatcher drives every parent scheduler of one tenant through a single
// WFQ pick loop. One Tick admits at most as many subtasks as the tenant pool and
// the per-parent hard caps allow, always choosing the parent with the smallest
// virtual_time first (design 4.13.8).
type TenantDispatcher struct {
	mu        sync.Mutex
	fair      *TenantFairness
	admission *Admission
	parents   map[string]*Scheduler
	order     []string
}

func NewTenantDispatcher(fair *TenantFairness) *TenantDispatcher {
	return &TenantDispatcher{fair: fair, parents: make(map[string]*Scheduler)}
}

// WithAdmission gates every parent attach on the tenant pool (T-P2-16): a parent
// is only registered when the pool still has an effective slot, so a new parent
// never joins just to queue behind a 15-minute slow task. Without it the
// dispatcher keeps its pre-T-P2-16 behaviour.
func (d *TenantDispatcher) WithAdmission(a *Admission) *TenantDispatcher {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.admission = a
	return d
}

// Attach registers a parent scheduler with the tenant WFQ queue.
func (d *TenantDispatcher) Attach(s *Scheduler, weight float64) error {
	if s == nil {
		return newError(CodeSchedulerLimitExceeded, "scheduler is nil")
	}
	d.mu.Lock()
	admission := d.admission
	d.mu.Unlock()
	if admission != nil {
		if _, err := admission.Admit(context.Background(), s.parentID); err != nil {
			return err
		}
	}
	if err := s.AttachFairness(d.fair, weight); err != nil {
		if admission != nil {
			admission.Release(context.Background(), s.parentID)
		}
		return err
	}
	parent := s.parentID
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.parents[parent]; !ok {
		d.order = append(d.order, parent)
	}
	d.parents[parent] = s
	return nil
}

// Parents returns the registered parent task ids in registration order.
func (d *TenantDispatcher) Parents() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.order))
	copy(out, d.order)
	return out
}

// Tick performs one WFQ round. It keeps admitting until the tenant pool is full
// or no parent has runnable work; nothing is ever preempted.
func (d *TenantDispatcher) Tick(ctx context.Context) ([]*subtask.Subtask, error) {
	d.mu.Lock()
	order := make([]string, len(d.order))
	copy(order, d.order)
	parents := make(map[string]*Scheduler, len(d.parents))
	for k, v := range d.parents {
		parents[k] = v
	}
	d.mu.Unlock()

	var started []*subtask.Subtask
	for {
		candidates := make([]FairCandidate, 0, len(order))
		for _, id := range order {
			s := parents[id]
			if s == nil {
				continue
			}
			st, ok := s.NextRunnable()
			if !ok {
				continue
			}
			candidates = append(candidates, FairCandidate{
				ParentTaskID:  st.ParentTaskID,
				SubtaskID:     st.SubtaskID,
				ParallelSetID: st.ParallelSetID,
				IndexInParent: st.IndexInParent,
				Tokens:        s.estimateTokens(st),
			})
		}
		pick, ok := d.fair.Pick(candidates)
		if !ok {
			// 没有任何父任务被准入：逐父标记 queued（带拒因），避免"整体卡住"
			// 在事件流上表现为什么都没发生。
			for _, candidate := range candidates {
				s := parents[candidate.ParentTaskID]
				if s == nil {
					continue
				}
				if admissible, _ := d.fair.CanAcquire(candidate.ParentTaskID, candidate.Tokens); admissible {
					continue
				}
				_, reason := d.fair.CanAcquire(candidate.ParentTaskID, candidate.Tokens)
				if err := s.NoteQueued(ctx, candidate.SubtaskID, reason); err != nil {
					return started, err
				}
			}
			break
		}
		s := parents[pick.ParentTaskID]
		if s == nil {
			break
		}
		next, err := s.DispatchOne(ctx, pick.SubtaskID)
		if err != nil {
			return started, err
		}
		if next == nil {
			// The parent lost the race for its own double quota; stop this round
			// rather than spinning (the next tick retries).
			break
		}
		started = append(started, next)
	}
	return started, nil
}

// AllocViewSQL returns the DDL of the persisted hlp_scheduler_alloc view. It is
// owned here (next to the in-memory rows it must agree with) but applied by the
// migration owner, because this package never writes schema.
func AllocViewSQL() string {
	return `CREATE OR REPLACE VIEW ` + SchedulerAllocViewName + ` AS
SELECT s.tenant_id,
       s.parent_task_id,
       count(*) FILTER (WHERE s.status = 'running')                            AS slots_used,
       coalesce(sum(s.alloc_tokens) FILTER (WHERE s.status = 'running'), 0)    AS tokens_alloc,
       count(*) FILTER (WHERE s.status = 'pending')                            AS queue_depth,
       max(s.updated_at)                                                       AS updated_at
FROM hlp_task.subtasks s
GROUP BY s.tenant_id, s.parent_task_id;`
}
