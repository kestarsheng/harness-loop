package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/retry"
	agentpb "agent-platform/pkg/pb/agent"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubAgent struct {
	mu         sync.Mutex
	req        *agentpb.ExecuteStreamRequest
	chunks     []*agentpb.ExecuteStreamChunk
	err        error
	beforeEach func(int)
}

func (s *stubAgent) ExecuteStream(ctx context.Context, req *agentpb.ExecuteStreamRequest, onChunk func(*agentpb.ExecuteStreamChunk) error) error {
	s.mu.Lock()
	s.req = req
	chunks := s.chunks
	before := s.beforeEach
	err := s.err
	s.mu.Unlock()

	for i, c := range chunks {
		if before != nil {
			before(i)
		}
		if herr := onChunk(c); herr != nil {
			return herr
		}
	}
	return err
}

func (s *stubAgent) request() *agentpb.ExecuteStreamRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.req
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func baseStageInput() StageRunInput {
	return StageRunInput{
		TaskID:           "task-1",
		TenantID:         "tenant-a",
		ProjectID:        "proj-a",
		SessionID:        "sess-1",
		Stage:            "c",
		AttemptID:        "task-1-c-1",
		EntryAgent:       "exec-agent",
		Message:          "implement the change",
		Goal:             "finish the stage",
		PolicySnapshot:   []byte(`{"mode":"enforce"}`),
		PolicyHash:       "sha256:policy",
		StageModel:       "qwen-max",
		ModelWindow:      32768,
		RequiresToolCall: true,
	}
}

func TestStageRunnerInjectsContextVars(t *testing.T) {
	agent := &stubAgent{chunks: []*agentpb.ExecuteStreamChunk{{Type: "response", Content: "working"}}}
	r := NewStageRunner(agent)

	if _, err := r.Run(context.Background(), baseStageInput()); err != nil {
		t.Fatalf("run: %v", err)
	}
	req := agent.request()
	if req == nil {
		t.Fatal("no request captured")
	}
	if len(req.ContextVars) != len(RequiredContextVars) {
		t.Fatalf("context vars = %v", req.ContextVars)
	}
	for _, key := range RequiredContextVars {
		if _, ok := req.ContextVars[key]; !ok {
			t.Fatalf("missing %s in %v", key, req.ContextVars)
		}
	}
	if req.ContextVars[VarStageMode] != "true" {
		t.Fatalf("stage mode = %q", req.ContextVars[VarStageMode])
	}
	if req.ContextVars[VarPolicySnapshot] != `{"mode":"enforce"}` {
		t.Fatalf("snapshot = %q", req.ContextVars[VarPolicySnapshot])
	}
	if req.ContextVars[VarModelWindow] != "32768" || req.ContextVars[VarRequiresToolCall] != "true" {
		t.Fatalf("derived vars = %v", req.ContextVars)
	}
	for key, value := range req.ContextVars {
		if strings.Contains(value, ".yaml") || strings.Contains(value, "config/harness_loop") {
			t.Fatalf("policy file path leaked into %s: %q", key, value)
		}
	}
}

func TestStageRunnerEmptyPolicySnapshotDefaultsToObject(t *testing.T) {
	agent := &stubAgent{}
	r := NewStageRunner(agent)
	in := baseStageInput()
	in.PolicySnapshot = nil
	in.Message = "no chunks"
	in.Timeout = time.Second

	res, err := r.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if agent.request().ContextVars[VarPolicySnapshot] != "{}" {
		t.Fatalf("snapshot = %q", agent.request().ContextVars[VarPolicySnapshot])
	}
	if res.FinishReceived {
		t.Fatal("empty stream must not be treated as finish")
	}
}

func TestStageRunnerRecognizesFinishTool(t *testing.T) {
	agent := &stubAgent{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "response", Content: "done coding"},
		{Type: "tool_call", ToolCall: &agentpb.ToolCall{
			Name:      "stage_finish",
			Arguments: `{"attempt_id":"task-1-c-1","stage":"c","summary":"implemented"}`,
		}},
		{Type: "final", Content: "done"},
	}}
	r := NewStageRunner(agent)
	res, err := r.Run(context.Background(), baseStageInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.FinishReceived {
		t.Fatal("finish tool call must be recognized")
	}
	if res.FinishArgs.AttemptID != "task-1-c-1" || res.FinishArgs.Stage != "c" {
		t.Fatalf("finish args = %+v", res.FinishArgs)
	}
	if res.Decision.Action != retry.ActionDone {
		t.Fatalf("decision = %+v", res.Decision)
	}
	if res.Content != "done coding" {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestStageRunnerNoToolCallDoesNotFinish(t *testing.T) {
	agent := &stubAgent{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "response", Content: "I am done, trust me"},
		{Type: "final", Content: "done"},
	}}
	r := NewStageRunner(agent)
	res, err := r.Run(context.Background(), baseStageInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinishReceived {
		t.Fatal("text-only completion must not finish the stage")
	}
	if res.Decision.Action != retry.ActionRetry || res.Decision.Category != retry.NoFinish {
		t.Fatalf("decision = %+v", res.Decision)
	}
}

func TestStageRunnerInvalidFinishPayload(t *testing.T) {
	agent := &stubAgent{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "tool_call", ToolCall: &agentpb.ToolCall{Name: "stage_finish", Arguments: `{"stage":"c"}`}},
	}}
	r := NewStageRunner(agent)
	res, err := r.Run(context.Background(), baseStageInput())
	if err == nil {
		t.Fatal("invalid finish payload must fail")
	}
	if res.Classification == nil || res.Classification.Category != retry.InvalidToolResult {
		t.Fatalf("classification = %+v", res.Classification)
	}
}

