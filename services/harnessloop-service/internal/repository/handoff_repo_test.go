package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	eventpkg "agent-platform/pkg/harnessloop/event"
)

func mkHandoffEvent(taskID, tenantID, projectID, handoffID, subtaskID string, typ eventpkg.EventType, seq int64, ts time.Time, payload string) *eventpkg.Event {
	return &eventpkg.Event{
		EventID:       fmt.Sprintf("ev-%s-%d", handoffID, seq),
		EventType:     typ,
		SchemaVersion: eventpkg.CurrentSchemaVersion,
		TaskID:        taskID,
		ParentTaskID:  taskID,
		SubtaskID:     subtaskID,
		HandoffID:     handoffID,
		Seq:           seq,
		Timestamp:     ts,
		Severity:      eventpkg.SeverityInfo,
		TenantID:      tenantID,
		ProjectID:     projectID,
		Payload:       []byte(payload),
	}
}

func TestHandoffRepoAggregatesEventsAndIsolatesTenants(t *testing.T) {
	db, taskRepo, _ := setupTask(t)
	repo := NewEventRepo(db)
	handoffRepo := NewHandoffRepo(db)
	ctx := context.Background()

	projectID := fmt.Sprintf("proj-ho-%d", time.Now().UnixNano())
	task := createOtherTenantTask(t, taskRepo, "tenant-a", projectID)
	base := time.Now().UTC()
	ho1 := "ho-1-" + task.TaskID
	ho2 := "ho-2-" + task.TaskID
	events := []*eventpkg.Event{
		mkHandoffEvent(task.TaskID, task.TenantID, task.ProjectID, ho1, "st-1", eventpkg.HandoffCreated, 10, base,
			`{"from_agent_id":"agent-a","to_agent_id":"agent-b","input_digest":"sha256:aaa"}`),
		mkHandoffEvent(task.TaskID, task.TenantID, task.ProjectID, ho1, "st-1", eventpkg.HandoffAccepted, 11, base.Add(time.Second),
			`{"reason":""}`),
		mkHandoffEvent(task.TaskID, task.TenantID, task.ProjectID, ho2, "st-2", eventpkg.HandoffCreated, 12, base.Add(2*time.Second),
			`{"from_agent_id":"agent-c","to_agent_id":"agent-d"}`),
	}
	if _, err := repo.Append(ctx, task.TenantID, task.ProjectID, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	otherTenant := "tenant-b-" + task.TaskID
	other := createOtherTenantTask(t, taskRepo, otherTenant, projectID)
	hoB := "ho-b-" + other.TaskID
	if _, err := repo.Append(ctx, other.TenantID, other.ProjectID, []*eventpkg.Event{
		mkHandoffEvent(other.TaskID, other.TenantID, other.ProjectID, hoB, "st-b", eventpkg.HandoffCreated, 1, base,
			`{"from_agent_id":"x","to_agent_id":"y"}`),
	}); err != nil {
		t.Fatalf("append tenant-b: %v", err)
	}

	rows, err := handoffRepo.List(ctx, task.TenantID, task.ProjectID, task.TaskID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("handoffs = %d, want 2", len(rows))
	}
	byID := map[string]*HandoffRow{}
	for _, row := range rows {
		byID[row.HandoffID] = row
	}
	if got := byID[ho1]; got == nil || got.Status != "accepted" || got.FromAgentID != "agent-a" ||
		got.ToAgentID != "agent-b" || got.InputDigest != "sha256:aaa" || got.SubtaskID != "st-1" {
		t.Fatalf("unexpected ho-1: %+v", got)
	}
	if got := byID[ho2]; got == nil || got.Status != "created" || got.FromAgentID != "agent-c" {
		t.Fatalf("unexpected ho-2: %+v", got)
	}
	if byID[ho1].CreatedAt.IsZero() || !byID[ho1].UpdatedAt.After(byID[ho1].CreatedAt) {
		t.Fatalf("timestamps not aggregated: %+v", byID[ho1])
	}

	rows, err = handoffRepo.List(ctx, task.TenantID, task.ProjectID, "", 10)
	if err != nil {
		t.Fatalf("list without task filter: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("unfiltered handoffs = %d, want 2", len(rows))
	}

	rows, err = handoffRepo.List(ctx, otherTenant, task.ProjectID, other.TaskID, 10)
	if err != nil {
		t.Fatalf("list tenant-b: %v", err)
	}
	if len(rows) != 1 || rows[0].HandoffID != hoB {
		t.Fatalf("tenant-b rows = %+v", rows)
	}

	rows, err = handoffRepo.List(ctx, otherTenant, task.ProjectID, task.TaskID, 10)
	if err != nil {
		t.Fatalf("cross tenant list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("cross tenant handoffs = %d, want 0", len(rows))
	}

	rows, err = handoffRepo.List(ctx, task.TenantID, "another-project", task.TaskID, 10)
	if err != nil {
		t.Fatalf("cross project list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("cross project handoffs = %d, want 0", len(rows))
	}
}
