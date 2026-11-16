package main

// caller/callee 方向诊断（只诊断，不调参）。
//
// 变量只有一个：图扩展的**方向**。其余全部锁定在 B 组（top-3 等权）：
//   - SeedLimit = 3，DecayStep = 0（等权），GraphWeight 留默认 0.8；
//   - 同一语料、同一 query 集、同一进程、同一台机器内连续跑；
//   - 每组重复 4 次，报区间（min ~ max + 中位数），不报单次值。
//
// 主指标：函数级 MRR；辅助：文件级 MRR。
// 不调 seed 上限、不调衰减、不调权重、不改融合公式——任何"把指标调绿"的操作都不在本线范围。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"agent-platform/pkg/codeindex"
)

// directionSpec 是一组方向配置。enable=false 即无图基线。
type directionSpec struct {
	name      string
	enable    bool
	direction string
	note      string
}

// 固定的 B 组参数：只加 direction 维度。
const (
	directionSeedLimit = 3
	directionDecay     = 0.0
	directionRepeats   = 4
	// directionNoiseBand 是"略正"的判定带宽：Δ < +1% 视为落在重复抖动带内，
	// 不足以单独支撑"图路有效"的结论。
	directionNoiseBand = 1.0
)

func directionSpecs() []directionSpec {
	return []directionSpec{
		{name: "rrf-baseline", enable: false, note: "无图基线"},
		{name: "caller-only", enable: true, direction: codeindex.GraphDirectionCaller, note: "只取调用方"},
		{name: "callee-only", enable: true, direction: codeindex.GraphDirectionCallee, note: "只取被调用方"},
		{name: "both", enable: true, direction: codeindex.GraphDirectionBoth, note: "双向（现状）"},
	}
}

type directionMetrics struct {
	Variant       string  `json:"variant"`
	Rep           int     `json:"rep"`
	Direction     string  `json:"direction"`
	MRRSym        float64 `json:"mrr_sym"`
	MRRFile       float64 `json:"mrr_file"`
	Recall5File   float64 `json:"recall5_file"`
	Recall20File  float64 `json:"recall20_file"`
	Recall20Sym   float64 `json:"recall20_sym"`
	GraphOnlyTop5 float64 `json:"graph_only_top5_ratio"`
	GraphAnyTop5  float64 `json:"graph_any_top5_ratio"`
	GraphCallers  int     `json:"graph_callers"`
	GraphCallees  int     `json:"graph_callees"`
	P50Ms         float64 `json:"p50_ms"`
	P95Ms         float64 `json:"p95_ms"`
	Queries       int     `json:"queries"`
}

type directionRun struct {
	spec directionSpec
	hits [][]hit
	lat  []time.Duration
	// graphCallers/graphCallees 累计 SearchStats，用来验证方向开关真的生效
	// （caller-only 组的 callees 必须为 0，否则说明方向没传下去）。
	graphCallers int
	graphCallees int
}

func runGraphDirection(ctx context.Context, writer *codeindex.CodeWriter, root string,
	queries []QueryCase, topK, repeats int, asJSON bool) error {
	if repeats <= 0 {
		repeats = directionRepeats
	}
	specs := directionSpecs()

	var all []directionMetrics
	for rep := 1; rep <= repeats; rep++ {
		runs := make([]*directionRun, len(specs))
		for i, spec := range specs {
			runs[i] = &directionRun{spec: spec}
		}
		for _, q := range queries {
			for _, run := range runs {
				opts := codeindex.SearchOptions{
					TenantID:  benchTenantID,
					ProjectID: benchProjectID,
					Query:     q.Query,
					TopK:      topK,
					Embedder:  fakeEmbedder,
					// 无图基线也必须带 embedder，否则 keyword/vector 路不一样，比不了。
					EnableGraph:    run.spec.enable,
					GraphSeedLimit: directionSeedLimit,
					GraphSeedDecay: directionDecay,
					GraphDirection: run.spec.direction,
				}
				start := time.Now()
				hits, stats, err := writer.Search(ctx, opts)
				if err != nil {
					return fmt.Errorf("%s rep%d %s: %w", run.spec.name, rep, q.ID, err)
				}
				run.lat = append(run.lat, time.Since(start))
				run.hits = append(run.hits, toHits(root, hits))
				run.graphCallers += stats.GraphCallers
				run.graphCallees += stats.GraphCallees
			}
		}
		for _, run := range runs {
			m := measureDirection(run, queries)
			m.Rep = rep
			all = append(all, m)
		}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, m := range all {
			if err := enc.Encode(m); err != nil {
				return err
			}
		}
		return nil
	}
	printDirectionReport(all, specs, queries, repeats)
	return nil
}