func TestStageRunnerClassifiesGRPCError(t *testing.T) {
	agent := &stubAgent{err: status.Error(codes.Unavailable, "agent-service down")}
	r := NewStageRunner(agent)
	res, err := r.Run(context.Background(), baseStageInput())
	if err == nil {
		t.Fatal("error must propagate")
	}
	if res.Classification == nil || res.Classification.Category != retry.Network {
		t.Fatalf("classification = %+v", res.Classification)
	}
}

func TestStageRunnerClassifiesContentFilter(t *testing.T) {
	agent := &stubAgent{chunks: []*agentpb.ExecuteStreamChunk{{Type: "error", Content: "content_filter"}}}
	r := NewStageRunner(agent)
	res, err := r.Run(context.Background(), baseStageInput())
	if err != nil {
		t.Fatalf("stream error is informational: %v", err)
	}
	if res.Classification == nil || res.Classification.Category != retry.ContentFilter {
		t.Fatalf("classification = %+v", res.Classification)
	}
	if res.Classification.Retryable {
		t.Fatal("content_filter must not be retryable")
	}
}

func TestStageRunnerSilentThreshold(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	agent := &stubAgent{
		chunks: []*agentpb.ExecuteStreamChunk{
			{Type: "response", Content: "working"},
			{Type: "agent_step"},
		},
		beforeEach: func(i int) {
			if i == 1 {
				clk.Advance(121 * time.Second)
			}
		},
	}
	r := NewStageRunner(agent)
	in := baseStageInput()
	in.Now = clk.Now
	in.SilentThreshold = 120 * time.Second

	res, err := r.Run(context.Background(), in)
	if !errors.Is(err, ErrSilentThreshold) {
		t.Fatalf("expected silent threshold error, got %v", err)
	}
	if res.Classification == nil || res.Classification.Category != retry.NoFinish {
		t.Fatalf("classification = %+v", res.Classification)
	}
}

func TestStageRunnerInputValidation(t *testing.T) {
	r := NewStageRunner(&stubAgent{})
	cases := []func(*StageRunInput){
		func(in *StageRunInput) { in.TaskID = "" },
		func(in *StageRunInput) { in.Stage = "" },
		func(in *StageRunInput) { in.AttemptID = "" },
		func(in *StageRunInput) { in.EntryAgent = "" },
		func(in *StageRunInput) { in.PolicyHash = "" },
		func(in *StageRunInput) { in.StageModel = "" },
		func(in *StageRunInput) { in.ModelWindow = 0 },
	}
	for _, mut := range cases {
		in := baseStageInput()
		mut(&in)
		if _, err := r.Run(context.Background(), in); !errors.Is(err, ErrStageRunnerInput) {
			t.Fatalf("invalid input must be rejected: %+v -> %v", in, err)
		}
	}
	if _, err := NewStageRunner(nil).Run(context.Background(), baseStageInput()); err == nil {
		t.Fatal("nil agent streamer must fail")
	}
}

func TestStageRunnerPublishesErrorEvent(t *testing.T) {
	agent := &stubAgent{err: status.Error(codes.Unavailable, "down")}
	pub := &capturingPublisher{}
	r := NewStageRunner(agent).WithEvents(pub)
	if _, err := r.Run(context.Background(), baseStageInput()); err == nil {
		t.Fatal("expected error")
	}
	if len(pub.events) != 1 {
		t.Fatalf("events = %d", len(pub.events))
	}
	if string(pub.events[0].EventType) != "error" {
		t.Fatalf("event type = %s", pub.events[0].EventType)
	}
}

type capturingPublisher struct {
	mu     sync.Mutex
	events []*event.Event
}

func (p *capturingPublisher) Publish(ctx context.Context, e *event.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return nil
}

func TestClassifyStageErrorFallback(t *testing.T) {
	c := ClassifyStageError(errors.New("something odd"), "")
	if !c.Fallback || c.Category != retry.System {
		t.Fatalf("unknown error must fall back to system: %+v", c)
	}
	c = ClassifyStageError(context.DeadlineExceeded, "")
	if c.Category != retry.Timeout {
		t.Fatalf("deadline must be timeout: %+v", c)
	}
}
