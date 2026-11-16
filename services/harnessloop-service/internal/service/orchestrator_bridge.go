package service

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/subtask"
)

// SubtaskResult is the structured outcome reported by a subtask executor. The
// bridge writes the conclusion and artifacts back onto the scheduler's subtask
// before marking it accepted, so the d-stage aggregator has real data instead
// of an incomplete plan.
type SubtaskResult struct {
	Conclusion   string
	Artifacts    []subtask.ArtifactRef
	AcceptanceID string
}

// SubtaskExecutor runs one scheduled subtask. The production implementation is
// expected to reuse the stage runner/agent-service; the bridge itself never
// invents a second execution path.
type SubtaskExecutor interface {
	Execute(ctx context.Context, ec OrchestratorStageContext, st *subtask.Subtask) (*SubtaskResult, error)
}

type orchestrationState struct {
	scheduler *orchestrator.Scheduler
	plan      *orchestrator.PlanResult
	dagID     string
}

// tenantPool is the per-tenant orchestration pool. The three pieces must be one
// object graph: Admission watches the very TenantGate the schedulers take slots
// from, so "准入说还有空位"和"调度层真能拿到 slot"指的是同一件事——否则准入层
// 的二次确认读的是另一本账，等于自证清白（T-P2-16 生产化）。
type tenantPool struct {
	gate      *orchestrator.TenantGate
	fairness  *orchestrator.TenantFairness
	admission *orchestrator.Admission
}

// OrchestratorBridge wires orchestrator.Planner/Scheduler/Aggregator into the
// runtime. When the feature flag is off the runtime never calls it and keeps
// the existing single-agent stage flow.
type OrchestratorBridge struct {
	enabled  bool
	emitter  *orchestrator.PublisherEmitter
	executor SubtaskExecutor

	quota     orchestrator.QuotaConfig
	admission orchestrator.AdmissionConfig

	mu    sync.Mutex
	tasks map[string]*orchestrationState
	pools map[string]*tenantPool
}

func NewOrchestratorBridge(enabled bool, emitter *orchestrator.PublisherEmitter, executor SubtaskExecutor) *OrchestratorBridge {
	quota := orchestrator.DefaultQuotaConfig()
	admission := orchestrator.AdmissionConfigFromQuota(quota)
	// 生产桥不排队：父任务在 b 阶段就被拒（E_SCHEDULER_LIMIT_EXCEEDED +
	// retry_after_ms），比"接单后干等别人的 15 分钟慢任务"更可预期。
	admission.QueueWhenFull = false
	return &OrchestratorBridge{
		enabled:   enabled,
		emitter:   emitter,
		executor:  executor,
		quota:     quota,
		admission: admission,
		tasks:     make(map[string]*orchestrationState),
		pools:     make(map[string]*tenantPool),
	}
}

func (b *OrchestratorBridge) Enabled() bool { return b != nil && b.enabled }

// withQuota resizes the tenant pool before any parent joins it. Pools are built
// lazily on first use, so this only works while nobody has planned a DAG yet;
// it exists so the shared-gate behaviour can be exercised without 20 parents.
func (b *OrchestratorBridge) withQuota(quota orchestrator.QuotaConfig) *OrchestratorBridge {
	if b == nil {
		return b
	}
	admission := orchestrator.AdmissionConfigFromQuota(quota)
	admission.QueueWhenFull = false
	b.mu.Lock()
	defer b.mu.Unlock()
	b.quota = quota
	b.admission = admission
	b.pools = make(map[string]*tenantPool)
	return b
}

func (b *OrchestratorBridge) SetExecutor(executor SubtaskExecutor) {
	if b != nil {
		b.executor = executor
	}
}

// OrchestratorStageContext is the task/stage context used by the orchestration
// bridge; WorkspaceRoot keeps the plan-time sandbox check aligned with 4.15.3.
type OrchestratorStageContext struct {
	TenantID      string
	ProjectID     string
	TaskID        string
	Stage         string
	AttemptNo     int
	WorkspaceRoot string
}