func measureDirection(run *directionRun, queries []QueryCase) directionMetrics {
	m := directionMetrics{
		Variant:      run.spec.name,
		Direction:    run.spec.direction,
		GraphCallers: run.graphCallers,
		GraphCallees: run.graphCallees,
		Queries:      len(queries),
	}
	var fileHits5, fileHits20, symHits20, graphOnly, graphAny int
	var sumFile, sumSym float64

	for i, hits := range run.hits {
		if i >= len(queries) {
			break
		}
		q := queries[i]
		if rank := rankFile(hits, q); rank > 0 {
			sumFile += 1 / float64(rank)
			if rank <= 5 {
				fileHits5++
			}
			if rank <= 20 {
				fileHits20++
			}
		}
		if rank := rankSym(hits, q); rank > 0 {
			sumSym += 1 / float64(rank)
			if rank <= 20 {
				symHits20++
			}
		}
		limit := 5
		if len(hits) < limit {
			limit = len(hits)
		}
		for _, h := range hits[:limit] {
			if !containsSource(h.Sources, codeindex.RouteGraph) {
				continue
			}
			graphAny++
			if len(h.Sources) == 1 {
				graphOnly++
			}
		}
	}

	n := float64(len(queries))
	m.Recall5File = float64(fileHits5) / n
	m.Recall20File = float64(fileHits20) / n
	m.Recall20Sym = float64(symHits20) / n
	m.MRRSym = sumSym / n
	m.MRRFile = sumFile / n
	m.GraphOnlyTop5 = float64(graphOnly) / (5 * n)
	m.GraphAnyTop5 = float64(graphAny) / (5 * n)
	m.P50Ms = percentile(run.lat, 0.50).Seconds() * 1000
	m.P95Ms = percentile(run.lat, 0.95).Seconds() * 1000
	return m
}

// dirAgg 把同一组的多次重复收成区间。
type dirAgg struct {
	spec    directionSpec
	mrrSym  []float64
	mrrFile []float64
	// 其余指标只保留中位数，作为读表时的背景信息。
	recall5File float64
	recall20Sym float64
	graphOnly   float64
	graphAny    float64
	callers     int
	callees     int
	p50         float64
	p95         float64
}

func aggregateDirection(all []directionMetrics, specs []directionSpec) []dirAgg {
	byName := map[string][]directionMetrics{}
	for _, m := range all {
		byName[m.Variant] = append(byName[m.Variant], m)
	}
	out := make([]dirAgg, 0, len(specs))
	for _, spec := range specs {
		ms := byName[spec.name]
		if len(ms) == 0 {
			continue
		}
		agg := dirAgg{spec: spec}
		for _, m := range ms {
			agg.mrrSym = append(agg.mrrSym, m.MRRSym)
			agg.mrrFile = append(agg.mrrFile, m.MRRFile)
			agg.callers += m.GraphCallers
			agg.callees += m.GraphCallees
		}
		last := ms[len(ms)-1]
		agg.recall5File = last.Recall5File
		agg.recall20Sym = last.Recall20Sym
		agg.graphOnly = last.GraphOnlyTop5
		agg.graphAny = last.GraphAnyTop5
		agg.p50 = last.P50Ms
		agg.p95 = last.P95Ms
		out = append(out, agg)
	}
	return out
}

func minMaxMedian(values []float64) (lo, hi, median float64) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	lo, hi = sorted[0], sorted[len(sorted)-1]
	if len(sorted)%2 == 1 {
		return lo, hi, sorted[len(sorted)/2]
	}
	return lo, hi, (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
}

