package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/acceptance"
	"agent-platform/pkg/harnessloop/checkpoint"
	"agent-platform/pkg/harnessloop/compaction"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/retry"
	"agent-platform/pkg/harnessloop/tracing"
	"agent-platform/pkg/llm"
	"agent-platform/services/harnessloop-service/internal/metrics"
	"agent-platform/services/harnessloop-service/internal/repository"
)

var (
	ErrRuntimeInput = errors.New("runtime: invalid input")
	ErrRuntimeState = errors.New("runtime: task is not runnable")
)

const (
	ReasonCompleted = "completed"
	ReasonAborted   = "aborted"
	ReasonFailed    = "failed"
	// ReasonCompletedWithFailedSubtask is the tolerated half-success: every
	// subtask reached a terminal state, at least one was accepted, and the
	// failures all sit in tolerate sets (T-P2-29 point 3). The DB status stays
	// "completed", the terminal reason is what distinguishes it from a clean run.
	ReasonCompletedWithFailedSubtask = "completed_with_failed_subtasks"
)

type TaskStore interface {
	Get(ctx context.Context, tenantID, taskID string) (*repository.TaskRun, error)
	Transfer(ctx context.Context, tenantID, taskID string, to fsm.Stage, expectedVersion int64, events []repository.EventInsert) (*repository.TaskRun, error)
	UpdateStatus(ctx context.Context, tenantID, taskID string, status repository.TaskStatus, reason string, expectedVersion int64) (*repository.TaskRun, error)
	UpdateAttemptNo(ctx context.Context, tenantID, taskID string, stage fsm.Stage, no int, expectedVersion int64) error
}

type AttemptStore interface {
	NextAttemptNo(ctx context.Context, taskID string, stage fsm.Stage) (int, error)
	Create(ctx context.Context, a fsm.Attempt) error
	Finish(ctx context.Context, attemptID string, endedAt time.Time) error
	AttachAcceptance(ctx context.Context, attemptID, acceptanceID string) error
}

type StageExecutor interface {
	Run(ctx context.Context, in StageRunInput) (*StageRunResult, error)
}

type CheckpointStore interface {
	Save(ctx context.Context, cp *checkpoint.Checkpoint) error
}

type AcceptanceInput struct {
	TaskID    string
	Stage     string
	AttemptID string
	Goal      string
	GoalHash  string
	Result    *StageRunResult
}

type Acceptor interface {
	Evaluate(ctx context.Context, in AcceptanceInput) (acceptance.Verdict, error)
}

type RunInput struct {
	TenantID         string
	ProjectID        string
	TaskID           string
	SessionID        string
	UserID           string
	EntryAgent       string
	Goal             string
	GoalHash         string
	PolicySnapshot   []byte
	PolicyHash       string
	StageModel       string
	ModelWindow      int
	RequiresToolCall bool
	StageTimeout     time.Duration
	MaxSteps         int
}

type RunResult struct {
	TerminalReason string
	FinalStage     string
	Steps          int
	FailedStage    string
	Category       retry.Category
	RetryCount     int
	LastError      string
	CheckpointID   string
	// FailedSubtasks lists the children a tolerated half-success finished with
	// (T-P2-29). It is empty for a clean completed run.
	FailedSubtasks []orchestrator.FailedSubtaskRef
}

type AbortResult struct {
	TerminalReason string
	Timestamp      time.Time
	Duplicate      bool
}

type abortKey struct {
	taskID string
	epoch  int64
}

type Runtime struct {
	Tasks       TaskStore
	Attempts    AttemptStore
	Stages      StageExecutor
	Checkpoints CheckpointStore
	Events      EventPublisher
	Acceptor    Acceptor
	Policy      retry.Policy
	Now         func() time.Time

	// StageVerifier selects the independent program/model/mixed acceptance
	// channel. A nil value keeps the historical Acceptor/program-only path.
	StageVerifier acceptance.StageVerifier

	Injector    *MemoryInjector
	HarnessHome string

	Models       *ModelBridge
	Orchestrator *OrchestratorBridge

	CompactionTrigger    *compaction.Trigger
	CompactionSummarizer compaction.Summarizer
	CompactionLimits     compaction.DegradeLimits
	CompactionDir        string

	// Metrics and Tracer are optional; nil values are no-ops so existing
	// constructors in tests keep working unchanged.
	Metrics *metrics.Harness
	Tracer  *tracing.Tracer

	mu                sync.Mutex
	pubMu             sync.Mutex
	aborts            map[abortKey]AbortResult
	aborted           map[string]bool
	terminalPublished map[string]bool
}

func NewRuntime(tasks TaskStore, attempts AttemptStore, stages StageExecutor) *Runtime {
	return &Runtime{
		Tasks:             tasks,
		Attempts:          attempts,
		Stages:            stages,
		Policy:            retry.DefaultPolicy(),
		Now:               time.Now,
		StageVerifier:     newStageVerifierFromEnv(),
		aborts:            make(map[abortKey]AbortResult),
		aborted:           make(map[string]bool),
		terminalPublished: make(map[string]bool),
	}
}

