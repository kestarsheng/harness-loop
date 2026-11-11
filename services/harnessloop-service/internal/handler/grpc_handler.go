package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"agent-platform/pkg/harnessloop/checkpoint"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/policy"
	harnesspb "agent-platform/pkg/pb/harnessloop"
	"agent-platform/services/harnessloop-service/internal/config"
	"agent-platform/services/harnessloop-service/internal/repository"
	"agent-platform/services/harnessloop-service/internal/service"
)

type Handler struct {
	harnesspb.UnimplementedHarnessLoopServiceServer

	cfg         config.Config
	tasks       *repository.TaskRepo
	attempts    *repository.AttemptRepo
	events      *repository.EventRepo
	checkpoints *repository.CheckpointRepo
	acceptances *repository.AcceptanceRepo
	runtime     *service.Runtime
	policy      *policy.Engine
	models      *modelregistry.Registry
	publisher   *event.Publisher
	flags       *config.FlagSet

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

type Deps struct {
	Config      config.Config
	Tasks       *repository.TaskRepo
	Attempts    *repository.AttemptRepo
	Events      *repository.EventRepo
	Checkpoints *repository.CheckpointRepo
	Acceptances *repository.AcceptanceRepo
	Runtime     *service.Runtime
	Policy      *policy.Engine
	Models      *modelregistry.Registry
	Publisher   *event.Publisher
	Flags       *config.FlagSet
}

func New(d Deps) *Handler {
	return &Handler{
		cfg:         d.Config,
		tasks:       d.Tasks,
		attempts:    d.Attempts,
		events:      d.Events,
		checkpoints: d.Checkpoints,
		acceptances: d.Acceptances,
		runtime:     d.Runtime,
		policy:      d.Policy,
		models:      d.Models,
		publisher:   d.Publisher,
		flags:       d.Flags,
		running:     make(map[string]context.CancelFunc),
	}
}

func tenantFrom(ctx context.Context, fallback string) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-tenant-id"); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	if fallback != "" {
		return fallback
	}
	return "default"
}

func newID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf)
}

func (h *Handler) CreateTask(ctx context.Context, req *harnesspb.CreateTaskRequest) (*harnesspb.CreateTaskResponse, error) {
	if req.GetGoal() == "" || req.GetWorkspaceRoot() == "" {
		return nil, status.Error(codes.InvalidArgument, "goal and workspace_root are required")
	}
	tenantID := tenantFrom(ctx, req.GetTenantId())
	fsmEnabled := h.fsmEnabled(config.FlagContext{
		TenantID:  tenantID,
		ProjectID: req.GetProjectId(),
		TaskType:  "long_running",
	})
	if !fsmEnabled {
		return nil, status.Error(codes.FailedPrecondition, "E_FSM_DISABLED: harness loop is disabled")
	}
	taskID := newID("task")

	snapshot, err := json.Marshal(map[string]any{
		"criteria": req.GetStageCriteria(),
		"budget":   req.GetBudget(),
		"mode":     "enforce",
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "cannot encode policy snapshot")
	}

	run := &repository.TaskRun{
		TaskID:         taskID,
		TenantID:       tenantID,
		ProjectID:      firstNonEmpty(req.GetProjectId(), "default"),
		WorkspaceRoot:  req.GetWorkspaceRoot(),
		Goal:           req.GetGoal(),
		GoalHash:       hashGoal(req.GetGoal()),
		CurrentStage:   fsm.StageRequirement,
		Status:         repository.TaskRunning,
		AttemptNo:      map[string]int{},
		RetryCount:     map[string]int{},
		AcceptanceFail: map[string]int{},
		BudgetTokens:   req.GetBudget().GetTokens(),
		BudgetCost:     req.GetBudget().GetCost(),
		FSMEnabled:     fsmEnabled,
		PolicySnapshot: snapshot,
	}
	if err := h.tasks.Create(ctx, run); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	view, err := h.tasks.Get(ctx, tenantID, taskID)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if h.cfg.Harness.AutoStart {
		h.startTask(view.TenantID, view.TaskID)
	}
	return &harnesspb.CreateTaskResponse{Task: toProtoTask(view)}, nil
}

func (h *Handler) fsmEnabled(fc config.FlagContext) bool {
	if h.flags != nil {
		return h.flags.EvaluateFSM(fc)
	}
	return h.cfg.Harness.Enabled
}

func hashGoal(goal string) string {
	return fmt.Sprintf("sha256:%x", len(goal))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (h *Handler) startTask(tenantID, taskID string) {
	if h.runtime == nil {
		log.Printf("harnessloop: runtime is not configured, task %s will not auto-start", taskID)
		return
	}
	h.mu.Lock()
	if _, exists := h.running[taskID]; exists {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.running[taskID] = cancel
	h.mu.Unlock()

	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.running, taskID)
			h.mu.Unlock()
			cancel()
		}()
		_, _ = h.runtime.Run(ctx, h.runInput(tenantID, taskID))
	}()
}

