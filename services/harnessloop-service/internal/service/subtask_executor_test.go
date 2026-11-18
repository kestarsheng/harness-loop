package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/subtask"
	agentpb "agent-platform/pkg/pb/agent"
)

type fakeAgentStreamer struct {
	chunks []*agentpb.ExecuteStreamChunk
	err    error
	req    *agentpb.ExecuteStreamRequest
}

func (f *fakeAgentStreamer) ExecuteStream(_ context.Context, req *agentpb.ExecuteStreamRequest, onChunk func(*agentpb.ExecuteStreamChunk) error) error {
	f.req = req
	for _, chunk := range f.chunks {
		if err := onChunk(chunk); err != nil {
			return err
		}
	}
	return f.err
}

func newTestSubtask(id string) *subtask.Subtask {
	return &subtask.Subtask{
		SubtaskID:       id,
		ParentTaskID:    "task-1",
		TenantID:        "tenant-a",
		ProjectID:       "proj-a",
		DagID:           "dag-1",
		DagNodeID:       "n1",
		Depth:           1,
		Role:            "coder",
		Status:          subtask.StatusPending,
		FailureStrategy: subtask.StrategyFailFast,
		OutputContract:  map[string]any{"type": "artifacts", "paths": []any{"out/result.txt"}},
		InputContract:   map[string]any{"goal": "write the result file"},
	}
}

func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestAgentSubtaskExecutorReportsArtifactsAndConclusion(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "out/result.txt", "subtask output")
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "response", Content: "已完成子任务，输出如下。"},
		{Type: "final", Content: ""},
	}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{EntryAgent: "default", Model: "qwen-plus"})
	st := newTestSubtask("sub-1")
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "c", WorkspaceRoot: root}

	res, err := executor.Execute(context.Background(), ec, st)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res == nil || res.AcceptanceID != "acc-sub-1" {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Path != "out/result.txt" {
		t.Fatalf("artifacts = %+v", res.Artifacts)
	}
	if !strings.HasPrefix(res.Artifacts[0].Hash, "sha256:") || res.Artifacts[0].Size == 0 {
		t.Fatalf("artifact hash/size = %+v", res.Artifacts[0])
	}
	if !strings.Contains(res.Conclusion, "已完成子任务") {
		t.Fatalf("conclusion = %q", res.Conclusion)
	}
	if got, _ := st.OutputContract["conclusion"].(string); got == "" {
		t.Fatal("conclusion must be written back onto the subtask")
	}
	// role=coder 的子任务直接落到 coder-agent：走 main-agent 分发会触发二次
	// handoff，被守卫拒绝后子任务必 failed（见 entryAgentFor）。
	if streamer.req == nil || streamer.req.EntryAgent != "coder-agent" {
		t.Fatalf("request = %+v", streamer.req)
	}
	if streamer.req.TenantId != "tenant-a" || streamer.req.Goal != "write the result file" {
		t.Fatalf("request routing = %+v", streamer.req)
	}
	if streamer.req.ContextVars["__subtask_id__"] != "sub-1" || streamer.req.ContextVars["__stage_mode__"] != "false" {
		t.Fatalf("context vars = %v", streamer.req.ContextVars)
	}
}

func TestAgentSubtaskExecutorUsesActualUsageWhenStreamReportsIt(t *testing.T) {
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "response", Content: "done"},
		{Type: "final", Content: "", Step: &agentpb.AgentExecutionRecord{TokensUsed: 42}},
	}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{EntryAgent: "default"})
	st := newTestSubtask("sub-actual")
	st.OutputContract = nil
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "c"}

	res, err := executor.Execute(context.Background(), ec, st)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if st.ConsumedTokens != 42 || st.TokenSource != "actual" {
		t.Errorf("consumed_tokens=%d token_source=%q, want 42/actual", st.ConsumedTokens, st.TokenSource)
	}
	if res.TokenSource != "actual" || res.ConsumedTokens != 42 {
		t.Errorf("result tokens=%d source=%q", res.ConsumedTokens, res.TokenSource)
	}
}