func (r *Runtime) Run(ctx context.Context, in RunInput) (*RunResult, error) {
	if r.Tasks == nil || r.Attempts == nil || r.Stages == nil {
		return nil, fmt.Errorf("%w: tasks/attempts/stages are required", ErrRuntimeInput)
	}
	if in.TenantID == "" || in.TaskID == "" {
		return nil, fmt.Errorf("%w: tenant_id and task_id are required", ErrRuntimeInput)
	}
	if in.EntryAgent == "" || in.PolicyHash == "" || in.StageModel == "" || in.ModelWindow <= 0 {
		return nil, fmt.Errorf("%w: entry_agent/policy_hash/stage_model/model_window are required", ErrRuntimeInput)
	}
	if in.Goal == "" {
		in.Goal = "harness loop task"
	}
	if in.GoalHash == "" {
		in.GoalHash = in.PolicyHash
	}

	taskSpan := r.Tracer.StartTask(ctx, in.TenantID, in.ProjectID, in.TaskID)
	defer taskSpan.End()

	task, err := r.Tasks.Get(ctx, in.TenantID, in.TaskID)
	if err != nil {
		return nil, err
	}
	if task.Status != repository.TaskRunning {
		return nil, fmt.Errorf("%w: status=%s", ErrRuntimeState, task.Status)
	}

	result := &RunResult{}
	if task.CurrentStage == fsm.StageDone {
		return r.finalize(ctx, in, result, ReasonCompleted, fsm.StageDone, "", "", 0, "", task.Version)
	}

	budget := retry.NewBudget(r.Policy)
	entryReason := fsm.EntryInitial
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 200
	}
	modelStage := ""

	for step := 0; step < maxSteps; step++ {
		select {
		case <-ctx.Done():
			return r.finalize(ctx, in, result, ReasonAborted, task.CurrentStage, "", "", 0, ctx.Err().Error(), task.Version)
		default:
		}
		if r.isAborted(in.TaskID) {
			return r.finalize(ctx, in, result, ReasonAborted, task.CurrentStage, "", "", 0, "aborted by user", task.Version)
		}

		stage := task.CurrentStage
		attemptNo, err := r.Attempts.NextAttemptNo(ctx, task.TaskID, stage)
		if err != nil {
			return nil, fmt.Errorf("runtime: next attempt no: %w", err)
		}
		attempt, err := fsm.NewAttempt(task.TaskID, stage, attemptNo, entryReason, r.now())
		if err != nil {
			return nil, fmt.Errorf("runtime: create attempt: %w", err)
		}
		if err := r.Attempts.Create(ctx, attempt); err != nil {
			return nil, fmt.Errorf("runtime: persist attempt: %w", err)
		}
		if err := r.Tasks.UpdateAttemptNo(ctx, in.TenantID, task.TaskID, stage, attemptNo, task.Version); err != nil {
			return nil, fmt.Errorf("runtime: update attempt no: %w", err)
		}
		task.AttemptNo[string(stage)] = attemptNo
		task.Version++

		if r.Models != nil && r.Models.Enabled() && modelStage != string(stage) {
			r.Models.OnStageEnter(ModelStageContext{
				TenantID: in.TenantID, ProjectID: in.ProjectID, TaskID: in.TaskID,
				Stage: string(stage), AttemptNo: attemptNo,
			})
			modelStage = string(stage)
		}

		r.saveCheckpoint(ctx, in, task, attempt, checkpoint.KindStageEntry, result)

		stageInput := StageRunInput{
			TaskID:           in.TaskID,
			WorkspaceRoot:    task.WorkspaceRoot,
			TenantID:         in.TenantID,
			ProjectID:        in.ProjectID,
			SessionID:        in.SessionID,
			UserID:           in.UserID,
			Stage:            string(stage),
			AttemptID:        attempt.AttemptID,
			EntryAgent:       in.EntryAgent,
			Message:          stageMessage(stage, attemptNo),
			Goal:             in.Goal,
			SuccessCriteria:  "",
			PolicySnapshot:   in.PolicySnapshot,
			PolicyHash:       in.PolicyHash,
			StageModel:       in.StageModel,
			ModelWindow:      in.ModelWindow,
			RequiresToolCall: in.RequiresToolCall,
			Timeout:          in.StageTimeout,
		}
		// b 阶段的模型钉死在 stageModelPin 里：模型策略开关关掉时也要走同一个模型，
		// 否则 b 阶段跟着全局 stage_model 走，弱模型产不出 DAG 就只能降级单节点。
		pinnedModel, pinnedWindow, pinned := stageModelPin(stage)
		if pinned {
			stageInput.StageModel = pinnedModel
			stageInput.ModelWindow = pinnedWindow
		}
		if r.Models != nil && r.Models.Enabled() {
			choice, modelErr := r.Models.SelectForStage(ctx, ModelStageRequest{
				ModelStageContext: ModelStageContext{
					TenantID: in.TenantID, ProjectID: in.ProjectID, TaskID: in.TaskID,
					Stage: string(stage), AttemptNo: attemptNo,
				},
				// 显式 ModelID 记为 resolved_from=explicit；models.yaml 的
				// stage_overrides.b 优先级更高（override），两者指向同一个模型。
				ModelID:          pinnedModel,
				RequireToolCall:  in.RequiresToolCall,
				TaskBudgetTokens: task.BudgetTokens,
			})
			if modelErr == nil && choice != nil {
				stageInput.StageModel = choice.ModelID
				stageInput.ModelWindow = choice.Window
			}
		}
		if r.Injector != nil {
			fallback := &MDFallback{
				GlobalFile:  r.globalMemoryPath(),
				ProjectFile: filepath.Join(task.WorkspaceRoot, ".harness", "memory", "project.md"),
			}
			if injected, injErr := r.Injector.Inject(ctx, InjectOptions{
				TenantID: in.TenantID, ProjectID: in.ProjectID, Stage: string(stage),
				Query: in.Goal, ContextBudget: in.ModelWindow, Fallback: fallback,
			}); injErr == nil && len(injected.Items) > 0 {
				stageInput.Message = formatInjectedMessage(injected, stageInput.Message)
			}
		}
		stageInput.Message = r.maybeCompact(ctx, in, task, stage, attempt, stageInput.Message, result)
		stageStart := r.now()
		stageSpan := r.Tracer.StartStage(taskSpan, string(stage), attemptNo)
		stageRes, stageErr := r.Stages.Run(stageSpan.Context(), stageInput)
		r.Metrics.StageDuration(string(stage), attemptNo, r.now().Sub(stageStart))
		stageSpan.End()
		result.Steps++
		entryReason = fsm.EntryRetry
		if stageRes == nil {
			stageRes = &StageRunResult{}
		}

		if stageErr != nil || stageRes.Decision.Action != retry.ActionDone {
			if stageErr != nil && stageRes.Decision.Action == retry.ActionFail {
				category := retry.System
				lastErr := stageRes.Decision.Reason
				if stageRes.Classification != nil {
					category = stageRes.Classification.Category
					lastErr = stageRes.Classification.Reason
				}
				r.publishError(ctx, in, stage, attempt, category, lastErr)
				return r.finalize(ctx, in, result, ReasonFailed, stage, string(stage), string(category), 0, lastErr, task.Version)
			}
			category := retry.System
			lastErr := "stage did not finish"
			if stageRes.Classification != nil {
				category = stageRes.Classification.Category
				lastErr = stageRes.Classification.Reason
			} else if stageErr != nil {
				classified := ClassifyStageError(stageErr, "")
				category = classified.Category
				lastErr = classified.Reason
			}
			decision := budget.RecordError(category, retry.FixedJitter)
			r.publishError(ctx, in, stage, attempt, category, lastErr)
			if r.Models != nil && r.Models.Enabled() {
				r.Models.ObserveStageFailure(ModelStageContext{
					TenantID: in.TenantID, ProjectID: in.ProjectID, TaskID: in.TaskID,
					Stage: string(stage), AttemptNo: attemptNo,
				}, stageInput.StageModel, modelFailureKind(category))
			}
			result.Category = category
			result.LastError = lastErr
			if decision.Terminal {
				return r.finalize(ctx, in, result, ReasonFailed, stage, string(stage), string(category), decision.RetryIndex, lastErr, task.Version)
			}
			continue
		}

		if err := r.Attempts.Finish(ctx, attempt.AttemptID, r.now()); err != nil {
			return nil, fmt.Errorf("runtime: finish attempt: %w", err)
		}
		aggregation, aggErr := r.runOrchestration(ctx, in, task.WorkspaceRoot, stage, attempt, stageRes)
		// T-P2-29：子任务没收口时，父任务连 d 阶段验收都不该开始——否则
		// "父任务 completed"会变成子任务 running 的遮羞布。
		if gateErr := r.dStageClosure(stage, aggregation, aggErr); gateErr != nil {
			r.publishError(ctx, in, stage, attempt, retry.System, gateErr.Error())
			fmt.Printf("[orchestrator] d-stage closure gate blocked completion: %v\n", gateErr)
			return r.finalize(ctx, in, result, ReasonFailed, stage, string(stage), string(retry.System), 0, gateErr.Error(), task.Version)
		}

		accSpan := r.Tracer.StartAcceptance(stageSpan, string(stage), attemptNo, "")
		verdict := r.evaluate(ctx, in, task, aggregation, stage, attempt, stageRes)
		accSpan.SetAttr("verdict", string(verdict.Verdict))
		accSpan.End()
		r.Metrics.Acceptance(string(stage), string(verdict.Verdict), string(verdict.VerifiedBy))
		attempt.AcceptanceID = verdict.AcceptanceID
		if verdict.AcceptanceID != "" {
			_ = r.Attempts.AttachAcceptance(ctx, attempt.AttemptID, verdict.AcceptanceID)
		}

		if verdict.Verdict == acceptance.VerdictPass {
			target, ok := fsm.SelectTarget(stage, fsm.VerdictPass, "")
			if !ok {
				return r.finalize(ctx, in, result, ReasonFailed, stage, string(stage), string(retry.System), 0, "no legal transition", task.Version)
			}
			updated, err := r.Tasks.Transfer(ctx, in.TenantID, task.TaskID, target, task.Version, r.transitionEvents(in, stage, target, attempt))
			if err != nil {
				return nil, fmt.Errorf("runtime: transfer: %w", err)
			}
			task = updated
			budget.OnStageEnter()
			entryReason = fsm.EntryInitial
			if target == fsm.StageDone {
				reason := ReasonCompleted
				if aggregation != nil && aggregation.CompletedWithFailures {
					// tolerate 策略下确有失败但全部收口：完成，但不假装干净。
					reason = ReasonCompletedWithFailedSubtask
					result.FailedSubtasks = aggregation.FailedSubtasks
				}
				return r.finalize(ctx, in, result, reason, target, "", "", 0, "", task.Version)
			}
			continue
		}

		decision := budget.RecordAcceptanceFailure()
		category := rollbackCategory(verdict)
		result.Category = category
		result.LastError = verdict.Reason

		if stage == fsm.StageRequirement {
			entryReason = fsm.EntryAcceptanceFailed
			if decision.Terminal {
				return r.finalize(ctx, in, result, ReasonFailed, stage, string(stage), string(category), decision.RetryIndex, verdict.Reason, task.Version)
			}
			budget.OnStageEnter()
			continue
		}

		target := fsm.Stage(verdict.RollbackTarget)
		if !fsm.IsLegalTarget(stage, target) {
			target = fsm.Stage(defaultRollbackTarget(stage))
		}
		if target == "" {
			target = fsm.Stage(defaultRollbackTarget(stage))
		}
		updated, err := r.Tasks.Transfer(ctx, in.TenantID, task.TaskID, target, task.Version, r.transitionEvents(in, stage, target, attempt))
		if err != nil {
			return nil, fmt.Errorf("runtime: rollback transfer: %w", err)
		}
		task = updated
		budget.OnStageEnter()
		entryReason = fsm.EntryAcceptanceFailed
		if decision.Terminal {
			return r.finalize(ctx, in, result, ReasonFailed, stage, string(stage), string(category), decision.RetryIndex, verdict.Reason, task.Version)
		}
	}

	decision := budget.RecordError(retry.System, retry.FixedJitter)
	return r.finalize(ctx, in, result, ReasonFailed, task.CurrentStage, string(task.CurrentStage), string(retry.System), decision.RetryIndex, "max steps reached", task.Version)
}

