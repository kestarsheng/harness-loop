package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/acceptance"
	"agent-platform/pkg/harnessloop/checkpoint"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/retry"
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
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 200
	}

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
		attempt, err := fsm.NewAttempt(task.TaskID, stage, attemptNo, entryReasonFor(step, result), r.now())
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

		r.saveCheckpoint(ctx, in, task, attempt, checkpoint.KindStageEntry, result)

		stageRes, stageErr := r.Stages.Run(ctx, StageRunInput{
			TaskID:           in.TaskID,
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
		})
		result.Steps++
		if stageRes == nil {
			stageRes = &StageRunResult{}
		}

		if stageErr != nil || stageRes.Decision.Action != retry.ActionDone {
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

		verdict := r.evaluate(ctx, in, stage, attempt, stageRes)
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

func (r *Runtime) evaluate(ctx context.Context, in RunInput, stage fsm.Stage, attempt fsm.Attempt, out *StageRunResult) acceptance.Verdict {
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
		Seq:       r.now().UnixNano(),
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

func (r *Runtime) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func entryReasonFor(step int, res *RunResult) fsm.EntryReason {
	if step == 0 {
		return fsm.EntryInitial
	}
	if res != nil && res.Category != "" {
		return fsm.EntryRetry
	}
	return fsm.EntryAcceptanceFailed
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