func TestAgentSubtaskExecutorEstimatesWhenStreamOmitsUsage(t *testing.T) {
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "response", Content: "a reply without a usage block"},
		{Type: "final", Content: ""},
	}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{EntryAgent: "default"})
	st := newTestSubtask("sub-estimate")
	st.OutputContract = nil
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1", Stage: "c"}

	res, err := executor.Execute(context.Background(), ec, st)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if st.ConsumedTokens <= 0 {
		t.Fatalf("estimate = %d, want > 0", st.ConsumedTokens)
	}
	if st.TokenSource != "estimate" || res.TokenSource != "estimate" {
		t.Errorf("token_source=%q result=%q, want estimate", st.TokenSource, res.TokenSource)
	}
}

func TestAgentSubtaskExecutorFailsOnMissingArtifact(t *testing.T) {
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{{Type: "response", Content: "done"}}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{})
	st := newTestSubtask("sub-2")
	st.OutputContract = map[string]any{"paths": []any{"out/missing.txt"}}
	ec := OrchestratorStageContext{TaskID: "task-1", Stage: "c", WorkspaceRoot: t.TempDir()}

	if _, err := executor.Execute(context.Background(), ec, st); err == nil {
		t.Fatal("missing declared artifact must fail the subtask")
	}
}

func TestAgentSubtaskExecutorPropagatesStreamError(t *testing.T) {
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{{Type: "error", Content: "model failed"}}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{})
	st := newTestSubtask("sub-3")
	st.OutputContract = nil
	ec := OrchestratorStageContext{TaskID: "task-1", Stage: "c"}

	if _, err := executor.Execute(context.Background(), ec, st); err == nil || !strings.Contains(err.Error(), "model failed") {
		t.Fatalf("stream error must propagate: %v", err)
	}
}

func TestAgentSubtaskExecutorWithoutStreamer(t *testing.T) {
	executor := NewAgentSubtaskExecutor(nil, SubtaskExecutorConfig{})
	if _, err := executor.Execute(context.Background(), OrchestratorStageContext{}, newTestSubtask("sub-4")); err == nil {
		t.Fatal("nil streamer must be rejected")
	}
}

// TestAgentSubtaskExecutorRequestMatchesStageContract pins the c-stage request
// shape: tenant, stage, model, protocol prompt and (empty) tool list must be the
// ones the stage runner sends, otherwise the subtask agent runs under a
// different contract than a successful a-stage call.
func TestAgentSubtaskExecutorRequestMatchesStageContract(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "out/result.txt", "body")
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "final", Content: "结论：已产出 out/result.txt"},
	}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{EntryAgent: "coder-agent", Model: "qwen-plus"})
	st := newTestSubtask("sub-req")
	st.TenantID = "tenant-p2-subtask"
	ec := OrchestratorStageContext{
		TenantID: "tenant-p2-subtask", ProjectID: "proj-a", TaskID: "task-req",
		Stage: "c", WorkspaceRoot: root,
	}
	res, err := executor.Execute(context.Background(), ec, st)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	req := streamer.req
	if req == nil {
		t.Fatal("request not captured")
	}
	if req.TenantId != "tenant-p2-subtask" || req.UserId != "harness-loop" {
		t.Fatalf("tenant/user = %q/%q", req.TenantId, req.UserId)
	}
	if req.EntryAgent != "coder-agent" {
		t.Fatalf("entry_agent = %q", req.EntryAgent)
	}
	if req.SessionId != "task-req-sub-req" {
		t.Fatalf("session = %q", req.SessionId)
	}
	vars := req.ContextVars
	if vars[VarStage] != "c" || vars[VarStageMode] != "false" || vars[VarStageModel] != "qwen-plus" {
		t.Fatalf("stage vars = %v", vars)
	}
	if vars[VarTenantID] != "tenant-p2-subtask" || vars[VarWorkspaceRoot] != root {
		t.Fatalf("tenant/workspace vars = %v", vars)
	}
	if vars["__subtask_id__"] != "sub-req" || vars["__dag_id__"] != "dag-1" {
		t.Fatalf("subtask vars = %v", vars)
	}
	// messages: the protocol prompt must carry the role, contracts and criteria.
	for _, want := range []string{"coder", "sub-req", "write the result file"} {
		if !strings.Contains(req.Message, want) {
			t.Fatalf("prompt %q missing %q", req.Message, want)
		}
	}
	if req.Goal != "write the result file" {
		t.Fatalf("goal = %q", req.Goal)
	}
	// The a-stage call reports the prompt-derived tokens; the subtask must stop
	// recording 0 when the stream carries no usage block.
	if st.ConsumedTokens <= 0 || res.ConsumedTokens != st.ConsumedTokens {
		t.Fatalf("consumed tokens = %d (result %d), want > 0 and mirrored", st.ConsumedTokens, res.ConsumedTokens)
	}
	// `final` must fill both the conclusion and the artifact resolution.
	if !strings.Contains(res.Conclusion, "out/result.txt") {
		t.Fatalf("final chunk was ignored: %q", res.Conclusion)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", res.Artifacts)
	}
	if res.ModelID != "qwen-plus" || res.DurationMS < 0 {
		t.Fatalf("result metadata = %+v", res)
	}
}

