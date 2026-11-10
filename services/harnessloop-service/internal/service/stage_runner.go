package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	agentpkg "agent-platform/pkg/agent"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/retry"
	agentpb "agent-platform/pkg/pb/agent"
	"agent-platform/services/harnessloop-service/internal/watchdog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrSilentThreshold = errors.New("stage runner: silent threshold exceeded")
var ErrInvalidFinishPayload = errors.New("stage runner: invalid stage_finish payload")

const (
	VarStageMode        = "__stage_mode__"
	VarPolicySnapshot   = "__policy_snapshot__"
	VarPolicyHash       = "__policy_hash__"
	VarStageModel       = "__stage_model__"
	VarModelWindow      = "__model_window__"
	VarRequiresToolCall = "__requires_tool_call__"
)

var RequiredContextVars = []string{
	VarStageMode, VarPolicySnapshot, VarPolicyHash, VarStageModel, VarModelWindow, VarRequiresToolCall,
}

var ErrStageRunnerInput = errors.New("stage runner: invalid input")

type AgentStreamer interface {
	ExecuteStream(ctx context.Context, req *agentpb.ExecuteStreamRequest, onChunk func(*agentpb.ExecuteStreamChunk) error) error
}

type EventPublisher interface {
	Publish(ctx context.Context, e *event.Event) error
}

type StageRunInput struct {
	TaskID           string
	TenantID         string
	ProjectID        string
	SessionID        string
	UserID           string
	Stage            string
	AttemptID        string
	EntryAgent       string
	Message          string
	Goal             string
	SuccessCriteria  string
	PolicySnapshot   []byte
	PolicyHash       string
	StageModel       string
	ModelWindow      int
	RequiresToolCall bool
	Timeout          time.Duration
	SilentThreshold  time.Duration
	Now              func() time.Time
}

type StageRunResult struct {
	Content        string
	FinishReceived bool
	FinishArgs     fsm.StageFinishArgs
	Decision       retry.FinishDecision
	Classification *retry.Classification
	Elapsed        time.Duration
}

type StageRunner struct {
	agents AgentStreamer
	events EventPublisher
}

func NewStageRunner(agents AgentStreamer) *StageRunner {
	return &StageRunner{agents: agents}
}

func (r *StageRunner) WithEvents(p EventPublisher) *StageRunner {
	r.events = p
	return r
}

func (in StageRunInput) validate() error {
	if strings.TrimSpace(in.TaskID) == "" || strings.TrimSpace(in.Stage) == "" {
		return fmt.Errorf("%w: task_id and stage are required", ErrStageRunnerInput)
	}
	if strings.TrimSpace(in.AttemptID) == "" || strings.TrimSpace(in.EntryAgent) == "" {
		return fmt.Errorf("%w: attempt_id and entry_agent are required", ErrStageRunnerInput)
	}
	if strings.TrimSpace(in.PolicyHash) == "" {
		return fmt.Errorf("%w: policy_hash is required", ErrStageRunnerInput)
	}
	if strings.TrimSpace(in.StageModel) == "" {
		return fmt.Errorf("%w: stage_model is required", ErrStageRunnerInput)
	}
	if in.ModelWindow <= 0 {
		return fmt.Errorf("%w: model_window must be positive", ErrStageRunnerInput)
	}
	return nil
}

func (in StageRunInput) contextVars() map[string]string {
	snapshot := in.PolicySnapshot
	if len(snapshot) == 0 {
		snapshot = []byte("{}")
	}
	vars := map[string]string{
		VarStageMode:        "true",
		VarPolicySnapshot:   string(snapshot),
		VarPolicyHash:       in.PolicyHash,
		VarStageModel:       in.StageModel,
		VarModelWindow:      strconv.Itoa(in.ModelWindow),
		VarRequiresToolCall: strconv.FormatBool(in.RequiresToolCall),
	}
	if canonical, err := json.Marshal(json.RawMessage(snapshot)); err == nil && len(canonical) > 0 {
		vars[VarPolicySnapshot] = string(snapshot)
	}
	return vars
}

