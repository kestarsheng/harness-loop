package repository

// T-P2-32 事件 seq 单写者回归测试。
//
// 缺陷复现链路：event.Publisher 内存 SeqAllocator 每个任务从 1 开始编号，
// runtime 的 stage.transition / task.failed 又走 TaskRepo.Transfer 直写库取
// max+1，两条写者各算各的 → 撞 (task_id, seq) 唯一键 → 撞车的事件被吞，
// 实测丢了 stage b/c 的 tool.denied 与 task.completed。
//
// 下面三条测试锁住修复后的契约：
//  1. seq 只由 EventRepo.Append 在写入事务内分配，并回填到入参 Event；
//  2. 调用方塞进来的 seq 不被采信（否则又是第二个分配者）；
//  3. 状态转移事件与普通事件共用同一条任务内序列，不重号、不留洞。

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	eventpkg "agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/pgindex"
)

// taskSeqs 读出某任务实际落库的 seq（升序）。
func taskSeqs(t *testing.T, db *pgindex.DB, tenantID, taskID string) []int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.Query(ctx, `SELECT seq FROM hlp_event.events WHERE tenant_id = $1 AND task_id = $2 ORDER BY seq`, tenantID, taskID)
	if err != nil {
		t.Fatalf("query seqs: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan seq: %v", err)
		}
		out = append(out, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("seq rows: %v", err)
	}
	return out
}

func TestEventRepoIsSingleSeqAllocator(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	const perWorker, workers = 5, 6
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		for i := 0; i < perWorker; i++ {
			wg.Add(1)
			go func(w, i int) {
				defer wg.Done()
				e := mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 0,
					"b", "tool.denied", time.Now())
				e.EventID = fmt.Sprintf("ev-conc-%d-%d", w, i)
				// Seq=0 表示「待库分配」——发布器不再自己编号。
				if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, []*eventpkg.Event{e}); err != nil {
					errs <- fmt.Errorf("append %d/%d: %w", w, i, err)
					return
				}
				if e.Seq <= 0 {
					errs <- fmt.Errorf("event %s was not backfilled with a seq", e.EventID)
				}
			}(w, i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}

	seqs := taskSeqs(t, db, task.TenantID, task.TaskID)
	if len(seqs) != workers*perWorker {
		t.Fatalf("stored events = %d, want %d (no event may be dropped)", len(seqs), workers*perWorker)
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("seq not contiguous at %d: %v", i, seqs)
		}
	}
}

func TestEventRepoIgnoresCallerSeq(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	events := []*eventpkg.Event{
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 42, "a", "stage.transition", time.Now()),
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 43, "b", "stage.transition", time.Now()),
	}
	events[0].EventID = "ev-forced-1"
	events[1].EventID = "ev-forced-2"
	if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, events); err != nil {
		t.Fatalf("append: %v", err)
	}
	// 调用方的 42/43 不被采信：库从 1 开始分配并回填。
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("backfilled seqs = %d,%d, want 1,2", events[0].Seq, events[1].Seq)
	}
	seqs := taskSeqs(t, db, task.TenantID, task.TaskID)
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("stored seqs = %v, want [1 2]", seqs)
	}
}

// 两条写路径（状态转移 / 普通事件）必须共用同一条任务内序列。
func TestEventSeqSharedByTransferAndAppend(t *testing.T) {
	db, tasks, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	stages := []fsm.Stage{fsm.StageRequirement, fsm.StagePlan, fsm.StageExecute, fsm.StageVerify, fsm.StageDone}
	task.Version = 0
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			_, err := tasks.Transfer(ctx, task.TenantID, task.TaskID, stages[(i/2)%len(stages)], task.Version, []EventInsert{{
				EventID:   fmt.Sprintf("%s-transfer-%d", task.TaskID, i),
				EventType: "stage.transition",
				Stage:     stages[(i/2)%len(stages)],
				Payload:   []byte(fmt.Sprintf(`{"i":%d}`, i)),
			}})
			if err != nil {
				t.Fatalf("transfer %d: %v", i, err)
			}
			got, err := tasks.Get(ctx, task.TenantID, task.TaskID)
			if err != nil {
				t.Fatalf("reload task: %v", err)
			}
			task.Version = got.Version
			continue
		}
		e := mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 0, "b", "tool.denied", time.Now())
		e.EventID = fmt.Sprintf("%s-append-%d", task.TaskID, i)
		if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, []*eventpkg.Event{e}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if e.Seq <= 0 {
			t.Fatalf("append %d was not backfilled with a seq", i)
		}
	}

	seqs := taskSeqs(t, db, task.TenantID, task.TaskID)
	if len(seqs) != 10 {
		t.Fatalf("stored events = %d, want 10 (both writers must land): %v", len(seqs), seqs)
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("seq not contiguous at %d: %v", i, seqs)
		}
	}
	if got := repo.Stats(); got.SeqConflicts != 0 {
		t.Fatalf("seq conflicts = %d, want 0 on the single-writer path", got.SeqConflicts)
	}
}
