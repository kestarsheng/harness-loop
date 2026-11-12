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

// SubtaskExecutor runs one scheduled subtask. The production implementation is
// expected to reuse the stage runner/agent-service; the bridge itself never
// invents a second execution path.
type SubtaskExecutor interface {
	Execute(ctx context.Context, st *subtask.Subtask) error
}

type orchestrationState struct {
	scheduler *orchestrator.Scheduler
	plan      *orchestrator.PlanResult
	dagID     string
}

// OrchestratorBridge wires orchestrator.Planner/Scheduler/Aggregator into the
// runtime. When the feature flag is off the runtime never calls it and keeps
// the existing single-agent stage flow.
type OrchestratorBridge struct {
	enabled  bool
	emitter  *orchestrator.PublisherEmitter
	executor SubtaskExecutor

	mu    sync.Mutex
	tasks map[string]*orchestrationState
}

func NewOrchestratorBridge(enabled bool, emitter *orchestrator.PublisherEmitter, executor SubtaskExecutor) *OrchestratorBridge {
	return &OrchestratorBridge{
		enabled:  enabled,
		emitter:  emitter,
		executor: executor,
		tasks:    make(map[string]*orchestrationState),
	}
}

func (b *OrchestratorBridge) Enabled() bool { return b != nil && b.enabled }

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
func (b *OrchestratorBridge) PlanBStage(ctx context.Context, ec OrchestratorStageContext, rawPlan []byte) (*orchestrator.PlanResult, error) {
	if !b.Enabled() {
		return nil, nil
	}
	rawPlan = extractJSON(rawPlan)
	if len(rawPlan) == 0 {
		return nil, fmt.Errorf("orchestrator bridge: b stage output has no DAG plan")
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
		return nil, err
	}
	scheduler := orchestrator.NewScheduler(orchestrator.DefaultQuotaConfig(), taskEmitter)
	if err := scheduler.Enqueue(result.Subtasks); err != nil {
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
		if execErr := b.executor.Execute(ctx, st); execErr != nil {
			_ = state.scheduler.MarkFailed(ctx, st.SubtaskID, "subtask_failed")
			continue
		}
		_ = state.scheduler.MarkAccepted(ctx, st.SubtaskID, fmt.Sprintf("acc-%s", st.SubtaskID))
	}
	return running, nil
}

// AggregateDStage builds the d-stage aggregation report from the scheduler's
// latest subtask states.
func (b *OrchestratorBridge) AggregateDStage(ctx context.Context, ec OrchestratorStageContext) (*orchestrator.AggregationReport, error) {
	if !b.Enabled() {
		return nil, nil
	}
	state := b.stateFor(ec.TaskID)
	if state == nil || state.plan == nil {
		return nil, fmt.Errorf("orchestrator bridge: no DAG was planned for task %s", ec.TaskID)
	}
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
	aggregator := orchestrator.NewAggregator(nil, taskEmitter)
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