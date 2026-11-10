package pgindex

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	DSN                string
	MaxConns           int32
	MinConns           int32
	MaxConnLifetime    time.Duration
	MaxConnIdleTime    time.Duration
	ConnectTimeout     time.Duration
	SlowQueryThreshold time.Duration
}

func DefaultConfig(dsn string) Config {
	return Config{
		DSN:                dsn,
		MaxConns:           8,
		MinConns:           1,
		MaxConnLifetime:    30 * time.Minute,
		MaxConnIdleTime:    5 * time.Minute,
		ConnectTimeout:     5 * time.Second,
		SlowQueryThreshold: 500 * time.Millisecond,
	}
}

type DB struct {
	pool *pgxpool.Pool
	slow time.Duration
}

func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.DSN == "" {
		return nil, errors.New("pgindex: DSN is required")
	}
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("pgindex: parse dsn: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.ConnectTimeout > 0 {
		pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("pgindex: create pool: %w", err)
	}

	db := &DB{pool: pool, slow: cfg.SlowQueryThreshold}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgindex: ping: %w", err)
	}
	return db, nil
}

func (db *DB) Pool() *pgxpool.Pool { return db.pool }

func (db *DB) Close() {
	if db != nil && db.pool != nil {
		db.pool.Close()
	}
}

func (db *DB) Ping(ctx context.Context) error {
	if db == nil || db.pool == nil {
		return errors.New("pgindex: closed")
	}
	return db.pool.Ping(ctx)
}

func (db *DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	start := time.Now()
	tag, err := db.pool.Exec(ctx, sql, args...)
	db.observe(ctx, "exec", start, err)
	return tag, err
}

func (db *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	start := time.Now()
	rows, err := db.pool.Query(ctx, sql, args...)
	db.observe(ctx, "query", start, err)
	return rows, err
}

func (db *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	start := time.Now()
	row := db.pool.QueryRow(ctx, sql, args...)
	db.observe(ctx, "query_row", start, nil)
	return row
}

func (db *DB) observe(ctx context.Context, op string, start time.Time, err error) {
	elapsed := time.Since(start)
	if err != nil {
		log.Printf("pgindex query_error op=%s duration_ms=%d err=%v", op, elapsed.Milliseconds(), err)
		return
	}
	if db.slow > 0 && elapsed >= db.slow {
		log.Printf("pgindex slow_query op=%s duration_ms=%d", op, elapsed.Milliseconds())
	}
}