func (h *Handler) runInput(tenantID, taskID string) service.RunInput {
	window := 0
	if model, ok := h.models.Get(h.cfg.Agent.StageModel); ok {
		window = model.ContextWindow
	}
	projectID := ""
	goal := ""
	goalHash := ""
	if task, err := h.tasks.Get(context.Background(), tenantID, taskID); err == nil {
		projectID = task.ProjectID
		goal = task.Goal
		goalHash = task.GoalHash
	}
	return service.RunInput{
		TenantID:         tenantID,
		ProjectID:        projectID,
		TaskID:           taskID,
		Goal:             goal,
		GoalHash:         goalHash,
		EntryAgent:       h.cfg.Agent.EntryAgent,
		StageModel:       h.cfg.Agent.StageModel,
		ModelWindow:      window,
		PolicyHash:       "sha256:snapshot",
		RequiresToolCall: true,
		StageTimeout:     time.Duration(h.cfg.StageTimeout()) * time.Second,
		MaxSteps:         h.cfg.Harness.MaxSteps,
	}
}

func (h *Handler) GetTask(ctx context.Context, req *harnesspb.GetTaskRequest) (*harnesspb.TaskView, error) {
	t, err := h.tasks.Get(ctx, tenantFrom(ctx, req.GetTenantId()), req.GetTaskId())
	if err != nil {
		return nil, mapRepoError(err)
	}
	return toProtoTask(t), nil
}

func (h *Handler) ListTasks(ctx context.Context, req *harnesspb.ListTasksRequest) (*harnesspb.ListTasksResponse, error) {
	tasks, err := h.tasks.List(ctx, tenantFrom(ctx, req.GetTenantId()), req.GetProjectId(), req.GetStatus(), int(req.GetLimit()))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &harnesspb.ListTasksResponse{}
	for _, t := range tasks {
		resp.Tasks = append(resp.Tasks, toProtoTask(t))
	}
	return resp, nil
}

func (h *Handler) PauseTask(ctx context.Context, req *harnesspb.TaskCommand) (*harnesspb.TaskView, error) {
	tenantID := tenantFrom(ctx, req.GetTenantId())
	t, err := h.tasks.Get(ctx, tenantID, req.GetTaskId())
	if err != nil {
		return nil, mapRepoError(err)
	}
	h.cancelRunning(req.GetTaskId())
	updated, err := h.tasks.UpdateStatus(ctx, tenantID, req.GetTaskId(), repository.TaskPaused, "", t.Version)
	if err != nil {
		return nil, mapRepoError(err)
	}
	return toProtoTask(updated), nil
}

func (h *Handler) ResumeTask(ctx context.Context, req *harnesspb.ResumeTaskRequest) (*harnesspb.TaskView, error) {
	tenantID := tenantFrom(ctx, req.GetTenantId())
	t, err := h.tasks.Get(ctx, tenantID, req.GetTaskId())
	if err != nil {
		return nil, mapRepoError(err)
	}
	if t.Status == repository.TaskCompleted || t.Status == repository.TaskAborted || t.Status == repository.TaskFailed {
		return nil, status.Error(codes.FailedPrecondition, "task already reached a terminal state")
	}
	updated, err := h.tasks.UpdateStatus(ctx, tenantID, req.GetTaskId(), repository.TaskRunning, "", t.Version)
	if err != nil {
		return nil, mapRepoError(err)
	}
	h.startTask(tenantID, req.GetTaskId())
	return toProtoTask(updated), nil
}

func (h *Handler) AbortTask(ctx context.Context, req *harnesspb.TaskCommand) (*harnesspb.TaskView, error) {
	tenantID := tenantFrom(ctx, req.GetTenantId())
	h.cancelRunning(req.GetTaskId())
	if _, err := h.runtime.Abort(ctx, tenantID, req.GetTaskId(), req.GetAbortEpoch()); err != nil {
		return nil, mapRepoError(err)
	}
	t, err := h.tasks.Get(ctx, tenantID, req.GetTaskId())
	if err != nil {
		return nil, mapRepoError(err)
	}
	return toProtoTask(t), nil
}

func (h *Handler) cancelRunning(taskID string) {
	h.mu.Lock()
	cancel, ok := h.running[taskID]
	h.mu.Unlock()
	if ok {
		cancel()
	}
}

func (h *Handler) ListEvents(ctx context.Context, req *harnesspb.ListEventsRequest) (*harnesspb.ListEventsResponse, error) {
	events, err := h.events.List(ctx, repository.EventFilter{
		TenantID:  tenantFrom(ctx, req.GetTenantId()),
		TaskID:    req.GetTaskId(),
		Stage:     req.GetStage(),
		EventType: req.GetEventType(),
		Severity:  req.GetSeverity(),
		SinceSeq:  req.GetSinceSeq(),
		Limit:     int(req.GetLimit()),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &harnesspb.ListEventsResponse{}
	for _, e := range events {
		resp.Events = append(resp.Events, toProtoEvent(e))
		if e.Seq > resp.NextSinceSeq {
			resp.NextSinceSeq = e.Seq
		}
	}
	if len(events) == 0 {
		resp.NextSinceSeq = req.GetSinceSeq()
	}
	return resp, nil
}

func (h *Handler) WatchEvents(req *harnesspb.WatchEventsRequest, stream harnesspb.HarnessLoopService_WatchEventsServer) error {
	ctx := stream.Context()
	tenantID := tenantFrom(ctx, req.GetTenantId())
	since := req.GetSinceSeq()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			events, next, err := h.events.WatchFrom(ctx, tenantID, req.GetTaskIds(), since, 200)
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			for _, e := range events {
				if err := stream.Send(toProtoEvent(e)); err != nil {
					return err
				}
			}
			if next > since {
				since = next
			}
		}
	}
}

