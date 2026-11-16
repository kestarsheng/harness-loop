package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"agent-platform/pkg/codeindex"
)

// variantSpec 是一组图扩展种子策略。
//
// A：全量种子等权（改动前的行为，稀释来源）
// B：top-3 等权（硬截断，会丢掉「排名 4-5 但跨文件桥接」的种子）
// C：top-5 + 按种子排名线性衰减（本线默认）
type variantSpec struct {
	name      string
	enable    bool
	seedLimit int
	decay     float64
}

type abcMetrics struct {
	Variant       string  `json:"variant"`
	SeedLimit     int     `json:"seed_limit"`
	Decay         float64 `json:"decay"`
	Recall5File   float64 `json:"recall5_file"`
	Recall20File  float64 `json:"recall20_file"`
	Recall20Sym   float64 `json:"recall20_sym"`
	MRRFile       float64 `json:"mrr_file"`
	MRRSym        float64 `json:"mrr_sym"`
	GraphOnlyTop5 float64 `json:"graph_only_top5_ratio"`
	GraphAnyTop5  float64 `json:"graph_any_top5_ratio"`
	P50Ms         float64 `json:"p50_ms"`
	P95Ms         float64 `json:"p95_ms"`
	Queries       int     `json:"queries"`
}

type abcRun struct {
	spec variantSpec
	hits [][]hit
	lat  []time.Duration
}

func graphVariants(decay float64) []variantSpec {
	return []variantSpec{
		{name: "rrf", enable: false},
		{name: "A-full-equal", enable: true, seedLimit: 0, decay: 0},
		{name: "B-top3-equal", enable: true, seedLimit: 3, decay: 0},
		{name: "C-top5-decay", enable: true, seedLimit: 5, decay: decay},
	}
}

func runGraphABC(ctx context.Context, writer *codeindex.CodeWriter, root string,
	queries []QueryCase, topK int, decay float64, asJSON bool) error {
	runs := make([]*abcRun, 0, len(graphVariants(decay)))
	for _, spec := range graphVariants(decay) {
		runs = append(runs, &abcRun{spec: spec})
	}

	// 同一语料、同一 query 顺序、同一进程内连续跑四组，避免跨运行的环境漂移。
	for _, q := range queries {
		for _, run := range runs {
			start := time.Now()
			hits, _, err := writer.Search(ctx, codeindex.SearchOptions{
				TenantID:       benchTenantID,
				ProjectID:      benchProjectID,
				Query:          q.Query,
				TopK:           topK,
				Embedder:       fakeEmbedder,
				EnableGraph:    run.spec.enable,
				GraphSeedLimit: run.spec.seedLimit,
				GraphSeedDecay: run.spec.decay,
			})
			if err != nil {
				return fmt.Errorf("%s %s: %w", run.spec.name, q.ID, err)
			}
			run.hits = append(run.hits, toHits(root, hits))
			run.lat = append(run.lat, time.Since(start))
		}
	}

	metrics := make([]abcMetrics, 0, len(runs))
	for _, run := range runs {
		metrics = append(metrics, measureABC(run, queries))
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, m := range metrics {
			if err := enc.Encode(m); err != nil {
				return err
			}
		}
		return nil
	}
	printABCReport(metrics)
	return nil
}

func measureABC(run *abcRun, queries []QueryCase) abcMetrics {
	m := abcMetrics{
		Variant:   run.spec.name,
		SeedLimit: run.spec.seedLimit,
		Decay:     run.spec.decay,
		Queries:   len(queries),
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
				// 只有图路一条来源：这一条是纯靠扩展被拉上来的，属于稀释信号。
				graphOnly++
			}
		}
	}

	n := float64(len(queries))
	m.Recall5File = float64(fileHits5) / n
	m.Recall20File = float64(fileHits20) / n
	m.Recall20Sym = float64(symHits20) / n
	m.MRRFile = sumFile / n
	m.MRRSym = sumSym / n
	// 分母固定 5*n：即使返回不足 5 条也按满额算，保证四组可比。
	m.GraphOnlyTop5 = float64(graphOnly) / (5 * n)
	m.GraphAnyTop5 = float64(graphAny) / (5 * n)
	m.P50Ms = percentile(run.lat, 0.50).Seconds() * 1000
	m.P95Ms = percentile(run.lat, 0.95).Seconds() * 1000
	return m
}

func containsSource(sources []string, want string) bool {
	for _, source := range sources {
		if source == want {
			return true
		}
	}
	return false
}

func printABCReport(metrics []abcMetrics) {
	base := metrics[0]
	byName := map[string]abcMetrics{}
	for _, m := range metrics {
		byName[m.Variant] = m
	}

	fmt.Println("## 图扩展种子策略对照（A/B/C，自动采样集）")
	fmt.Println()
	fmt.Println("| 组 | seed 上限 | 衰减步长 | Recall@5(文件) | Recall@20(文件) | MRR(文件) | MRR(函数) | 图独占 top5 | 图参与 top5 | P50 | P95 |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|")
	for _, m := range metrics {
		limit := fmt.Sprintf("%d", m.SeedLimit)
		if m.SeedLimit <= 0 {
			limit = "全量"
		}
		decay := "等权"
		if m.Decay > 0 {
			decay = fmt.Sprintf("%.2f", m.Decay)
		}
		fmt.Printf("| %s | %s | %s | %.3f | %.3f | %.3f | %.3f | %.1f%% | %.1f%% | %.1fms | %.1fms |\n",
			m.Variant, limit, decay, m.Recall5File, m.Recall20File, m.MRRFile, m.MRRSym,
			m.GraphOnlyTop5*100, m.GraphAnyTop5*100, m.P50Ms, m.P95Ms)
	}

	fmt.Println()
	fmt.Println("| 对比 | ΔMRR(文件) | ΔMRR(函数) | Δ图独占 top5 |")
	fmt.Println("|---|---|---|---|")
	for _, name := range []string{"A-full-equal", "B-top3-equal", "C-top5-decay"} {
		m, ok := byName[name]
		if !ok {
			continue
		}
		fmt.Printf("| %s vs rrf | %+.1f%% | %+.1f%% | %+.1fpp |\n",
			name, pct(m.MRRFile, base.MRRFile), pct(m.MRRSym, base.MRRSym),
			(m.GraphOnlyTop5-base.GraphOnlyTop5)*100)
	}
	fmt.Println()
	fmt.Println("注：图独占 top5 = 前 5 条中「只有 graph 一路来源」的占比（分母固定 5×query 数），衡量稀释度。")
	fmt.Println("注：queries.jsonl 为自动采样，非人工标注；Recall@20 已饱和 1.000，看 MRR 与稀释度。")
}
