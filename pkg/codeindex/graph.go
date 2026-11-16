package codeindex

import (
	"context"
	"fmt"
	"sort"
)

// 图扩展默认参数（design 4.9.2：每个命中符号 ≤ 5 跳邻居，图路权重 0.8）。
const (
	DefaultGraphPerSymbol = 5
	DefaultGraphMaxNodes  = 50
	DefaultGraphWeight    = 0.8
)

// 扩展方向标记，写入 sources/统计，便于前端与压测区分调用方与被调用方。
const (
	GraphDirectionCaller = "caller"
	GraphDirectionCallee = "callee"
)

// GraphExpandOptions 描述一次 1 跳扩展。
type GraphExpandOptions struct {
	TenantID  string
	ProjectID string
	Seeds     []string
	MaxNodes  int
	PerSymbol int
}

// GraphNeighbor 是扩展出的邻居 chunk。
type GraphNeighbor struct {
	ChunkID   string
	FilePath  string
	Symbol    string
	StartLine int
	EndLine   int
	Snippet   string
	Seed      string
	Direction string
}

// GraphExpand 从命中符号沿 code_edges 做 1 跳双向扩展（调用方 + 被调用方）。
//
// 实现分两段，避免「每条边回表查 chunk」的嵌套循环（code_chunks 上没有
// (tenant,project,symbol) 索引时会退化成逐边扫描，实测单种子 50ms+）：
//  1. 用种子符号在 code_edges 上取一跳邻居的符号集合（双向）；
//  2. 用这组符号一次性取 chunk，再在 Go 侧按 seed 分组、去重、封顶。
//
// 两段查询都强制 tenant_id + project_id 行级过滤，跨租户/项目返回空。
func (w *CodeWriter) GraphExpand(ctx context.Context, opts GraphExpandOptions) ([]GraphNeighbor, error) {
	seeds := cleanSeeds(opts.Seeds)
	if len(seeds) == 0 {
		return nil, nil
	}
	perSymbol := opts.PerSymbol
	if perSymbol <= 0 {
		perSymbol = DefaultGraphPerSymbol
	}
	maxNodes := opts.MaxNodes
	if maxNodes <= 0 {
		maxNodes = DefaultGraphMaxNodes
	}

	edges, err := w.graphEdges(ctx, opts.TenantID, opts.ProjectID, seeds, perSymbol*len(seeds)*2)
	if err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, nil
	}

	symbols := make([]string, 0, len(edges))
	seenSymbol := make(map[string]bool, len(edges))
	for _, edge := range edges {
		if edge.symbol == "" || seenSymbol[edge.symbol] {
			continue
		}
		seenSymbol[edge.symbol] = true
		symbols = append(symbols, edge.symbol)
	}

	chunks, err := w.chunksBySymbols(ctx, opts.TenantID, opts.ProjectID, symbols, maxNodes*4)
	if err != nil {
		return nil, err
	}

	bySymbol := make(map[string][]chunkRow, len(chunks))
	for _, row := range chunks {
		bySymbol[row.symbol] = append(bySymbol[row.symbol], row)
	}

	perSeedCount := make(map[string]int, len(seeds))
	used := make(map[string]bool, maxNodes)
	out := make([]GraphNeighbor, 0, maxNodes)
	for _, edge := range edges {
		if len(out) >= maxNodes {
			break
		}
		if perSeedCount[edge.seed] >= perSymbol {
			continue
		}
		row, ok := matchChunk(bySymbol[edge.symbol], edge.filePath, used)
		if !ok {
			continue
		}
		used[row.chunkID] = true
		perSeedCount[edge.seed]++
		out = append(out, GraphNeighbor{
			ChunkID:   row.chunkID,
			FilePath:  row.filePath,
			Symbol:    row.symbol,
			StartLine: row.startLine,
			EndLine:   row.endLine,
			Snippet:   row.snippet,
			Seed:      edge.seed,
			Direction: edge.direction,
		})
	}
	return out, nil
}

