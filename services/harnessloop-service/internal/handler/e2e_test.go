//go:build integration

package handler

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	"agent-platform/pkg/harnessclient"
	"agent-platform/pkg/harnessloop/checkpoint"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/pkg/harnessloop/retry"
	agentpb "agent-platform/pkg/pb/agent"
	harnesspb "agent-platform/pkg/pb/harnessloop"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/config"
	"agent-platform/services/harnessloop-service/internal/repository"
	"agent-platform/services/harnessloop-service/internal/service"
)

type fakeAgentServer struct {
	agentpb.UnimplementedAgentServiceServer
	calls int32
}

func (f *fakeAgentServer) ExecuteStream(req *agentpb.ExecuteStreamRequest, srv agentpb.AgentService_ExecuteStreamServer) error {
	atomic.AddInt32(&f.calls, 1)
	if err := srv.Send(&agentpb.ExecuteStreamChunk{Type: "response", Content: "working"}); err != nil {
		return err
	}
	return srv.Send(&agentpb.ExecuteStreamChunk{
		Type: "tool_call",
		ToolCall: &agentpb.ToolCall{
			Name:      "stage_finish",
			Arguments: `{"attempt_id":"fake-attempt","stage":"a","summary":"done"}`,
		},
	})
}

func startFakeAgent(t *testing.T) (string, *fakeAgentServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeAgentServer{}
	agentpb.RegisterAgentServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), fake
}

type eventSinkRepo struct{ repo *repository.EventRepo }

func (s eventSinkRepo) Append(ctx context.Context, events []*event.Event) error {
	_, err := s.repo.Append(ctx, "", "", events)
	return err
}

func setupE2E(t *testing.T) (*Handler, *pgindex.DB, *repository.EventRepo, *repository.CheckpointRepo) {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	agentAddr, _ := startFakeAgent(t)
	agentClient, err := harnessclient.DialLazy(ctx, harnessclient.DefaultConfig(agentAddr))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	t.Cleanup(func() { _ = agentClient.Close() })

	taskRepo := repository.NewTaskRepo(db)
	attemptRepo := repository.NewAttemptRepo(db)
	eventRepo := repository.NewEventRepo(db)
	checkpointRepo := repository.NewCheckpointRepo(db)
	acceptanceRepo := repository.NewAcceptanceRepo(db)
	publisher := event.NewPublisher(eventSinkRepo{repo: eventRepo}, event.NewMemoryIdempotency(1000), event.NewMemoryBuffer())
	models := modelregistry.DefaultRegistry()

	stages := service.NewStageRunner(agentClient).WithEvents(publisher)
	rt := service.NewRuntime(taskRepo, attemptRepo, stages)
	rt.Events = publisher
	rt.Checkpoints = checkpointRepo
	rt.Policy = retry.DefaultPolicy()

	cfg := config.Default()
	cfg.Postgres.DSN = dsn
	cfg.Harness.AutoStart = true
	cfg.Harness.Enabled = true
	cfg.Agent.StageModel = "qwen-plus"

	h := New(Deps{
		Config:      cfg,
		Tasks:       taskRepo,
		Attempts:    attemptRepo,
		Events:      eventRepo,
		Checkpoints: checkpointRepo,
		Acceptances: acceptanceRepo,
		Runtime:     rt,
		Policy:      policy.NewEngine(),
		Models:      models,
		Publisher:   publisher,
	})
	return h, db, eventRepo, checkpointRepo
}

func TestE2ECompletedFiveStages(t *testing.T) {
	h, _, eventRepo, _ := setupE2E(t)
	ctx := context.Background()

	workspace := t.TempDir()
	created, err := h.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId: "tenant-e2e", ProjectId: "proj-e2e", WorkspaceRoot: workspace, Goal: "run all stages",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	taskID := created.Task.TaskId

	deadline := time.Now().Add(20 * time.Second)
	var view *harnesspb.TaskView
	for time.Now().Before(deadline) {
		view, err = h.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: "tenant-e2e", TaskId: taskID})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if view.GetStatus() == "completed" || view.GetStatus() == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if view.GetStatus() != "completed" {
		t.Fatalf("task did not complete: status=%s reason=%s", view.GetStatus(), view.GetTerminalReason())
	}
	if view.GetCurrentStage() != "e" {
		t.Fatalf("final stage = %s, want e", view.GetCurrentStage())
	}
	for _, stage := range []string{"a", "b", "c", "d"} {
		if view.GetAttemptNoByStage()[stage] == 0 {
			t.Fatalf("stage %s has no attempt: %v", stage, view.GetAttemptNoByStage())
		}
	}

	events, err := eventRepo.List(ctx, repository.EventFilter{TenantID: "tenant-e2e", TaskID: taskID})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var transitions int
	for _, e := range events {
		if string(e.EventType) == "stage.transition" {
			transitions++
		}
	}
	if transitions != 4 {
		t.Fatalf("expected 4 stage transitions (a->b->c->d->e), got %d", transitions)
	}
}

