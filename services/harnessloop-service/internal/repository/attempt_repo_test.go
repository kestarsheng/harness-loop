package repository

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/pgindex"
)

func setupRepo(t *testing.T) (*pgindex.DB, *AttemptRepo, string) {
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

	taskID := "t-" + time.Now().Format("150405.000000000")
	if _, err := db.Exec(ctx, `
		INSERT INTO hlp_task.task_runs
			(task_id, tenant_id, project_id, workspace_root, goal, goal_hash, current_stage, status, policy_snapshot)
		VALUES ($1, 'tenant-a', 'proj-a', '/tmp/ws', 'goal', 'hash', 'c', 'running', '{}'::jsonb)`, taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return db, NewAttemptRepo(db), taskID
}

func mustAttempt(t *testing.T, taskID string, stage fsm.Stage, no int, reason fsm.EntryReason) fsm.Attempt {
	t.Helper()
	a, err := fsm.NewAttempt(taskID, stage, no, reason, time.Now())
	if err != nil {
		t.Fatalf("NewAttempt: %v", err)
	}
	return a
}

func TestCreateAndDuplicateRejected(t *testing.T) {
	_, repo, taskID := setupRepo(t)
	ctx := context.Background()
	a := mustAttempt(t, taskID, fsm.StageExecute, 1, fsm.EntryInitial)

	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Create(ctx, a); err == nil {
		t.Fatal("duplicate (task_id, stage, attempt_no) must be rejected")
	}
}

func TestNextAttemptNo(t *testing.T) {
	_, repo, taskID := setupRepo(t)
	ctx := context.Background()

	no, err := repo.NextAttemptNo(ctx, taskID, fsm.StageExecute)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if no != 1 {
		t.Fatalf("first next = %d", no)
	}
	if err := repo.Create(ctx, mustAttempt(t, taskID, fsm.StageExecute, no, fsm.EntryInitial)); err != nil {
		t.Fatalf("create: %v", err)
	}
	no, err = repo.NextAttemptNo(ctx, taskID, fsm.StageExecute)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if no != 2 {
		t.Fatalf("second next = %d", no)
	}
}

func TestAttachAcceptanceRequiresFinish(t *testing.T) {
	_, repo, taskID := setupRepo(t)
	ctx := context.Background()
	a := mustAttempt(t, taskID, fsm.StageVerify, 1, fsm.EntryInitial)
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.AttachAcceptance(ctx, a.AttemptID, "acc-1"); !errors.Is(err, ErrAttemptNotFinished) {
		t.Fatalf("attach before finish: %v", err)
	}
	if err := repo.Finish(ctx, a.AttemptID, time.Now()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := repo.AttachAcceptance(ctx, a.AttemptID, "acc-1"); err != nil {
		t.Fatalf("attach after finish: %v", err)
	}
	if err := repo.AttachAcceptance(ctx, a.AttemptID, ""); !errors.Is(err, ErrAttemptNotFinished) {
		t.Fatalf("empty acceptance id: %v", err)
	}
}

func TestDatabaseRejectsAcceptanceBeforeFinish(t *testing.T) {
	db, repo, taskID := setupRepo(t)
	ctx := context.Background()
	a := mustAttempt(t, taskID, fsm.StagePlan, 1, fsm.EntryInitial)
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := db.Exec(ctx, `
		UPDATE hlp_task.stage_attempts SET acceptance_id = 'acc' WHERE attempt_id = $1`, a.AttemptID)
	if err == nil {
		t.Fatal("db constraint must reject acceptance_id while finish_received=false")
	}
}

func TestCStageReentryAfterRollback(t *testing.T) {
	db, repo, taskID := setupRepo(t)
	ctx := context.Background()

	c1 := mustAttempt(t, taskID, fsm.StageExecute, 1, fsm.EntryInitial)
	if err := repo.Create(ctx, c1); err != nil {
		t.Fatalf("create c1: %v", err)
	}
	if err := repo.Finish(ctx, c1.AttemptID, time.Now()); err != nil {
		t.Fatalf("finish c1: %v", err)
	}
	if err := repo.AttachAcceptance(ctx, c1.AttemptID, "acc-c1"); err != nil {
		t.Fatalf("attach c1: %v", err)
	}

	no, err := repo.NextAttemptNo(ctx, taskID, fsm.StageExecute)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if no != 2 {
		t.Fatalf("after rollback next = %d, want 2", no)
	}
	c2 := mustAttempt(t, taskID, fsm.StageExecute, no, fsm.EntryAcceptanceFailed)
	if err := repo.Create(ctx, c2); err != nil {
		t.Fatalf("create c2: %v", err)
	}

	attempts, err := repo.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d", len(attempts))
	}
	if attempts[0].AttemptNo != 1 || attempts[1].AttemptNo != 2 {
		t.Fatalf("attempt order = %d, %d", attempts[0].AttemptNo, attempts[1].AttemptNo)
	}
	if attempts[1].EntryReason != fsm.EntryAcceptanceFailed {
		t.Fatalf("c2 entry_reason = %q", attempts[1].EntryReason)
	}
	if !attempts[0].FinishReceived || attempts[0].AcceptanceID != "acc-c1" {
		t.Fatalf("c1 state = %+v", attempts[0])
	}

	var endedAtNull bool
	if err := db.QueryRow(ctx, `SELECT ended_at IS NULL FROM hlp_task.stage_attempts WHERE attempt_id = $1`, c2.AttemptID).Scan(&endedAtNull); err != nil {
		t.Fatalf("query ended_at: %v", err)
	}
	if !endedAtNull {
		t.Fatal("unfinished attempt must have null ended_at")
	}
}