type graphEdge struct {
	seed      string
	filePath  string
	symbol    string
	direction string
}

// graphEdges 取一跳邻居的符号集合：target=seed 是调用方，source=seed 是被调用方。
func (w *CodeWriter) graphEdges(ctx context.Context, tenantID, projectID string, seeds []string, limit int) ([]graphEdge, error) {
	if limit <= 0 {
		limit = DefaultGraphMaxNodes * 2
	}
	rows, err := w.db.Query(ctx, `
		WITH seeds(sym) AS (
			SELECT DISTINCT unnest($3::text[]) AS sym
		)
		, callers AS (
			SELECT s.sym AS seed, e.source_file AS file_path, e.source_symbol AS symbol, 'caller' AS direction
			FROM seeds s
			JOIN hlp_code.code_edges e
			  ON e.tenant_id = $1 AND e.project_id = $2 AND e.target_symbol = s.sym
			WHERE e.source_symbol <> s.sym
		)
		, callees AS (
			SELECT s.sym AS seed, ''::text AS file_path, e.target_symbol AS symbol, 'callee' AS direction
			FROM seeds s
			JOIN hlp_code.code_edges e
			  ON e.tenant_id = $1 AND e.project_id = $2 AND e.source_symbol = s.sym
			WHERE e.target_symbol <> s.sym
		)
		SELECT DISTINCT seed, file_path, symbol, direction
		FROM (SELECT * FROM callers UNION ALL SELECT * FROM callees) neighbors
		ORDER BY seed, direction, file_path, symbol
		LIMIT $4`, tenantID, projectID, seeds, limit)
	if err != nil {
		return nil, fmt.Errorf("codeindex: graph edges: %w", err)
	}
	defer rows.Close()

	var out []graphEdge
	for rows.Next() {
		var edge graphEdge
		if err := rows.Scan(&edge.seed, &edge.filePath, &edge.symbol, &edge.direction); err != nil {
			return nil, fmt.Errorf("codeindex: scan graph edge: %w", err)
		}
		out = append(out, edge)
	}
	return out, rows.Err()
}

// chunksBySymbols 用一套符号集合一次取回 chunk（按文件/行号稳定排序）。
func (w *CodeWriter) chunksBySymbols(ctx context.Context, tenantID, projectID string, symbols []string, limit int) ([]chunkRow, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = DefaultGraphMaxNodes * 4
	}
	rows, err := w.db.Query(ctx, `
		SELECT chunk_id::text, file_path, start_line, end_line, COALESCE(symbol, ''), left(content, 400), 0
		FROM hlp_code.code_chunks
		WHERE tenant_id = $1 AND project_id = $2 AND symbol = ANY($3::text[])
		ORDER BY file_path, start_line
		LIMIT $4`, tenantID, projectID, symbols, limit)
	if err != nil {
		return nil, fmt.Errorf("codeindex: graph chunks: %w", err)
	}
	defer rows.Close()
	return scanChunkRows(rows)
}

// matchChunk 在候选 chunk 里挑一个未被占用的；优先同文件命中（调用方的 source_file）。
func matchChunk(candidates []chunkRow, preferredFile string, used map[string]bool) (chunkRow, bool) {
	for _, row := range candidates {
		if used[row.chunkID] {
			continue
		}
		if preferredFile != "" && row.filePath != preferredFile {
			continue
		}
		return row, true
	}
	if preferredFile == "" {
		return chunkRow{}, false
	}
	for _, row := range candidates {
		if !used[row.chunkID] {
			return row, true
		}
	}
	return chunkRow{}, false
}

func cleanSeeds(seeds []string) []string {
	seen := make(map[string]bool, len(seeds))
	out := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		if seed == "" || seen[seed] {
			continue
		}
		seen[seed] = true
		out = append(out, seed)
	}
	sort.Strings(out)
	return out
}
