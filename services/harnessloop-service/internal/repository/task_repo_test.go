package repository

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/pgindex"
)

func setupTask(t *testing.T) (*pgindex.DB, *TaskRepo, *TaskRun) {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		if os.Getenv("HARNESS_REQUIRE_PG") == "1" {
			t.Fatal("HARNESS_TEST_PG_DSN is required when HARNESS_REQUIRE_PG=1")
		}
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

	task := &TaskRun{
		TaskID:         "task-" + time.Now().Format("150405.000000000"),
		TenantID:       "tenant-a",
		ProjectID:      "proj-a",
		WorkspaceRoot:  "/tmp/ws",
		Goal:           "goal",
		GoalHash:       "hash",
		CurrentStage:   fsm.StageRequirement,
		Status:         TaskRunning,
		BudgetTokens:   100000,
		BudgetCost:     10,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}
	repo := NewTaskRepo(db)
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	return db, repo, task
}

func TestTaskCreateAndTenantIsolation(t *testing.T) {
	_, repo, task := setupTask(t)
	ctx := context.Background()

	got, err := repo.Get(ctx, task.TenantID, task.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CurrentStage != fsm.StageRequirement || got.Version != 0 || got.Status != TaskRunning {
		t.Fatalf("unexpected task: %+v", got)
	}
	var policy map[string]any
	if err := json.Unmarshal(got.PolicySnapshot, &policy); err != nil {
		t.Fatalf("policy snapshot is not json: %v", err)
	}
	if policy["mode"] != "enforce" {
		t.Fatalf("policy snapshot = %s", got.PolicySnapshot)
	}

	if _, err := repo.Get(ctx, "tenant-b", task.TaskID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("cross-tenant get must fail: %v", err)
	}
}

func TestTaskCreateRejectsInvalid(t *testing.T) {
	_, repo, _ := setupTask(t)
	ctx := context.Background()
	if err := repo.Create(ctx, &TaskRun{TenantID: "t", ProjectID: "p", CurrentStage: fsm.StageRequirement}); err == nil {
		t.Fatal("missing task id must fail")
	}
	if err := repo.Create(ctx, &TaskRun{TaskID: "x", TenantID: "t", ProjectID: "p", CurrentStage: fsm.Stage("z")}); err == nil {
		t.Fatal("invalid stage must fail")
	}
}

func TestTransferCAS(t *testing.T) {
	_, repo, task := setupTask(t)
	ctx := context.Background()

	var success int32
	var conflicts int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ev := []EventInsert{{
				EventID:   task.TaskID + "-ev-" + string(rune('a'+idx)),
				Seq:       int64(idx + 1),
				EventType: "stage.transition",
				Severity:  "info",
				Stage:     fsm.StagePlan,
				Payload:   []byte(`{"from":"a","to":"b"}`),
			}}
			if _, err := repo.Transfer(ctx, task.TenantID, task.TaskID, fsm.StagePlan, 0, ev); err != nil {
				if errors.Is(err, ErrVersionConflict) {
					atomic.AddInt32(&conflicts, 1)
					return
				}
				t.Errorf("transfer: %v", err)
				return
			}
			atomic.AddInt32(&success, 1)
		}(i)
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("expected exactly one successful transfer, got %d (conflicts=%d)", success, conflicts)
	}
	if conflicts != 9 {
		t.Fatalf("expected 9 conflicts, got %d", conflicts)
	}
	got, err := repo.Get(ctx, task.TenantID, task.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CurrentStage != fsm.StagePlan {
		t.Fatalf("stage = %s", got.CurrentStage)
	}
	if got.Version != 1 {
		t.Fatalf("version = %d", got.Version)
	}
}

func TestTransferWritesEventsAtomically(t *testing.T) {
	db, repo, task := setupTask(t)
	ctx := context.Background()

	ev := []EventInsert{{
		EventID:   task.TaskID + "-e1",
		Seq:       1,
		EventType: "stage.transition",
		Severity:  "info",
		Stage:     fsm.StagePlan,
		Payload:   []byte(`{"from":"a","to":"b"}`),
	}}
	if _, err := repo.Transfer(ctx, task.TenantID, task.TaskID, fsm.StagePlan, 0, ev); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM hlp_event.events WHERE task_id = $1`, task.TaskID).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Fatalf("events = %d", count)
	}

	_, err := repo.Transfer(ctx, task.TenantID, task.TaskID, fsm.StageExecute, 0, []EventInsert{{
		EventID:   task.TaskID + "-e2",
		Seq:       2,
		EventType: "stage.transition",
		Payload:   []byte(`{"from":"a","to":"c"}`),
	}})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale transfer: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM hlp_event.events WHERE task_id = $1`, task.TaskID).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Fatalf("conflicting transfer must not persist events, events = %d", count)
	}
}

