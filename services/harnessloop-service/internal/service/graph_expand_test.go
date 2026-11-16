package service

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"agent-platform/pkg/codeindex"
)

const extraCallers = `package sample

func CallerOne() {
	LoginFlow()
}

func CallerTwo() {
	LoginFlow()
}

func CallerThree() {
	LoginFlow()
}
`

func graphFixture(t *testing.T) searchFixture {
	t.Helper()
	f := setupSearch(t)
	if _, err := f.writer.IndexFile(context.Background(), idxTenant, idxProject,
		f.path+"-extra.go", []byte(extraCallers), time.Now().UTC(), true); err != nil {
		t.Fatalf("index extra callers: %v", err)
	}
	return f
}

func TestGraphExpandReturnsDirectCallers(t *testing.T) {
	f := setupSearch(t)
	svc := NewIndexService(f.writer)
	ctx := context.Background()

	res, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow", 10, nil, true)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Stats.GraphHits == 0 || res.Stats.GraphCallers == 0 {
		t.Fatalf("graph route must expand callers: %+v", res.Stats)
	}

	found := false
	for _, hit := range res.Hits {
		if hit.Symbol != "Caller" {
			continue
		}
		found = true
		hasGraph := false
		for _, source := range hit.Sources {
			if source == codeindex.RouteGraph {
				hasGraph = true
			}
		}
		if !hasGraph {
			t.Fatalf("caller hit must carry graph source: %+v", hit.Sources)
		}
	}
	if !found {
		t.Fatalf("direct caller must be retrievable: %+v", res.Hits)
	}
}

func TestGraphExpandSourcesCoverAllRoutes(t *testing.T) {
	f := graphFixture(t)
	ctx := context.Background()
	vec := make([]float32, 1024)
	vec[0] = 1
	if _, err := f.db.Exec(ctx, `
		UPDATE hlp_code.code_chunks SET embedding = $1::vector
		WHERE tenant_id = $2 AND project_id = $3 AND symbol = 'LoginFlow'`,
		"[1"+repeatZero(1023)+"]", idxTenant, idxProject); err != nil {
		t.Fatalf("set embedding: %v", err)
	}
	embedder := func(ctx context.Context, texts []string) ([][]float32, error) {
		return [][]float32{vec}, nil
	}

	svc := NewIndexService(f.writer)
	res, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow", 10, embedder, true)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	seen := map[string]bool{}
	combined := false
	for _, hit := range res.Hits {
		local := map[string]bool{}
		for _, source := range hit.Sources {
			seen[source] = true
			local[source] = true
		}
		if local[codeindex.RouteFulltext] && local[codeindex.RouteGraph] {
			combined = true
		}
	}
	for _, route := range []string{codeindex.RouteFulltext, codeindex.RouteVector, codeindex.RouteGraph} {
		if !seen[route] {
			t.Fatalf("route %s missing from sources: %v", route, seen)
		}
	}
	if !combined {
		t.Fatalf("expected a hit carrying both fulltext and graph: %+v", res.Hits)
	}
}

func TestGraphExpandRespectsNodeCap(t *testing.T) {
	f := graphFixture(t)
	svc := NewIndexService(f.writer)
	svc.SetGraphPolicy(GraphPolicy{Enabled: true, MaxNodes: 2, PerSymbol: 1, Weight: 0.8, SeedLimit: 5})
	ctx := context.Background()

	hits, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow", 10, nil, false)
	if err != nil {
		t.Fatalf("seed search: %v", err)
	}
	expansion, err := svc.ExpandGraph(ctx, idxTenant, idxProject, hits.Hits)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if expansion.Nodes() > 2 {
		t.Fatalf("expansion must respect MaxNodes=2, got %d", expansion.Nodes())
	}
	if expansion.Nodes() == 0 {
		t.Fatalf("cap must not drop all neighbors: %+v", expansion)
	}
	if expansion.Callers == 0 {
		t.Fatalf("callers expected in expansion: %+v", expansion)
	}
	if !expansion.Truncated && expansion.Nodes() == 2 {
		t.Fatalf("truncation must be reported when the cap is reached: %+v", expansion)
	}
	if sources := expansion.Sources(); len(sources) != 1 || sources[0] != codeindex.RouteGraph {
		t.Fatalf("sources = %v", sources)
	}
}

func TestGraphExpandTenantIsolation(t *testing.T) {
	f := graphFixture(t)
	svc := NewIndexService(f.writer)
	ctx := context.Background()

	hits, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow", 5, nil, false)
	if err != nil {
		t.Fatalf("seed search: %v", err)
	}
	otherTenant, err := svc.ExpandGraph(ctx, "another-tenant", idxProject, hits.Hits)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if otherTenant.Nodes() != 0 {
		t.Fatalf("cross-tenant expansion must be empty: %+v", otherTenant)
	}
	otherProject, err := svc.ExpandGraph(ctx, idxTenant, "another-project", hits.Hits)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if otherProject.Nodes() != 0 {
		t.Fatalf("cross-project expansion must be empty: %+v", otherProject)
	}
	if _, err := svc.ExpandGraph(ctx, "", idxProject, hits.Hits); err == nil {
		t.Fatal("missing tenant must fail")
	}
}

func TestGraphExpandDisabledPolicy(t *testing.T) {
	f := setupSearch(t)
	svc := NewIndexService(f.writer)
	svc.SetGraphPolicy(GraphPolicy{Enabled: false})
	res, err := svc.Search(context.Background(), idxTenant, idxProject, "LoginFlow", 5, nil, true)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Stats.GraphHits != 0 {
		t.Fatalf("disabled policy must not expand: %+v", res.Stats)
	}
}

func TestGraphExpandLatencyP95(t *testing.T) {
	// 延迟门槛必须在独占 PG 上测量：并发跑全量测试时同一实例的慢查询会互相污染。
	if os.Getenv("HARNESS_PERF") == "" {
		t.Skip("set HARNESS_PERF=1 to run the P95 latency assertion")
	}
	f := setupSearch(t)
	svc := NewIndexService(f.writer)
	ctx := context.Background()

	// 预热，避免首个请求承担连接池/计划缓存成本。
	if _, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow", 10, nil, true); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	const rounds = 20
	durations := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		if _, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow login", 10, nil, true); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
		durations = append(durations, time.Since(start))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[len(durations)*95/100-1]
	if p95 > 500*time.Millisecond {
		t.Fatalf("graph P95 = %v, want <= 500ms (samples=%v)", p95, durations)
	}
	t.Logf("graph search P95=%v min=%v max=%v rounds=%d", p95, durations[0], durations[len(durations)-1], rounds)
}

func TestSeedsFromHitsDedupAndLimit(t *testing.T) {
	hits := []codeindex.SearchHit{
		{Symbol: "A"}, {Symbol: "A"}, {Symbol: ""}, {Symbol: "B"}, {Symbol: "C"},
	}
	seeds := SeedsFromHits(hits, 2)
	if len(seeds) != 2 || seeds[0] != "A" || seeds[1] != "B" {
		t.Fatalf("seeds = %v", seeds)
	}
	if got := DefaultGraphPolicy().Normalized(); got.PerSymbol != 5 || got.MaxNodes != 50 || got.Weight != 0.8 {
		t.Fatalf("defaults = %+v", got)
	}
}

func repeatZero(n int) string {
	out := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		out = append(out, ',', '0')
	}
	return string(out)
}
