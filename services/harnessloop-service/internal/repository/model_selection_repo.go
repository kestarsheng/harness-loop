package repository

import (
	"context"
	"encoding/json"
	"fmt"

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