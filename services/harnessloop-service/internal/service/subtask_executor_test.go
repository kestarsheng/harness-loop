package service

import (
	"context"
	"encoding/json"
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
	if streamer.req == nil || streamer.req.EntryAgent != "default" {
		t.Fatalf("request = %+v", streamer.req)
	}
	if streamer.req.TenantId != "tenant-a" || streamer.req.Goal != "write the result file" {
		t.Fatalf("request routing = %+v", streamer.req)
	}
	if streamer.req.ContextVars["__subtask_id__"] != "sub-1" || streamer.req.ContextVars["__stage_mode__"] != "false" {
		t.Fatalf("context vars = %v", streamer.req.ContextVars)
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
