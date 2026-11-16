package service

import (
	"context"

	"agent-platform/pkg/codeindex"
)

// defaultGraphSeedLimit 只从检索排名前 5 的符号扩展。
//
// 全量扩展（20 seed 等权）会让低排名种子的邻居与头部 seed 同权进入 RRF，
// 稀释最相关结果；截断到 top-5 同时保留跨文件桥接召回。
const defaultGraphSeedLimit = codeindex.DefaultGraphSeedLimit

// GraphPolicy 是图扩展（RRF 第三路）的可配策略：
//   - MaxNodes  ：单次扩展产出的邻居 chunk 总数上限；
//   - PerSymbol ：每个种子符号最多扩展出的邻居数（design 4.9.2 取 5）；
//   - Weight    ：图路在 RRF 中的权重（默认 0.8）；
//   - SeedLimit ：参与扩展的种子符号数上限（默认 5；负数=全量）；
//   - SeedDecay ：按种子排名的线性衰减步长（默认 0.1，rank5=0.6；负数=等权）；
//   - Enabled   ：关闭后即使调用方打开 enableGraph 也不走图路。
type GraphPolicy struct {
	Enabled   bool
	MaxNodes  int
	PerSymbol int
	Weight    float64
	SeedLimit int
	SeedDecay float64
}

func DefaultGraphPolicy() GraphPolicy {
	return GraphPolicy{
		Enabled:   true,
		MaxNodes:  codeindex.DefaultGraphMaxNodes,
		PerSymbol: codeindex.DefaultGraphPerSymbol,
		Weight:    codeindex.DefaultGraphWeight,
		SeedLimit: defaultGraphSeedLimit,
		SeedDecay: codeindex.DefaultGraphSeedDecay,
	}
}

// Normalized 把零值补成默认值：调用方只填关心的字段即可。
// SeedLimit / SeedDecay 的负值是有意义的（全量 / 等权），不会被默认值覆盖。
func (p GraphPolicy) Normalized() GraphPolicy {
	out := p
	if out.MaxNodes <= 0 {
		out.MaxNodes = codeindex.DefaultGraphMaxNodes
	}
	if out.PerSymbol <= 0 {
		out.PerSymbol = codeindex.DefaultGraphPerSymbol
	}
	if out.Weight <= 0 {
		out.Weight = codeindex.DefaultGraphWeight
	}
	if out.SeedLimit == 0 {
		out.SeedLimit = defaultGraphSeedLimit
	}
	if out.SeedDecay == 0 {
		out.SeedDecay = codeindex.DefaultGraphSeedDecay
	}
	return out
}

// GraphExpansion 是一次 1 跳扩展的可观测结果。
type GraphExpansion struct {
	Seeds     []string
	Neighbors []codeindex.GraphNeighbor
	Callers   int
	Callees   int
	Truncated bool
}

// Nodes 返回扩展出的节点数。
func (e GraphExpansion) Nodes() int { return len(e.Neighbors) }

// Sources 返回该路对命中来源列表的贡献标记。
func (e GraphExpansion) Sources() []string {
	if len(e.Neighbors) == 0 {
		return nil
	}
	return []string{codeindex.RouteGraph}
}

// GraphExpander 把检索命中转成图扩展查询，并按策略限制规模。
type GraphExpander struct {
	writer *codeindex.CodeWriter
	policy GraphPolicy
}

func NewGraphExpander(writer *codeindex.CodeWriter) *GraphExpander {
	return &GraphExpander{writer: writer, policy: DefaultGraphPolicy()}
}

func (g *GraphExpander) Policy() GraphPolicy {
	if g == nil {
		return DefaultGraphPolicy()
	}
	return g.policy
}

func (g *GraphExpander) SetPolicy(p GraphPolicy) {
	if g == nil {
		return
	}
	g.policy = p.Normalized()
}

// Apply 把策略写进检索参数，保证 Search 与 Expand 用同一套上限/权重。
func (g *GraphExpander) Apply(opts codeindex.SearchOptions, enabled bool) codeindex.SearchOptions {
	policy := g.Policy()
	opts.EnableGraph = enabled && policy.Enabled
	opts.GraphMaxNodes = policy.MaxNodes
	opts.GraphPerSymbol = policy.PerSymbol
	opts.GraphWeight = policy.Weight
	// 负值在 SearchOptions 里用 0 表达「全量 / 等权」。
	opts.GraphSeedLimit = policy.SeedLimit
	if policy.SeedLimit < 0 {
		opts.GraphSeedLimit = 0
	}
	opts.GraphSeedDecay = policy.SeedDecay
	if policy.SeedDecay < 0 {
		opts.GraphSeedDecay = 0
	}
	return opts
}

// Expand 从一批种子符号做 1 跳扩展。tenant/project 为空直接拒绝，
// 所有查询在 SQL 层强制行级过滤（design 4.9.3）。
func (g *GraphExpander) Expand(ctx context.Context, tenantID, projectID string, seeds []string) (GraphExpansion, error) {
	if g == nil || g.writer == nil {
		return GraphExpansion{}, ErrIndexDisabled
	}
	if tenantID == "" || projectID == "" {
		return GraphExpansion{}, ErrTenantRequired
	}
	policy := g.Policy()
	if !policy.Enabled {
		return GraphExpansion{}, nil
	}
	trimmed := trimSeeds(seeds, policy.SeedLimit)
	if len(trimmed) == 0 {
		return GraphExpansion{}, nil
	}
	neighbors, err := g.writer.GraphExpand(ctx, codeindex.GraphExpandOptions{
		TenantID:  tenantID,
		ProjectID: projectID,
		Seeds:     trimmed,
		MaxNodes:  policy.MaxNodes,
		PerSymbol: policy.PerSymbol,
		SeedLimit: policy.SeedLimit,
		DecayStep: policy.SeedDecay,
	})
	if err != nil {
		return GraphExpansion{}, err
	}
	expansion := GraphExpansion{Seeds: trimmed, Neighbors: neighbors}
	for _, n := range neighbors {
		switch n.Direction {
		case codeindex.GraphDirectionCaller:
			expansion.Callers++
		case codeindex.GraphDirectionCallee:
			expansion.Callees++
		}
	}
	expansion.Truncated = len(neighbors) >= policy.MaxNodes
	return expansion, nil
}

// ExpandFromHits 从检索命中的 symbol 出发扩展；没有符号命中时返回空。
func (g *GraphExpander) ExpandFromHits(ctx context.Context, tenantID, projectID string, hits []codeindex.SearchHit) (GraphExpansion, error) {
	return g.Expand(ctx, tenantID, projectID, SeedsFromHits(hits, g.Policy().SeedLimit))
}

// SeedsFromHits 收集命中 chunk 的符号作为扩展种子。
func SeedsFromHits(hits []codeindex.SearchHit, limit int) []string {
	seeds := make([]string, 0, len(hits))
	for _, hit := range hits {
		seeds = append(seeds, hit.Symbol)
	}
	return trimSeeds(seeds, limit)
}

func trimSeeds(seeds []string, limit int) []string {
	seen := make(map[string]bool, len(seeds))
	out := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		if seed == "" || seen[seed] {
			continue
		}
		seen[seed] = true
		out = append(out, seed)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
