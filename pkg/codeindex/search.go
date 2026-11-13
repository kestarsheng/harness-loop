package codeindex

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-platform/pkg/rrf"
)

type SearchOptions struct {
	TenantID    string
	ProjectID   string
	Query       string
	TopK        int
	RRFK        int
	EnableGraph bool
	Embedder    EmbedFunc
}

type SearchHit struct {
	ChunkID   string
	FilePath  string
	StartLine int
	EndLine   int
	Symbol    string
	Snippet   string
	Scores    float64
	Sources   []string
	Ranks     []int
}

type SearchStats struct {
	VectorHits  int
	KeywordHits int
	GraphHits   int
	Merged      int
	LatencyMs   int64
}

func (w *CodeWriter) Search(ctx context.Context, opts SearchOptions) ([]SearchHit, SearchStats, error) {
	start := time.Now()
	stats := SearchStats{}
	topK := opts.TopK
	if topK <= 0 || topK > 100 {
		topK = 10
	}
	rrfK := opts.RRFK
	if rrfK <= 0 {
		rrfK = 60
	}

	keywordRows, err := w.keywordSearch(ctx, opts.TenantID, opts.ProjectID, opts.Query, topK*2)
	if err != nil {
		return nil, stats, err
	}
	stats.KeywordHits = len(keywordRows)

	var vectorRows []chunkRow
	if opts.Embedder != nil && IsSearchableQuery(opts.Query) {
		if vecs, embedErr := opts.Embedder(ctx, []string{opts.Query}); embedErr == nil && len(vecs) > 0 && len(vecs[0]) > 0 {
			vectorRows, err = w.vectorSearch(ctx, opts.TenantID, opts.ProjectID, vecs[0], topK*2)
			if err != nil {
				vectorRows = nil
			}
		}
	}
	stats.VectorHits = len(vectorRows)

	var graphRows []chunkRow
	if opts.EnableGraph {
		symbols := collectSymbols(keywordRows, vectorRows)
		if len(symbols) > 0 {
			graphRows, err = w.graphSearch(ctx, opts.TenantID, opts.ProjectID, symbols, topK*2)
			if err != nil {
				graphRows = nil
			}
		}
	}
	stats.GraphHits = len(graphRows)

	routes := []rrf.Route{
		toRoute("keyword", keywordRows),
		toRoute("vector", vectorRows),
		toRoute("graph", graphRows),
	}
	merged := rrf.New(rrfK).MergeRoutes(routes...)

	byID := map[string]chunkRow{}
	for _, row := range append(append(append([]chunkRow{}, keywordRows...), vectorRows...), graphRows...) {
		if _, ok := byID[row.chunkID]; !ok {
			byID[row.chunkID] = row
		}
	}

	hits := make([]SearchHit, 0, topK)
	for _, hit := range merged {
		if len(hits) >= topK {
			break
		}
		row, ok := byID[hit.Entity.ID]
		if !ok {
			continue
		}
		hits = append(hits, SearchHit{
			ChunkID:   row.chunkID,
			FilePath:  row.filePath,
			StartLine: row.startLine,
			EndLine:   row.endLine,
			Symbol:    row.symbol,
			Snippet:   row.snippet,
			Scores:    hit.RRFScore,
			Sources:   hit.Sources,
			Ranks:     hit.Ranks,
		})
	}
	stats.Merged = len(hits)
	stats.LatencyMs = time.Since(start).Milliseconds()
	return hits, stats, nil
}

type chunkRow struct {
	chunkID   string
	filePath  string
	startLine int
	endLine   int
	symbol    string
	snippet   string
	rank      float64
}

func toRoute(name string, rows []chunkRow) rrf.Route {
	items := make([]rrf.RankedItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, rrf.RankedItem{
			Entity:   rrf.Entity{ID: row.chunkID, FilePath: row.filePath, StartLine: row.startLine},
			RawScore: row.rank,
		})
	}
	return rrf.Route{Name: name, Items: items}
}

func IsSearchableQuery(query string) bool {
	return strings.TrimSpace(query) != ""
}

func collectSymbols(rows ...[]chunkRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range rows {
		for _, row := range group {
			if row.symbol == "" || seen[row.symbol] {
				continue
			}
			seen[row.symbol] = true
			out = append(out, row.symbol)
		}
	}
	return out
}

func (w *CodeWriter) keywordSearch(ctx context.Context, tenantID, projectID, query string, limit int) ([]chunkRow, error) {
	tokens := strings.Join(IdentifierTokens(query), " | ")
	if tokens == "" {
		return nil, nil
	}
	rows, err := w.db.Query(ctx, `
		SELECT chunk_id::text, file_path, start_line, end_line, COALESCE(symbol,''), left(content, 400),
		       ts_rank_cd(content_tsvector, to_tsquery('simple', $4))
		FROM hlp_code.code_chunks
		WHERE tenant_id = $1 AND project_id = $2
		  AND content_tsvector @@ to_tsquery('simple', $4)
		ORDER BY ts_rank_cd(content_tsvector, to_tsquery('simple', $4)) DESC, file_path ASC, start_line ASC
		LIMIT $3`, tenantID, projectID, limit, tokens)
	if err != nil {
		return nil, fmt.Errorf("codeindex: keyword search: %w", err)
	}
	defer rows.Close()
	return scanChunkRows(rows)
}

func (w *CodeWriter) vectorSearch(ctx context.Context, tenantID, projectID string, vec []float32, limit int) ([]chunkRow, error) {
	rows, err := w.db.Query(ctx, `
		SELECT chunk_id::text, file_path, start_line, end_line, COALESCE(symbol,''), left(content, 400),
		       1 - (embedding <=> $3::vector)
		FROM hlp_code.code_chunks
		WHERE tenant_id = $1 AND project_id = $2 AND embedding IS NOT NULL
		ORDER BY embedding <=> $3::vector
		LIMIT $4`, tenantID, projectID, vectorLiteral(vec), limit)
	if err != nil {
		return nil, fmt.Errorf("codeindex: vector search: %w", err)
	}
	defer rows.Close()
	return scanChunkRows(rows)
}

func (w *CodeWriter) graphSearch(ctx context.Context, tenantID, projectID string, symbols []string, limit int) ([]chunkRow, error) {
	rows, err := w.db.Query(ctx, `
		SELECT c.chunk_id::text, c.file_path, c.start_line, c.end_line, COALESCE(c.symbol,''), left(c.content, 400), 0
		FROM hlp_code.code_edges e
		JOIN hlp_code.code_chunks c
		  ON c.tenant_id = e.tenant_id AND c.project_id = e.project_id
		 AND c.file_path = e.source_file AND c.symbol = e.source_symbol
		WHERE e.tenant_id = $1 AND e.project_id = $2
		  AND e.target_symbol = ANY($3)
		LIMIT $4`, tenantID, projectID, symbols, limit)
	if err != nil {
		return nil, fmt.Errorf("codeindex: graph search: %w", err)
	}
	defer rows.Close()
	return scanChunkRows(rows)
}

type chunkScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanChunkRows(rows chunkScanner) ([]chunkRow, error) {
	var out []chunkRow
	for rows.Next() {
		var row chunkRow
		if err := rows.Scan(&row.chunkID, &row.filePath, &row.startLine, &row.endLine, &row.symbol, &row.snippet, &row.rank); err != nil {
			return nil, fmt.Errorf("codeindex: scan chunk: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
