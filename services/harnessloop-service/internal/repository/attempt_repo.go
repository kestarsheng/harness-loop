package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/pgindex"
)

var ErrAttemptNotFinished = errors.New("attempt is not finished")

type AttemptRepo struct {
	db *pgindex.DB
}

func NewAttemptRepo(db *pgindex.DB) *AttemptRepo {
	return &AttemptRepo{db: db}
}

func (r *AttemptRepo) NextAttemptNo(ctx context.Context, taskID string, stage fsm.Stage) (int, error) {
	var no int
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(MAX(attempt_no), 0) + 1
		FROM hlp_task.stage_attempts
		WHERE task_id = $1 AND stage = $2`, taskID, string(stage)).Scan(&no)
	if err != nil {
		return 0, fmt.Errorf("attempt_repo: next attempt no: %w", err)
	}
	return no, nil
}

func (r *AttemptRepo) Create(ctx context.Context, a fsm.Attempt) error {
	var endedAt any
	if !a.EndedAt.IsZero() {
		endedAt = a.EndedAt
	}
	var acceptanceID any
	if a.AcceptanceID != "" {
		acceptanceID = a.AcceptanceID
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO hlp_task.stage_attempts
			(attempt_id, task_id, stage, attempt_no, entry_reason, started_at, ended_at,
			 finish_received, acceptance_id, retry_count, acceptance_fail_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		a.AttemptID, a.TaskID, string(a.Stage), a.AttemptNo, string(a.EntryReason),
		a.StartedAt, endedAt, a.FinishReceived, acceptanceID, a.RetryCount, a.AcceptanceFailCount)
	if err != nil {
		return fmt.Errorf("attempt_repo: create %s: %w", a.AttemptID, err)
	}
	return nil
}

func (r *AttemptRepo) Finish(ctx context.Context, attemptID string, endedAt time.Time) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE hlp_task.stage_attempts
		SET finish_received = true, ended_at = $2
		WHERE attempt_id = $1`, attemptID, endedAt)
	if err != nil {
		return fmt.Errorf("attempt_repo: finish %s: %w", attemptID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("attempt_repo: finish %s: not found", attemptID)
	}
	return nil
}

func (r *AttemptRepo) AttachAcceptance(ctx context.Context, attemptID, acceptanceID string) error {
	if acceptanceID == "" {
		return ErrAttemptNotFinished
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE hlp_task.stage_attempts
		SET acceptance_id = $2
		WHERE attempt_id = $1 AND finish_received = true`, attemptID, acceptanceID)
	if err != nil {
		return fmt.Errorf("attempt_repo: attach acceptance %s: %w", attemptID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAttemptNotFinished
	}
	return nil
}

func (r *AttemptRepo) ListByTask(ctx context.Context, taskID string) ([]fsm.Attempt, error) {
	rows, err := r.db.Query(ctx, `
		SELECT attempt_id, task_id, stage, attempt_no, entry_reason, started_at, ended_at,
		       finish_received, COALESCE(acceptance_id, ''), retry_count, acceptance_fail_count
		FROM hlp_task.stage_attempts
		WHERE task_id = $1
		ORDER BY stage, attempt_no`, taskID)
	if err != nil {
		return nil, fmt.Errorf("attempt_repo: list: %w", err)
	}
	defer rows.Close()

	var out []fsm.Attempt
	for rows.Next() {
		var (
			a       fsm.Attempt
			stage   string
			reason  string
			endedAt *time.Time
		)
		if err := rows.Scan(&a.AttemptID, &a.TaskID, &stage, &a.AttemptNo, &reason,
			&a.StartedAt, &endedAt, &a.FinishReceived, &a.AcceptanceID, &a.RetryCount, &a.AcceptanceFailCount); err != nil {
			return nil, fmt.Errorf("attempt_repo: scan: %w", err)
		}
		a.Stage = fsm.Stage(stage)
		a.EntryReason = fsm.EntryReason(reason)
		if endedAt != nil {
			a.EndedAt = *endedAt
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
