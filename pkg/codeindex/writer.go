package codeindex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	// batch 是单次 embedding 调用的文本条数（沿用历史默认 64）。
	batch int
	// insert 是单条多行 INSERT 的行数上限；0 视为 DefaultInsertBatch。
	insert int

	// guardMu 保护下面的维度守卫字段；守卫是可选能力，零值即关闭。
	guardMu       sync.Mutex
	guard         EmbeddingGuard
	guardDim      int
	guardResolved bool
}

func NewCodeWriter(db *pgdb.DB, parse ParseFunc, embed EmbedFunc, metrics *Metrics) *CodeWriter {
	if metrics == nil {
		metrics = NewMetrics()
	}
	return &CodeWriter{db: db, parse: parse, embed: embed, metrics: metrics, batch: 64, insert: DefaultInsertBatch}
}

func (w *CodeWriter) Metrics() *Metrics { return w.metrics }

// SetEmbedBatch 调整单次 embedding 的文本条数；<=0 时恢复默认 64。
func (w *CodeWriter) SetEmbedBatch(n int) {
	if w == nil {
		return
	}
	if n <= 0 {
		n = 64
	}
	w.batch = n
}

// SetInsertBatch 调整单条多行 INSERT 的行数；<=0 时恢复 DefaultInsertBatch。
// 批量索引把它与并发度一起调，能显著减少单文件写库的往返次数。
func (w *CodeWriter) SetInsertBatch(n int) {
	if w == nil {
		return
	}
	if n <= 0 {
		n = DefaultInsertBatch
	}
	if n > 1000 {
		n = 1000
	}
	w.insert = n
}

func (w *CodeWriter) insertBatch() int {
	if w.insert <= 0 {
		return DefaultInsertBatch
	}
	return w.insert
}

// IndexOptions 是 IndexFileWith 的行为开关。
type IndexOptions struct {
	// Force 为 true 时忽略 content_hash 相同即跳过的短路（全量语义）。
	Force bool
	// SkipStateLookup 为 true 时跳过 indexed_files 预检；仅在 Force 时安全。
	SkipStateLookup bool
	// InsertBatch 覆盖单条多行 INSERT 的行数；<=0 用 writer 默认。
	InsertBatch int
	// EmbedBatch 覆盖单次 embedding 的文本条数；<=0 用 writer 默认。
	EmbedBatch int
}

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

// IndexFile 保持历史签名：force 等价于 IndexOptions{Force: force}。
func (w *CodeWriter) IndexFile(ctx context.Context, tenantID, projectID, path string, src []byte, mtime time.Time, force bool) (IndexResult, error) {
	return w.IndexFileWith(ctx, tenantID, projectID, path, src, mtime, IndexOptions{Force: force})
}