func (r *StageRunner) Run(ctx context.Context, in StageRunInput) (*StageRunResult, error) {
	if r.agents == nil {
		return nil, fmt.Errorf("%w: agent streamer is not configured", ErrStageRunnerInput)
	}
	if err := in.validate(); err != nil {
		return nil, err
	}

	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	now := in.Now
	if now == nil {
		now = time.Now
	}
	tracker := watchdog.NewTracker(in.SilentThreshold, now)
	tracker.Start()
	defer tracker.Reset()

	req := &agentpb.ExecuteStreamRequest{
		SessionId:       in.SessionID,
		TenantId:        in.TenantID,
		UserId:          in.UserID,
		Message:         in.Message,
		EntryAgent:      in.EntryAgent,
		ContextVars:     in.contextVars(),
		Goal:            in.Goal,
		SuccessCriteria: in.SuccessCriteria,
	}

	res := &StageRunResult{}
	var finishErr error
	var streamErr string
	start := time.Now()

	err := r.agents.ExecuteStream(runCtx, req, func(chunk *agentpb.ExecuteStreamChunk) error {
		if chunk == nil {
			return nil
		}
		switch chunk.Type {
		case "response", "token":
			if chunk.Content != "" {
				res.Content += chunk.Content
				tracker.Touch(watchdog.ActivityToken)
			}
		case "error":
			streamErr = chunk.Content
		}

		if tc := chunk.ToolCall; tc != nil {
			tracker.Touch(watchdog.ActivityToolResult)
			if fsm.IsStageFinishTool(tc.Name) {
				args, perr := fsm.ParseStageFinish([]byte(tc.Arguments))
				if perr != nil {
					finishErr = fmt.Errorf("%w: %v", ErrInvalidFinishPayload, perr)
					return finishErr
				}
				res.FinishArgs = args
				res.FinishReceived = true
				tracker.MarkFinish()
			}
		}
		if v := tracker.Evaluate(); v.Trigger {
			return fmt.Errorf("%w after %s", ErrSilentThreshold, v.Elapsed)
		}
		return nil
	})
	res.Elapsed = time.Since(start)

	if err != nil {
		stated := ClassifyStageError(err, streamErr)
		res.Classification = &stated
		res.Decision = retry.FinishDecision{Action: retry.ActionRetry, Category: stated.Category, Reason: stated.Reason}
		r.publish(ctx, in, string(stated.Category), stated.Reason)
		return res, err
	}
	if finishErr != nil {
		stated := retry.Classification{Category: retry.InvalidToolResult, Retryable: true, Reason: "invalid stage_finish payload"}
		res.Classification = &stated
		res.Decision = retry.FinishDecision{Action: retry.ActionRetry, Category: retry.InvalidToolResult, Reason: stated.Reason}
		return res, finishErr
	}
	if streamErr != "" {
		env := retry.ErrorEnvelope{Kind: retry.KindAgent, Code: "E_STREAM_ERROR"}
		if strings.EqualFold(strings.TrimSpace(streamErr), "content_filter") {
			env = retry.ErrorEnvelope{Kind: retry.KindFinish, FinishReason: "content_filter"}
		}
		stated := retry.Classify(env)
		res.Classification = &stated
		res.Decision = retry.FinishDecision{Action: retry.ActionRetry, Category: stated.Category, Reason: stated.Reason}
		if stated.Category == retry.ContentFilter {
			res.Decision = retry.FinishDecision{Action: retry.ActionFail, Category: retry.ContentFilter, Reason: stated.Reason}
		}
		return res, nil
	}

	if res.FinishReceived {
		res.Decision = retry.DecideFinishAction(retry.FinishStop, true, false)
		return res, nil
	}
	res.Decision = retry.DecideFinishAction(retry.FinishStop, false, false)
	return res, nil
}

func (r *StageRunner) publish(ctx context.Context, in StageRunInput, category, reason string) {
	if r.events == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"stage":      in.Stage,
		"attempt_id": in.AttemptID,
		"category":   category,
		"reason":     reason,
	})
	_ = r.events.Publish(ctx, &event.Event{
		EventID:       fmt.Sprintf("%s-%s-error", in.TaskID, in.Stage),
		EventType:     event.Error,
		SchemaVersion: event.CurrentSchemaVersion,
		TaskID:        in.TaskID,
		Stage:         in.Stage,
		Timestamp:     time.Now().UTC(),
		Severity:      event.SeverityWarn,
		TenantID:      in.TenantID,
		ProjectID:     in.ProjectID,
		AgentID:       in.EntryAgent,
		Payload:       payload,
	})
}

func ClassifyStageError(err error, streamErr string) retry.Classification {
	if err == nil {
		return retry.Classification{Category: retry.System, Retryable: true, Reason: "unknown stage error", Fallback: true}
	}
	if errors.Is(err, ErrSilentThreshold) {
		return retry.Classification{Category: retry.NoFinish, Retryable: true, Reason: "silent threshold exceeded"}
	}
	if errors.Is(err, ErrInvalidFinishPayload) {
		return retry.Classification{Category: retry.InvalidToolResult, Retryable: true, Reason: "invalid stage_finish payload"}
	}
	if errors.Is(err, agentpkg.ErrStageNotFinished) {
		return retry.Classify(retry.ErrorEnvelope{Kind: retry.KindAgent, Code: "E_STAGE_NOT_FINISHED"})
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return retry.Classify(retry.ErrorEnvelope{Kind: retry.KindGRPC, GRPCStatus: "DeadlineExceeded"})
	}
	if errors.Is(err, context.Canceled) {
		return retry.Classify(retry.ErrorEnvelope{Kind: retry.KindGRPC, GRPCStatus: "Canceled"})
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.DeadlineExceeded:
			return retry.Classify(retry.ErrorEnvelope{Kind: retry.KindGRPC, GRPCStatus: "DeadlineExceeded"})
		case codes.Unavailable, codes.Aborted, codes.Internal:
			return retry.Classify(retry.ErrorEnvelope{Kind: retry.KindGRPC, GRPCStatus: st.Code().String()})
		case codes.ResourceExhausted:
			return retry.Classify(retry.ErrorEnvelope{Kind: retry.KindHTTP, HTTPStatus: 429, Code: "quota_exceeded"})
		case codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument:
			return retry.Classify(retry.ErrorEnvelope{Kind: retry.KindGRPC, GRPCStatus: st.Code().String()})
		}
	}
	if strings.Contains(strings.ToLower(streamErr), "content_filter") {
		return retry.Classify(retry.ErrorEnvelope{Kind: retry.KindFinish, FinishReason: "content_filter"})
	}
	return retry.Classification{Category: retry.System, Retryable: true, Reason: "unclassified stage error", Fallback: true}
}
