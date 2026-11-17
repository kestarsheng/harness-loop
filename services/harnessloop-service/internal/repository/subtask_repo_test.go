package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"agent-platform/pkg/harnessloop/fsm"
)

func createSubtask(t *testing.T, db *pgxpool.Pool, subtaskID, parentTaskID, tenantID, projectID, dagID, nodeID string, index int, role, status string, dependsOn string) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		INSERT INTO hlp_task.subtasks
			(subtask_id, parent_task_id, tenant_id, project_id, dag_id, dag_node_id, depth, index_in_parent,
			 parallel_set_id, role, status, depends_on, input_contract, output_contract, acceptance_criteria,
			 acceptance_id, failure_strategy, attempt_no, artifact_refs, handoff_id, alloc_tokens, consumed_tokens, consumed_cost)
		VALUES ($1,$2,$3,$4,$5,$6,1,$7,NULL,$8,$9,$10::jsonb,'{"goal":"g"}'::jsonb,'{"out":"o"}'::jsonb,
			'[{"criterion_id":"c-1","required":true}]'::jsonb,
			CASE WHEN $9 = 'accepted' THEN 'acc-1' ELSE NULL END,'fail-fast',1,'["artifacts/a.txt"]'::jsonb,NULL,100,40,0.5)`,
		subtaskID, parentTaskID, tenantID, projectID, dagID, nodeID, index, role, status, dependsOn)
	if err != nil {
		t.Fatalf("insert subtask %s: %v", subtaskID, err)
	}
}

// TestSubtaskRepoUpdateExecutionPersistsTransitions covers the write-back the
// scheduler performs at its real transition points: pending→running carries the
// preliminary usage, running→accepted carries artifacts + acceptance, and
// terminal rows can never be rewound.
func TestSubtaskRepoUpdateExecutionPersistsTransitions(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewSubtaskRepo(db)
	ctx := context.Background()
	subtaskID := "st-write-" + task.TaskID
	createSubtask(t, db.Pool(), subtaskID, task.TaskID, task.TenantID, task.ProjectID, "dag-w", "n-w", 0, "coder", "pending", `[]`)

	if err := repo.UpdateExecution(ctx, SubtaskExecution{
		SubtaskID: subtaskID, FromStatus: "pending", ToStatus: "running",
		AttemptNo: 1, ConsumedTokens: 256, ConsumedCost: 0.001,
	}); err != nil {
		t.Fatalf("pending->running: %v", err)
	}
	if err := repo.UpdateExecution(ctx, SubtaskExecution{
		SubtaskID: subtaskID, FromStatus: "running", ToStatus: "accepted",
		AttemptNo: 1, ConsumedTokens: 1024, ConsumedCost: 0.25,
		ArtifactRefs: []byte(`[{"path":"out/result.txt","hash":"sha256:abc","size":12}]`),
		AcceptanceID: "acc-" + subtaskID,
	}); err != nil {
		t.Fatalf("running->accepted: %v", err)
	}

	rows, err := repo.List(ctx, task.TenantID, task.ProjectID, task.TaskID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	row := rows[0]
	if row.Status != "accepted" || row.AcceptanceID != "acc-"+subtaskID {
		t.Fatalf("row status/acceptance = %q/%q", row.Status, row.AcceptanceID)
	}
	if row.ConsumedTokens != 1024 || row.ConsumedCost != 0.25 || row.AttemptNo != 1 {
		t.Fatalf("consumption = tokens:%d cost:%v attempt:%d", row.ConsumedTokens, row.ConsumedCost, row.AttemptNo)
	}
	if !strings.Contains(string(row.ArtifactRefs), "out/result.txt") {
		t.Fatalf("artifact refs = %s", row.ArtifactRefs)
	}

	// No rewind: the state machine rejects accepted→running before touching SQL.
	if err := repo.UpdateExecution(ctx, SubtaskExecution{SubtaskID: subtaskID, FromStatus: "accepted", ToStatus: "running"}); !errors.Is(err, ErrSubtaskTransitionIllegal) {
		t.Fatalf("rewind error = %v, want ErrSubtaskTransitionIllegal", err)
	}
	// Stale from_status: the row is accepted, so a pending→running write (e.g. a
	// second replica replaying a queued dispatch) must be refused, not applied.
	if err := repo.UpdateExecution(ctx, SubtaskExecution{SubtaskID: subtaskID, FromStatus: "pending", ToStatus: "running"}); !errors.Is(err, ErrSubtaskStatusConflict) {
		t.Fatalf("stale write error = %v, want ErrSubtaskStatusConflict", err)
	}
	// Idempotent retry of the same transition is a success, not a conflict.
	if err := repo.UpdateExecution(ctx, SubtaskExecution{
		SubtaskID: subtaskID, FromStatus: "running", ToStatus: "accepted",
		AcceptanceID: "acc-" + subtaskID,
	}); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
}

func TestSubtaskRepoUpdateExecutionFailurePaths(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewSubtaskRepo(db)
	subtaskID := "st-fail-" + task.TaskID
	createSubtask(t, db.Pool(), subtaskID, task.TaskID, task.TenantID, task.ProjectID, "dag-f", "n-f", 0, "coder", "running", `[]`)

	// running→failed is a legal terminal write.
	if err := repo.UpdateExecution(context.Background(), SubtaskExecution{
		SubtaskID: subtaskID, FromStatus: "running", ToStatus: "failed", AttemptNo: 1, ConsumedTokens: 64,
	}); err != nil {
		t.Fatalf("running->failed: %v", err)
	}
	rows, err := repo.List(context.Background(), task.TenantID, task.ProjectID, task.TaskID, 5)
	if err != nil || len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}

	// accepted without an acceptance id violates the DB check; reject it here so
	// the caller gets the coded error instead of a constraint violation.
	other := "st-noid-" + task.TaskID
	createSubtask(t, db.Pool(), other, task.TaskID, task.TenantID, task.ProjectID, "dag-f2", "n-f2", 1, "coder", "running", `[]`)
	if err := repo.UpdateExecution(context.Background(), SubtaskExecution{SubtaskID: other, FromStatus: "running", ToStatus: "accepted"}); err == nil {
		t.Fatal("accepted without acceptance_id must be rejected")
	}

	// A database failure must surface, not be mistaken for a successful write.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := repo.UpdateStatus(cancelled, other, "running", "failed"); err == nil {
		t.Fatal("a cancelled context must surface the write failure")
	}
	// The row is untouched after the failed write: still running.
	rows, err = repo.List(context.Background(), task.TenantID, task.ProjectID, task.TaskID, 5)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, row := range rows {
		if row.SubtaskID == other && row.Status != "running" {
			t.Fatalf("failed write changed the row: %+v", row)
		}
	}
}

func createOtherTenantTask(t *testing.T, repo *TaskRepo, tenantID, projectID string) *TaskRun {
	t.Helper()
	task := &TaskRun{
		TaskID:         "task-xt-" + time.Now().Format("150405.000000000"),
		TenantID:       tenantID,
		ProjectID:      projectID,
		WorkspaceRoot:  "/tmp/ws-xt",
		Goal:           "other tenant goal",
		GoalHash:       "hash-xt",
		CurrentStage:   fsm.StageRequirement,
		Status:         TaskRunning,
		BudgetTokens:   1000,
		BudgetCost:     1,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}
	if err := repo.Create(context.Background(), task); err != nil {
		t.Fatalf("create other tenant task: %v", err)
	}
	return task
}

func TestSubtaskRepoListReturnsRealRowsAndIsolatesTenants(t *testing.T) {
	db, taskRepo, task := setupTask(t)
	repo := NewSubtaskRepo(db)
	ctx := context.Background()

	subA1 := "st-a-1-" + task.TaskID
	subA2 := "st-a-2-" + task.TaskID
	createSubtask(t, db.Pool(), subA1, task.TaskID, task.TenantID, task.ProjectID, "dag-1", "n-1", 0, "coder", "running", `[]`)
	createSubtask(t, db.Pool(), subA2, task.TaskID, task.TenantID, task.ProjectID, "dag-1", "n-2", 1, "reviewer", "accepted", `["n-1"]`)

	rows, err := repo.List(ctx, task.TenantID, task.ProjectID, task.TaskID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("subtasks = %d, want 2", len(rows))
	}
	first := rows[0]
	if first.SubtaskID != subA1 || first.Role != "coder" || first.Status != "running" {
		t.Fatalf("unexpected first row: %+v", first)
	}
	if len(first.DependsOn) != 0 {
		t.Fatalf("depends_on = %v, want empty", first.DependsOn)
	}
	if rows[1].Role != "reviewer" || rows[1].Status != "accepted" || rows[1].AcceptanceID != "acc-1" {
		t.Fatalf("unexpected second row: %+v", rows[1])
	}
	if len(rows[1].DependsOn) != 1 || rows[1].DependsOn[0] != "n-1" {
		t.Fatalf("depends_on = %v", rows[1].DependsOn)
	}
	if string(rows[1].InputContract) == "" || string(rows[1].ArtifactRefs) == "" {
		t.Fatalf("json columns not returned: %+v", rows[1])
	}
	if rows[1].ConsumedCost != 0.5 || rows[1].ConsumedTokens != 40 {
		t.Fatalf("consumption = %v/%d", rows[1].ConsumedCost, rows[1].ConsumedTokens)
	}

	otherTenant := "tenant-b-" + task.TaskID
	other := createOtherTenantTask(t, taskRepo, otherTenant, task.ProjectID)
	subB1 := "st-b-1-" + other.TaskID
	createSubtask(t, db.Pool(), subB1, other.TaskID, other.TenantID, other.ProjectID, "dag-b", "n-b", 0, "coder", "running", `[]`)

	rows, err = repo.List(ctx, otherTenant, task.ProjectID, other.TaskID, 10)
	if err != nil {
		t.Fatalf("list tenant-b: %v", err)
	}
	if len(rows) != 1 || rows[0].SubtaskID != subB1 {
		t.Fatalf("tenant-b rows = %+v", rows)
	}

	// tenant-b must not see tenant-a's parent task even when the task id is
	// supplied explicitly (design 9.9 isolation case).
	rows, err = repo.List(ctx, otherTenant, task.ProjectID, task.TaskID, 10)
	if err != nil {
		t.Fatalf("cross tenant list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("cross tenant rows = %d, want 0", len(rows))
	}

	rows, err = repo.List(ctx, task.TenantID, "another-project", task.TaskID, 10)
	if err != nil {
		t.Fatalf("cross project list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("cross project rows = %d, want 0", len(rows))
	}
}
