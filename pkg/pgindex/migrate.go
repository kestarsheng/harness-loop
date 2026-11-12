package pgindex

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

	// 001_mem.sql / 002_code.sql 使用 vector(1024)，但 11 份 DDL 内均不创建扩展。
	// 空库必须在应用 DDL 前由迁移器自建，不能依赖运维手工安装（T-P1-30）。
	// 放在 advisory lock 之后，避免多实例同时 CREATE EXTENSION 的竞态。
	if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("pgindex: create vector extension: %w", err)
	}

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
	if err := validateMigrationSequence(names); err != nil {
		return err
	}

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
	if err := ensurePostMigrationIndexes(ctx, conn); err != nil {
		return err
	}
	return nil
}

// validateMigrationSequence 要求 DDL 文件名前缀从 001 开始严格连续。
// 迁移只允许追加，缺号意味着历史文件被删除或编号撞号。
func validateMigrationSequence(names []string) error {
	for i, name := range names {
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return fmt.Errorf("pgindex: ddl %q has no numeric prefix", name)
		}
		n, err := strconv.Atoi(prefix)
		if err != nil {
			return fmt.Errorf("pgindex: ddl %q has non-numeric prefix: %w", name, err)
		}
		if n != i+1 {
			return fmt.Errorf("pgindex: ddl sequence gap at %s: want %03d", name, i+1)
		}
	}
	return nil
}

// ensurePostMigrationIndexes 补齐"已记录为已应用、但文件后来追加了索引"的场景。
// 008_model_selection.sql 在存量库中已写入 schema_migrations，修改文件不会重放，
// 因此 combo 索引既写在 008 中（空库路径），也在这里幂等兜底（存量库路径）。
func ensurePostMigrationIndexes(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx,
		`CREATE INDEX IF NOT EXISTS idx_model_selection_combo
		 ON hlp_task.model_selections (stage, model_id)`); err != nil {
		return fmt.Errorf("pgindex: ensure model selection combo index: %w", err)
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
