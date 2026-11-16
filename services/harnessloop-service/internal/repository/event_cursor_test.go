package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	eventpkg "agent-platform/pkg/harnessloop/event"
)

// T-P2-17：事件游标必须是租户级 global_seq，不是任务内 seq。
// 这里的用例直接复现 50 并发漏事件的那个机制：新任务的 seq 又从 1 开始。

func cursorTenant(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("tenant-cursor-%d", time.Now().UnixNano())
}

func cursorEvent(t *testing.T, tenant, taskID string, seq int64, suffix string) *eventpkg.Event {
	t.Helper()
	return &eventpkg.Event{
		EventID:       fmt.Sprintf("ev-%s-%s-%s", tenant, taskID, suffix),
		EventType:     eventpkg.StageTransition,
		SchemaVersion: eventpkg.CurrentSchemaVersion,
		TaskID:        taskID,
		Seq:           seq,
		Timestamp:     time.Now().UTC(),
		Severity:      eventpkg.SeverityInfo,
		TenantID:      tenant,
		ProjectID:     "proj-cursor",
		Payload:       []byte(`{"ok":true}`),
	}
}

func TestWatchFromDeliversEventsOfBrandNewTask(t *testing.T) {
	db, _, _ := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()
	tenant := cursorTenant(t)
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM hlp_event.events WHERE tenant_id = $1`, tenant)
	})

	var first []*eventpkg.Event
	for i := 1; i <= 3; i++ {
		first = append(first, cursorEvent(t, tenant, "task-old", int64(i), fmt.Sprintf("old-%d", i)))
	}
	if _, err := repo.Append(ctx, tenant, "proj-cursor", first); err != nil {
		t.Fatalf("append old task: %v", err)
	}

	_, cursor, err := repo.WatchFrom(ctx, tenant, nil, 0, 500)
	if err != nil {
		t.Fatalf("watch old: %v", err)
	}
	if cursor != 3 {
		t.Fatalf("cursor after old task = %d, want 3", cursor)
	}

	// 新任务的 seq 从 1 重新开始——修复前它会被 `seq > cursor` 整段滤掉。
	var fresh []*eventpkg.Event
	for i := 1; i <= 3; i++ {
		fresh = append(fresh, cursorEvent(t, tenant, "task-new", int64(i), fmt.Sprintf("new-%d", i)))
	}
	if _, err := repo.Append(ctx, tenant, "proj-cursor", fresh); err != nil {
		t.Fatalf("append new task: %v", err)
	}

	got, next, err := repo.WatchFrom(ctx, tenant, nil, cursor, 500)
	if err != nil {
		t.Fatalf("watch new: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("new task events = %d, want 3 (cursor semantics still per-task?)", len(got))
	}
	for i, e := range got {
		if e.TaskID != "task-new" {
			t.Fatalf("event %d belongs to %s, want task-new", i, e.TaskID)
		}
		if want := cursor + int64(i) + 1; e.GlobalSeq != want {
			t.Fatalf("event %d global_seq = %d, want %d", i, e.GlobalSeq, want)
		}
		if e.Seq != int64(i+1) {
			t.Fatalf("per-task seq must stay untouched, got %d want %d", e.Seq, i+1)
		}
	}
	if next != cursor+3 {
		t.Fatalf("next cursor = %d, want %d", next, cursor+3)
	}
}

func TestWatchFromPagesWithoutGapsOrDuplicates(t *testing.T) {
	db, _, _ := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()
	tenant := cursorTenant(t)
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM hlp_event.events WHERE tenant_id = $1`, tenant)
	})

	// 两个任务交替写入，模拟多任务并发：per-task seq 会来回跳（1,1,2,2…），
	// global_seq 必须是单调连续的一条。
	var batch []*eventpkg.Event
	for i := 1; i <= 6; i++ {
		batch = append(batch, cursorEvent(t, tenant, "task-a", int64(i), fmt.Sprintf("a-%d", i)))
		batch = append(batch, cursorEvent(t, tenant, "task-b", int64(i), fmt.Sprintf("b-%d", i)))
	}
	if _, err := repo.Append(ctx, tenant, "proj-cursor", batch); err != nil {
		t.Fatalf("append: %v", err)
	}

	seen := map[string]bool{}
	since := int64(0)
	for page := 0; page < 10; page++ {
		events, next, err := repo.WatchFrom(ctx, tenant, nil, since, 3)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(events) == 0 {
			break
		}
		for _, e := range events {
			if seen[e.EventID] {
				t.Fatalf("event %s delivered twice", e.EventID)
			}
			seen[e.EventID] = true
			if e.GlobalSeq != since+1 {
				t.Fatalf("cursor jumped: got %d, want %d", e.GlobalSeq, since+1)
			}
			since = e.GlobalSeq
		}
		if next != since {
			t.Fatalf("next = %d, since = %d", next, since)
		}
	}
	if len(seen) != 12 || since != 12 {
		t.Fatalf("collected %d events, cursor %d; want 12/12", len(seen), since)
	}
}

func TestAppendKeepsGlobalSeqContiguousUnderConcurrency(t *testing.T) {
	db, _, _ := setupTask(t)
	repo := NewEventRepo(db)
	tenant := cursorTenant(t)
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM hlp_event.events WHERE tenant_id = $1`, tenant)
	})

	const writers, perWriter = 6, 5
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 1; i <= perWriter; i++ {
				e := cursorEvent(t, tenant, fmt.Sprintf("task-%02d", w), int64(i), fmt.Sprintf("w%d-%d", w, i))
				if _, err := repo.Append(context.Background(), tenant, "proj-cursor", []*eventpkg.Event{e}); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	for _, err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}

	rows, err := db.Query(context.Background(),
		`SELECT global_seq FROM hlp_event.events WHERE tenant_id = $1 ORDER BY global_seq`, tenant)
	if err != nil {
		t.Fatalf("query cursor: %v", err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seqs = append(seqs, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(seqs) != writers*perWriter {
		t.Fatalf("rows = %d, want %d", len(seqs), writers*perWriter)
	}
	for i, v := range seqs {
		if v != int64(i+1) {
			t.Fatalf("global_seq[%d] = %d, want %d (not contiguous)", i, v, i+1)
		}
	}
}

func TestAppendIdempotentDoesNotBurnGlobalSeq(t *testing.T) {
	db, _, _ := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()
	tenant := cursorTenant(t)
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM hlp_event.events WHERE tenant_id = $1`, tenant)
	})

	e1 := cursorEvent(t, tenant, "task-idem", 1, "one")
	if _, err := repo.Append(ctx, tenant, "proj-cursor", []*eventpkg.Event{e1}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if n, err := repo.Append(ctx, tenant, "proj-cursor", []*eventpkg.Event{e1}); err != nil || n != 0 {
		t.Fatalf("replay = %d, %v; want 0, nil", n, err)
	}
	e2 := cursorEvent(t, tenant, "task-idem", 2, "two")
	if _, err := repo.Append(ctx, tenant, "proj-cursor", []*eventpkg.Event{e2}); err != nil {
		t.Fatalf("append second: %v", err)
	}

	events, next, err := repo.WatchFrom(ctx, tenant, nil, 0, 10)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if len(events) != 2 || events[0].GlobalSeq != 1 || events[1].GlobalSeq != 2 || next != 2 {
		t.Fatalf("global_seq must stay 1,2 after an idempotent replay; got %d,%d next=%d",
			events[0].GlobalSeq, events[1].GlobalSeq, next)
	}
}
