package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"agent-platform/pkg/harnessloop/checkpoint"
	"agent-platform/pkg/pgindex"
)

type CheckpointRepo struct {
	db *pgindex.DB
}

func NewCheckpointRepo(db *pgindex.DB) *CheckpointRepo { return &CheckpointRepo{db: db} }

func (r *CheckpointRepo) Save(ctx context.Context, cp *checkpoint.Checkpoint) error {
	if cp == nil {
		return errors.New("checkpoint_repo: nil checkpoint")
	}
	if cp.Checksum == "" {
		cp.Seal()
	}
	if err := cp.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("checkpoint_repo: marshal: %w", err)
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO hlp_task.checkpoints
			(checkpoint_id, task_id, tenant_id, kind, stage, attempt_no, event_seq_watermark, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (checkpoint_id) DO UPDATE
		SET payload = EXCLUDED.payload, event_seq_watermark = EXCLUDED.event_seq_watermark`,
		cp.CheckpointID, cp.TaskID, cp.TenantID, cp.Kind, cp.Stage, cp.AttemptNo,
		cp.EventSeqWatermark, raw, cp.CreatedAt)
	if err != nil {
		return fmt.Errorf("checkpoint_repo: save %s: %w", cp.CheckpointID, err)
	}
	return nil
}

func (r *CheckpointRepo) Load(ctx context.Context, checkpointID string) (*checkpoint.Checkpoint, error) {
	var raw []byte
	err := r.db.QueryRow(ctx, `SELECT payload FROM hlp_task.checkpoints WHERE checkpoint_id = $1`, checkpointID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, checkpoint.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint_repo: load %s: %w", checkpointID, err)
	}
	var cp checkpoint.Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, fmt.Errorf("checkpoint_repo: decode %s: %w", checkpointID, err)
	}
	return &cp, nil
}

func (r *CheckpointRepo) Latest(ctx context.Context, taskID string) (*checkpoint.Checkpoint, error) {
	list, err := r.List(ctx, taskID, 1)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, checkpoint.ErrNotFound
	}
	return list[0], nil
}

func (r *CheckpointRepo) List(ctx context.Context, taskID string, limit int) ([]*checkpoint.Checkpoint, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT payload FROM hlp_task.checkpoints
		WHERE task_id = $1 ORDER BY created_at DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("checkpoint_repo: list: %w", err)
	}
	defer rows.Close()

	var out []*checkpoint.Checkpoint
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("checkpoint_repo: scan: %w", err)
		}
		var cp checkpoint.Checkpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			return nil, fmt.Errorf("checkpoint_repo: decode: %w", err)
		}
		out = append(out, &cp)
	}
	return out, rows.Err()
}

func (r *CheckpointRepo) Delete(ctx context.Context, checkpointID string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM hlp_task.checkpoints WHERE checkpoint_id = $1`, checkpointID)
	if err != nil {
		return fmt.Errorf("checkpoint_repo: delete %s: %w", checkpointID, err)
	}
	return nil
}
