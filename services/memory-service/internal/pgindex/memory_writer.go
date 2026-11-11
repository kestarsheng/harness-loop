package pgindex

import (
	"context"
	"fmt"
	"time"

	pgdb "agent-platform/pkg/pgindex"
)

type TopicRecord struct {
	TenantID    string
	ProjectID   string
	Scope       string
	TopicKey    string
	Category    string
	Title       string
	Body        string
	Pinned      bool
	OrderRank   int
	ContentHash string
	SourceFile  string
	UpdatedAt   time.Time
	Decayed     bool
}

type MemoryWriter struct {
	db *pgdb.DB
}

func NewMemoryWriter(db *pgdb.DB) *MemoryWriter { return &MemoryWriter{db: db} }

func (w *MemoryWriter) UpsertTopics(ctx context.Context, records []TopicRecord) (int, error) {
	if len(records) == 0 {
		return 0, nil
	}
	tx, err := w.db.Pool().Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("memory_writer: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	written := 0
	for _, rec := range records {
		tokens := TokensForTsv(rec.Title + " " + rec.Body)
		_, err := tx.Exec(ctx, `
			INSERT INTO hlp_mem.memory_topics
				(tenant_id, project_id, scope, topic_key, category, title, body, pinned,
				 order_rank, content_hash, source_file, updated_at, indexed_at, decayed, tsv)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now(),false,to_tsvector('simple',$13))
			ON CONFLICT (tenant_id, project_id, scope, topic_key) DO UPDATE
			SET category = EXCLUDED.category,
			    title = EXCLUDED.title,
			    body = EXCLUDED.body,
			    pinned = EXCLUDED.pinned,
			    order_rank = EXCLUDED.order_rank,
			    content_hash = EXCLUDED.content_hash,
			    source_file = EXCLUDED.source_file,
			    updated_at = EXCLUDED.updated_at,
			    indexed_at = now(),
			    decayed = false,
			    tsv = EXCLUDED.tsv
			WHERE hlp_mem.memory_topics.content_hash <> EXCLUDED.content_hash
			   OR hlp_mem.memory_topics.order_rank <> EXCLUDED.order_rank`,
			rec.TenantID, rec.ProjectID, rec.Scope, rec.TopicKey, rec.Category, rec.Title, rec.Body,
			rec.Pinned, rec.OrderRank, rec.ContentHash, rec.SourceFile, rec.UpdatedAt, tokens)
		if err != nil {
			return 0, fmt.Errorf("memory_writer: upsert %s: %w", rec.TopicKey, err)
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO hlp_mem.memory_chunks (topic_id, chunk_index, content, content_hash)
			SELECT topic_id, 0, $1, $2 FROM hlp_mem.memory_topics
			WHERE tenant_id = $3 AND project_id = $4 AND scope = $5 AND topic_key = $6
			ON CONFLICT (topic_id, chunk_index) DO UPDATE
			SET content = EXCLUDED.content, content_hash = EXCLUDED.content_hash, embedding = NULL
			WHERE hlp_mem.memory_chunks.content_hash <> EXCLUDED.content_hash`,
			rec.Body, rec.ContentHash, rec.TenantID, rec.ProjectID, rec.Scope, rec.TopicKey)
		if err != nil {
			return 0, fmt.Errorf("memory_writer: chunk %s: %w", rec.TopicKey, err)
		}
		written++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("memory_writer: commit: %w", err)
	}
	return written, nil
}

func (w *MemoryWriter) DeleteTopics(ctx context.Context, tenantID, projectID, scope string, keys []string) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	tag, err := w.db.Exec(ctx, `
		DELETE FROM hlp_mem.memory_topics
		WHERE tenant_id = $1 AND project_id = $2 AND scope = $3 AND topic_key = ANY($4)`,
		tenantID, projectID, scope, keys)
	if err != nil {
		return 0, fmt.Errorf("memory_writer: delete: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (w *MemoryWriter) SetDecayed(ctx context.Context, tenantID, projectID, scope, key string, decayed bool) error {
	_, err := w.db.Exec(ctx, `
		UPDATE hlp_mem.memory_topics SET decayed = $5
		WHERE tenant_id = $1 AND project_id = $2 AND scope = $3 AND topic_key = $4`,
		tenantID, projectID, scope, key, decayed)
	if err != nil {
		return fmt.Errorf("memory_writer: set decayed %s: %w", key, err)
	}
	return nil
}

func (w *MemoryWriter) SearchTopics(ctx context.Context, tenantID, projectID, query string, limit int) ([]TopicRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	tokens := TokensForTsv(query)
	rows, err := w.db.Query(ctx, `
		SELECT tenant_id, project_id, scope, topic_key, category, title, body, pinned,
		       order_rank, content_hash, source_file, updated_at, decayed
		FROM hlp_mem.memory_topics
		WHERE tenant_id = $1 AND project_id = $2 AND decayed = false
		  AND ($4 = '' OR tsv @@ plainto_tsquery('simple', $4))
		ORDER BY ts_rank_cd(tsv, plainto_tsquery('simple', $4)) DESC, order_rank ASC
		LIMIT $3`,
		tenantID, projectID, limit, tokens)
	if err != nil {
		return nil, fmt.Errorf("memory_writer: search: %w", err)
	}
	defer rows.Close()

	var out []TopicRecord
	for rows.Next() {
		var rec TopicRecord
		if err := rows.Scan(&rec.TenantID, &rec.ProjectID, &rec.Scope, &rec.TopicKey, &rec.Category,
			&rec.Title, &rec.Body, &rec.Pinned, &rec.OrderRank, &rec.ContentHash, &rec.SourceFile,
			&rec.UpdatedAt, &rec.Decayed); err != nil {
			return nil, fmt.Errorf("memory_writer: scan: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (w *MemoryWriter) CountTopics(ctx context.Context, tenantID, projectID string) (int, error) {
	var n int
	err := w.db.QueryRow(ctx, `
		SELECT count(*) FROM hlp_mem.memory_topics WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, projectID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("memory_writer: count: %w", err)
	}
	return n, nil
}
func (w *MemoryWriter) ListTopicKeys(ctx context.Context, tenantID, projectID, scope string) ([]string, error) {
	rows, err := w.db.Query(ctx, `
		SELECT topic_key FROM hlp_mem.memory_topics
		WHERE tenant_id = $1 AND project_id = $2 AND scope = $3`, tenantID, projectID, scope)
	if err != nil {
		return nil, fmt.Errorf("memory_writer: list keys: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("memory_writer: scan key: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
