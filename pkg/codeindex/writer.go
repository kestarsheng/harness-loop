package codeindex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	pgdb "agent-platform/pkg/pgindex"
)

var ErrParseFailed = errors.New("codeindex: parse failed")
var ErrNoParser = errors.New("codeindex: parse function is not configured")

type ParseFunc func(path string, src []byte) (ParseResult, error)
type EmbedFunc func(ctx context.Context, texts []string) ([][]float32, error)

const (
	StatusPending  = "pending"
	StatusIndexing = "indexing"
	StatusIndexed  = "indexed"
	StatusFailed   = "failed"
)

type FileState struct {
	TenantID     string
	ProjectID    string
	FilePath     string
	Language     string
	ContentHash  string
	MTime        time.Time
	IndexedAt    *time.Time
	Status       string
	FailureCount int
	LastError    string
	ParserID     string
	Degraded     bool
}

type IndexResult struct {
	FilePath string
	Status   string
	Skipped  bool
	Chunks   int
	Edges    int
	Duration time.Duration
}

type CodeWriter struct {
	db      *pgdb.DB
	parse   ParseFunc
	embed   EmbedFunc
	metrics *Metrics
	batch   int
}

func NewCodeWriter(db *pgdb.DB, parse ParseFunc, embed EmbedFunc, metrics *Metrics) *CodeWriter {
	if metrics == nil {
		metrics = NewMetrics()
	}
	return &CodeWriter{db: db, parse: parse, embed: embed, metrics: metrics, batch: 64}
}

func (w *CodeWriter) Metrics() *Metrics { return w.metrics }

func (w *CodeWriter) GetFileState(ctx context.Context, tenantID, projectID, filePath string) (*FileState, error) {
	var (
		state   FileState
		indexed *time.Time
		lastErr *string
		parser  *string
	)
	err := w.db.QueryRow(ctx, `
		SELECT tenant_id, project_id, file_path, language, content_hash, mtime, indexed_at,
		       status, failure_count, last_error, parser_id, degraded
		FROM hlp_code.indexed_files
		WHERE tenant_id = $1 AND project_id = $2 AND file_path = $3`,
		tenantID, projectID, filePath).Scan(
		&state.TenantID, &state.ProjectID, &state.FilePath, &state.Language, &state.ContentHash,
		&state.MTime, &indexed, &state.Status, &state.FailureCount, &lastErr, &parser, &state.Degraded)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codeindex: get state: %w", err)
	}
	state.IndexedAt = indexed
	if lastErr != nil {
		state.LastError = *lastErr
	}
	if parser != nil {
		state.ParserID = *parser
	}
	return &state, nil
}

