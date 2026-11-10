package pgindex

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (db *DB) WithTaskLock(ctx context.Context, taskID string, fn func(pgx.Tx) error) error {
	if taskID == "" {
		return fmt.Errorf("pgindex: taskID is required")
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgindex: begin task lock: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, taskID); err != nil {
		return fmt.Errorf("pgindex: advisory lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
