package codeindex

import (
	"context"
	"fmt"
	"strings"
)

// 图扩展默认参数（design 4.9.2：每个命中符号 ≤ 5 跳邻居，图路权重 0.8）。
const (
	DefaultGraphPerSymbol = 5
	DefaultGraphMaxNodes  = 50
	DefaultGraphWeight    = 0.8
	// DefaultGraphSeedLimit 只从检索排名前 N 的种子符号扩展。
	// 全量扩展会让低排名种子的邻居与头部 seed 同权进入 RRF，稀释最相关结果。
	DefaultGraphSeedLimit = 5
	// DefaultGraphSeedDecay 是种子排名每降一位的权重衰减步长：
	// w(rank) = max(0, 1-(rank-1)*step)，rank1=1.0、rank5=0.6。
	DefaultGraphSeedDecay = 0.1
)

// 扩展方向标记，写入 sources/统计，便于前端与压测区分调用方与被调用方。
const (
	GraphDirectionCaller = "caller"
	GraphDirectionCallee = "callee"
	// GraphDirectionBoth 是双向扩展（caller + callee），也是零值等价方向。
	GraphDirectionBoth = "both"
)

// NormalizeGraphDirection 把方向归一化为 caller / callee / both 三取一。
//
// 空串与未知值一律回退 both：方向只决定「召回哪一侧邻居」，不影响权重与融合公式，
// 不该让一个拼错的开关把整条检索打挂。归一化集中在这里，调用方无需自己判空。
func NormalizeGraphDirection(direction string) string {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case GraphDirectionCaller:
		return GraphDirectionCaller
	case GraphDirectionCallee:
		return GraphDirectionCallee
	default:
		return GraphDirectionBoth
	}
}

// GraphExpandOptions 描述一次 1 跳扩展。
type GraphExpandOptions struct {
	TenantID  string
	ProjectID string
	// Seeds 必须按检索排名顺序传入（高相关在前）；扩展额度与衰减都按这个顺序算。
	Seeds     []string
	MaxNodes  int
	PerSymbol int
	// SeedLimit > 0 时只取前 N 个种子；<= 0 表示全量扩展。
	SeedLimit int
	// DecayStep > 0 时按种子排名线性衰减权重；<= 0 表示等权。
	DecayStep float64
	// Direction 限定扩展方向：caller / callee / both。
	// 空串与未知值归一化为 both（既有调用方零改动即保持双向行为）。
	// 方向只决定召回哪一侧邻居，不改变种子截断、额度上限与权重公式。
	Direction string
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
	// SeedRank 是 seed 在本次扩展种子列表中的排名（1 起）。
	SeedRank int
	// Weight 是该邻居的相对权重（已含排名衰减，1.0 为满权）。
	Weight float64
}

// GraphExpand 从命中符号沿 code_edges 做 1 跳扩展，方向由 opts.Direction 决定
// （caller / callee / both，空串 = both）。
//
// 实现分两段，避免「每条边回表查 chunk」的嵌套循环（code_chunks 上没有
// (tenant,project,symbol) 索引时会退化成逐边扫描，实测单种子 50ms+）：
//  1. 用种子符号在 code_edges 上取一跳邻居的符号集合（按 opts.Direction 取臂，按种子排名排序）；
//  2. 用这组符号一次性取 chunk，再在 Go 侧按 seed 分组、去重、封顶。
//
// 两段查询都强制 tenant_id + project_id 行级过滤，跨租户/项目返回空。
func (w *CodeWriter) GraphExpand(ctx context.Context, opts GraphExpandOptions) ([]GraphNeighbor, error) {
	seeds := cleanSeeds(opts.Seeds)
	if opts.SeedLimit > 0 && len(seeds) > opts.SeedLimit {
		seeds = seeds[:opts.SeedLimit]
	}
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
	direction := NormalizeGraphDirection(opts.Direction)

	seedRank := make(map[string]int, len(seeds))
	for i, seed := range seeds {
		seedRank[seed] = i + 1
	}

	edges, err := w.graphEdges(ctx, opts.TenantID, opts.ProjectID, seeds, maxNodes*4, perSymbol*2, direction)
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
		rank := seedRank[edge.seed]
		weight := seedDecayWeight(rank, opts.DecayStep)
		if weight <= 0 {
			// 衰减到 0 等同不参与融合，不必占节点额度。
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
			SeedRank:  rank,
			Weight:    weight,
		})
	}
	return out, nil
}

