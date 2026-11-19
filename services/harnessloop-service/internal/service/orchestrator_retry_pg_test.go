package service

import (
	"context"
	"testing"

	"agent-platform/pkg/harnessloop/subtask"
	"agent-platform/services/harnessloop-service/internal/repository"
)

// 重试必须落库：d→c 回退后 failed 子任务重新执行并 accepted，数据库里的
// status/attempt_no 要和调度器、事件说同一件事（不允许内存里重试、库里还是 failed）。
func TestBridgeRetryPersistsAttemptAndStatus(t *testing.T) {
	repo, taskID := setupBridgeRepo(t)
	exec := newFlakyExecutor(map[string]int{"n1": 1}) // 第一次失败，重跑成功
	bridge := newWriteBackBridge(t, repo, exec)

	ec := OrchestratorStageContext{
		TenantID: "tenant-p2-subtask", ProjectID: "proj-p2-subtask",
		TaskID: taskID, Stage: "b", AttemptNo: 1, WorkspaceRoot: t.TempDir(),
	}
	if _, err := bridge.PlanBStage(context.Background(), ec, []byte(retrySingleNodePlan)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	ec.Stage = "c"
	if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
		t.Fatalf("dispatch 1: %v", err)
	}
	row := retryRow(t, repo, ec)
	if row.Status != string(subtask.StatusFailed) || row.AttemptNo != 1 {
		t.Fatalf("after the first failure: status=%s attempt_no=%d, want failed/1", row.Status, row.AttemptNo)
	}
	ec.Stage = "d"
	if _, err := bridge.AggregateDStage(context.Background(), ec); err == nil {
		t.Fatalf("a failed child must not close the d stage")
	}

	// c 阶段重入：failed → pending → running → accepted，全部走同一条写回通道。
	ec.Stage = "c"
	if _, err := bridge.DispatchCStage(context.Background(), ec); err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	row = retryRow(t, repo, ec)
	if row.Status != string(subtask.StatusAccepted) {
		t.Fatalf("status = %s, want accepted", row.Status)
	}
	if row.AttemptNo != 2 {
		t.Fatalf("attempt_no = %d, want 2 (the retry opened a second attempt)", row.AttemptNo)
	}
	if row.AcceptanceID == "" {
		t.Fatalf("an accepted row must carry its acceptance_id")
	}
}

func retryRow(t *testing.T, repo *repository.SubtaskRepo, ec OrchestratorStageContext) *repository.SubtaskRow {
	t.Helper()
	rows, err := repo.List(context.Background(), ec.TenantID, ec.ProjectID, ec.TaskID, 10)
	if err != nil {
		t.Fatalf("list subtasks: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("subtask rows = %d, want 1", len(rows))
	}
	return rows[0]
}