func TestAgentSubtaskExecutorAcceptsDirectoryContract(t *testing.T) {
	root := t.TempDir()
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{{Type: "response", Content: "完成"}}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{})
	st := newTestSubtask("sub-dir")
	// singleNodePlan() declares "." as the output: a directory contract must not
	// be mistaken for a missing file (that made every fallback subtask fail in 1s).
	st.OutputContract = map[string]any{"type": "files", "paths": []any{"."}}
	ec := OrchestratorStageContext{TaskID: "task-1", Stage: "c", WorkspaceRoot: root}
	if _, err := executor.Execute(context.Background(), ec, st); err != nil {
		t.Fatalf("directory contract must be satisfied by existence: %v", err)
	}
}

func TestAgentSubtaskExecutorErrorChunkWithoutDetailFails(t *testing.T) {
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{{Type: "error"}}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{Model: "qwen-plus"})
	st := newTestSubtask("sub-err")
	st.OutputContract = nil
	ec := OrchestratorStageContext{TaskID: "task-1", Stage: "c"}
	_, err := executor.Execute(context.Background(), ec, st)
	var execErr *SubtaskExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("error = %v, want *SubtaskExecError", err)
	}
	if execErr.AgentStatus != "error" || execErr.ModelID != "qwen-plus" || execErr.Reason == "" {
		t.Fatalf("detail = %+v", execErr)
	}
}

