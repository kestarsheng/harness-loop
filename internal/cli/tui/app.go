package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/metadata"

	harnesspb "agent-platform/pkg/pb/harnessloop"
)

type RunOptions struct {
	NoColor      bool
	WebURL       string
	Hyperlink    bool
	OpenURL      func(string) error
	KB           KBSource
	KBTopK       int
	InitialView  string
	InitialQuery string
}

type RunResult struct {
	State     TaskState
	QuitEarly bool
}

// TaskFactory creates one task for the resident session.
type TaskFactory func(ctx context.Context, goal string) (*harnesspb.TaskView, error)

// ChatChunk is one piece of a direct-chat reply streamed from agent-service.
// Type mirrors the gRPC chunk type ("token", "final", "error", …) so the model
// can tell a partial token from the authoritative final text and from a
// failure that must stay visible in the conversation.
type ChatChunk struct {
	Type    string
	Content string
	Agent   string
	// Thinking carries reasoning text from a "think" chunk.
	Thinking string
	// ToolName/ToolArgs describe a "tool_start" call; ToolResult is filled on
	// "tool_result" and doubles as the success/failure signal.
	ToolName   string
	ToolArgs   string
	ToolResult string
	// Usage is the cumulative token usage reported on the final step.
	Usage int64
}

// ChatFunc sends one user message to agent-service and streams the reply back.
// The channel is closed when the stream ends; an error chunk (or a non-nil
// error here) is rendered in the conversation rather than swallowed.
type ChatFunc func(ctx context.Context, message string) (<-chan ChatChunk, error)

// SessionConfig turns the TUI into a resident session: each submitted goal
// creates a task, terminal tasks return to the input box, and typed commands
// (for example exit) are dispatched through DispatchSessionLine.
type SessionConfig struct {
	// ExitOnComplete restores the legacy "quit when the task completes"
	// behaviour instead of returning to the input box.
	ExitOnComplete bool
	// InitialGoal is submitted immediately on start; empty starts at the input
	// box. It always creates a task (the --goal one-shot a~e path).
	InitialGoal string
	CreateTask  TaskFactory
	// Chat answers a plain line without creating a task. Nil means direct chat
	// is unavailable and the TUI says so instead of falling back to a~e.
	Chat ChatFunc
	// SourceFor builds the monitor source for a newly created task.
	SourceFor func(taskID string) Source
}

// SessionResult reports what happened during one resident session.
type SessionResult struct {
	TaskIDs   []string
	LastState TaskState
	HasLast   bool
	Exited    bool
}

func Run(ctx context.Context, client harnesspb.HarnessLoopServiceClient, tenant string, task *harnesspb.TaskView, opts RunOptions) (RunResult, error) {
	if client == nil {
		return RunResult{}, fmt.Errorf("tui: harnessloop client is required")
	}
	if task == nil || task.GetTaskId() == "" {
		return RunResult{}, fmt.Errorf("tui: task view is required")
	}
	src := &grpcSource{client: client, tenant: tenant, taskID: task.GetTaskId(), ctx: ctx}
	model := NewModel(taskStateFromProto(task), src, modelOptions(opts)).WithContext(ctx)
	program := tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithFPS(10),
		tea.WithAltScreen(),
	)
	if _, err := program.Run(); err != nil {
		return RunResult{}, err
	}
	return RunResult{State: model.state, QuitEarly: !model.state.terminal()}, nil
}

// RunSession runs the resident multi-task session until the user types exit,
// the context is cancelled, or (with ExitOnComplete) the current task finishes.
func RunSession(ctx context.Context, client harnesspb.HarnessLoopServiceClient, tenant string, opts RunOptions, cfg SessionConfig) (SessionResult, error) {
	if client == nil {
		return SessionResult{}, fmt.Errorf("tui: harnessloop client is required")
	}
	if cfg.CreateTask == nil {
		return SessionResult{}, fmt.Errorf("tui: session task factory is required")
	}
	if cfg.SourceFor == nil {
		cfg.SourceFor = func(taskID string) Source {
			return &grpcSource{client: client, tenant: tenant, taskID: taskID, ctx: ctx}
		}
	}
	options := modelOptions(opts)
	options.Session = &cfg
	model := NewModel(TaskState{}, nil, options).WithContext(ctx)
	program := tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithFPS(10),
		tea.WithAltScreen(),
	)
	if _, err := program.Run(); err != nil {
		return SessionResult{}, err
	}
	result := SessionResult{
		TaskIDs: append([]string(nil), model.taskIDs...),
		Exited:  model.exitRequested,
	}
	if model.state.TaskID != "" {
		result.LastState = model.state
		result.HasLast = true
	}
	return result, nil
}