func TestTransferRejectsUnknownTask(t *testing.T) {
	_, repo, _ := setupTask(t)
	_, err := repo.Transfer(context.Background(), "tenant-a", "no-such-task", fsm.StagePlan, 0, nil)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
}

func TestUpdateStatusAndConsumed(t *testing.T) {
	_, repo, task := setupTask(t)
	ctx := context.Background()

	updated, err := repo.UpdateStatus(ctx, task.TenantID, task.TaskID, TaskAborted, "aborted", 0)
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if updated.Status != TaskAborted || updated.TerminalReason != "aborted" || updated.Version != 1 {
		t.Fatalf("unexpected task: %+v", updated)
	}
	if _, err := repo.UpdateStatus(ctx, task.TenantID, task.TaskID, TaskPaused, "", 0); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale status update must conflict: %v", err)
	}
	if _, err := repo.UpdateStatus(ctx, task.TenantID, task.TaskID, TaskStatus("bad"), "", 1); err == nil {
		t.Fatal("invalid status must fail")
	}

	if err := repo.AddConsumed(ctx, task.TenantID, task.TaskID, 120, 0.5); err != nil {
		t.Fatalf("add consumed: %v", err)
	}
	got, err := repo.Get(ctx, task.TenantID, task.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ConsumedTokens != 120 || got.ConsumedCost != 0.5 {
		t.Fatalf("consumed = %d / %v", got.ConsumedTokens, got.ConsumedCost)
	}
}

func TestUpdateAttemptNo(t *testing.T) {
	_, repo, task := setupTask(t)
	ctx := context.Background()
	if err := repo.UpdateAttemptNo(ctx, task.TenantID, task.TaskID, fsm.StageExecute, 2, 0); err != nil {
		t.Fatalf("update attempt no: %v", err)
	}
	got, err := repo.Get(ctx, task.TenantID, task.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AttemptNo["c"] != 2 || got.Version != 1 {
		t.Fatalf("attempt map = %v version=%d", got.AttemptNo, got.Version)
	}
}

func TestAbortLock(t *testing.T) {
	_, repo, task := setupTask(t)
	ctx := context.Background()

	err := repo.AbortLock(ctx, task.TenantID, task.TaskID, func(tx pgx.Tx, tr *TaskRun) error {
		if tr.TaskID != task.TaskID {
			t.Fatalf("locked wrong task: %s", tr.TaskID)
		}
		_, err := tx.Exec(ctx, `
			UPDATE hlp_task.task_runs SET status = 'aborted', terminal_reason = 'aborted', version = version + 1
			WHERE task_id = $1`, task.TaskID)
		return err
	})
	if err != nil {
		t.Fatalf("abort lock: %v", err)
	}
	got, err := repo.Get(ctx, task.TenantID, task.TaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != TaskAborted {
		t.Fatalf("status = %s", got.Status)
	}

	if err := repo.AbortLock(ctx, task.TenantID, "missing", func(tx pgx.Tx, tr *TaskRun) error { return nil }); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestReconcileDetectsDrift(t *testing.T) {
	db, repo, task := setupTask(t)
	ctx := context.Background()

	if _, err := repo.Transfer(ctx, task.TenantID, task.TaskID, fsm.StagePlan, 0, []EventInsert{{
		EventID:   task.TaskID + "-r1",
		Seq:       1,
		EventType: "stage.transition",
		Payload:   []byte(`{"from":"a","to":"b"}`),
	}}); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	res, err := repo.Reconcile(ctx, task.TenantID, task.TaskID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Drift || res.SnapshotStage != fsm.StagePlan || res.EventStage != fsm.StagePlan {
		t.Fatalf("unexpected reconcile: %+v", res)
	}
	if res.MaxSeq != 1 {
		t.Fatalf("max seq = %d", res.MaxSeq)
	}

	if _, err := db.Exec(ctx, `UPDATE hlp_task.task_runs SET current_stage = 'a' WHERE task_id = $1`, task.TaskID); err != nil {
		t.Fatalf("force drift: %v", err)
	}
	res, err = repo.Reconcile(ctx, task.TenantID, task.TaskID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Drift {
		t.Fatalf("drift must be detected: %+v", res)
	}
}

func TestTaskList(t *testing.T) {
	_, repo, task := setupTask(t)
	ctx := context.Background()

	items, err := repo.List(ctx, task.TenantID, "", nil, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected at least one task")
	}
	items, err = repo.List(ctx, "tenant-b", "", nil, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("cross-tenant list must be empty, got %d", len(items))
	}
}
