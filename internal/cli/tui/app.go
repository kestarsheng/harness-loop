package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/metadata"

	harnesspb "agent-platform/pkg/pb/harnessloop"
)

type RunOptions struct {
	NoColor bool
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
	model := NewModel(taskStateFromProto(task), src, Options{NoColor: opts.NoColor}).WithContext(ctx)
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
