package pgindex

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed ddl/*.sql
var ddlFS embed.FS

const migrationLockKey int64 = 0x686c705f6d6967

func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS hlp_event`); err != nil {
		return fmt.Errorf("pgindex: create schema: %w", err)
	}
	if _, err := db.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS hlp_event.schema_migrations (
			version    text PRIMARY KEY,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("pgindex: create schema_migrations: %w", err)
	}

	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("pgindex: acquire conn: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("pgindex: acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	entries, err := ddlFS.ReadDir("ddl")
	if err != nil {
		return fmt.Errorf("pgindex: read ddl dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM hlp_event.schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
			return fmt.Errorf("pgindex: check migration %s: %w", version, err)
		}
		if exists {
			continue
		}
		body, err := ddlFS.ReadFile("ddl/" + name)
		if err != nil {
			return fmt.Errorf("pgindex: read %s: %w", name, err)
		}
		sum := sha256.Sum256(body)

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("pgindex: begin %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body), pgx.QueryExecModeSimpleProtocol); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("pgindex: apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO hlp_event.schema_migrations (version, checksum) VALUES ($1, $2)`,
			version, hex.EncodeToString(sum[:])); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("pgindex: record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("pgindex: commit %s: %w", name, err)
		}
	}
	return nil
}

func AppliedMigrations(ctx context.Context, db *DB) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT version FROM hlp_event.schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
