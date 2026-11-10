package handler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"agent-platform/pkg/harnessloop/checkpoint"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/policy"
	harnesspb "agent-platform/pkg/pb/harnessloop"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/config"
	"agent-platform/services/harnessloop-service/internal/repository"
)

func setupHandler(t *testing.T) *Handler {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := config.Default()
	cfg.Postgres.DSN = dsn
	cfg.Harness.AutoStart = false
	cfg.Harness.Enabled = true

	return New(Deps{
		Config:      cfg,
		Tasks:       repository.NewTaskRepo(db),
		Attempts:    repository.NewAttemptRepo(db),
		Events:      repository.NewEventRepo(db),
		Checkpoints: repository.NewCheckpointRepo(db),
		Acceptances: repository.NewAcceptanceRepo(db),
		Policy:      policy.NewEngine(),
		Models:      modelregistry.DefaultRegistry(),
	})
}

func TestCreateTaskAndGet(t *testing.T) {
	h := setupHandler(t)
	ctx := context.Background()

	resp, err := h.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId:      "tenant-a",
		ProjectId:     "proj-a",
		WorkspaceRoot: t.TempDir(),
		Goal:          "implement the harness loop",
		Budget:        &harnesspb.Budget{Tokens: 1000, Cost: 1.5},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.Task.TaskId == "" || resp.Task.Status != "running" || resp.Task.CurrentStage != "a" {
		t.Fatalf("unexpected task view: %+v", resp.Task)
	}
	if !resp.Task.FsmEnabled {
		t.Fatal("fsm must be enabled")
	}
	if resp.Task.Budget.GetTokens() != 1000 {
		t.Fatalf("budget = %+v", resp.Task.Budget)
	}

	got, err := h.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: "tenant-a", TaskId: resp.Task.TaskId})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TaskId != resp.Task.TaskId {
		t.Fatalf("task id mismatch: %s", got.TaskId)
	}

	_, err = h.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: "tenant-b", TaskId: resp.Task.TaskId})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("cross tenant must be NotFound, got %v", err)
	}
}