func (r *Runtime) Abort(ctx context.Context, tenantID, taskID string, epoch int64) (AbortResult, error) {
	if tenantID == "" || taskID == "" {
		return AbortResult{}, fmt.Errorf("%w: tenant_id and task_id are required", ErrRuntimeInput)
	}
	key := abortKey{taskID: taskID, epoch: epoch}
	r.mu.Lock()
	defer r.mu.Unlock()

	if prev, ok := r.aborts[key]; ok {
		prev.Duplicate = true
		return prev, nil
	}

	task, err := r.Tasks.Get(ctx, tenantID, taskID)
	if err != nil {
		return AbortResult{}, err
	}
	res := AbortResult{TerminalReason: ReasonAborted, Timestamp: r.now()}
	if task.Status == repository.TaskAborted || task.Status == repository.TaskCompleted || task.Status == repository.TaskFailed {
		res.TerminalReason = string(task.Status)
		res.Timestamp = task.UpdatedAt
		if task.Status == repository.TaskAborted {
			r.aborted[taskID] = true
		}
	} else {
		updated, uerr := r.Tasks.UpdateStatus(ctx, tenantID, taskID, repository.TaskAborted, ReasonAborted, task.Version)
		if uerr != nil && !errors.Is(uerr, repository.ErrVersionConflict) {
			return AbortResult{}, uerr
		}
		if uerr == nil {
			res.Timestamp = updated.UpdatedAt
		}
		r.aborted[taskID] = true
		r.publishTerminal(ctx, tenantID, task.ProjectID, taskID, ReasonAborted, nil)
	}
	r.aborts[key] = res
	return res, nil
}