func (h *Handler) ListCheckpoints(ctx context.Context, req *harnesspb.ListCheckpointsRequest) (*harnesspb.ListCheckpointsResponse, error) {
	items, err := h.checkpoints.List(ctx, req.GetTaskId(), int(req.GetLimit()))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &harnesspb.ListCheckpointsResponse{}
	for _, cp := range items {
		resp.Checkpoints = append(resp.Checkpoints, toProtoCheckpoint(cp))
	}
	return resp, nil
}

func (h *Handler) RestoreFromCheckpoint(ctx context.Context, req *harnesspb.RestoreRequest) (*harnesspb.TaskView, error) {
	tenantID := tenantFrom(ctx, req.GetTenantId())
	t, err := h.tasks.Get(ctx, tenantID, req.GetTaskId())
	if err != nil {
		return nil, mapRepoError(err)
	}
	maxSeq, err := h.events.NextSeq(ctx, tenantID, req.GetTaskId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	restorer := checkpoint.NewRestorer(h.checkpoints)
	res, err := restorer.Restore(ctx, req.GetTaskId(), req.GetCheckpointId(), checkpoint.TaskSnapshot{
		Stage:       string(t.CurrentStage),
		AttemptNo:   t.AttemptNo[string(t.CurrentStage)],
		Consumed:    checkpoint.Consumed{Tokens: t.ConsumedTokens, Cost: t.ConsumedCost},
		EventMaxSeq: maxSeq,
	}, checkpoint.RestoreOptions{Workspace: t.WorkspaceRoot})
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if res.Drift != nil && res.Drift.Drifted {
		return nil, status.Errorf(codes.FailedPrecondition, "checkpoint drift detected: action=%s", res.Drift.Action)
	}
	return toProtoTask(t), nil
}

func (h *Handler) ListModels(ctx context.Context, req *harnesspb.ListModelsRequest) (*harnesspb.ListModelsResponse, error) {
	resp := &harnesspb.ListModelsResponse{RegistryVersion: fmt.Sprintf("%d", h.models.Version)}
	for _, v := range h.models.Views() {
		resp.Models = append(resp.Models, &harnesspb.ModelEntry{
			Id:               v.ID,
			Provider:         v.Provider,
			ContextWindow:    int32(v.ContextWindow),
			MaxOutputTokens:  int32(v.MaxOutputTokens),
			ToolCall:         v.ToolCall,
			StructuredOutput: v.StructuredOutput,
			PriceInPer_1K:    v.PriceInPer1K,
			PriceOutPer_1K:   v.PriceOutPer1K,
			Status:           v.Status,
			Tags:             v.Tags,
		})
	}
	return resp, nil
}

func (h *Handler) ResolveModel(ctx context.Context, req *harnesspb.ResolveModelRequest) (*harnesspb.ResolveModelResponse, error) {
	res, err := h.models.Resolve(modelregistry.ResolveRequest{
		Stage:                 req.GetStage(),
		Strategy:              req.GetStrategy(),
		RequireToolCall:       req.GetRequireToolCall(),
		RequiredContextWindow: int(req.GetRequiredContextWindow()),
	})
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	m := res.Model
	return &harnesspb.ResolveModelResponse{
		Strategy:   res.Strategy,
		Candidates: []*harnesspb.ModelEntry{toModelEntry(m.View())},
	}, nil
}

func (h *Handler) PreflightTool(ctx context.Context, req *harnesspb.PreflightToolRequest) (*harnesspb.ToolDecision, error) {
	tenantID := tenantFrom(ctx, req.GetTenantId())
	root := ""
	if req.GetTaskId() != "" {
		if t, err := h.tasks.Get(ctx, tenantID, req.GetTaskId()); err == nil {
			root = t.WorkspaceRoot
		}
	}
	d := h.policy.Decide(ctx, policy.Request{
		Tool:          req.GetToolName(),
		Stage:         req.GetStage(),
		WorkspaceRoot: root,
		Args: policy.ToolArgs{
			Paths:   req.GetPathArgs(),
			Command: req.GetCommandArgs(),
		},
	})
	return &harnesspb.ToolDecision{
		Allowed:          d.Allowed,
		ReasonCode:       d.Code,
		RuleId:           d.RuleID,
		Layer:            string(d.Layer),
		RequiresApproval: false,
		Detail:           d.Reason,
	}, nil
}

func mapRepoError(err error) error {
	switch {
	case err == nil:
		return nil
	case isNotFound(err):
		return status.Error(codes.NotFound, err.Error())
	case isConflict(err):
		return status.Error(codes.Aborted, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
