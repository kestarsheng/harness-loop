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
	// GraphWeight 是图扩展路在 RRF 里的权重（默认 0.8），<0 视为 0。
	GraphWeight float64
	// GraphMaxNodes / GraphPerSymbol 限制扩展规模，<=0 走默认值。
	GraphMaxNodes  int
	GraphPerSymbol int
	// GraphSeedLimit > 0 时只从检索排名前 N 的符号扩展；<= 0 全量（历史行为）。
	GraphSeedLimit int
	// GraphSeedDecay > 0 时按种子排名线性衰减扩展项权重（rank1 满权，每降一位减该步长）；
	// <= 0 表示等权。
	GraphSeedDecay float64
	// GraphDirection 限定图扩展方向：caller / callee / both。
	// 空串等同 both，与加方向维度前的行为完全一致；只改召回侧，不改权重与融合公式。
	GraphDirection string
	Embedder       EmbedFunc
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
	VectorHits   int
	KeywordHits  int
	GraphHits    int
	GraphCallers int
	GraphCallees int
	Merged       int
	LatencyMs    int64
}

// 检索路名。fulltext/vector/graph 与 design 4.9.2、指标标签 hlp_recall_hit_total 一致。
const (
	RouteFulltext = "fulltext"
	RouteVector   = "vector"
	RouteGraph    = "graph"
)

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
			// 维度守卫的查询侧：查询向量和索引列维度不一致时向量路必然查不到，
			// 这里直接报错而不是静默退化成 keyword-only。
			if guardErr := w.validateEmbeddingQuery(vecs[0]); guardErr != nil {
				return nil, stats, guardErr
			}
			vectorRows, err = w.vectorSearch(ctx, opts.TenantID, opts.ProjectID, vecs[0], topK*2)
			if err != nil {
				vectorRows = nil
			}
		}
	}
	stats.VectorHits = len(vectorRows)

	var graphRows []chunkRow
	var graphWeights map[string]float64
	if opts.EnableGraph {
		symbols := collectSymbols(keywordRows, vectorRows)
		if len(symbols) > 0 {
			neighbors, graphErr := w.GraphExpand(ctx, GraphExpandOptions{
				TenantID:  opts.TenantID,
				ProjectID: opts.ProjectID,
				Seeds:     symbols,
				MaxNodes:  opts.GraphMaxNodes,
				PerSymbol: opts.GraphPerSymbol,
				SeedLimit: opts.GraphSeedLimit,
				DecayStep: opts.GraphSeedDecay,
				Direction: opts.GraphDirection,
			})
			if graphErr == nil {
				graphRows = neighborsToRows(neighbors)
				graphWeights = make(map[string]float64, len(neighbors))
				for _, n := range neighbors {
					if n.Weight > 0 {
						graphWeights[n.ChunkID] = n.Weight
					}
					switch n.Direction {
					case GraphDirectionCaller:
						stats.GraphCallers++
					case GraphDirectionCallee:
						stats.GraphCallees++
					}
				}
			}
		}
	}
	stats.GraphHits = len(graphRows)

	routes := []rrf.Route{
		toRoute(RouteFulltext, keywordRows),
		toRoute(RouteVector, vectorRows),
		toRoute(RouteGraph, graphRows),
	}
	weights := []float64{1, 1, graphWeight(opts.GraphWeight)}
	// 图路的逐项系数来自种子排名衰减；keyword/vector 路不加权。
	itemWeights := []map[string]float64{nil, nil, graphWeights}
	merged := mergeWeighted(rrfK, routes, weights, itemWeights)

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

func graphWeight(weight float64) float64 {
	switch {
	case weight == 0:
		return DefaultGraphWeight
	case weight < 0:
		return 0
	default:
		return weight
	}
}

func neighborsToRows(neighbors []GraphNeighbor) []chunkRow {
	rows := make([]chunkRow, 0, len(neighbors))
	for i, n := range neighbors {
		rows = append(rows, chunkRow{
			chunkID:   n.ChunkID,
			filePath:  n.FilePath,
			startLine: n.StartLine,
			endLine:   n.EndLine,
			symbol:    n.Symbol,
			snippet:   n.Snippet,
			rank:      float64(len(neighbors) - i),
			weight:    n.Weight,
		})
	}
	return rows
}

// mergeWeighted 在 pkg/rrf 的融合结果上按路权重重算分，
// 公式同 design 4.9.1：score(d) = Σ w_r * c_r(d) / (k + rank_r(d))，k 取默认 60。
// c_r(d) 是该条目在该路的逐项系数（图路用它承载种子排名衰减），缺省为 1。
func mergeWeighted(rrfK int, routes []rrf.Route, weights []float64, itemWeights []map[string]float64) []rrf.Hit {
	fusion := rrf.New(rrfK)
	merged := fusion.MergeRoutes(routes...)
	denominator := rrfK
	if denominator <= 0 {
		denominator = rrf.DefaultK
	}
	for i := range merged {
		score := 0.0
		for routeIndex, rank := range merged[i].Ranks {
			if rank < 0 || routeIndex >= len(weights) {
				continue
			}
			weight := weights[routeIndex]
			if routeIndex < len(itemWeights) && itemWeights[routeIndex] != nil {
				if factor, ok := itemWeights[routeIndex][merged[i].Entity.ID]; ok {
					weight *= factor
				}
			}
			score += weight / float64(denominator+rank+1)
		}
		merged[i].RRFScore = score
	}
	rrf.StableSort(merged)
	return merged
}

type chunkRow struct {
	chunkID   string
	filePath  string
	startLine int
	endLine   int
	symbol    string
	snippet   string
	rank      float64
	// weight 是该行在所属路的逐项系数，0 表示未设置（按 1 处理）。
	weight float64
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
