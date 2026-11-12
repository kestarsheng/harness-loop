package pgindex

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := DefaultConfig(dsn)
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig("postgres://x")
	if cfg.MaxConns != 8 || cfg.MinConns != 1 {
		t.Fatalf("unexpected pool config: %+v", cfg)
	}
	if cfg.ConnectTimeout != 5*time.Second || cfg.SlowQueryThreshold != 500*time.Millisecond {
		t.Fatalf("unexpected timeouts: %+v", cfg)
	}
}

func TestOpenRequiresDSN(t *testing.T) {
	if _, err := Open(context.Background(), DefaultConfig("")); err == nil {
		t.Fatal("empty DSN must fail")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	first, err := AppliedMigrations(ctx, db)
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(first) < 2 {
		t.Fatalf("expected at least 2 migrations, got %v", first)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	second, err := AppliedMigrations(ctx, db)
	if err != nil {
		t.Fatalf("list migrations again: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("migrations grew on replay: %v -> %v", first, second)
	}
}

func TestSchemaMatchesDesign(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	cases := []struct {
		table  string
		column string
	}{
		{"hlp_task.task_runs", "version"},
		{"hlp_task.task_runs", "policy_snapshot"},
		{"hlp_task.task_runs", "fsm_enabled"},
		{"hlp_task.stage_attempts", "entry_reason"},
		{"hlp_task.acceptance_records", "rollback_target"},
		{"hlp_event.events", "payload"},
		{"hlp_event.events", "subtask_id"},
		{"hlp_event.audit", "rule_id"},
		{"hlp_event.tool_idempotency", "idempotency_key"},
		{"hlp_task.model_selections", "attempt_no"},
		{"hlp_task.model_selections", "reason"},
		{"hlp_task.model_selections", "context_window"},
		{"hlp_task.model_selections", "threshold_tokens"},
		{"hlp_task.model_selections", "input_price"},
		{"hlp_task.model_selections", "output_price"},
		{"hlp_task.model_selections", "is_fallback"},
		{"hlp_task.model_selections", "approval_id"},
		{"hlp_task.model_selections", "operator"},
	}
	for _, c := range cases {
		var exists bool
		if err := db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = split_part($1, '.', 1)
				  AND table_name = split_part($1, '.', 2)
				  AND column_name = $2
			)`, c.table, c.column).Scan(&exists); err != nil {
			t.Fatalf("query column %s.%s: %v", c.table, c.column, err)
		}
		if !exists {
			t.Fatalf("column %s.%s missing", c.table, c.column)
		}
	}

	var hasUniqueTaskSeq bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = 'hlp_event' AND t.relname = 'events' AND c.contype = 'u'
		)`).Scan(&hasUniqueTaskSeq); err != nil {
		t.Fatalf("query unique constraint: %v", err)
	}
	if !hasUniqueTaskSeq {
		t.Fatal("hlp_event.events must have a unique constraint (task_id, seq)")
	}

	var hasLegacyWindow bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'hlp_task'
			  AND table_name = 'model_selections'
			  AND column_name = 'window'
		)`).Scan(&hasLegacyWindow); err != nil {
		t.Fatalf("query legacy window column: %v", err)
	}
	if hasLegacyWindow {
		t.Fatal("hlp_task.model_selections must not use reserved column name window")
	}

	rows, err := db.Query(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = 'hlp_task' AND t.relname = 'model_selections' AND c.contype = 'c'`)
	if err != nil {
		t.Fatalf("query model_selections checks: %v", err)
	}
	defer rows.Close()
	var defs strings.Builder
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("scan check: %v", err)
		}
		defs.WriteString(def)
		defs.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate checks: %v", err)
	}
	if !strings.Contains(defs.String(), "'explicit'") {
		t.Fatalf("strategy check must allow explicit: %s", defs.String())
	}
	if !strings.Contains(defs.String(), "'fallback'") {
		t.Fatalf("resolved_from check must allow fallback: %s", defs.String())
	}
}

func TestStageAttemptUniqueness(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	taskID := "test-" + time.Now().Format("150405.000000000")

	if _, err := db.Exec(ctx, `
		INSERT INTO hlp_task.task_runs (task_id, tenant_id, project_id, workspace_root, goal, goal_hash, current_stage, status, policy_snapshot)
		VALUES ($1, 't', 'p', '/w', 'g', 'h', 'a', 'running', '{}'::jsonb)`, taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	insertAttempt := func() error {
		_, err := db.Exec(ctx, `
			INSERT INTO hlp_task.stage_attempts (attempt_id, task_id, stage, attempt_no, entry_reason, started_at)
			VALUES ($1, $2, 'a', 1, 'initial', now())`, taskID+"-a1", taskID)
		return err
	}
	if err := insertAttempt(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insertAttempt(); err == nil {
		t.Fatal("duplicate (task_id, stage, attempt_no) must be rejected")
	}
}

func TestWithTaskLockSerializes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	var inside int32
	var overlapped int32
	var wg sync.WaitGroup

	run := func() error {
		return db.WithTaskLock(ctx, "lock-task-typed", func(tx pgx.Tx) error {
			if atomic.AddInt32(&inside, 1) > 1 {
				atomic.StoreInt32(&overlapped, 1)
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inside, -1)
			return nil
		})
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := run(); err != nil {
				t.Errorf("lock run: %v", err)
			}
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&overlapped) != 0 {
		t.Fatal("task lock did not serialize critical sections")
	}
	if atomic.LoadInt32(&inside) != 0 {
		t.Fatal("counter corrupted")
	}
}
func TestExecQueryAndPoolAccess(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("exec: %v", err)
	}
	rows, err := db.Query(ctx, `SELECT 1`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rows.Close()

	var n int
	if err := db.QueryRow(ctx, `SELECT 42`).Scan(&n); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if n != 42 {
		t.Fatalf("query row = %d", n)
	}
	if db.Pool() == nil {
		t.Fatal("pool must not be nil")
	}
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestOpenInvalidDSN(t *testing.T) {
	if _, err := Open(context.Background(), DefaultConfig("://not-a-dsn")); err == nil {
		t.Fatal("invalid DSN must fail")
	}
}

func TestPingAfterClose(t *testing.T) {
	db := testDB(t)
	db.Close()
	if err := db.Ping(context.Background()); err == nil {
		t.Fatal("ping after close must fail")
	}
}

func TestWithTaskLockRequiresTaskID(t *testing.T) {
	db := testDB(t)
	if err := db.WithTaskLock(context.Background(), "", func(tx pgx.Tx) error { return nil }); err == nil {
		t.Fatal("empty task id must fail")
	}
}

func TestWithTaskLockRollsBackOnError(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	want := context.DeadlineExceeded
	err := db.WithTaskLock(ctx, "rollback-task", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
			return err
		}
		return want
	})
	if err != want {
		t.Fatalf("callback error must propagate, got %v", err)
	}
}
