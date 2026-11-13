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
	"agent-platform/pkg/llm"
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
			Message:          fmt.Sprintf("stage=%s attempt=%d", stage, attemptNo),
			Goal:             in.Goal,
			SuccessCriteria:  "",
			PolicySnapshot:   in.PolicySnapshot,
			PolicyHash:       in.PolicyHash,
			StageModel:       in.StageModel,
			ModelWindow:      in.ModelWindow,
			RequiresToolCall: in.RequiresToolCall,
			Timeout:          in.StageTimeout,
		}
		if r.Models != nil && r.Models.Enabled() {
			choice, modelErr := r.Models.SelectForStage(ctx, ModelStageRequest{
				ModelStageContext: ModelStageContext{
					TenantID: in.TenantID, ProjectID: in.ProjectID, TaskID: in.TaskID,
					Stage: string(stage), AttemptNo: attemptNo,
				},
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
		stageRes, stageErr := r.Stages.Run(ctx, stageInput)
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
		aggregation := r.runOrchestration(ctx, in, task.WorkspaceRoot, stage, attempt, stageRes)

		verdict := r.evaluate(ctx, in, task, aggregation, stage, attempt, stageRes)
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
				return r.finalize(ctx, in, result, ReasonCompleted, target, "", "", 0, "", task.Version)
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
		r.publishTerminal(ctx, tenantID, task.ProjectID, taskID, ReasonAborted)
	}
	r.aborts[key] = res
	return res, nil
}

func (r *Runtime) isAborted(taskID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aborted[taskID]
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
	r.publishTerminal(ctx, in.TenantID, in.ProjectID, in.TaskID, reason)

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
// It is best-effort: a plan that cannot be parsed or a missing executor never
// aborts the parent stage, preserving the pre-flag behaviour.
func (r *Runtime) runOrchestration(ctx context.Context, in RunInput, workspaceRoot string, stage fsm.Stage, attempt fsm.Attempt, res *StageRunResult) *orchestrator.AggregationReport {
	if r.Orchestrator == nil || !r.Orchestrator.Enabled() || res == nil {
		return nil
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
		raw := []byte(res.Content)
		if len(raw) == 0 {
			raw = []byte(res.FinishArgs.Summary)
		}
		_, _ = r.Orchestrator.PlanBStage(runCtx, ec, raw)
	case fsm.StageExecute:
		_, _ = r.Orchestrator.DispatchCStage(runCtx, ec)
	case fsm.StageVerify:
		report, _ := r.Orchestrator.AggregateDStage(runCtx, ec)
		return report
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
	_ = r.Checkpoints.Save(ctx, cp)
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

func (r *Runtime) publishTerminal(ctx context.Context, tenantID, projectID, taskID, reason string) {
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
	case ReasonAborted:
		typ = event.TaskAborted
		severity = event.SeverityWarn
	}
	payload := []byte(fmt.Sprintf(`{"terminal_reason":%q}`, reason))
	_ = r.Events.Publish(ctx, &event.Event{
		EventID:       fmt.Sprintf("%s-%s", taskID, reason),
		EventType:     typ,
		SchemaVersion: event.CurrentSchemaVersion,
		TaskID:        taskID,
		Timestamp:     r.now().UTC(),
		Severity:      severity,
		TenantID:      tenantID,
		ProjectID:     projectID,
		Payload:       payload,
	})
}

func (r *Runtime) publishError(ctx context.Context, in RunInput, stage fsm.Stage, attempt fsm.Attempt, category retry.Category, reason string) {
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
