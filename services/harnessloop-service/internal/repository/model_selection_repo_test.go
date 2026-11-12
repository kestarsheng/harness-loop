package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestModelSelectionRepoRealPostgresRoundTrip(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewModelSelectionRepo(db)
	ctx := context.Background()

	candidates := []byte(`[{"model_id":"qwen-max","score":0.91},{"model_id":"qwen-plus","score":0.72}]`)
	row := ModelSelectionRow{
		SelectionID:     "sel-real-" + task.TaskID,
		TenantID:        task.TenantID,
		ProjectID:       task.ProjectID,
		TaskID:          task.TaskID,
		Stage:           "c",
		AttemptNo:       2,
		ModelID:         "qwen-max",
		Strategy:        "balanced",
		ResolvedFrom:    "fallback",
		Reason:          "primary unavailable",
		Candidates:      candidates,
		ContextWindow:   131072,
		ThresholdTokens: 104857,
		InputPrice:      0.0024,
		OutputPrice:     0.0096,
		TokensIn:        1234,
		TokensOut:       567,
		Cost:            0.0084,
		IsFallback:      true,
		ApprovalID:      "appr-1",
		Operator:        "operator-1",
	}
	if err := repo.Record(ctx, row); err != nil {
		t.Fatalf("record: %v", err)
	}

	var (
		got           ModelSelectionRow
		gotCandidates []byte
		gotCreatedAt  time.Time
	)
	err := db.Pool().QueryRow(ctx, `
		SELECT selection_id, tenant_id, project_id, task_id, stage, attempt_no, model_id,
		       strategy, resolved_from, reason, candidates, context_window, threshold_tokens,
		       input_price::float8, output_price::float8, tokens_in, tokens_out, cost::float8,
		       is_fallback, approval_id, operator, created_at
		FROM hlp_task.model_selections WHERE selection_id = $1`, row.SelectionID).Scan(
		&got.SelectionID, &got.TenantID, &got.ProjectID, &got.TaskID, &got.Stage, &got.AttemptNo,
		&got.ModelID, &got.Strategy, &got.ResolvedFrom, &got.Reason, &gotCandidates,
		&got.ContextWindow, &got.ThresholdTokens, &got.InputPrice, &got.OutputPrice,
		&got.TokensIn, &got.TokensOut, &got.Cost, &got.IsFallback, &got.ApprovalID,
		&got.Operator, &gotCreatedAt,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if got.SelectionID != row.SelectionID || got.TaskID != task.TaskID || got.Stage != "c" || got.AttemptNo != 2 {
		t.Fatalf("identity columns = %+v", got)
	}
	if got.ModelID != "qwen-max" || got.Strategy != "balanced" || got.ResolvedFrom != "fallback" {
		t.Fatalf("model columns = %+v", got)
	}
	if got.Reason != "primary unavailable" || got.ContextWindow != 131072 || got.ThresholdTokens != 104857 {
		t.Fatalf("threshold columns = %+v", got)
	}
	if got.InputPrice != 0.0024 || got.OutputPrice != 0.0096 || got.Cost != 0.0084 {
		t.Fatalf("price/cost columns = %+v", got)
	}
	if got.TokensIn != 1234 || got.TokensOut != 567 || !got.IsFallback {
		t.Fatalf("token/fallback columns = %+v", got)
	}
	if got.ApprovalID != "appr-1" || got.Operator != "operator-1" {
		t.Fatalf("approval columns = %+v", got)
	}
	if gotCreatedAt.IsZero() {
		t.Fatal("created_at must be populated by the default")
	}
	var decoded []map[string]any
	if err := json.Unmarshal(gotCandidates, &decoded); err != nil || len(decoded) != 2 {
		t.Fatalf("candidates = %s (%v)", gotCandidates, err)
	}
}

func TestModelSelectionSchemaHas21ValueColumns(t *testing.T) {
	db, _, _ := setupTask(t)
	ctx := context.Background()

	var total int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'hlp_task' AND table_name = 'model_selections'`).Scan(&total); err != nil {
		t.Fatalf("count columns: %v", err)
	}
	// 21 columns are written explicitly by ModelSelectionRow; created_at is
	// the 22nd column and is filled by its default.
	if total != 22 {
		t.Fatalf("model_selections columns = %d, want 22 (21 value columns + created_at)", total)
	}

	var values int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'hlp_task' AND table_name = 'model_selections'
		  AND column_name <> 'created_at'`).Scan(&values); err != nil {
		t.Fatalf("count value columns: %v", err)
	}
	if values != 21 {
		t.Fatalf("writable columns = %d, want 21", values)
	}

	rows, err := db.Pool().Query(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'hlp_task' AND table_name = 'model_selections'`)
	if err != nil {
		t.Fatalf("list columns: %v", err)
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		seen[name] = true
	}
	for _, name := range []string{
		"selection_id", "tenant_id", "project_id", "task_id", "stage", "attempt_no",
		"model_id", "strategy", "resolved_from", "reason", "candidates", "context_window",
		"threshold_tokens", "input_price", "output_price", "tokens_in", "tokens_out",
		"cost", "is_fallback", "approval_id", "operator",
	} {
		if !seen[name] {
			t.Fatalf("missing column %q", name)
		}
	}
}

func TestModelSelectionRepoCheckConstraints(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewModelSelectionRepo(db)
	ctx := context.Background()

	base := ModelSelectionRow{
		TenantID: task.TenantID, ProjectID: task.ProjectID, TaskID: task.TaskID,
		Stage: "c", AttemptNo: 1, ModelID: "qwen-max", Strategy: "balanced",
		ResolvedFrom: "strategy", ContextWindow: 32768, ThresholdTokens: 26214,
	}
	cases := []struct {
		name string
		mut  func(*ModelSelectionRow)
	}{
		{"invalid stage", func(r *ModelSelectionRow) { r.Stage = "z" }},
		{"invalid strategy", func(r *ModelSelectionRow) { r.Strategy = "cheapest" }},
		{"invalid resolved_from", func(r *ModelSelectionRow) { r.ResolvedFrom = "guessed" }},
	}
	for i, tc := range cases {
		row := base
		row.SelectionID = "sel-check-" + task.TaskID + "-" + string(rune('a'+i))
		tc.mut(&row)
		if err := repo.Record(ctx, row); err == nil || !strings.Contains(err.Error(), "23514") {
			t.Fatalf("%s must violate a CHECK constraint, got %v", tc.name, err)
		}
	}
}

func TestModelSelectionRepoRejectsUnknownTaskAndDuplicate(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewModelSelectionRepo(db)
	ctx := context.Background()

	base := ModelSelectionRow{
		TenantID: task.TenantID, ProjectID: task.ProjectID, TaskID: task.TaskID,
		Stage: "c", AttemptNo: 1, ModelID: "qwen-max", Strategy: "balanced",
		ResolvedFrom: "strategy", ContextWindow: 32768, ThresholdTokens: 26214,
	}
	orphan := base
	orphan.SelectionID = "sel-orphan-" + task.TaskID
	orphan.TaskID = "task-does-not-exist"
	if err := repo.Record(ctx, orphan); err == nil || !strings.Contains(err.Error(), "23503") {
		t.Fatalf("unknown task must violate the foreign key, got %v", err)
	}

	first := base
	first.SelectionID = "sel-dup-" + task.TaskID
	if err := repo.Record(ctx, first); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := repo.Record(ctx, first); err == nil || !strings.Contains(err.Error(), "23505") {
		t.Fatalf("duplicate selection_id must violate the primary key, got %v", err)
	}
}