// PlanBStage validates the DAG emitted by the b stage and enqueues its
// subtasks. A malformed plan is returned to the caller, which records it in the
// stage result instead of aborting the run.
//
// 共享闸门：scheduler 与 admission 用的是同一个 TenantGate 实例，"准入放行 →
// 调度层立刻能拿到 slot"这条链才算闭合。准入被拒（租户池真实空位为 0）时不建
// scheduler、不发 plan_created：拒绝必须发生在接单之前。
func (b *OrchestratorBridge) PlanBStage(ctx context.Context, ec OrchestratorStageContext, rawPlan []byte) (*orchestrator.PlanResult, error) {
	if !b.Enabled() {
		return nil, nil
	}
	rawPlan = extractJSON(rawPlan)
	if len(rawPlan) == 0 {
		return nil, fmt.Errorf("orchestrator bridge: b stage output has no DAG plan")
	}
	pool, quota := b.poolFor(ec.TenantID)
	if _, err := pool.admission.Admit(ctx, ec.TaskID); err != nil {
		return nil, err
	}
	taskEmitter := b.taskEmitter(ec)
	planner := orchestrator.NewPlanner(orchestrator.PlanConfig{MaxDirectChildren: 20, MaxDepth: 2, TolerateRatioMax: 0.5, WorkspaceRoot: ec.WorkspaceRoot}, taskEmitter)
	result, err := planner.Plan(ctx, orchestrator.PlanRequest{
		ParentTaskID: ec.TaskID,
		TenantID:     ec.TenantID,
		ProjectID:    ec.ProjectID,
		Stage:        ec.Stage,
		DagID:        "",
		RawPlan:      rawPlan,
	})
	if err != nil {
		pool.admission.Release(ctx, ec.TaskID)
		return nil, err
	}
	scheduler := orchestrator.NewSchedulerWithGate(quota, pool.gate, taskEmitter)
	if err := scheduler.Enqueue(result.Subtasks); err != nil {
		pool.admission.Release(ctx, ec.TaskID)
		return nil, err
	}
	// 调度层从同一个租户池取 slot（fairness 内部就是那把 gate），admission 看到
	// 的占用因此与调度层完全同源。
	if err := scheduler.AttachFairness(pool.fairness, 0); err != nil {
		pool.admission.Release(ctx, ec.TaskID)
		return nil, err
	}
	b.mu.Lock()
	b.tasks[ec.TaskID] = &orchestrationState{scheduler: scheduler, plan: result, dagID: result.Plan.DagID}
	b.mu.Unlock()
	return result, nil
}

// DispatchCStage starts every subtask the scheduler admits. With no executor
// wired it only advances the scheduler bookkeeping; the runtime still keeps
// running, which is why a missing executor is not treated as a task failure.
func (b *OrchestratorBridge) DispatchCStage(ctx context.Context, ec OrchestratorStageContext) ([]*subtask.Subtask, error) {
	if !b.Enabled() {
		return nil, nil
	}
	state := b.stateFor(ec.TaskID)
	if state == nil || state.scheduler == nil {
		return nil, fmt.Errorf("orchestrator bridge: no DAG was planned for task %s", ec.TaskID)
	}
	running, err := state.scheduler.Dispatch(ctx)
	if err != nil {
		return nil, err
	}
	if b.executor == nil {
		return running, nil
	}
	for _, st := range running {
		res, execErr := b.executor.Execute(ctx, ec, st)
		if execErr != nil {
			_ = state.scheduler.MarkFailed(ctx, st.SubtaskID, "subtask_failed")
			continue
		}
		acceptanceID := fmt.Sprintf("acc-%s", st.SubtaskID)
		if res != nil {
			applySubtaskResult(st, res)
			if res.AcceptanceID != "" {
				acceptanceID = res.AcceptanceID
			}
		}
		_ = state.scheduler.MarkAccepted(ctx, st.SubtaskID, acceptanceID)
	}
	return running, nil
}

// applySubtaskResult writes the executor outcome back onto the subtask the
// scheduler holds. The aggregator reads these fields in the d stage.
func applySubtaskResult(st *subtask.Subtask, res *SubtaskResult) {
	if st == nil || res == nil {
		return
	}
	if len(res.Artifacts) > 0 {
		st.ArtifactRefs = res.Artifacts
	}
	if res.Conclusion != "" {
		if st.OutputContract == nil {
			st.OutputContract = make(map[string]any)
		}
		st.OutputContract["conclusion"] = res.Conclusion
	}
}