func (r *Runtime) isAborted(taskID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aborted[taskID]
}

// planArtifact returns the b-stage payload most likely to be a SubtaskPlan/v1.
// stage_finish 的 summary 是协议规定的结构化字段，但模型对「总结」的理解很稳定：
// 它会把 DAG 写进 content、把散文写进 summary。两处都按「扫出第一个能被 ParsePlan
// 接受的 JSON 对象」来找；都不行时仍返回 summary，让下游报出真实的解析错误。
func planArtifact(res *StageRunResult) []byte {
	if res == nil {
		return nil
	}
	summary := []byte(res.FinishArgs.Summary)
	if plan, ok := firstParseablePlan(summary); ok {
		return plan
	}
	if plan, ok := firstParseablePlan([]byte(res.Content)); ok {
		return plan
	}
	if len(summary) > 0 {
		return summary
	}
	return []byte(res.Content)
}

// firstParseablePlan walks every balanced {...} object in text and returns the
// first one ParsePlan accepts. b 阶段的 content 常是「计划 JSON + [stage_finish]
// {...}」两段相邻的 JSON，按首尾大括号整段截取会拼成非法 JSON。
func firstParseablePlan(text []byte) ([]byte, bool) {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		end := matchClosingBrace(text, i)
		if end <= i {
			continue
		}
		obj := text[i : end+1]
		if _, err := orchestrator.ParsePlan(obj); err == nil {
			return obj, true
		}
	}
	return nil, false
}

// matchClosingBrace returns the index of the brace closing the one at start, or
// -1 when the object is unterminated. Strings and escapes are skipped so braces
// inside JSON strings never move the depth.
func matchClosingBrace(text []byte, start int) int {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (r *Runtime) finalize(ctx context.Context, in RunInput, res *RunResult, reason string, stage fsm.Stage, failedStage, categoryName string, retries int, lastErr string, version int64) (*RunResult, error) {
	status := repository.TaskCompleted
	switch reason {
	case ReasonAborted:
		status = repository.TaskAborted
	case ReasonFailed:
		status = repository.TaskFailed
	}
	if _, err := r.Tasks.UpdateStatus(ctx, in.TenantID, in.TaskID, status, reason, version); err != nil {
		if !errors.Is(err, repository.ErrVersionConflict) {
			return nil, fmt.Errorf("runtime: finalize status: %w", err)
		}
	}
	// DB 与事件用同一份 terminal_reason（含 failed_subtasks），避免"库里 completed、
	// 事件里 completed_with_failed_subtasks"这种两边不一致（T-P2-29 验收）。
	r.publishTerminal(ctx, in.TenantID, in.ProjectID, in.TaskID, reason, res.FailedSubtasks)
	r.Metrics.TaskTerminal(string(status))

	res.TerminalReason = reason
	res.FinalStage = string(stage)
	res.FailedStage = failedStage
	if categoryName != "" {
		res.Category = retry.Category(categoryName)
	}
	res.RetryCount = retries
	res.LastError = lastErr
	return res, nil
}

// runOrchestration bridges b/c/d stage completion into the orchestration layer.
// It is best-effort for the parent stage — a plan that cannot be parsed or a
// missing executor never aborts the parent stage, preserving the pre-flag
// behaviour — but the d-stage report and its error are returned so the caller
// can enforce the "children must be closed" invariant (T-P2-29).
func (r *Runtime) runOrchestration(ctx context.Context, in RunInput, workspaceRoot string, stage fsm.Stage, attempt fsm.Attempt, res *StageRunResult) (*orchestrator.AggregationReport, error) {
	if r.Orchestrator == nil || !r.Orchestrator.Enabled() || res == nil {
		return nil, nil
	}
	ec := OrchestratorStageContext{
		TenantID: in.TenantID, ProjectID: in.ProjectID, TaskID: in.TaskID,
		Stage: string(stage), AttemptNo: attempt.AttemptNo, WorkspaceRoot: workspaceRoot,
	}
	timeout := in.StageTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, orchestrationTimeout(timeout))
	defer cancel()
	switch stage {
	case fsm.StagePlan:
		// StageMode 下 content 是 `[stage_finish] {...}` 协议文本，结构化产物在 summary。
		// 但模型经常把 DAG 写在 content、把散文写在 summary（或反过来），只认 summary
		// 就会重新掉回「b accepted 但 subtasks=0」的单节点降级。
		raw := planArtifact(res)
		if _, err := r.Orchestrator.PlanBStage(runCtx, ec, raw); err != nil {
			// 设计上 b 阶段规划失败不阻塞父阶段，但绝不静默：
			// 否则表现为「b accepted 但 subtasks=0」，无法定位。
			fmt.Printf("[orchestrator] b-stage plan rejected: %v (raw_len=%d raw=%.400s)\n", err, len(raw), string(raw))
		}
	case fsm.StageExecute:
		_, _ = r.Orchestrator.DispatchCStage(runCtx, ec)
	case fsm.StageVerify:
		return r.Orchestrator.AggregateDStage(runCtx, ec)
	}
	return nil, nil
}

// dStageClosure is the parent terminal invariant of T-P2-29: a parent may only
// leave d as completed when every child reached a terminal state and at least
// one child was accepted. A missing DAG (nothing was ever planned) is not a
// violation — there are no children to close — and the pre-orchestration
// single-agent path keeps working unchanged.
func (r *Runtime) dStageClosure(stage fsm.Stage, report *orchestrator.AggregationReport, aggErr error) error {
	if r.Orchestrator == nil || !r.Orchestrator.Enabled() || stage != fsm.StageVerify {
		return nil
	}
	if errors.Is(aggErr, ErrNoDagPlanned) {
		return nil
	}
	if aggErr != nil {
		return fmt.Errorf("subtasks_not_closed: d-stage aggregation failed: %w", aggErr)
	}
	if report == nil {
		return errors.New("subtasks_not_closed: d-stage produced no aggregation report")
	}
	if report.Summary.NonTerminal > 0 {
		return fmt.Errorf("subtasks_not_closed: %d of %d subtasks are still pending/running",
			report.Summary.NonTerminal, report.Summary.Total)
	}
	if !report.Complete {
		return fmt.Errorf("subtasks_not_closed: aggregation report is incomplete (%d conflicts, %d/%d accepted)",
			len(report.Conflicts), report.Summary.Accepted, report.Summary.Total)
	}
	if report.Summary.Accepted == 0 {
		return fmt.Errorf("subtasks_not_closed: 0 of %d subtasks were accepted", report.Summary.Total)
	}
	return nil
}

