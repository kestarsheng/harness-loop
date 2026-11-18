package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/repository"
)

// T-P2-29：completed_with_failed_subtasks 必须真的能落库——DB 的 terminal_reason
// CHECK 若还是三值，这一写入会 23514 失败，恰好是"库里 completed、事件里
// completed_with_failed_subtasks"不一致的根源。本用例同时钉住：
//   1) 新终态可写可读；
//   2) 非法终态仍被 CHECK 拒绝（约束没有被整体放开）；
//   3) 事件 payload 与库里读到的是同一个 reason。
func TestTaskRepoPersistsToleratedTerminalReason(t *testing.T) {
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

	repo := repository.NewTaskRepo(db)
	stamp := time.Now().Format("150405.000000000")
	task := &repository.TaskRun{
		TaskID:         "task-term-" + stamp,
		TenantID:       "tenant-p2-parent-terminal",
		ProjectID:      "proj-p2-parent-terminal",
		WorkspaceRoot:  "/tmp/ws-p2",
		Goal:           "parent terminal waits for subtask closure",
		GoalHash:       "hash-" + stamp,
		CurrentStage:   fsm.StageVerify,
		Status:         repository.TaskRunning,
		BudgetTokens:   100000,
		BudgetCost:     10,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	updated, err := repo.UpdateStatus(ctx, task.TenantID, task.TaskID,
		repository.TaskCompleted, ReasonCompletedWithFailedSubtask, task.Version)
	if err != nil {
		t.Fatalf("persist %s: %v (the 016 migration must allow this reason)",
			ReasonCompletedWithFailedSubtask, err)
	}
	if updated.TerminalReason != ReasonCompletedWithFailedSubtask || updated.Status != repository.TaskCompleted {
		t.Fatalf("updated row = %s/%s", updated.Status, updated.TerminalReason)
	}

	reloaded, err := repo.Get(ctx, task.TenantID, task.TaskID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.TerminalReason != ReasonCompletedWithFailedSubtask {
		t.Fatalf("db terminal_reason = %q, want %q", reloaded.TerminalReason, ReasonCompletedWithFailedSubtask)
	}

	// 事件与 DB 用同一份 terminal_reason：payload 由 finalize 统一生成。
	failed := []orchestrator.FailedSubtaskRef{{SubtaskID: "st-3", DagNodeID: "n3", Status: "failed", Reason: "failed", Strategy: "tolerate"}}
	var payload map[string]any
	if err := json.Unmarshal(terminalPayload(reloaded.TerminalReason, failed), &payload); err != nil {
		t.Fatalf("decode terminal payload: %v", err)
	}
	if payload["terminal_reason"] != reloaded.TerminalReason {
		t.Fatalf("event reason %v != db reason %s", payload["terminal_reason"], reloaded.TerminalReason)
	}
	if items, ok := payload["failed_subtasks"].([]any); !ok || len(items) != 1 {
		t.Fatalf("event payload must list failed_subtasks: %+v", payload)
	}

	// 约束没有被整体放开：未登记的终态仍然写不进去。
	if _, err := repo.UpdateStatus(ctx, task.TenantID, task.TaskID,
		repository.TaskCompleted, "completed_but_whatever", reloaded.Version); err == nil {
		t.Fatal("an unregistered terminal reason must still be rejected by the CHECK constraint")
	}
}
