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

func Run(ctx context.Context, client harnesspb.HarnessLoopServiceClient, tenant string, task *harnesspb.TaskView, opts RunOptions) (RunResult, error) {
	if client == nil {
		return RunResult{}, fmt.Errorf("tui: harnessloop client is required")
	}
	if task == nil || task.GetTaskId() == "" {
		return RunResult{}, fmt.Errorf("tui: task view is required")
	}
	src := &grpcSource{client: client, tenant: tenant, taskID: task.GetTaskId(), ctx: ctx}
	model := NewModel(taskStateFromProto(task), src, Options{
		NoColor:      opts.NoColor,
		WebURL:       opts.WebURL,
		Hyperlink:    opts.Hyperlink,
		OpenURL:      opts.OpenURL,
		KB:           opts.KB,
		KBTopK:       opts.KBTopK,
		InitialView:  opts.InitialView,
		InitialQuery: opts.InitialQuery,
	}).WithContext(ctx)
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
	streamCtx, cancel := context.WithCancel(s.baseContext())
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