func (r *Runtime) evaluate(ctx context.Context, in RunInput, task *repository.TaskRun, aggregation *orchestrator.AggregationReport, stage fsm.Stage, attempt fsm.Attempt, out *StageRunResult) acceptance.Verdict {
	criteria, channel := acceptanceCriteriaForTask(task, stage)
	if len(criteria) > 0 && channel != acceptance.VerifiedByProgram {
		return r.evaluateStageVerifier(ctx, in, task, aggregation, stage, attempt, out, criteria, channel)
	}

	if r.Acceptor != nil {
		v, err := r.Acceptor.Evaluate(ctx, AcceptanceInput{
			TaskID: in.TaskID, Stage: string(stage), AttemptID: attempt.AttemptID,
			Goal: in.Goal, GoalHash: in.GoalHash, Result: out,
		})
		if err == nil {
			return v
		}
	}
	return acceptance.Verdict{
		AcceptanceID:  fmt.Sprintf("acc-%s-%s-%d", in.TaskID, stage, attempt.AttemptNo),
		Verdict:       acceptance.VerdictPass,
		Reason:        "stage finish accepted",
		Evidence:      []acceptance.Evidence{{Kind: "command", Ref: "stage_finish"}},
		VerifiedBy:    acceptance.VerifiedByProgram,
		PromptVersion: acceptance.PromptVersion,
		InputDigest:   in.PolicyHash,
	}
}

// acceptanceCriteriaForTask reads the stage criteria declared in the task
// policy snapshot. Criteria without a model/mixed declaration keep the legacy
// program-only path, so existing tasks observe zero behaviour change.
func acceptanceCriteriaForTask(task *repository.TaskRun, stage fsm.Stage) ([]acceptance.Criterion, string) {
	if task == nil || len(task.PolicySnapshot) == 0 {
		return nil, acceptance.VerifiedByProgram
	}
	criteria, err := acceptance.ParseStageCriteria(task.PolicySnapshot, string(stage))
	if err != nil || len(criteria) == 0 {
		return nil, acceptance.VerifiedByProgram
	}
	if err := acceptance.ValidateCriterionChannels(criteria); err != nil {
		return criteria, "invalid"
	}
	return criteria, acceptance.ResolveChannel(criteria)
}

func (r *Runtime) evaluateStageVerifier(ctx context.Context, in RunInput, task *repository.TaskRun, aggregation *orchestrator.AggregationReport, stage fsm.Stage, attempt fsm.Attempt, out *StageRunResult, criteria []acceptance.Criterion, channel string) acceptance.Verdict {
	accID := fmt.Sprintf("acc-%s-%s-%d", in.TaskID, stage, attempt.AttemptNo)
	verifiedBy := channel
	if verifiedBy == "invalid" || verifiedBy == "" {
		verifiedBy = acceptance.VerifiedByProgram
	}
	fail := func(code, reason string) acceptance.Verdict {
		return stageVerifierFailure(accID, in.PolicyHash, verifiedBy, code, reason)
	}

	if channel == "invalid" {
		return fail("invalid_channel", "acceptance criteria declare an invalid verification channel")
	}
	if r.StageVerifier == nil {
		return fail(acceptance.ReasonVerifierNotConfigured, "no stage verifier is configured for channel "+channel)
	}

	goal, goalHash, workspace := in.Goal, in.GoalHash, ""
	var budgetRemaining int64
	if task != nil {
		if goal == "" {
			goal = task.Goal
		}
		if goalHash == "" {
			goalHash = task.GoalHash
		}
		workspace = task.WorkspaceRoot
		budgetRemaining = task.BudgetTokens - task.ConsumedTokens
	}

	spec := acceptance.Spec{
		AcceptanceID: accID,
		TaskID:       in.TaskID,
		TenantID:     in.TenantID,
		ProjectID:    in.ProjectID,
		Stage:        string(stage),
		AttemptID:    attempt.AttemptID,
		Goal:         goal,
		GoalHash:     goalHash,
		Criteria:     criteria,
		Artifacts:    stageArtifacts(out),
		Aggregation:  stageAggregation(aggregation),
		Constraints: acceptance.Constraints{
			WorkspaceRoot:         workspace,
			ToolWhitelist:         append([]string(nil), acceptance.AcceptanceTools...),
			BudgetRemainingTokens: budgetRemaining,
		},
	}
	input, err := acceptance.Assemble(spec)
	if err != nil {
		return fail(acceptance.ReasonContextIncomplete, err.Error())
	}

	v, err := r.StageVerifier.Verify(ctx, acceptance.StageVerifyRequest{
		Input:    input,
		Criteria: input.Criteria,
		Channel:  channel,
		WorkDir:  workspace,
		Timeout:  in.StageTimeout,
	})
	if err != nil {
		return fail(acceptance.ReasonVerifierNotConfigured, "stage verifier error: "+err.Error())
	}
	if err := v.Validate(); err != nil {
		return fail(acceptance.ReasonContextIncomplete, "invalid verdict: "+err.Error())
	}
	return v
}

func stageVerifierFailure(acceptanceID, digest, verifiedBy, code, reason string) acceptance.Verdict {
	if digest == "" {
		digest = "policy"
	}
	detail := code + ": " + reason
	return acceptance.Verdict{
		AcceptanceID:   acceptanceID,
		Verdict:        acceptance.VerdictFail,
		Reason:         detail,
		Evidence:       []acceptance.Evidence{{Kind: "verdict", Ref: "acceptance", Excerpt: detail}},
		FailedCriteria: []acceptance.FailedCriterion{{CriterionID: "acceptance", Category: "unknown"}},
		VerifiedBy:     verifiedBy,
		PromptVersion:  acceptance.PromptVersion,
		InputDigest:    digest,
	}
}

