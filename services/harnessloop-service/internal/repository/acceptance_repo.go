package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"agent-platform/pkg/harnessloop/acceptance"
	"agent-platform/pkg/pgindex"
)

type AcceptanceRecord struct {
	AcceptanceID   string
	TaskID         string
	Stage          string
	AttemptID      string
	Verdict        string
	VerifiedBy     string
	Reason         string
	Evidence       []acceptance.Evidence
	FailedCriteria []acceptance.FailedCriterion
	Unverified     []string
	RollbackTarget string
	PromptVersion  string
	Model          string
	InputDigest    string
	TokensIn       int
	TokensOut      int
	DurationMs     int64
	CreatedAt      time.Time
}

type AcceptanceRepo struct {
	db *pgindex.DB
}

func NewAcceptanceRepo(db *pgindex.DB) *AcceptanceRepo { return &AcceptanceRepo{db: db} }

func (r *AcceptanceRepo) Create(ctx context.Context, rec AcceptanceRecord) error {
	if rec.AcceptanceID == "" || rec.TaskID == "" || rec.AttemptID == "" {
		return errors.New("acceptance_repo: acceptance_id/task_id/attempt_id are required")
	}
	evidence, err := json.Marshal(rec.Evidence)
	if err != nil {
		return fmt.Errorf("acceptance_repo: marshal evidence: %w", err)
	}
	failed, err := json.Marshal(rec.FailedCriteria)
	if err != nil {
		return fmt.Errorf("acceptance_repo: marshal failed criteria: %w", err)
	}
	unverified, err := json.Marshal(rec.Unverified)
	if err != nil {
		return fmt.Errorf("acceptance_repo: marshal unverified: %w", err)
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO hlp_task.acceptance_records
			(acceptance_id, task_id, stage, attempt_id, verdict, verified_by, reason, evidence,
			 failed_criteria, unverified, rollback_target, prompt_version, model, input_digest,
			 tokens_in, tokens_out, duration_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12,NULLIF($13,''),$14,$15,$16,$17)`,
		rec.AcceptanceID, rec.TaskID, rec.Stage, rec.AttemptID, rec.Verdict, rec.VerifiedBy,
		rec.Reason, evidence, failed, unverified, rec.RollbackTarget, rec.PromptVersion,
		rec.Model, rec.InputDigest, rec.TokensIn, rec.TokensOut, rec.DurationMs)
	if err != nil {
		return fmt.Errorf("acceptance_repo: create %s: %w", rec.AcceptanceID, err)
	}
	return nil
}

func (r *AcceptanceRepo) Get(ctx context.Context, acceptanceID string) (*AcceptanceRecord, error) {
	row := r.db.QueryRow(ctx, `
		SELECT acceptance_id, task_id, stage, attempt_id, verdict, verified_by, reason,
		       evidence, failed_criteria, unverified, COALESCE(rollback_target,''), prompt_version,
		       COALESCE(model,''), input_digest, COALESCE(tokens_in,0), COALESCE(tokens_out,0),
		       COALESCE(duration_ms,0), created_at
		FROM hlp_task.acceptance_records WHERE acceptance_id = $1`, acceptanceID)
	rec, err := scanAcceptance(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("acceptance_repo: get %s: %w", acceptanceID, err)
	}
	return rec, nil
}

func (r *AcceptanceRepo) ListByTaskStage(ctx context.Context, taskID, stage string, limit int) ([]*AcceptanceRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT acceptance_id, task_id, stage, attempt_id, verdict, verified_by, reason,
		       evidence, failed_criteria, unverified, COALESCE(rollback_target,''), prompt_version,
		       COALESCE(model,''), input_digest, COALESCE(tokens_in,0), COALESCE(tokens_out,0),
		       COALESCE(duration_ms,0), created_at
		FROM hlp_task.acceptance_records
		WHERE task_id = $1 AND ($2 = '' OR stage = $2)
		ORDER BY created_at DESC
		LIMIT $3`, taskID, stage, limit)
	if err != nil {
		return nil, fmt.Errorf("acceptance_repo: list: %w", err)
	}
	defer rows.Close()

	var out []*AcceptanceRecord
	for rows.Next() {
		rec, err := scanAcceptance(rows)
		if err != nil {
			return nil, fmt.Errorf("acceptance_repo: scan: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func scanAcceptance(s rowScanner) (*AcceptanceRecord, error) {
	var (
		rec                   AcceptanceRecord
		evidence, failed, unv []byte
	)
	if err := s.Scan(&rec.AcceptanceID, &rec.TaskID, &rec.Stage, &rec.AttemptID, &rec.Verdict,
		&rec.VerifiedBy, &rec.Reason, &evidence, &failed, &unv, &rec.RollbackTarget,
		&rec.PromptVersion, &rec.Model, &rec.InputDigest, &rec.TokensIn, &rec.TokensOut,
		&rec.DurationMs, &rec.CreatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(evidence, &rec.Evidence)
	_ = json.Unmarshal(failed, &rec.FailedCriteria)
	_ = json.Unmarshal(unv, &rec.Unverified)
	return &rec, nil
}

func (r *AcceptanceRepo) LatestEvidence(ctx context.Context, taskID, stage string) ([]acceptance.Evidence, []acceptance.FailedCriterion, error) {
	var evidence, failed []byte
	err := r.db.QueryRow(ctx, `
		SELECT evidence, failed_criteria FROM hlp_task.acceptance_records
		WHERE task_id = $1 AND stage = $2 AND verdict = 'fail'
		ORDER BY created_at DESC LIMIT 1`, taskID, stage).Scan(&evidence, &failed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("acceptance_repo: latest evidence: %w", err)
	}
	var ev []acceptance.Evidence
	var fc []acceptance.FailedCriterion
	_ = json.Unmarshal(evidence, &ev)
	_ = json.Unmarshal(failed, &fc)
	return ev, fc, nil
}
