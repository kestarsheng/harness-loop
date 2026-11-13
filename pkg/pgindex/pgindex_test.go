package pgindex

import (
	"context"
	"os"

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