func modelOptions(opts RunOptions) Options {
	return Options{
		NoColor:      opts.NoColor,
		WebURL:       opts.WebURL,
		Hyperlink:    opts.Hyperlink,
		OpenURL:      opts.OpenURL,
		KB:           opts.KB,
		KBTopK:       opts.KBTopK,
		InitialView:  opts.InitialView,
		InitialQuery: opts.InitialQuery,
	}
}

type grpcSource struct {
	client harnesspb.HarnessLoopServiceClient
	tenant string
	taskID string
	ctx    context.Context
}

func (s *grpcSource) baseContext() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *grpcSource) callContext(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-tenant-id", s.tenant)
}

func (s *grpcSource) Task(ctx context.Context) (TaskState, error) {
	view, err := s.client.GetTask(s.callContext(ctx), &harnesspb.GetTaskRequest{
		TenantId: s.tenant, TaskId: s.taskID,
	})
	if err != nil {
		return TaskState{}, err
	}
	return taskStateFromProto(view), nil
}

func (s *grpcSource) Abort(ctx context.Context) error {
	_, err := s.client.AbortTask(s.callContext(ctx), &harnesspb.TaskCommand{
		TenantId: s.tenant, TaskId: s.taskID, Reason: "aborted from tui", AbortEpoch: time.Now().Unix(),
	})
	return err
}

func (s *grpcSource) ListEvents(ctx context.Context, sinceSeq int64) ([]Event, int64, error) {
	const pageSize = 200
	events := make([]Event, 0, pageSize)
	since := sinceSeq
	for page := 0; page < 50; page++ {
		resp, err := s.client.ListEvents(s.callContext(ctx), &harnesspb.ListEventsRequest{
			TenantId: s.tenant, TaskId: s.taskID, SinceSeq: since, Limit: pageSize,
		})
		if err != nil {
			return nil, sinceSeq, err
		}
		for _, item := range resp.GetEvents() {
			events = append(events, eventFromProto(item))
		}
		if resp.GetNextSinceSeq() <= since {
			break
		}
		since = resp.GetNextSinceSeq()
		if len(resp.GetEvents()) < pageSize {
			break
		}
	}
	return events, since, nil
}