func TestCreateTaskValidation(t *testing.T) {
	h := setupHandler(t)
	if _, err := h.CreateTask(context.Background(), &harnesspb.CreateTaskRequest{Goal: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing workspace must be InvalidArgument: %v", err)
	}
	if _, err := h.CreateTask(context.Background(), &harnesspb.CreateTaskRequest{WorkspaceRoot: "/w"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing goal must be InvalidArgument: %v", err)
	}
}

func TestPreflightToolUsesPolicy(t *testing.T) {
	h := setupHandler(t)
	ctx := context.Background()

	root := t.TempDir()
	created, err := h.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId: "tenant-a", ProjectId: "proj-a", WorkspaceRoot: root, Goal: "g",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	allowed, err := h.PreflightTool(ctx, &harnesspb.PreflightToolRequest{
		TenantId: "tenant-a", TaskId: created.Task.TaskId, Stage: "c",
		ToolName: "write_file", PathArgs: []string{"a.go"},
	})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !allowed.Allowed {
		t.Fatalf("write in c must be allowed: %+v", allowed)
	}

	denied, err := h.PreflightTool(ctx, &harnesspb.PreflightToolRequest{
		TenantId: "tenant-a", TaskId: created.Task.TaskId, Stage: "a",
		ToolName: "write_file", PathArgs: []string{"a.go"},
	})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if denied.Allowed || denied.ReasonCode != policy.CodeToolDenied {
		t.Fatalf("write in a must be denied: %+v", denied)
	}

	escaped, err := h.PreflightTool(ctx, &harnesspb.PreflightToolRequest{
		TenantId: "tenant-a", TaskId: created.Task.TaskId, Stage: "c",
		ToolName: "read_file", PathArgs: []string{"../etc/passwd"},
	})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if escaped.Allowed || escaped.ReasonCode != policy.CodePathEscape {
		t.Fatalf("escape must be denied: %+v", escaped)
	}
}

func TestListModelsAndResolve(t *testing.T) {
	h := setupHandler(t)
	ctx := context.Background()

	list, err := h.ListModels(ctx, &harnesspb.ListModelsRequest{})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(list.Models) != 4 {
		t.Fatalf("models = %d", len(list.Models))
	}
	for _, m := range list.Models {
		if m.ContextWindow <= 0 || m.Id == "" {
			t.Fatalf("invalid model entry: %+v", m)
		}
	}

	res, err := h.ResolveModel(ctx, &harnesspb.ResolveModelRequest{Stage: "c", RequireToolCall: true, Strategy: "cost_first"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Candidates[0].Id != "qwen-plus" {
		t.Fatalf("resolved = %s", res.Candidates[0].Id)
	}

	if _, err := h.ResolveModel(ctx, &harnesspb.ResolveModelRequest{Stage: "c", RequireToolCall: true, RequiredContextWindow: 200000}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("impossible capability must be FailedPrecondition: %v", err)
	}
}

func TestPauseAndResume(t *testing.T) {
	h := setupHandler(t)
	ctx := context.Background()
	created, err := h.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId: "tenant-a", ProjectId: "proj-a", WorkspaceRoot: t.TempDir(), Goal: "g",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	paused, err := h.PauseTask(ctx, &harnesspb.TaskCommand{TenantId: "tenant-a", TaskId: created.Task.TaskId})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if paused.Status != "paused" {
		t.Fatalf("status = %s", paused.Status)
	}
	resumed, err := h.ResumeTask(ctx, &harnesspb.ResumeTaskRequest{TenantId: "tenant-a", TaskId: created.Task.TaskId})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Status != "running" {
		t.Fatalf("status = %s", resumed.Status)
	}
}

func TestListEventsEmpty(t *testing.T) {
	h := setupHandler(t)
	ctx := context.Background()
	resp, err := h.ListEvents(ctx, &harnesspb.ListEventsRequest{TenantId: "tenant-a", TaskId: "no-such-task"})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(resp.Events) != 0 || resp.NextSinceSeq != 0 {
		t.Fatalf("unexpected resp: %+v", resp)
	}
}

func TestListCheckpointsEmpty(t *testing.T) {
	h := setupHandler(t)
	resp, err := h.ListCheckpoints(context.Background(), &harnesspb.ListCheckpointsRequest{TaskId: "no-such-task"})
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	if len(resp.Checkpoints) != 0 {
		t.Fatalf("checkpoints = %d", len(resp.Checkpoints))
	}
}

func TestCreateTaskDisabledByFeatureFlag(t *testing.T) {
	h := setupHandler(t)
	h.cfg.Harness.Enabled = false
	_, err := h.CreateTask(context.Background(), &harnesspb.CreateTaskRequest{WorkspaceRoot: "/w", Goal: "g"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disabled flag must fail: %v", err)
	}
}

func TestConvertEventAndCheckpoint(t *testing.T) {
	e := &event.Event{
		EventID: "ev-1", EventType: event.StageTransition, SchemaVersion: event.CurrentSchemaVersion,
		TaskID: "task-1", Stage: "c", Attempt: 2, Seq: 5, Timestamp: time.Now(),
		Severity: event.SeverityInfo, TenantID: "tenant-a", ProjectID: "proj-a", Payload: []byte("{}"),
	}
	pbEvent := toProtoEvent(e)
	if pbEvent.Stage == nil || pbEvent.GetStage() != "c" {
		t.Fatalf("stage not converted: %+v", pbEvent)
	}
	if pbEvent.Attempt == nil || pbEvent.GetAttempt() != 2 {
		t.Fatalf("attempt not converted: %+v", pbEvent)
	}
	if toProtoEvent(nil) != nil {
		t.Fatal("nil event must convert to nil")
	}

	cp := &checkpoint.Checkpoint{
		CheckpointID: "cp-1", TaskID: "task-1", Stage: "c", AttemptNo: 1,
		Kind: checkpoint.KindStageEntry, CreatedAt: time.Now(), Checksum: "abc", EventSeqWatermark: 9,
	}
	pbCP := toProtoCheckpoint(cp)
	if pbCP.PayloadRef != "abc" || pbCP.EventSeqWatermark != 9 {
		t.Fatalf("checkpoint not converted: %+v", pbCP)
	}
	if toProtoCheckpoint(nil) != nil {
		t.Fatal("nil checkpoint must convert to nil")
	}
}

func TestFeatureFlagEnablesAndFreezes(t *testing.T) {
	h := setupHandler(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "features.yaml")
	body := "version: 1\nflags:\n  - name: harness.loop.fsm.enabled\n    default: true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	flags, err := config.LoadFlagSet(path)
	if err != nil {
		t.Fatalf("load flags: %v", err)
	}
	h.flags = flags

	ctx := context.Background()
	created, err := h.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId: "tenant-a", ProjectId: "proj-a", WorkspaceRoot: t.TempDir(), Goal: "g",
	})
	if err != nil {
		t.Fatalf("create with flag on: %v", err)
	}
	if !created.Task.FsmEnabled {
		t.Fatal("task must record fsm_enabled=true when the flag is on")
	}

	h.flags = nil
	h.cfg.Harness.Enabled = false

	got, err := h.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: "tenant-a", TaskId: created.Task.TaskId})
	if err != nil {
		t.Fatalf("historical task must remain readable: %v", err)
	}
	if !got.FsmEnabled {
		t.Fatal("fsm_enabled must stay frozen after the flag is turned off")
	}

	if _, err := h.ListCheckpoints(ctx, &harnesspb.ListCheckpointsRequest{TaskId: created.Task.TaskId}); err != nil {
		t.Fatalf("checkpoints must remain readable: %v", err)
	}

	_, err = h.CreateTask(ctx, &harnesspb.CreateTaskRequest{WorkspaceRoot: t.TempDir(), Goal: "g"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("new task must be rejected when disabled: %v", err)
	}
}