// IndexFileWith 是带行为开关的入口，批量索引用它跳过全量模式下多余的预检往返。
func (w *CodeWriter) IndexFileWith(ctx context.Context, tenantID, projectID, path string, src []byte, mtime time.Time, options IndexOptions) (IndexResult, error) {
	start := time.Now()
	result := IndexResult{FilePath: path}
	if w.parse == nil {
		return result, ErrNoParser
	}
	if mtime.IsZero() {
		mtime = time.Now().UTC()
	}
	hash := FileHash(src)

	// 全量模式下 hash 命中也要重写，跳过预检即可省掉一次往返（少一次 RTT/文件）。
	var existing *FileState
	if !options.SkipStateLookup {
		var err error
		existing, err = w.GetFileState(ctx, tenantID, projectID, path)
		if err != nil {
			return result, err
		}
	}
	if !options.Force && existing != nil && existing.ContentHash == hash && existing.Status == StatusIndexed {
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
		// 维度守卫的写入侧：先比对「模型 vs 索引列」，不通过就不去调 embedding 服务，
		// 更不会进事务——错误必须停在产生向量之前。
		if err := w.validateEmbeddingTarget(ctx); err != nil {
			return result, err
		}
		texts := make([]string, 0, len(parsed.Chunks))
		for _, chunk := range parsed.Chunks {
			texts = append(texts, chunk.Symbol+"\n"+chunk.Text)
		}
		vectors = w.embedTexts(ctx, texts, options.EmbedBatch)
		// 再核对 embedding 服务真正返回的向量长度；一批里有一条不对就整批不写。
		if err := w.validateEmbeddingVectors(vectors); err != nil {
			return result, err
		}
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
	chunks, err := w.insertChunks(ctx, tx, tenantID, projectID, path, language, parsed.Chunks, vectors, options.InsertBatch)
	if err != nil {
		return result, err
	}
	result.Chunks = chunks

	edges, err := w.insertEdges(ctx, tx, tenantID, projectID, path, parsed.Edges, options.InsertBatch)
	if err != nil {
		return result, err
	}
	result.Edges = edges

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

// chunkInsertColumns 是 code_chunks 多行 INSERT 每行占用的绑定参数个数。
// content_tokens 与 content_tsvector 复用同一个参数（to_tsvector 由库侧生成），故是 14 而不是 15。
const chunkInsertColumns = 14

// edgeInsertColumns 是 code_edges 多行 INSERT 每行占用的绑定参数个数。
const edgeInsertColumns = 7

// maxInsertBatch 是多行 INSERT 的行数硬上限（1000 行 × 14 参数远低于 65535 上限）。
const maxInsertBatch = 1000

func (w *CodeWriter) batchSize(override int) int {
	if override > 0 {
		if override > maxInsertBatch {
			return maxInsertBatch
		}
		return override
	}
	return w.insertBatch()
}

// insertChunks 用多行 INSERT 写入一个文件的全部 chunk。
// 单文件几十个 chunk 时把往返次数从 N 降到 ⌈N/InsertBatch⌉，是全量吞吐的主要来源之一。
func (w *CodeWriter) insertChunks(ctx context.Context, tx pgx.Tx, tenantID, projectID, path, language string,
	chunks []Chunk, vectors [][]float32, override int) (int, error) {
	if len(chunks) == 0 {
		return 0, nil
	}
	size := w.batchSize(override)
	inserted := 0
	for start := 0; start < len(chunks); start += size {
		end := start + size
		if end > len(chunks) {
			end = len(chunks)
		}
		group := chunks[start:end]
		var (
			sb   strings.Builder
			args = make([]any, 0, len(group)*chunkInsertColumns)
		)
		sb.WriteString(`INSERT INTO hlp_code.code_chunks
			(tenant_id, project_id, file_path, language, chunk_type, symbol, parent_symbol,
			 start_line, end_line, content, content_hash, parser_id, embedding, content_tokens, content_tsvector)
			VALUES `)
		for i, chunk := range group {
			if i > 0 {
				sb.WriteString(",")
			}
			base := i * chunkInsertColumns
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,NULLIF($%d,''),NULLIF($%d,''),$%d,$%d,$%d,$%d,$%d,NULLIF($%d,'')::vector,$%d,to_tsvector('simple',$%d))",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9,
				base+10, base+11, base+12, base+13, base+14, base+14)

			var embedding string
			if idx := start + i; idx < len(vectors) && len(vectors[idx]) > 0 {
				embedding = vectorLiteral(vectors[idx])
			}
			args = append(args,
				tenantID, projectID, path, language, string(chunk.ChunkType),
				chunk.Symbol, chunk.ParentSymbol, chunk.StartLine, chunk.EndLine,
				chunk.Text, chunk.ContentHash, chunk.ParserID, embedding,
				strings.Join(IdentifierTokens(chunk.Text), " "))
		}
		if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
			return inserted, fmt.Errorf("codeindex: insert chunks for %s: %w", path, err)
		}
		inserted += len(group)
	}
	return inserted, nil
}

// insertEdges 用多行 INSERT 写入边表，冲突由 UNIQUE 约束吸收（同一批内重复边也会被合并）。
func (w *CodeWriter) insertEdges(ctx context.Context, tx pgx.Tx, tenantID, projectID, path string,
	edges []Edge, override int) (int, error) {
	if len(edges) == 0 {
		return 0, nil
	}
	size := w.batchSize(override)
	inserted := 0
	for start := 0; start < len(edges); start += size {
		end := start + size
		if end > len(edges) {
			end = len(edges)
		}
		group := edges[start:end]
		var (
			sb   strings.Builder
			args = make([]any, 0, len(group)*edgeInsertColumns)
		)
		sb.WriteString(`INSERT INTO hlp_code.code_edges
			(tenant_id, project_id, edge_type, source_symbol, target_symbol, source_file, target_file)
			VALUES `)
		for i, edge := range group {
			if i > 0 {
				sb.WriteString(",")
			}
			base := i * edgeInsertColumns
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,NULLIF($%d,''))",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7)
			args = append(args, tenantID, projectID, edge.EdgeType, edge.Source, edge.Target, path, edge.TargetFile)
		}
		sb.WriteString(` ON CONFLICT (tenant_id, project_id, edge_type, source_symbol, target_symbol, source_file) DO NOTHING`)
		if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
			return inserted, fmt.Errorf("codeindex: insert edges for %s: %w", path, err)
		}
		inserted += len(group)
	}
	return inserted, nil
}

// embedTexts 按 batch 条一批调用 embedder；任一批失败即返回已得到的部分向量，
// 调用方据此降级为 NULL embedding（与历史行为一致：embedding 失败不丢 chunk）。
func (w *CodeWriter) embedTexts(ctx context.Context, texts []string, batch int) [][]float32 {
	size := batch
	if size <= 0 {
		size = w.batch
	}
	if size <= 0 {
		size = 64
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += size {
		end := start + size
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

// embedBatch 保留历史名字：按 writer 默认批量切分。
func (w *CodeWriter) embedBatch(ctx context.Context, texts []string) [][]float32 {
	return w.embedTexts(ctx, texts, 0)
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