// TestBridgeEmitsReadableFailure proves the d-stage sees why a subtask failed:
// the executor detail reaches subtask.failed instead of a bare category.
func TestBridgeEmitsReadableFailure(t *testing.T) {
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	streamer := &fakeAgentStreamer{err: errors.New("connection reset")}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{Model: "qwen-plus"})
	bridge := NewOrchestratorBridge(true, emitter, executor)

	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-fail", Stage: "b", AttemptNo: 1, WorkspaceRoot: t.TempDir()}
	plan := `{"dag_id":"dag-fail","nodes":[
	 {"node_id":"n1","role":"coder","input_contract":{"type":"workspace","paths":["."]},"output_contract":{"type":"files","paths":["."]}}],
	 "edges":[],"parallel_sets":[]}`
	if _, err := bridge.PlanBStage(context.Background(), ec, []byte(plan)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	ec.Stage = "c"
	if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	failed := sink.find(event.SubtaskFailed)
	if failed == nil {
		t.Fatalf("subtask.failed missing, all=%v", sink.types())
	}
	var payload map[string]any
	if err := json.Unmarshal(failed.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if !strings.Contains(payload["error"].(string), "connection reset") {
		t.Fatalf("error = %v", payload["error"])
	}
	if payload["model_id"] != "qwen-plus" || payload["agent_status"] != "success" {
		t.Fatalf("payload = %v", payload)
	}
	if _, ok := payload["duration_ms"]; !ok {
		t.Fatalf("duration_ms missing: %v", payload)
	}
}

func TestBridgePlanFallbackMarksSourceAndReason(t *testing.T) {
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	bridge := NewOrchestratorBridge(true, emitter, nil)
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-fb", Stage: "b", AttemptNo: 1}

	res, err := bridge.PlanBStage(context.Background(), ec, []byte("我要做的计划写在自然语言里"))
	if err != nil {
		t.Fatalf("fallback plan must still be accepted: %v", err)
	}
	if res == nil || len(res.Subtasks) != 1 {
		t.Fatalf("fallback subtasks = %+v", res)
	}
	events := sink.find(event.SubtaskPlanCreated)
	if events == nil {
		t.Fatalf("plan_created missing")
	}
	var payload map[string]any
	if err := json.Unmarshal(events.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["source"] != orchestrator.PlanSourceFallback {
		t.Fatalf("source = %v", payload["source"])
	}
	if reason, _ := payload["degraded_reason"].(string); reason != "empty_plan_output" {
		t.Fatalf("degraded_reason = %v", payload["degraded_reason"])
	}

	// A model plan must report source=model, not fallback.
	other := &modelEventSink{}
	pub2 := event.NewPublisher(other, nil, nil)
	bridge2 := NewOrchestratorBridge(true, orchestrator.NewPublisherEmitter(pub2, orchestrator.EmitContext{}), nil)
	ec2 := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-model", Stage: "b", AttemptNo: 1}
	if _, err := bridge2.PlanBStage(context.Background(), ec2, []byte(testDAGPlan)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	var modelPayload map[string]any
	if err := json.Unmarshal(other.find(event.SubtaskPlanCreated).Payload, &modelPayload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if modelPayload["source"] != orchestrator.PlanSourceModel {
		t.Fatalf("source = %v", modelPayload["source"])
	}
	if _, ok := modelPayload["degraded_reason"]; ok {
		t.Fatalf("model plan must not be degraded: %v", modelPayload)
	}
}

// TestOrchestratorBridgeRealExecutorCompletesAggregation proves the d-stage
// aggregation stops being incomplete once the executor writes artifacts and
// the bridge marks the subtask accepted.
func TestOrchestratorBridgeRealExecutorCompletesAggregation(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "out/result.txt", "artifact body")
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "response", Content: "conclusion: artifact written"},
	}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{EntryAgent: "default"})

	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	bridge := NewOrchestratorBridge(true, emitter, executor)

	plan := `{"dag_id":"dag-real","nodes":[
	 {"node_id":"n1","role":"coder","input_contract":{"type":"file_set","paths":["src/a/**"]},"output_contract":{"type":"artifacts","paths":["out/result.txt"]}}],
	 "edges":[],"parallel_sets":[]}`
	ec := OrchestratorStageContext{TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-real", Stage: "b", AttemptNo: 1, WorkspaceRoot: root}
	if _, err := bridge.PlanBStage(context.Background(), ec, []byte(plan)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	ec.Stage = "c"
	if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	ec.Stage = "d"
	report, err := bridge.AggregateDStage(context.Background(), ec)
	if err != nil {
		t.Fatalf("aggregation must be complete: %v", err)
	}
	if report == nil || !report.Complete || report.Summary.Accepted != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Items) != 1 || len(report.Items[0].Artifacts) != 1 || !report.Items[0].Artifacts[0].HashMatch {
		raw, _ := json.Marshal(report.Items)
		t.Fatalf("aggregated artifacts = %s", raw)
	}
}
