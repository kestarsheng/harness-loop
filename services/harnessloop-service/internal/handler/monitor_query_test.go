package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"agent-platform/pkg/harnessloop/event"
	harnesspb "agent-platform/pkg/pb/harnessloop"
	"agent-platform/services/harnessloop-service/internal/repository"
)

func TestListSubtasksReturnsRealRowsAndIsolatesTenant(t *testing.T) {
	h, db := setupHandlerWithDB(t)
	ctx := context.Background()

	created, err := h.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId: "tenant-a", ProjectId: "proj-a", WorkspaceRoot: t.TempDir(), Goal: "subtask goal",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	taskID := created.Task.TaskId
	subtaskID := "st-handler-" + taskID

	_, err = db.Pool().Exec(ctx, `
		INSERT INTO hlp_task.subtasks
			(subtask_id, parent_task_id, tenant_id, project_id, dag_id, dag_node_id, depth, index_in_parent,
			 role, status, depends_on, input_contract, output_contract, acceptance_criteria, failure_strategy,
			 attempt_no, artifact_refs, alloc_tokens, consumed_tokens, consumed_cost)
		VALUES ($1,$2,'tenant-a','proj-a','dag-1','n-1',1,0,'coder','running','[]'::jsonb,
			'{"goal":"g"}'::jsonb,'{}'::jsonb,'[]'::jsonb,'fail-fast',1,'[]'::jsonb,10,2,0.01)`, subtaskID, taskID)
	if err != nil {
		t.Fatalf("insert subtask: %v", err)
	}

	resp, err := h.ListSubtasks(ctx, &harnesspb.ListSubtasksRequest{TenantId: "tenant-a", TaskId: taskID})
	if err != nil {
		t.Fatalf("list subtasks: %v", err)
	}
	if len(resp.Subtasks) != 1 {
		t.Fatalf("subtasks = %d, want 1", len(resp.Subtasks))
	}
	got := resp.Subtasks[0]
	if got.SubtaskId != subtaskID || got.ParentTaskId != taskID || got.Role != "coder" || got.Status != "running" {
		t.Fatalf("unexpected subtask: %+v", got)
	}
	if got.AttemptNo != 1 || got.ConsumedTokens != 2 || got.ConsumedCost != 0.01 {
		t.Fatalf("unexpected counters: %+v", got)
	}
	if string(got.InputContract) == "" {
		t.Fatalf("input_contract empty")
	}

	cross, err := h.ListSubtasks(ctx, &harnesspb.ListSubtasksRequest{TenantId: "tenant-b", TaskId: taskID})
	if err != nil {
		t.Fatalf("cross tenant list: %v", err)
	}
	if len(cross.Subtasks) != 0 {
		t.Fatalf("cross tenant subtasks = %d, want 0", len(cross.Subtasks))
	}

	_, err = h.ListSubtasks(ctx, &harnesspb.ListSubtasksRequest{TenantId: "tenant-a"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing task_id code = %v", status.Code(err))
	}
}

func TestListModelSelectionsReturnsRealRowsAndIsolatesTenant(t *testing.T) {
	h, db := setupHandlerWithDB(t)
	ctx := context.Background()

	created, err := h.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId: "tenant-a", ProjectId: "proj-a", WorkspaceRoot: t.TempDir(), Goal: "selection goal",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	taskID := created.Task.TaskId

	repo := repository.NewModelSelectionRepo(db)
	if err := repo.Record(ctx, repository.ModelSelectionRow{
		SelectionID: "sel-handler-" + taskID, TenantID: "tenant-a", ProjectID: "proj-a", TaskID: taskID,
		Stage: "c", AttemptNo: 1, ModelID: "qwen-max", Strategy: "balanced", ResolvedFrom: "fallback",
		Reason: "primary unavailable", Candidates: []byte(`[{"model_id":"qwen-max","score":0.9}]`),
		ContextWindow: 32768, ThresholdTokens: 26214, InputPrice: 0.001, OutputPrice: 0.002,
		TokensIn: 10, TokensOut: 5, Cost: 0.0002, IsFallback: true,
	}); err != nil {
		t.Fatalf("record selection: %v", err)
	}

	resp, err := h.ListModelSelections(ctx, &harnesspb.ListModelSelectionsRequest{TenantId: "tenant-a", TaskId: taskID})
	if err != nil {
		t.Fatalf("list selections: %v", err)
	}
	if len(resp.Selections) != 1 {
		t.Fatalf("selections = %d, want 1", len(resp.Selections))
	}
	got := resp.Selections[0]
	if got.SelectionId != "sel-handler-"+taskID || got.ModelId != "qwen-max" || !got.IsFallback || got.Stage != "c" {
		t.Fatalf("unexpected selection: %+v", got)
	}
	if got.CreatedAtMs == 0 || string(got.Candidates) == "" {
		t.Fatalf("selection not fully populated: %+v", got)
	}

	cross, err := h.ListModelSelections(ctx, &harnesspb.ListModelSelectionsRequest{TenantId: "tenant-b", TaskId: taskID})
	if err != nil {
		t.Fatalf("cross tenant list: %v", err)
	}
	if len(cross.Selections) != 0 {
		t.Fatalf("cross tenant selections = %d, want 0", len(cross.Selections))
	}
}

func TestListHandoffsReturnsRealRowsAndIsolatesTenant(t *testing.T) {
	h, db := setupHandlerWithDB(t)
	ctx := context.Background()
	projectID := fmt.Sprintf("proj-handler-%d", time.Now().UnixNano())

	created, err := h.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId: "tenant-a", ProjectId: projectID, WorkspaceRoot: t.TempDir(), Goal: "handoff goal",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	taskID := created.Task.TaskId

	handoffID := "ho-handler-" + taskID
	events := []*event.Event{
		{
			EventID: "ev-" + handoffID, EventType: event.HandoffCreated, SchemaVersion: event.CurrentSchemaVersion,
			TaskID: taskID, ParentTaskID: taskID, SubtaskID: "st-1", HandoffID: handoffID,
			Seq: 1, Timestamp: time.Now().UTC(), Severity: event.SeverityInfo,
			TenantID: "tenant-a", ProjectID: projectID,
			Payload: []byte(`{"from_agent_id":"agent-a","to_agent_id":"agent-b","input_digest":"sha256:abc"}`),
		},
	}
	if _, err := repository.NewEventRepo(db).Append(ctx, "tenant-a", projectID, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	resp, err := h.ListHandoffs(ctx, &harnesspb.ListHandoffsRequest{TenantId: "tenant-a", TaskId: taskID})
	if err != nil {
		t.Fatalf("list handoffs: %v", err)
	}
	if len(resp.Handoffs) != 1 {
		t.Fatalf("handoffs = %d, want 1", len(resp.Handoffs))
	}
	got := resp.Handoffs[0]
	if got.HandoffId != handoffID || got.FromAgentId != "agent-a" || got.ToAgentId != "agent-b" ||
		got.InputDigest != "sha256:abc" || got.Status != "created" {
		t.Fatalf("unexpected handoff: %+v", got)
	}

	cross, err := h.ListHandoffs(ctx, &harnesspb.ListHandoffsRequest{TenantId: "tenant-b", ProjectId: projectID})
	if err != nil {
		t.Fatalf("cross tenant list: %v", err)
	}
	if len(cross.Handoffs) != 0 {
		t.Fatalf("cross tenant handoffs = %d, want 0", len(cross.Handoffs))
	}
}