func (w *CodeWriter) IndexFile(ctx context.Context, tenantID, projectID, path string, src []byte, mtime time.Time, force bool) (IndexResult, error) {
	start := time.Now()
	result := IndexResult{FilePath: path}
	if w.parse == nil {
		return result, ErrNoParser
	}
	if mtime.IsZero() {
		mtime = time.Now().UTC()
	}
	hash := FileHash(src)

	existing, err := w.GetFileState(ctx, tenantID, projectID, path)
	if err != nil {
		return result, err
	}
	if !force && existing != nil && existing.ContentHash == hash && existing.Status == StatusIndexed {
		result.Skipped = true
		result.Status = StatusIndexed
		w.metrics.RecordSkipped()
		return result, nil
	}

	parsed, parseErr := w.parse(path, src)
	if parseErr != nil {
		w.markFailed(ctx, tenantID, projectID, path, hash, mtime, parseErr)
		w.metrics.RecordFailed()
		return result, fmt.Errorf("%w: %v", ErrParseFailed, parseErr)
	}

	var vectors [][]float32
	if w.embed != nil && len(parsed.Chunks) > 0 {
		texts := make([]string, 0, len(parsed.Chunks))
		for _, chunk := range parsed.Chunks {
			texts = append(texts, chunk.Symbol+"\n"+chunk.Text)
		}
		vectors = w.embedBatch(ctx, texts)
	}

	tx, err := w.db.Pool().Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("codeindex: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	lockKey := tenantID + "|" + projectID + "|" + path
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return result, fmt.Errorf("codeindex: advisory lock: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM hlp_code.code_chunks WHERE tenant_id = $1 AND project_id = $2 AND file_path = $3`,
		tenantID, projectID, path); err != nil {
		return result, fmt.Errorf("codeindex: delete chunks: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM hlp_code.code_edges
		WHERE tenant_id = $1 AND project_id = $2 AND (source_file = $3 OR target_file = $3)`,
		tenantID, projectID, path); err != nil {
		return result, fmt.Errorf("codeindex: delete edges: %w", err)
	}

	language := LanguageOf(path)
	for i, chunk := range parsed.Chunks {
		tokens := strings.Join(IdentifierTokens(chunk.Text), " ")
		var embedding any
		if i < len(vectors) && len(vectors[i]) > 0 {
			embedding = vectorLiteral(vectors[i])
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO hlp_code.code_chunks
				(tenant_id, project_id, file_path, language, chunk_type, symbol, parent_symbol,
				 start_line, end_line, content, content_hash, parser_id, embedding, content_tokens, content_tsvector)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9,$10,$11,$12,
			        CASE WHEN $13 = '' THEN NULL ELSE $13::vector END,
			        $14, to_tsvector('simple', $14))`,
			tenantID, projectID, path, language, string(chunk.ChunkType), chunk.Symbol, chunk.ParentSymbol,
			chunk.StartLine, chunk.EndLine, chunk.Text, chunk.ContentHash, chunk.ParserID,
			toString(embedding), tokens); err != nil {
			return result, fmt.Errorf("codeindex: insert chunk %s: %w", chunk.Symbol, err)
		}
		result.Chunks++
	}
	for _, edge := range parsed.Edges {
		if _, err := tx.Exec(ctx, `
			INSERT INTO hlp_code.code_edges
				(tenant_id, project_id, edge_type, source_symbol, target_symbol, source_file, target_file)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''))
			ON CONFLICT (tenant_id, project_id, edge_type, source_symbol, target_symbol, source_file) DO NOTHING`,
			tenantID, projectID, edge.EdgeType, edge.Source, edge.Target, path, edge.TargetFile); err != nil {
			return result, fmt.Errorf("codeindex: insert edge %s->%s: %w", edge.Source, edge.Target, err)
		}
		result.Edges++
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO hlp_code.indexed_files
			(tenant_id, project_id, file_path, language, content_hash, mtime, indexed_at, status,
			 failure_count, last_error, parser_id, degraded)
		VALUES ($1,$2,$3,$4,$5,$6,now(),'indexed',0,NULL,$7,$8)
		ON CONFLICT (tenant_id, project_id, file_path) DO UPDATE
		SET content_hash = EXCLUDED.content_hash,
		    mtime = EXCLUDED.mtime,
		    indexed_at = now(),
		    status = 'indexed',
		    failure_count = 0,
		    last_error = NULL,
		    parser_id = EXCLUDED.parser_id,
		    degraded = EXCLUDED.degraded`,
		tenantID, projectID, path, language, hash, mtime, parsed.ParserID, parsed.Degraded); err != nil {
		return result, fmt.Errorf("codeindex: upsert indexed_files: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("codeindex: commit: %w", err)
	}
	committed = true

	result.Status = StatusIndexed
	result.Duration = time.Since(start)
	w.metrics.RecordIndexed(result.Chunks, result.Edges, result.Duration.Milliseconds())
	return result, nil
}

func (w *CodeWriter) DeleteFile(ctx context.Context, tenantID, projectID, path string) error {
	tx, err := w.db.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("codeindex: begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM hlp_code.code_chunks WHERE tenant_id=$1 AND project_id=$2 AND file_path=$3`, tenantID, projectID, path); err != nil {
		return fmt.Errorf("codeindex: delete chunks: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM hlp_code.code_edges WHERE tenant_id=$1 AND project_id=$2 AND (source_file=$3 OR target_file=$3)`, tenantID, projectID, path); err != nil {
		return fmt.Errorf("codeindex: delete edges: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM hlp_code.indexed_files WHERE tenant_id=$1 AND project_id=$2 AND file_path=$3`, tenantID, projectID, path); err != nil {
		return fmt.Errorf("codeindex: delete indexed_files: %w", err)
	}
	return tx.Commit(ctx)
}

func (w *CodeWriter) markFailed(ctx context.Context, tenantID, projectID, path, hash string, mtime time.Time, cause error) {
	_, err := w.db.Exec(ctx, `
		INSERT INTO hlp_code.indexed_files
			(tenant_id, project_id, file_path, language, content_hash, mtime, status, failure_count, last_error)
		VALUES ($1,$2,$3,$4,$5,$6,'failed',1,$7)
		ON CONFLICT (tenant_id, project_id, file_path) DO UPDATE
		SET status = 'failed',
		    failure_count = hlp_code.indexed_files.failure_count + 1,
		    last_error = EXCLUDED.last_error,
		    mtime = EXCLUDED.mtime`,
		tenantID, projectID, path, LanguageOf(path), hash, mtime, cause.Error())
	if err != nil {
		_ = err
	}
}

func (w *CodeWriter) CountChunks(ctx context.Context, tenantID, projectID, path string) (int, error) {
	var n int
	err := w.db.QueryRow(ctx, `
		SELECT count(*) FROM hlp_code.code_chunks
		WHERE tenant_id = $1 AND project_id = $2 AND file_path = $3`, tenantID, projectID, path).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("codeindex: count chunks: %w", err)
	}
	return n, nil
}

func (w *CodeWriter) CountEdges(ctx context.Context, tenantID, projectID, path string) (int, error) {
	var n int
	err := w.db.QueryRow(ctx, `
		SELECT count(*) FROM hlp_code.code_edges
		WHERE tenant_id = $1 AND project_id = $2 AND (source_file = $3 OR target_file = $3)`,
		tenantID, projectID, path).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("codeindex: count edges: %w", err)
	}
	return n, nil
}

func (w *CodeWriter) embedBatch(ctx context.Context, texts []string) [][]float32 {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += w.batch {
		end := start + w.batch
		if end > len(texts) {
			end = len(texts)
		}
		vectors, err := w.embed(ctx, texts[start:end])
		if err != nil {
			return out
		}
		out = append(out, vectors...)
	}
	return out
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, value := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(fmt.Sprintf("%.6f", value))
	}
	b.WriteByte(']')
	return b.String()
}