// decideDirection 把决策规则写死在代码里，避免报告口径漂移。
//
// 规则按顺序命中（"基线"= 无图 rrf 的函数级 MRR 中位数）：
//  1. 任一路 ≥ 基线 → 只保留该路（两路都 ≥ 则取更高者）；
//  2. 两路都 < 基线 → 默认关闭图路；
//  3. 属于 1 的边界情形：单路只在噪声带内略正（0 ≤ Δ < +1%）而双向综合为负
//     → 拆开关，默认只开单路。
//
// 返回（结论, 依据）。这是诊断结论，不是调参后的推荐值。
func decideDirection(aggs []dirAgg) (string, string) {
	byName := map[string]dirAgg{}
	for _, a := range aggs {
		byName[a.spec.name] = a
	}
	_, _, base := minMaxMedian(byName["rrf-baseline"].mrrSym)
	_, _, caller := minMaxMedian(byName["caller-only"].mrrSym)
	_, _, callee := minMaxMedian(byName["callee-only"].mrrSym)
	_, _, both := minMaxMedian(byName["both"].mrrSym)

	callerDelta := pct(caller, base)
	calleeDelta := pct(callee, base)
	bothDelta := pct(both, base)

	callerOK := caller >= base
	calleeOK := callee >= base
	evidence := fmt.Sprintf("ΔMRR(函数) 相对基线：caller %+.1f%%、callee %+.1f%%、both %+.1f%%",
		callerDelta, calleeDelta, bothDelta)

	if !callerOK && !calleeOK {
		return "默认关闭图路（rules#2：两路都 < 基线）", evidence
	}
	best, bestName := caller, "caller"
	if callee > best {
		best, bestName = callee, "callee"
	}
	bestDelta := pct(best, base)
	if both < base && bestDelta < directionNoiseBand {
		return fmt.Sprintf("拆开关，默认只开 %s（rules#3：单路略正 %+.1f%%、双向 %+.1f%%）", bestName, bestDelta, bothDelta),
			evidence
	}
	return fmt.Sprintf("只保留 %s 单路（rules#1：单路 ≥ 基线 %+.1f%%）", bestName, bestDelta), evidence
}

func printDirectionReport(all []directionMetrics, specs []directionSpec, queries []QueryCase, repeats int) {
	aggs := aggregateDirection(all, specs)
	base := aggs[0]
	_, _, baseSym := minMaxMedian(base.mrrSym)
	_, _, baseFile := minMaxMedian(base.mrrFile)

	fmt.Println("## 图扩展方向诊断（B 组 top-3 等权，只加 direction）")
	fmt.Println()
	fmt.Printf("query=%d；每组重复 %d 次（同集、同机、连续）;主指标=函数级 MRR，辅助=文件级 MRR\n\n",
		len(queries), repeats)
	fmt.Println("| 组 | 方向 | MRR(函数) 区间 | 中位 | Δ vs 基线 | MRR(文件) 中位 | Δ vs 基线 | 图独占 top5 | 图参与 top5 | 扩展caller/callee | P50 | P95 |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|---|")
	for _, a := range aggs {
		loS, hiS, medS := minMaxMedian(a.mrrSym)
		_, _, medF := minMaxMedian(a.mrrFile)
		dir := a.spec.direction
		if dir == "" {
			dir = "—"
		}
		fmt.Printf("| %s | %s | %.3f ~ %.3f | %.3f | %+.1f%% | %.3f | %+.1f%% | %.1f%% | %.1f%% | %d / %d | %.1fms | %.1fms |\n",
			a.spec.name, dir, loS, hiS, medS, pct(medS, baseSym),
			medF, pct(medF, baseFile), a.graphOnly*100, a.graphAny*100, a.callers, a.callees,
			a.p50, a.p95)
	}

	fmt.Println()
	fmt.Println("逐次函数级 MRR（验证区间不是单次抖动）：")
	fmt.Println()
	fmt.Println("| 组 | " + joinReps(repeats) + " |")
	sep := "|---|"
	for i := 0; i < repeats; i++ {
		sep += "---|"
	}
	fmt.Println(sep)
	byName := map[string][]directionMetrics{}
	for _, m := range all {
		byName[m.Variant] = append(byName[m.Variant], m)
	}
	for _, spec := range specs {
		cells := ""
		for _, m := range byName[spec.name] {
			cells += fmt.Sprintf(" %.3f |", m.MRRSym)
		}
		fmt.Printf("| %s |%s\n", spec.name, cells)
	}

	decision, evidence := decideDirection(aggs)
	fmt.Println()
	fmt.Println("### 决策（规则预先写死，不因结果改口径）")
	fmt.Println()
	fmt.Println("- 规则 1：单路 ≥ 基线 → 只保留该路")
	fmt.Println("- 规则 2：两路都 < 基线 → 默认关闭图路")
	fmt.Println("- 规则 3：单路略正（0 ≤ Δ < +1%，重复抖动带内）综合为负 → 拆开关，默认单路")
	fmt.Println()
	fmt.Printf("- 依据：%s\n", evidence)
	fmt.Printf("- 结论：%s\n", decision)
	fmt.Println()
	fmt.Println("注：这是诊断，不是调参——本轮只切方向，seed 上限/衰减/权重/融合公式全部冻结在 B 组。")
	fmt.Println("注：queries.jsonl 为自动采样（非人工标注），向量为词袋哈希伪向量（非语义 embedding）。")
}

func joinReps(repeats int) string {
	out := ""
	for i := 1; i <= repeats; i++ {
		if i > 1 {
			out += " | "
		}
		out += fmt.Sprintf("rep%d", i)
	}
	return out
}
