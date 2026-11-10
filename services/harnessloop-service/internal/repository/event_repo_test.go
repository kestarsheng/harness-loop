package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	eventpkg "agent-platform/pkg/harnessloop/event"
)

func mkTestEvent(taskID string, tenantID, projectID string, seq int64, stage, typ string, ts time.Time) *eventpkg.Event {
	return &eventpkg.Event{
		EventID:       fmt.Sprintf("ev-%s-%d", taskID, seq),
		EventType:     eventpkg.EventType(typ),
		SchemaVersion: eventpkg.CurrentSchemaVersion,
		TaskID:        taskID,
		Stage:         stage,
		Seq:           seq,
		Timestamp:     ts,
		Severity:      eventpkg.SeverityInfo,
		TenantID:      tenantID,
		ProjectID:     projectID,
		Payload:       []byte(`{"ok":true}`),
	}
}

func TestEventRepoAppendAndList(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	events := []*eventpkg.Event{
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 1, "a", "stage.transition", time.Now()),
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 2, "c", "stage.transition", time.Now()),
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 3, "c", "acceptance.passed", time.Now()),
	}
	inserted, err := repo.Append(ctx, task.TenantID, task.ProjectID, events)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if inserted != 3 {
		t.Fatalf("inserted = %d", inserted)
	}

	got, err := repo.List(ctx, EventFilter{TenantID: task.TenantID, TaskID: task.TaskID, Stage: "c"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stage=c events = %d", len(got))
	}
	if got[0].Seq != 2 || got[1].Seq != 3 {
		t.Fatalf("ordering = %d,%d", got[0].Seq, got[1].Seq)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq != got[i-1].Seq+1 {
			t.Fatalf("seq gap between %d and %d", got[i-1].Seq, got[i].Seq)
		}
	}
}

func TestEventRepoIsIdempotent(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	e := mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 1, "a", "stage.transition", time.Now())
	if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, []*eventpkg.Event{e}); err != nil {
		t.Fatalf("append: %v", err)
	}
	inserted, err := repo.Append(ctx, task.TenantID, task.ProjectID, []*eventpkg.Event{e})
	if err != nil {
		t.Fatalf("append duplicate: %v", err)
	}
	if inserted != 0 {
		t.Fatalf("duplicate inserted = %d", inserted)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM hlp_event.events WHERE task_id = $1`, task.TaskID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("event count = %d", count)
	}
}

func TestEventRepoFilters(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	events := []*eventpkg.Event{
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 1, "a", "stage.transition", time.Now()),
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 2, "c", "acceptance.failed", time.Now()),
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 3, "c", "acceptance.passed", time.Now()),
	}
	events[1].Severity = eventpkg.SeverityWarn
	events[2].Severity = eventpkg.SeverityError
	if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	byType, err := repo.List(ctx, EventFilter{TenantID: task.TenantID, TaskID: task.TaskID, EventType: "acceptance.passed"})
	if err != nil || len(byType) != 1 {
		t.Fatalf("by type: %v %d", err, len(byType))
	}
	bySeverity, err := repo.List(ctx, EventFilter{TenantID: task.TenantID, TaskID: task.TaskID, Severity: "warn"})
	if err != nil || len(bySeverity) != 1 || bySeverity[0].Severity != eventpkg.SeverityWarn {
		t.Fatalf("by severity: %v %d", err, len(bySeverity))
	}
	other, err := repo.List(ctx, EventFilter{TenantID: "tenant-b", TaskID: task.TaskID})
	if err != nil {
		t.Fatalf("cross tenant: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("cross tenant must be empty, got %d", len(other))
	}
}

func TestEventRepoPagination(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	var events []*eventpkg.Event
	for i := int64(1); i <= 5; i++ {
		events = append(events, mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, i, "c", "stage.transition", time.Now()))
	}
	if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	page1, err := repo.List(ctx, EventFilter{TenantID: task.TenantID, TaskID: task.TaskID, Limit: 2})
	if err != nil || len(page1) != 2 {
		t.Fatalf("page1: %v %d", err, len(page1))
	}
	page2, err := repo.List(ctx, EventFilter{TenantID: task.TenantID, TaskID: task.TaskID, SinceSeq: page1[len(page1)-1].Seq, Limit: 2})
	if err != nil || len(page2) != 2 {
		t.Fatalf("page2: %v %d", err, len(page2))
	}
	if page2[0].Seq != 3 {
		t.Fatalf("page2 first seq = %d", page2[0].Seq)
	}
	next, err := repo.NextSeq(ctx, task.TenantID, task.TaskID)
	if err != nil {
		t.Fatalf("next seq: %v", err)
	}
	if next != 5 {
		t.Fatalf("next seq = %d", next)
	}
}

func TestEventRepoWatch(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	events := []*eventpkg.Event{
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 1, "a", "stage.transition", time.Now()),
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 2, "b", "stage.transition", time.Now()),
	}
	if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, watermark, err := repo.WatchFrom(ctx, task.TenantID, []string{task.TaskID}, 0, 10)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if len(got) != 2 || watermark != 2 {
		t.Fatalf("watch len=%d watermark=%d", len(got), watermark)
	}
	got, watermark, err = repo.WatchFrom(ctx, task.TenantID, []string{task.TaskID}, watermark, 10)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if len(got) != 0 || watermark != 2 {
		t.Fatalf("idle watch len=%d watermark=%d", len(got), watermark)
	}
}

func TestEventRepoCleanup(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	old := []*eventpkg.Event{
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 1, "a", "stage.transition", time.Now().Add(-72*time.Hour)),
		mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 2, "a", "stage.transition", time.Now().Add(-48*time.Hour)),
	}
	fresh := mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 3, "a", "stage.transition", time.Now())
	if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, append(old, fresh)); err != nil {
		t.Fatalf("append: %v", err)
	}

	deleted, err := repo.Cleanup(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d", deleted)
	}
	left, err := repo.List(ctx, EventFilter{TenantID: task.TenantID, TaskID: task.TaskID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 1 || left[0].Seq != 3 {
		t.Fatalf("left = %v", left)
	}
}

func TestEventRepoGetByID(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	ctx := context.Background()

	e := mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 1, "a", "stage.transition", time.Now())
	if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, []*eventpkg.Event{e}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := repo.GetByID(ctx, task.TenantID, e.EventID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.EventID != e.EventID {
		t.Fatalf("got %s", got.EventID)
	}
	missing, err := repo.GetByID(ctx, task.TenantID, "nope")
	if err != nil || missing != nil {
		t.Fatalf("missing: %v %v", err, missing)
	}
}

func TestEventRepoRejectsInvalidEvent(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewEventRepo(db)
	e := mkTestEvent(task.TaskID, task.TenantID, task.ProjectID, 1, "a", "stage.transition", time.Now())
	e.Severity = eventpkg.Severity("bad")
	if _, err := repo.Append(context.Background(), task.TenantID, task.ProjectID, []*eventpkg.Event{e}); err == nil {
		t.Fatal("invalid event must be rejected")
	}
}