func (s *grpcSource) Watch(ctx context.Context, sinceSeq int64) (<-chan WatchEvent, error) {
	base := ctx
	if base == nil {
		base = s.baseContext()
	}
	streamCtx, cancel := context.WithCancel(base)
	stream, err := s.client.WatchEvents(s.callContext(streamCtx), &harnesspb.WatchEventsRequest{
		TenantId: s.tenant, SinceSeq: sinceSeq, TaskIds: []string{s.taskID},
	})
	if err != nil {
		cancel()
		return nil, err
	}
	ch := make(chan WatchEvent, 64)
	go func() {
		defer close(ch)
		defer cancel()
		for {
			item, err := stream.Recv()
			if err != nil {
				select {
				case ch <- WatchEvent{Err: err}:
				case <-streamCtx.Done():
				}
				return
			}
			select {
			case ch <- WatchEvent{Event: eventFromProto(item)}:
			case <-streamCtx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (s *grpcSource) Subtasks(ctx context.Context) ([]Subtask, error) {
	resp, err := s.client.ListSubtasks(s.callContext(ctx), &harnesspb.ListSubtasksRequest{
		TenantId: s.tenant, TaskId: s.taskID, Limit: 200,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Subtask, 0, len(resp.GetSubtasks()))
	for _, item := range resp.GetSubtasks() {
		out = append(out, Subtask{
			ID:          item.GetSubtaskId(),
			ParentID:    item.GetParentTaskId(),
			DAGID:       item.GetDagId(),
			NodeID:      item.GetDagNodeId(),
			ParallelSet: item.GetParallelSetId(),
			Role:        item.GetRole(),
			Status:      item.GetStatus(),
			Depth:       int(item.GetDepth()),
			Index:       int(item.GetIndexInParent()),
			Attempt:     int(item.GetAttemptNo()),
			DependsOn:   item.GetDependsOn(),
			HandoffID:   item.GetHandoffId(),
			AllocTokens: item.GetAllocTokens(),
			Tokens:      item.GetConsumedTokens(),
			Cost:        item.GetConsumedCost(),
			Artifacts:   parseArtifactRefs(item.GetArtifactRefs()),
			CreatedAt:   time.UnixMilli(item.GetCreatedAtMs()),
			UpdatedAt:   time.UnixMilli(item.GetUpdatedAtMs()),
		})
	}
	return out, nil
}

func (s *grpcSource) ModelSelections(ctx context.Context) ([]ModelSelection, error) {
	resp, err := s.client.ListModelSelections(s.callContext(ctx), &harnesspb.ListModelSelectionsRequest{
		TenantId: s.tenant, TaskId: s.taskID, Limit: 200,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ModelSelection, 0, len(resp.GetSelections()))
	for _, item := range resp.GetSelections() {
		out = append(out, ModelSelection{
			Stage:         item.GetStage(),
			Attempt:       int(item.GetAttemptNo()),
			ModelID:       item.GetModelId(),
			Strategy:      item.GetStrategy(),
			ResolvedFrom:  item.GetResolvedFrom(),
			Reason:        item.GetReason(),
			Operator:      item.GetOperator(),
			ApprovalID:    item.GetApprovalId(),
			Fallback:      item.GetIsFallback(),
			ContextWindow: int(item.GetContextWindow()),
			TokensIn:      item.GetTokensIn(),
			TokensOut:     item.GetTokensOut(),
			Cost:          item.GetCost(),
			CreatedAt:     time.UnixMilli(item.GetCreatedAtMs()),
		})
	}
	return out, nil
}

func (s *grpcSource) Handoffs(ctx context.Context) ([]Handoff, error) {
	resp, err := s.client.ListHandoffs(s.callContext(ctx), &harnesspb.ListHandoffsRequest{
		TenantId: s.tenant, TaskId: s.taskID, Limit: 200,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Handoff, 0, len(resp.GetHandoffs()))
	for _, item := range resp.GetHandoffs() {
		out = append(out, Handoff{
			ID:        item.GetHandoffId(),
			ParentID:  item.GetParentTaskId(),
			SubtaskID: item.GetSubtaskId(),
			From:      item.GetFromAgentId(),
			To:        item.GetToAgentId(),
			Status:    item.GetStatus(),
			Reason:    item.GetReason(),
			UpdatedAt: time.UnixMilli(item.GetUpdatedAtMs()),
		})
	}
	return out, nil
}

func (s *grpcSource) Checkpoints(ctx context.Context) ([]Checkpoint, error) {
	resp, err := s.client.ListCheckpoints(s.callContext(ctx), &harnesspb.ListCheckpointsRequest{
		TenantId: s.tenant, TaskId: s.taskID, Limit: 200,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Checkpoint, 0, len(resp.GetCheckpoints()))
	for _, item := range resp.GetCheckpoints() {
		out = append(out, Checkpoint{
			ID:        item.GetCheckpointId(),
			Kind:      item.GetKind(),
			Stage:     item.GetStage(),
			Attempt:   int(item.GetAttempt()),
			Watermark: item.GetEventSeqWatermark(),
			Payload:   item.GetPayloadRef(),
			CreatedAt: time.UnixMilli(item.GetCreatedAtMs()),
		})
	}
	return out, nil
}

func parseArtifactRefs(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var objects []map[string]any
	if err := json.Unmarshal(raw, &objects); err == nil {
		out := make([]string, 0, len(objects))
		for _, record := range objects {
			if ref := artifactRef(record); ref != "" {
				out = append(out, ref)
			}
		}
		return out
	}
	var wrapper map[string]any
	if err := json.Unmarshal(raw, &wrapper); err == nil {
		if items, ok := wrapper["artifacts"].([]any); ok {
			out := make([]string, 0, len(items))
			for _, item := range items {
				if record, ok := item.(map[string]any); ok {
					if ref := artifactRef(record); ref != "" {
						out = append(out, ref)
					}
				} else if ref, ok := item.(string); ok {
					out = append(out, ref)
				}
			}
			return out
		}
		if ref := artifactRef(wrapper); ref != "" {
			return []string{ref}
		}
	}
	return nil
}

func artifactRef(record map[string]any) string {
	for _, key := range []string{"ref", "path", "uri", "url", "id", "name"} {
		if value, ok := record[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func eventFromProto(item *harnesspb.HarnessEvent) Event {
	if item == nil {
		return Event{}
	}
	return Event{
		Seq:      item.GetSeq(),
		Type:     item.GetEventType(),
		Stage:    item.GetStage(),
		Severity: item.GetSeverity(),
		Attempt:  int(item.GetAttempt()),
		Time:     time.UnixMilli(item.GetTimestampMs()),
		Payload:  item.GetPayload(),
	}
}

func taskStateFromProto(view *harnesspb.TaskView) TaskState {
	state := TaskState{
		Status:              view.GetStatus(),
		TerminalReason:      view.GetTerminalReason(),
		CurrentStage:        view.GetCurrentStage(),
		RetryCount:          int(view.GetRetryCount()),
		AcceptanceFailCount: int(view.GetAcceptanceFailCount()),
		Attempts:            map[string]int{},
	}
	state.TaskID = view.GetTaskId()
	if budget := view.GetBudget(); budget != nil {
		state.BudgetTokens = budget.GetTokens()
		state.BudgetCost = budget.GetCost()
	}
	state.ConsumedTokens = view.GetConsumedTokens()
	state.ConsumedCost = view.GetConsumedCost()
	for stage, attempt := range view.GetAttemptNoByStage() {
		state.Attempts[stage] = int(attempt)
	}
	state.Attempt = state.Attempts[state.CurrentStage]
	return state
}