func stageArtifacts(out *StageRunResult) []acceptance.Artifact {
	if out == nil || len(out.FinishArgs.Artifacts) == 0 {
		return nil
	}
	arts := make([]acceptance.Artifact, 0, len(out.FinishArgs.Artifacts))
	for _, a := range out.FinishArgs.Artifacts {
		arts = append(arts, acceptance.Artifact{Path: a.Path, Hash: a.Hash, Size: a.Size})
	}
	return arts
}

func stageAggregation(rep *orchestrator.AggregationReport) *acceptance.AggregationReport {
	if rep == nil {
		return nil
	}
	out := &acceptance.AggregationReport{
		ReportID:       rep.DagID,
		AcceptedCount:  rep.Summary.Accepted,
		FailedCount:    rep.Summary.Failed + rep.Summary.Rejected,
		CancelledCount: rep.Summary.Cancelled,
	}
	if out.ReportID == "" {
		out.ReportID = rep.TaskID
	}
	for _, c := range rep.Conflicts {
		entry := c.Kind
		if c.Path != "" {
			entry += ":" + c.Path
		}
		out.Conflicts = append(out.Conflicts, entry)
	}
	for _, item := range rep.Items {
		for _, a := range item.Artifacts {
			out.Artifacts = append(out.Artifacts, acceptance.Artifact{Path: a.Path, Hash: a.Hash})
		}
	}
	return out
}

func (r *Runtime) saveCheckpoint(ctx context.Context, in RunInput, task *repository.TaskRun, attempt fsm.Attempt, kind string, res *RunResult) {
	if r.Checkpoints == nil {
		return
	}
	cp := &checkpoint.Checkpoint{
		CheckpointID: fmt.Sprintf("cp-%s-%s-%d-%d", in.TaskID, attempt.Stage, attempt.AttemptNo, r.now().UnixNano()),
		TaskID:       in.TaskID,
		TenantID:     in.TenantID,
		SessionID:    in.SessionID,
		Stage:        string(attempt.Stage),
		AttemptNo:    attempt.AttemptNo,
		AttemptID:    attempt.AttemptID,
		Step:         res.Steps,
		Kind:         kind,
		FSM: checkpoint.FSMSnapshot{
			CurrentStage:          string(task.CurrentStage),
			Status:                string(task.Status),
			AttemptNoByStage:      task.AttemptNo,
			RetryCountByStage:     task.RetryCount,
			AcceptanceFailByStage: task.AcceptanceFail,
		},
		Consumed:  checkpoint.Consumed{Tokens: task.ConsumedTokens, Cost: task.ConsumedCost},
		CreatedAt: r.now().UTC(),
	}
	cp.Seal()
	if err := r.Checkpoints.Save(ctx, cp); err == nil {
		r.Metrics.Checkpoint("saved")
	}
}

func (r *Runtime) transitionEvents(in RunInput, from, to fsm.Stage, attempt fsm.Attempt) []repository.EventInsert {
	payload := []byte(fmt.Sprintf(`{"from":%q,"to":%q,"attempt_id":%q}`, from, to, attempt.AttemptID))
	return []repository.EventInsert{{
		EventID:   fmt.Sprintf("%s-%s-%s", in.TaskID, from, to),
		Seq:       0,
		EventType: "stage.transition",
		Severity:  "info",
		Stage:     to,
		Payload:   payload,
	}}
}

func (r *Runtime) publishTerminal(ctx context.Context, tenantID, projectID, taskID, reason string, failed []orchestrator.FailedSubtaskRef) {
	if r.Events == nil {
		return
	}
	key := taskID + "/" + reason
	r.pubMu.Lock()
	if r.terminalPublished[key] {
		r.pubMu.Unlock()
		return
	}
	r.terminalPublished[key] = true
	r.pubMu.Unlock()

	typ := event.TaskFailed
	severity := event.SeverityError
	switch reason {
	case ReasonCompleted:
		typ = event.TaskCompleted
		severity = event.SeverityInfo
	case ReasonCompletedWithFailedSubtask:
		// 完成，但确实有子任务失败：事件类型仍是 task.completed（父任务真的
		// 走完了），严重级别降为 warn，并把失败子任务列在 payload 里。
		typ = event.TaskCompleted
		severity = event.SeverityWarn
	case ReasonAborted:
		typ = event.TaskAborted
		severity = event.SeverityWarn
	}
	_ = r.Events.Publish(ctx, &event.Event{
		EventID:       fmt.Sprintf("%s-%s", taskID, reason),
		EventType:     typ,
		SchemaVersion: event.CurrentSchemaVersion,
		TaskID:        taskID,
		Timestamp:     r.now().UTC(),
		Severity:      severity,
		TenantID:      tenantID,
		ProjectID:     projectID,
		Payload:       terminalPayload(reason, failed),
	})
}

// terminalPayload keeps the historical {"terminal_reason":...} shape and adds
// failed_subtasks only when the parent finished with tolerated failures.
func terminalPayload(reason string, failed []orchestrator.FailedSubtaskRef) []byte {
	payload := map[string]any{"terminal_reason": reason}
	if len(failed) > 0 {
		payload["failed_subtasks"] = failed
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return []byte(fmt.Sprintf(`{"terminal_reason":%q}`, reason))
	}
	return raw
}

func (r *Runtime) publishError(ctx context.Context, in RunInput, stage fsm.Stage, attempt fsm.Attempt, category retry.Category, reason string) {
	r.Metrics.Error(string(category), string(stage), r.Policy.Retryable(category))
	if r.Events == nil {
		return
	}
	payload := []byte(fmt.Sprintf(`{"category":%q,"reason":%q}`, category, reason))
	_ = r.Events.Publish(ctx, &event.Event{
		EventID:       fmt.Sprintf("%s-%s-error-%d", in.TaskID, stage, attempt.AttemptNo),
		EventType:     event.Error,
		SchemaVersion: event.CurrentSchemaVersion,
		TaskID:        in.TaskID,
		Stage:         string(stage),
		Attempt:       attempt.AttemptNo,
		Timestamp:     r.now().UTC(),
		Severity:      event.SeverityWarn,
		TenantID:      in.TenantID,
		ProjectID:     in.ProjectID,
		Payload:       payload,
	})
}

func (r *Runtime) compactionLimits() compaction.DegradeLimits {
	limits := r.CompactionLimits
	if limits.KeepRecent == 0 {
		limits = compaction.DefaultDegradeLimits()
	}
	return limits
}