// AggregateDStage builds the d-stage aggregation report from the scheduler's
// latest subtask states. The parent then leaves the admission pool: its slots
// are already back with the tenant gate, this only clears the parent's record.
func (b *OrchestratorBridge) AggregateDStage(ctx context.Context, ec OrchestratorStageContext) (*orchestrator.AggregationReport, error) {
	if !b.Enabled() {
		return nil, nil
	}
	state := b.stateFor(ec.TaskID)
	if state == nil || state.plan == nil {
		return nil, fmt.Errorf("orchestrator bridge: no DAG was planned for task %s", ec.TaskID)
	}
	defer b.releaseAdmission(ec.TenantID, ec.TaskID)
	latest := make([]*subtask.Subtask, 0, len(state.plan.Subtasks))
	for _, st := range state.plan.Subtasks {
		if state.scheduler != nil {
			if cur, ok := state.scheduler.Item(st.SubtaskID); ok {
				latest = append(latest, cur)
				continue
			}
		}
		latest = append(latest, st)
	}
	taskEmitter := b.taskEmitter(ec)
	aggregator := orchestrator.NewAggregator(orchestrator.FSVerifier{Root: ec.WorkspaceRoot}, taskEmitter)
	return aggregator.Aggregate(ctx, orchestrator.AggregationRequest{
		ParentTaskID: ec.TaskID,
		DagID:        state.dagID,
		TenantID:     ec.TenantID,
		ProjectID:    ec.ProjectID,
		Subtasks:     latest,
	})
}

func (b *OrchestratorBridge) stateFor(taskID string) *orchestrationState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tasks[taskID]
}

// poolFor returns the tenant's shared pool, creating it on first use, together
// with the quota snapshot it was built from. One pool per tenant is what makes
// the gate shared: two parents of the same tenant must compete for the same
// slots instead of each opening a private 20-slot pool.
func (b *OrchestratorBridge) poolFor(tenantID string) (*tenantPool, orchestrator.QuotaConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if pool, ok := b.pools[tenantID]; ok {
		return pool, b.quota
	}
	// 注意：gate 在这里只创建一次，然后同时交给 fairness 和 admission。
	gate := orchestrator.NewTenantGate(b.quota.MaxAgentsPerTenant)
	fairness := orchestrator.NewTenantFairness(orchestrator.FairnessConfigFromQuota(b.quota), gate)
	pool := &tenantPool{
		gate:      gate,
		fairness:  fairness,
		admission: orchestrator.NewAdmission(fairness, b.admission),
	}
	b.pools[tenantID] = pool
	return pool, b.quota
}

// releaseAdmission gives the parent's admission back once its d stage is done.
// Without it a retried parent would hit the idempotent short-circuit in Admit
// and skip the tenant-pool check entirely.
func (b *OrchestratorBridge) releaseAdmission(tenantID, taskID string) {
	b.mu.Lock()
	pool := b.pools[tenantID]
	b.mu.Unlock()
	if pool != nil {
		pool.admission.Release(context.Background(), taskID)
	}
}

func (b *OrchestratorBridge) taskEmitter(ec OrchestratorStageContext) *orchestrator.PublisherEmitter {
	if b.emitter == nil {
		return nil
	}
	return b.emitter.For(orchestrator.EmitContext{
		TenantID: ec.TenantID, ProjectID: ec.ProjectID, TaskID: ec.TaskID,
		Stage: ec.Stage, Attempt: ec.AttemptNo,
	})
}

// emitSecurityViolation reports a cross-tenant orchestration attempt.
func (b *OrchestratorBridge) emitSecurityViolation(ctx context.Context, ec OrchestratorStageContext, reason string) {
	if b == nil || b.emitter == nil {
		return
	}
	_ = b.taskEmitter(ec).EmitType(ctx, orchestrator.EmitContext{}, "security.violation", orchestrator.SeverityError, map[string]any{
		"reason": reason, "stage": ec.Stage,
	})
}

func extractJSON(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' && bytes.HasSuffix(trimmed, []byte("}")) {
		return trimmed
	}
	start := bytes.IndexByte(trimmed, '{')
	end := bytes.LastIndexByte(trimmed, '}')
	if start < 0 || end <= start {
		return nil
	}
	return trimmed[start : end+1]
}

// orchestrationTimeout bounds the extra bridge work so it can never hold a
// stage longer than its own budget.
func orchestrationTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return 30 * time.Second
	}
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	return d
}
