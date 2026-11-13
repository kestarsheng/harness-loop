package service

import (
	"context"
	"os"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/repository"
)

// TestModelBridgeWritesRealModelSelectionRow proves the production recorder
// path writes hlp_task.model_selections with a real PostgreSQL connection
// instead of only a fake writer.
func TestModelBridgeWritesRealModelSelectionRow(t *testing.T) {
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
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
	task := &repository.TaskRun{
		TaskID:         "task-model-" + time.Now().Format("150405.000000000"),
		TenantID:       "tenant-a",
		ProjectID:      "proj-a",
		WorkspaceRoot:  t.TempDir(),
		Goal:           "verify model selection persistence",
		GoalHash:       "hash",
		CurrentStage:   fsm.StageRequirement,
		Status:         repository.TaskRunning,
		BudgetTokens:   100000,
		BudgetCost:     10,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}
	if err := taskRepo.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	reg := modelregistry.DefaultRegistry()
	selectionRepo := repository.NewModelSelectionRepo(db)
	bridge := NewModelBridge(true, reg, "balanced", selectionRepo, nil)
	ec := ModelStageContext{
		TenantID: task.TenantID, ProjectID: task.ProjectID, TaskID: task.TaskID,
		Stage: "c", AttemptNo: 1,
	}
	choice := &ModelChoice{
		ModelID: "qwen-max", Window: 131072, ThresholdTokens: 104857,
		Strategy: "balanced", ResolvedFrom: "strategy",
	}
	bridge.record(ctx, reg, choice, ec, []modelregistry.CandidateScore{{ModelID: "qwen-max", Score: 0.9}})

	var (
		count        int
		modelID      string
		strategy     string
		resolvedFrom string
		attemptNo    int
		stage        string
	)
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM hlp_task.model_selections WHERE task_id = $1`, task.TaskID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		if count == 0 {
			t.Fatal("ModelBridge did not write a model_selections row")
		}
		t.Fatalf("rows = %d", count)
	}
	if err := db.Pool().QueryRow(ctx, `
		SELECT model_id, strategy, resolved_from, attempt_no, stage
		FROM hlp_task.model_selections WHERE task_id = $1`, task.TaskID).Scan(
		&modelID, &strategy, &resolvedFrom, &attemptNo, &stage); err != nil {
		t.Fatalf("select: %v", err)
	}
	if modelID != "qwen-max" || strategy != "balanced" || resolvedFrom != "strategy" || attemptNo != 1 || stage != "c" {
		t.Fatalf("row = model=%s strategy=%s resolved_from=%s attempt=%d stage=%s",
			modelID, strategy, resolvedFrom, attemptNo, stage)
	}
}