func (r *Runtime) maybeCompact(ctx context.Context, in RunInput, task *repository.TaskRun, stage fsm.Stage, attempt fsm.Attempt, message string, res *RunResult) string {
	if r.CompactionTrigger == nil {
		return message
	}
	r.CompactionTrigger.SetModel(in.StageModel)
	decision := r.CompactionTrigger.EvaluateText(message)
	if !decision.Should {
		return message
	}

	r.saveCheckpoint(ctx, in, task, attempt, checkpoint.KindPreCompaction, res)
	r.publishCompaction(ctx, in, stage, attempt, event.CompactionTriggered, compaction.TriggerEventPayload(decision))

	history := compaction.History{
		Goal:     in.Goal,
		GoalHash: in.GoalHash,
		Phase:    string(stage),
		Messages: []compaction.Message{{Role: "user", Content: message}},
	}

	if r.CompactionSummarizer != nil {
		summary, err := r.CompactionSummarizer.Summarize(ctx, history, compaction.TemplateV1)
		if err == nil {
			if lines, buildErr := compaction.BuildLines(summary, string(stage), r.now()); buildErr == nil {
				outputRef := ""
				if r.CompactionDir != "" {
					if path, writeErr := compaction.WriteJSONL(r.CompactionDir, task.TaskID, string(stage), lines); writeErr == nil {
						outputRef = path
					}
				}
				compacted := compaction.RenderLinesAsMessage(lines).Content
				payload := map[string]any{
					"trigger_reason": decision.Reason,
					"tokens_before":  decision.EstimatedTokens,
					"tokens_after":   len(compacted) / 2,
					"ratio":          0.5,
					"degraded":       false,
					"strategy_id":    string(compaction.TemplateV1),
					"prompt_version": string(compaction.TemplateV1),
				}
				if outputRef != "" {
					payload["output_ref"] = outputRef
				}
				r.Metrics.Compaction(decision.Reason, false)
				r.Metrics.CompactionRatio(string(compaction.TemplateV1), 0.5)
				r.publishCompaction(ctx, in, stage, attempt, event.CompactionCompleted, payload)
				return compacted
			}
		}
	}

	degraded, degradeErr := compaction.DegradeL1([]compaction.ChatMessage{{Role: "user", Content: message}}, history, r.compactionLimits())
	if degradeErr == nil {
		payload := map[string]any{
			"trigger_reason": decision.Reason,
			"tokens_before":  degraded.TokensBefore,
			"tokens_after":   degraded.TokensAfter,
			"ratio":          degraded.Ratio,
			"degraded":       true,
			"degrade_level":  degraded.Level,
			"reason":         degraded.Reason,
		}
		r.Metrics.Compaction(decision.Reason, true)
		r.Metrics.CompactionRatio("degrade_l1", degraded.Ratio)
		r.publishCompaction(ctx, in, stage, attempt, event.CompactionDegraded, payload)
		return compaction.RenderLinesAsMessage(degraded.Lines).Content
	}
	return message
}

func (r *Runtime) publishCompaction(ctx context.Context, in RunInput, stage fsm.Stage, attempt fsm.Attempt, eventType event.EventType, payload map[string]any) {
	if r.Events == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = r.Events.Publish(ctx, &event.Event{
		EventID:       fmt.Sprintf("%s-%s-%d", in.TaskID, eventType, time.Now().UnixNano()),
		EventType:     eventType,
		SchemaVersion: event.CurrentSchemaVersion,
		TaskID:        in.TaskID,
		Stage:         string(stage),
		Attempt:       attempt.AttemptNo,
		Timestamp:     r.now().UTC(),
		Severity:      event.SeverityInfo,
		TenantID:      in.TenantID,
		ProjectID:     in.ProjectID,
		Payload:       raw,
	})
}

func (r *Runtime) globalMemoryPath() string {
	home := r.HarnessHome
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".harness")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "memory", "global.md")
}

func formatInjectedMessage(injected *InjectResult, base string) string {
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n[injected memory]\n")
	for rank, item := range injected.Items {
		b.WriteString(fmt.Sprintf("[topic=%s file=%s updated=%s score=%.3f rank=%d]\n",
			item.TopicKey, item.SourceFile, item.UpdatedAt.UTC().Format(time.RFC3339), item.Score, rank+1))
		b.WriteString(item.Body)
		b.WriteString("\n")
	}
	return b.String()
}

func (r *Runtime) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func rollbackCategory(v acceptance.Verdict) retry.Category {
	for _, f := range v.FailedCriteria {
		switch f.Category {
		case "plan_gap", "requirement_gap":
			return retry.System
		}
	}
	return retry.Runtime
}

// planStagePlanExample is the sample DAG embedded in the b-stage instruction.
//
// 示例必须是 3 节点、带真实依赖边与并行集：单节点示例会被模型当成合格答案照抄，
// 而单节点计划与 singleNodePlan 降级无法区分，验收侧只能算降级、不能算拆成。
// 示例本身必须能通过 orchestrator.Plan.Validate（单测 TestPlanStageExampleIsValidPlan 守着）。
const planStagePlanExample = `{
  "dag_id": "dag-1",
  "nodes": [
    {"node_id": "n1", "role": "coder", "input_contract": {"type": "workspace", "paths": ["."]}, "output_contract": {"type": "files", "paths": ["cmd/server/healthz.go"]}, "acceptance": [{"type": "file_exists", "expr": "cmd/server/healthz.go"}]},
    {"node_id": "n2", "role": "coder", "input_contract": {"type": "file_set", "paths": ["cmd/server/healthz.go"]}, "output_contract": {"type": "files", "paths": ["cmd/server/healthz_test.go"]}, "acceptance": [{"type": "file_exists", "expr": "cmd/server/healthz_test.go"}]},
    {"node_id": "n3", "role": "writer", "input_contract": {"type": "file_set", "paths": ["cmd/server/healthz.go"]}, "output_contract": {"type": "files", "paths": ["docs/healthz.md"]}, "acceptance": [{"type": "file_exists", "expr": "docs/healthz.md"}]}
  ],
  "edges": [{"from": "n1", "to": "n2"}, {"from": "n1", "to": "n3"}],
  "parallel_sets": [{"set_id": "p1", "nodes": ["n2", "n3"], "failure_strategy": "tolerate", "tolerate_ratio": 0.5}]
}`

