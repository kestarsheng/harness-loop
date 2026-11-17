package service

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/orchestrator"
	agentpb "agent-platform/pkg/pb/agent"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/repository"
)

// setupBridgeRepo gives the bridge a real subtask table, so the test observes
// what the database says rather than what the scheduler remembers.
func setupBridgeRepo(t *testing.T) (*repository.SubtaskRepo, string) {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		if os.Getenv("HARNESS_REQUIRE_PG") == "1" {
			t.Fatal("HARNESS_TEST_PG_DSN is required when HARNESS_REQUIRE_PG=1")
		}
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	taskRepo := repository.NewTaskRepo(db)
	stamp := time.Now().Format("150405.000000000")
	task := &repository.TaskRun{
		TaskID:         "task-wb-" + stamp,
		TenantID:       "tenant-p2-subtask",
		ProjectID:      "proj-p2-subtask",
		WorkspaceRoot:  "/tmp/ws-p2",
		Goal:           "subtask write-back",
		GoalHash:       "hash-" + stamp,
		CurrentStage:   fsm.StageRequirement,
		Status:         repository.TaskRunning,
		BudgetTokens:   100000,
		BudgetCost:     10,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}
	if err := taskRepo.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return repository.NewSubtaskRepo(db), task.TaskID
}

func newWriteBackBridge(t *testing.T, repo *repository.SubtaskRepo, executor SubtaskExecutor) *OrchestratorBridge {
	t.Helper()
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	return NewOrchestratorBridge(true, emitter, executor).WithSubtaskRepo(repo)
}

// TestBridgePersistsSubtaskLifecycle proves the whole chain: the executor's
// real outcome (tokens, artifacts, acceptance) reaches the database through the
// scheduler's transitions, so a finished subtask is never left pending/running.
func TestBridgePersistsSubtaskLifecycle(t *testing.T) {
	repo, taskID := setupBridgeRepo(t)
	root := t.TempDir()
	writeWorkspaceFile(t, root, "out/result.txt", "artifact body")
	streamer := &fakeAgentStreamer{chunks: []*agentpb.ExecuteStreamChunk{
		{Type: "response", Content: "done: out/result.txt written"},
	}}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{Model: "qwen-plus"})
	bridge := newWriteBackBridge(t, repo, executor)

	ec := OrchestratorStageContext{
		TenantID: "tenant-p2-subtask", ProjectID: "proj-p2-subtask",
		TaskID: taskID, Stage: "b", AttemptNo: 1, WorkspaceRoot: root,
	}
	plan := `{"dag_id":"dag-wb","nodes":[
	 {"node_id":"n1","role":"coder","input_contract":{"type":"workspace","paths":["."]},"output_contract":{"type":"files","paths":["out/result.txt"]}}],
	 "edges":[],"parallel_sets":[]}`
	res, err := bridge.PlanBStage(context.Background(), ec, []byte(plan))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ec.Stage = "c"
	if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	rows, err := repo.List(context.Background(), ec.TenantID, ec.ProjectID, taskID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	row := rows[0]
	if row.SubtaskID != res.Subtasks[0].SubtaskID {
		t.Fatalf("row = %+v", row)
	}
	if row.Status != "accepted" {
		t.Fatalf("status = %q, want accepted (DB must see the final state)", row.Status)
	}
	if row.ConsumedTokens <= 0 {
		t.Fatalf("consumed_tokens = %d, want > 0 (tokens=0 was the bug)", row.ConsumedTokens)
	}
	if row.AcceptanceID == "" {
		t.Fatal("accepted row must carry acceptance_id")
	}
	if len(row.ArtifactRefs) == 0 || string(row.ArtifactRefs) == "[]" {
		t.Fatalf("artifact_refs = %s", row.ArtifactRefs)
	}
}

// TestBridgePersistsFailureWithDetail covers the failure branch: the row ends
// failed (never accepted) and the event explains why.
func TestBridgePersistsFailureWithDetail(t *testing.T) {
	repo, taskID := setupBridgeRepo(t)
	streamer := &fakeAgentStreamer{err: errors.New("agent crashed")}
	executor := NewAgentSubtaskExecutor(streamer, SubtaskExecutorConfig{Model: "qwen-plus"})
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	bridge := NewOrchestratorBridge(true, orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{}), executor).
		WithSubtaskRepo(repo)

	ec := OrchestratorStageContext{
		TenantID: "tenant-p2-subtask", ProjectID: "proj-p2-subtask",
		TaskID: taskID, Stage: "b", AttemptNo: 1, WorkspaceRoot: t.TempDir(),
	}
	plan := `{"dag_id":"dag-wbf","nodes":[
	 {"node_id":"n1","role":"coder","input_contract":{"type":"workspace","paths":["."]},"output_contract":{"type":"files","paths":["."]}}],
	 "edges":[],"parallel_sets":[]}`
	if _, err := bridge.PlanBStage(context.Background(), ec, []byte(plan)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	ec.Stage = "c"
	if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	rows, err := repo.List(context.Background(), ec.TenantID, ec.ProjectID, taskID, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
	if rows[0].Status != "failed" {
		t.Fatalf("status = %q, want failed", rows[0].Status)
	}
	if rows[0].AcceptanceID != "" {
		t.Fatalf("a failed row must not be accepted: %+v", rows[0])
	}
	if rows[0].ConsumedTokens <= 0 {
		t.Fatalf("a failed run still consumed tokens: %d", rows[0].ConsumedTokens)
	}
	failed := sink.find(event.SubtaskFailed)
	if failed == nil {
		t.Fatal("subtask.failed missing")
	}
}
