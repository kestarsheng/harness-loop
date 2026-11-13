package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agent-platform/pkg/pgindex"
)

// ModelSelectionRow mirrors hlp_task.model_selections. Every column that the
// DDL declares NOT NULL without a default is populated explicitly here; a
// partial insert would be rejected by PostgreSQL.
type ModelSelectionRow struct {
	SelectionID     string
	TenantID        string
	ProjectID       string
	TaskID          string
	Stage           string
	AttemptNo       int
	ModelID         string
	Strategy        string
	ResolvedFrom    string
	Reason          string
	Candidates      []byte
	ContextWindow   int
	ThresholdTokens int64
	InputPrice      float64
	OutputPrice     float64
	TokensIn        int64
	TokensOut       int64
	Cost            float64
	IsFallback      bool
	ApprovalID      string
	Operator        string
	CreatedAt       time.Time
}

type ModelSelectionRepo struct {
	db *pgindex.DB
}

func NewModelSelectionRepo(db *pgindex.DB) *ModelSelectionRepo { return &ModelSelectionRepo{db: db} }

func (r *ModelSelectionRepo) Record(ctx context.Context, row ModelSelectionRow) error {
	if row.TenantID == "" || row.ProjectID == "" || row.TaskID == "" || row.Stage == "" || row.ModelID == "" {
		return fmt.Errorf("model_selection_repo: tenant/project/task/stage/model are required")
	}
	if row.SelectionID == "" {
		row.SelectionID = fmt.Sprintf("sel-%s-%s-%d", row.TaskID, row.Stage, row.AttemptNo)
	}
	candidates := row.Candidates
	if len(candidates) == 0 {
		candidates = []byte("[]")
	} else if !json.Valid(candidates) {
		return fmt.Errorf("model_selection_repo: candidates is not valid json")
	}
	_, err := r.db.Pool().Exec(ctx, `
		INSERT INTO hlp_task.model_selections
			(selection_id, tenant_id, project_id, task_id, stage, attempt_no, model_id,
			 strategy, resolved_from, reason, candidates, context_window, threshold_tokens,
			 input_price, output_price, tokens_in, tokens_out, cost, is_fallback, approval_id, operator)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,$13,$14,$15,$16,$17,$18,$19,NULLIF($20,''),NULLIF($21,''))`,
		row.SelectionID, row.TenantID, row.ProjectID, row.TaskID, row.Stage, row.AttemptNo, row.ModelID,
		row.Strategy, row.ResolvedFrom, row.Reason, candidates, row.ContextWindow, row.ThresholdTokens,
		row.InputPrice, row.OutputPrice, row.TokensIn, row.TokensOut, row.Cost, row.IsFallback,
		row.ApprovalID, row.Operator)
	if err != nil {
		return fmt.Errorf("model_selection_repo: insert: %w", err)
	}
	return nil
}

// List returns model selections for a task, ordered newest first. Queries are
// always tenant scoped; projectID and stage are optional filters.
func (r *ModelSelectionRepo) List(ctx context.Context, tenantID, projectID, taskID, stage string, limit int) ([]ModelSelectionRow, error) {
	if tenantID == "" || taskID == "" {
		return nil, fmt.Errorf("model_selection_repo: tenant and task are required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := r.db.Query(ctx, `
		SELECT selection_id, tenant_id, project_id, task_id, stage, attempt_no, model_id,
		       strategy, resolved_from, COALESCE(reason,''), candidates, context_window, threshold_tokens,
		       input_price::float8, output_price::float8, tokens_in, tokens_out, cost::float8,
		       is_fallback, COALESCE(approval_id,''), COALESCE(operator,''), created_at
		FROM hlp_task.model_selections
		WHERE tenant_id = $1
		  AND ($2 = '' OR project_id = $2)
		  AND task_id = $3
		  AND ($4 = '' OR stage = $4)
		ORDER BY created_at DESC, selection_id
		LIMIT $5`, tenantID, projectID, taskID, stage, limit)
	if err != nil {
		return nil, fmt.Errorf("model_selection_repo: list: %w", err)
	}
	defer rows.Close()

	var out []ModelSelectionRow
	for rows.Next() {
		var row ModelSelectionRow
		if err := rows.Scan(&row.SelectionID, &row.TenantID, &row.ProjectID, &row.TaskID,
			&row.Stage, &row.AttemptNo, &row.ModelID, &row.Strategy, &row.ResolvedFrom,
			&row.Reason, &row.Candidates, &row.ContextWindow, &row.ThresholdTokens,
			&row.InputPrice, &row.OutputPrice, &row.TokensIn, &row.TokensOut, &row.Cost,
			&row.IsFallback, &row.ApprovalID, &row.Operator, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("model_selection_repo: scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
