// searchbench 是 T-P2-T1 的「代码混合检索」压测程序（DFX-P-03）。
//
// 口径：50 个 query × 10 轮，top_k=20，三路召回（keyword + vector + graph）
// 经 RRF 合并，不含 Reranker。门槛 P95 ≤ 500ms。
//
// 两个必须写明的限制（不写就是误导）：
//  1. 基准夹具 repo-500 索引时 embedding 传的是 nil（见 bench/repo-500/cmd），
//     所以 code_chunks 的向量列全为 NULL，vector 路实际召回 0 条。
//     本程序因此只覆盖 keyword + graph 两路经 RRF 合并的路径——
//     延迟数字成立，但「三路都有命中」的场景没有实测。
//  2. query 取自夹具里的真实符号名（确定性生成），不是人手挑的"好查的词"，
//     避免挑查询把 P95 压低。
//
// 用法：
//
//	go run ./bench/p2/scripts/cmd/searchbench -queries 50 -rounds 10
//
// 前置：仓库已用 bench/repo-500 的 tenant/project 索引过（last-run.json 里有记录）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/pgindex"
)

type result struct {
	Name      string          `json:"name"`
	StartedAt string          `json:"started_at"`
	Tenant    string          `json:"tenant"`
	Project   string          `json:"project"`
	Queries   int             `json:"queries"`
	Rounds    int             `json:"rounds"`
	TopK      int             `json:"top_k"`
	Total     int             `json:"total_calls"`
	Failures  int             `json:"failures"`
	P50Ms     float64         `json:"p50_ms"`
	P95Ms     float64         `json:"p95_ms"`
	P99Ms     float64         `json:"p99_ms"`
	MaxMs     float64         `json:"max_ms"`
	AvgHits   float64         `json:"avg_hits"`
	EmptyHits int             `json:"empty_result_calls"`
	RouteMix  map[string]int  `json:"route_hits_total"`
	SampleQs  []string        `json:"sample_queries"`
	Budgets   map[string]bool `json:"budgets"`
	Hardware  map[string]any  `json:"hardware"`
	Passed    bool            `json:"passed"`
	Notes     []string        `json:"notes"`
}

func main() {
	dsn := flag.String("dsn", os.Getenv("HARNESS_TEST_PG_DSN"), "PostgreSQL DSN")
	tenant := flag.String("tenant", "tenant-bench-repo500", "tenant id (must match the indexed fixture)")
	project := flag.String("project", "proj-bench-repo500", "project id")
	fixture := flag.String("dir", filepath.Join("bench", "repo-500", "testdata", "repo"), "fixture dir used to derive queries")
	nq := flag.Int("queries", 50, "number of distinct queries")
	rounds := flag.Int("rounds", 10, "rounds over the query set")
	topK := flag.Int("top-k", 20, "top_k")
	budgetMs := flag.Float64("budget-ms", 500, "P95 budget (DFX-P-03)")
	out := flag.String("out", "", "write JSON result here")
	flag.Parse()

	if *dsn == "" {
		fatal("HARNESS_TEST_PG_DSN / -dsn is required")
	}

	queries, err := deriveQueries(*fixture, *nq)
	if err != nil {
		fatal("derive queries: %v", err)
	}
	if len(queries) == 0 {
		fatal("no query could be derived from %s", *fixture)
	}

	ctx := context.Background()
	cfg := pgindex.DefaultConfig(*dsn)
	db, err := pgindex.Open(ctx, cfg)
	if err != nil {
		fatal("open pg: %v", err)
	}
	defer db.Close()

	// embed 传 nil：与索引夹具保持一致（向量列为 NULL），避免这里偷偷换一套条件。
	var registry parser.Registry
	writer := codeindex.NewCodeWriter(db, func(path string, src []byte) (codeindex.ParseResult, error) {
		return registry.Parse(path, src), nil
	}, nil, nil)

	res := result{
		Name:      "code_search",
		StartedAt: time.Now().Format(time.RFC3339),
		Tenant:    *tenant,
		Project:   *project,
		Queries:   len(queries),
		Rounds:    *rounds,
		TopK:      *topK,
		Budgets:   map[string]bool{},
		RouteMix:  map[string]int{},
	}
	res.SampleQs = queries[:min(len(queries), 5)]

	var lat []float64
	totalHits := 0
	for r := 0; r < *rounds; r++ {
		for _, q := range queries {
			t0 := time.Now()
			hits, stats, err := writer.Search(ctx, codeindex.SearchOptions{
				TenantID:    *tenant,
				ProjectID:   *project,
				Query:       q,
				TopK:        *topK,
				RRFK:        60,
				EnableGraph: true,
			})
			el := float64(time.Since(t0).Microseconds()) / 1000
			if err != nil {
				res.Failures++
				res.Notes = append(res.Notes, fmt.Sprintf("query %q: %v", q, err))
				continue
			}
			lat = append(lat, el)
			totalHits += len(hits)
			if len(hits) == 0 {
				res.EmptyHits++
			}
			res.RouteMix["fulltext"] += stats.KeywordHits
			res.RouteMix["vector"] += stats.VectorHits
			res.RouteMix["graph"] += stats.GraphHits
		}
	}
	res.Total = len(lat) + res.Failures
	if len(lat) == 0 {
		fatal("no successful search; check tenant/project and that the fixture is indexed")
	}

	sort.Float64s(lat)
	res.P50Ms = pctF(lat, 50)
	res.P95Ms = pctF(lat, 95)
	res.P99Ms = pctF(lat, 99)
	res.MaxMs = lat[len(lat)-1]
	res.AvgHits = float64(totalHits) / float64(len(lat))

	res.Hardware = map[string]any{
		"os":            "windows/amd64",
		"index_fixture": "bench/repo-500 (500 files, 5994 chunks, 15482 edges)",
		"routes":        "keyword + graph via RRF (vector is NULL in this fixture)",
		"reranker":      "disabled (per DFX-P-03)",
		"budget":        "P95 <= 500ms",
	}
	res.Budgets["p95_le_budget"] = res.P95Ms <= *budgetMs
	res.Budgets["no_failures"] = res.Failures == 0
	res.Passed = res.Budgets["p95_le_budget"] && res.Budgets["no_failures"]
	if res.RouteMix["vector"] == 0 {
		res.Notes = append(res.Notes, "vector route returned 0 hits: fixture indexed with nil embedder (expected)")
	}

	emit(res, *out)
}

// deriveQueries 从夹具里提取符号名作为查询。
//
// 用符号名而不是自然语言，是因为这是代码检索最典型的用法（找函数/类型），
// 而且符号名来自夹具本身，任何人在任何机器上重跑都会得到同一组 query。
func deriveQueries(dir string, want int) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" && filepath.Base(filepath.Dir(path)) == "repo-500" {
				return nil
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	re := regexp.MustCompile(`func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]{3,})`)
	seen := map[string]bool{}
	var queries []string
	for _, f := range files {
		if len(queries) >= want {
			break
		}
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			name := m[1]
			if seen[name] || len(name) < 4 {
				continue
			}
			// 过滤掉噪声名（main/init 之类没有区分度）。
			if name == "main" || name == "init" {
				continue
			}
			seen[name] = true
			queries = append(queries, name)
			if len(queries) >= want {
				break
			}
		}
	}
	sort.Strings(queries)
	return queries, nil
}

func pctF(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100)
	return sorted[idx]
}

func emit(res result, out string) {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fatal("marshal: %v", err)
	}
	if out != "" {
		_ = os.WriteFile(out, b, 0o644)
	}
	fmt.Println(string(b))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "searchbench: "+format+"\n", args...)
	os.Exit(1)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