func TestE2EEventIdempotencyAcrossThreeDeliveries(t *testing.T) {
	_, _, eventRepo, _ := setupE2E(t)
	ctx := context.Background()
	tenant := "tenant-e2e-idem"
	taskID := fmt.Sprintf("task-idem-%d", time.Now().UnixNano())

	e := &event.Event{
		EventID:       taskID + "-ev-1",
		EventType:     event.StageTransition,
		SchemaVersion: event.CurrentSchemaVersion,
		TaskID:        taskID,
		Seq:           1,
		Timestamp:     time.Now().UTC(),
		Severity:      event.SeverityInfo,
		TenantID:      tenant,
		ProjectID:     "proj-e2e",
		Payload:       []byte(`{"from":"a","to":"b"}`),
	}
	inserted := 0
	for i := 0; i < 3; i++ {
		n, err := eventRepo.Append(ctx, tenant, "proj-e2e", []*event.Event{e})
		if err != nil {
			t.Fatalf("append #%d: %v", i, err)
		}
		inserted += n
	}
	if inserted != 1 {
		t.Fatalf("duplicate deliveries inserted %d rows, want 1", inserted)
	}
	events, err := eventRepo.List(ctx, repository.EventFilter{TenantID: tenant, TaskID: taskID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event rows = %d, want 1", len(events))
	}
}

func TestE2ECheckpointRestoreAndDriftGuard(t *testing.T) {
	h, _, _, cpRepo := setupE2E(t)
	h.cfg.Harness.AutoStart = false
	ctx := context.Background()
	tenant := "tenant-e2e-cp"

	created, err := h.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId: tenant, ProjectId: "proj-e2e", WorkspaceRoot: t.TempDir(), Goal: "checkpoint run",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	taskID := created.Task.TaskId
	task, err := h.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: tenant, TaskId: taskID})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	cp := &checkpoint.Checkpoint{
		CheckpointID: "cp-e2e-" + taskID,
		TaskID:       taskID,
		TenantID:     tenant,
		Stage:        task.GetCurrentStage(),
		AttemptNo:    int(task.GetAttemptNoByStage()[task.GetCurrentStage()]),
		Kind:         checkpoint.KindStageEntry,
		FSM:          checkpoint.FSMSnapshot{CurrentStage: task.GetCurrentStage(), Status: task.GetStatus()},
		CreatedAt:    time.Now().UTC(),
	}
	cp.Seal()
	if err := cpRepo.Save(ctx, cp); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	restored, err := h.RestoreFromCheckpoint(ctx, &harnesspb.RestoreRequest{
		TenantId: tenant, TaskId: taskID, CheckpointId: cp.CheckpointID,
	})
	if err != nil {
		t.Fatalf("restore consistent checkpoint: %v", err)
	}
	if restored.GetTaskId() != taskID {
		t.Fatalf("restored wrong task: %s", restored.GetTaskId())
	}

	drifted := &checkpoint.Checkpoint{
		CheckpointID: "cp-drift-" + taskID,
		TaskID:       taskID,
		TenantID:     tenant,
		Stage:        "b",
		AttemptNo:    1,
		Kind:         checkpoint.KindStageEntry,
		FSM:          checkpoint.FSMSnapshot{CurrentStage: "b", Status: "running"},
		CreatedAt:    time.Now().UTC(),
	}
	drifted.Seal()
	if err := cpRepo.Save(ctx, drifted); err != nil {
		t.Fatalf("save drifted checkpoint: %v", err)
	}
	if _, err := h.RestoreFromCheckpoint(ctx, &harnesspb.RestoreRequest{
		TenantId: tenant, TaskId: taskID, CheckpointId: drifted.CheckpointID,
	}); err == nil {
		t.Fatal("restoring a drifted checkpoint must be rejected")
	}
}