// planStageInstruction asks the b-stage agent for the SubtaskPlan/v1 artifact.
// 没有这段指令时 agent 只回普通总结，PlanBStage 因 rawPlan 无法解析而静默跳过，
// 结果就是「b 阶段 accepted 但 subtasks=0」。
//
// 指令只接受 3 节点示例 + 拆分判据：b 阶段的目标是把目标拆成可独立验收的多个子任务，
// 一个节点就交付全部内容的计划等于没拆，只能算降级（fallback）而不算达标。
const planStageInstruction = `你是规划阶段（b）。你的唯一产出是一个 SubtaskPlan/v1 JSON：不要解释文字、不要总结、不要用 Markdown 代码块包裹。

【先判断怎么拆】按下面的判据决定拆成几个节点，能拆就必须拆到 3 个及以上：

必须拆的情况：
1. 目标包含 2 个以上彼此独立、可分别验收的交付物（不同文件/模块/端点）；
2. 目标同时包含「实现」和「验证」两类工作（写代码 / 写测试 / 写文档），验证类工作和实现类工作必须各自成节点；
3. 目标里有可并行推进的独立单元（不同目录、不同接口），且每个单元能单独声明自己的验收。

不许拆的情况：
1. 改动集中在单个具体文件、且只有一条确定性验收（tests_pass / build_pass）——这是原子修改；
2. 节点带跨文件一致性约束（consistency_group）或人工审批点；
3. 拆出来的子任务无法各自写清 input_contract / output_contract / acceptance——拆了也验不了。

节点数量：1 到 20；能拆就 >= 3 个。宁少拆、不错拆；宁可 3 个节点各自验收清楚，也不要 6 个节点互相踩文件。

【示例】目标「新增 /healthz 端点、补单测、写运行文档」的合格产出是 3 节点 + 2 条依赖边 + 1 个并行集：

` + planStagePlanExample + `

【输出顺序】先把上面的 JSON 本体原样输出（不要放进代码块、不要加注释），再调用 stage_finish；summary 只写一句话说明拆成了几个节点，不要在里面重复 JSON。

【硬约束】
- 每个节点必须有 role、input_contract（type + paths）、output_contract（type + paths）、acceptance（至少 1 条）。
- 节点间的先后关系只能用 edges 表达（from 为前置、to 为后继），DAG 不得成环。
- 嵌套最多一层（children 之下不能再有 children），节点总数含 children 不超过 20。
- 每个 parallel_set 内的节点之间不得存在依赖路径，且必须声明 failure_strategy（fail-fast|tolerate|retry）；用 tolerate 时 tolerate_ratio 必须落在 (0,0.5]。
- 多节点计划必须给出 edges 或 parallel_sets，否则整份计划按不完整驳回。
- acceptance.type 用 file_exists | tests_pass | build_pass，expr 写实际可执行的路径或命令。`

// b 阶段的模型绑定。pkg/config/harness_loop/models.yaml 里 stage_overrides.b
// 声明同一个模型（hot reload 可改，改完无需重启）；这里的常量是模型策略门关掉
// 时的兜底，两条路径必须指同一个模型，否则同一份提示词会在两个模型上跑出两种行为。
const (
	planStageModel       = "qwen3.7-max"
	planStageModelWindow = 32768
)

// stageModelPin reports the model a stage must use regardless of the global
// stage_model. Only the b stage is pinned today: it is the only stage whose
// output is a machine-readable DAG, and a weak default silently degrades to the
// single-node fallback.
func stageModelPin(stage fsm.Stage) (model string, window int, ok bool) {
	if stage != fsm.StagePlan {
		return "", 0, false
	}
	return planStageModel, planStageModelWindow, true
}

func stageMessage(stage fsm.Stage, attemptNo int) string {
	base := fmt.Sprintf("stage=%s attempt=%d", stage, attemptNo)
	if stage == fsm.StagePlan {
		return base + "\n\n" + planStageInstruction
	}
	return base
}

// stageGoal appends the b-stage plan instruction to the goal as well: the agent
// engine injects Goal into the system prompt, and system-level guidance is what
// the model actually follows (user message alone is often ignored).
func stageGoal(stage fsm.Stage, goal string) string {
	if stage != fsm.StagePlan {
		return goal
	}
	return goal + "\n\n" + planStageInstruction
}

func defaultRollbackTarget(stage fsm.Stage) string {
	switch stage {
	case fsm.StageRequirement:
		return "a"
	case fsm.StagePlan:
		return "a"
	case fsm.StageExecute:
		return "b"
	case fsm.StageVerify:
		return "c"
	}
	return "a"
}

type llmJudge struct {
	client llm.Client
}

func (j llmJudge) Judge(ctx context.Context, req acceptance.JudgeRequest) (acceptance.JudgeResponse, error) {
	resp, err := j.client.Chat(ctx, &llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "system", Content: req.SystemPrompt},
			{Role: "user", Content: req.Prompt},
		},
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	})
	if err != nil {
		return acceptance.JudgeResponse{}, err
	}
	tokensOut := resp.TotalTokens - resp.PromptTokens
	if tokensOut < 0 {
		tokensOut = 0
	}
	return acceptance.JudgeResponse{
		Content:   resp.Content,
		TokensIn:  resp.PromptTokens,
		TokensOut: tokensOut,
	}, nil
}

// newStageVerifierFromEnv wires the LLM judge when the same environment
// variables used by the compaction summarizer are present. With no base URL the
// model channel stays unavailable and fails closed; undeclared criteria keep
// using the historical program path.
func newStageVerifierFromEnv() acceptance.StageVerifier {
	baseURL := strings.TrimSpace(os.Getenv("HARNESSLOOP_LLM_BASE_URL"))
	model := strings.TrimSpace(os.Getenv("HARNESSLOOP_LLM_MODEL"))
	if baseURL == "" {
		return acceptance.NewChannelStageVerifier(nil, model)
	}
	client, err := llm.NewClient(llm.Config{
		Provider: "openai",
		APIKey:   os.Getenv("HARNESSLOOP_LLM_API_KEY"),
		BaseURL:  baseURL,
		Model:    model,
	})
	if err != nil {
		return acceptance.NewChannelStageVerifier(nil, model)
	}
	return acceptance.NewChannelStageVerifier(llmJudge{client: client}, model)
}