// seedDecayWeight 按种子排名做线性衰减：rank1 = 1.0，每降一位减 step，下限 0。
func seedDecayWeight(rank int, step float64) float64 {
	if step <= 0 || rank <= 1 {
		return 1
	}
	weight := 1 - float64(rank-1)*step
	if weight < 0 {
		return 0
	}
	return weight
}

type graphEdge struct {
	seed      string
	seedOrd   int
	filePath  string
	symbol    string
	direction string
}

// graphEdges 取一跳邻居的符号集合：target=seed 是调用方，source=seed 是被调用方。
//
// 两个细节决定多 seed 扩展是否真的成立：
//   - 种子带 ORDINALITY 透出排名，ORDER BY ord 保证高排名种子先占 MaxNodes 额度；
//   - 每 seed 先按 ROW_NUMBER 截到 perSeed 条再汇总，否则单个高频 seed 会用光
//     LIMIT，后面的种子一条都进不来（实测首个 seed 有 197 条边时，20 条预算全被吃掉）。
//
// direction 决定拼哪几个臂：单臂时该侧独占 perSeed 份额度（测的是单路真实上限），
// 双臂时沿用 direction 排序共享同一份额度——即加方向维度前的既有行为。
func (w *CodeWriter) graphEdges(ctx context.Context, tenantID, projectID string, seeds []string,
	limit, perSeed int, direction string) ([]graphEdge, error) {
	if limit <= 0 {
		limit = DefaultGraphMaxNodes * 4
	}
	if perSeed <= 0 {
		perSeed = DefaultGraphPerSymbol * 2
	}

	const callerArm = `
			SELECT s.sym AS seed, s.ord AS ord, e.source_file AS file_path,
			       e.source_symbol AS symbol, 'caller' AS direction
			FROM seeds s
			JOIN hlp_code.code_edges e
			  ON e.tenant_id = $1 AND e.project_id = $2 AND e.target_symbol = s.sym
			WHERE e.source_symbol <> s.sym`
	const calleeArm = `
			SELECT s.sym AS seed, s.ord AS ord, ''::text AS file_path,
			       e.target_symbol AS symbol, 'callee' AS direction
			FROM seeds s
			JOIN hlp_code.code_edges e
			  ON e.tenant_id = $1 AND e.project_id = $2 AND e.source_symbol = s.sym
			WHERE e.target_symbol <> s.sym`

	var arms string
	switch direction {
	case GraphDirectionCaller:
		arms = callerArm
	case GraphDirectionCallee:
		arms = calleeArm
	default:
		arms = callerArm + "\n\t\t\tUNION ALL" + calleeArm
	}

	rows, err := w.db.Query(ctx, `
		WITH seeds(sym, ord) AS (
			SELECT sym, ord FROM unnest($3::text[]) WITH ORDINALITY AS u(sym, ord)
		)
		, neighbors AS (`+arms+`
		)
		, ranked AS (
			SELECT seed, ord, file_path, symbol, direction,
			       row_number() OVER (PARTITION BY seed ORDER BY direction, file_path, symbol) AS rn
			FROM (SELECT DISTINCT seed, ord, file_path, symbol, direction FROM neighbors) deduped
		)
		SELECT seed, ord, file_path, symbol, direction
		FROM ranked
		WHERE rn <= $5
		ORDER BY ord, rn
		LIMIT $4`, tenantID, projectID, seeds, limit, perSeed)
	if err != nil {
		return nil, fmt.Errorf("codeindex: graph edges: %w", err)
	}
	defer rows.Close()

	var out []graphEdge
	for rows.Next() {
		var edge graphEdge
		if err := rows.Scan(&edge.seed, &edge.seedOrd, &edge.filePath, &edge.symbol, &edge.direction); err != nil {
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

// cleanSeeds 去重但保留传入顺序——顺序即检索排名，截断与衰减都依赖它。
// 这里刻意不排序：按字母序会丢失「哪个种子更相关」的信息。
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
	return out
}
