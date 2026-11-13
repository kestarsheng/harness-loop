// Command retrieval 是 T-P1-T4 的代码检索基准。
//
// 四模式：fulltext / vector / rrf(keyword+vector) / rrf+graph(keyword+vector+graph)。
//
// 用法：
//
//	go run ./bench/retrieval                  # 使用已提交的 queries.jsonl
//	go run ./bench/retrieval -build           # 从仓库重新自动采样并覆盖 queries.jsonl
//	go run ./bench/retrieval -k 20 -distractors 60
//
// 数据来源与标注口径：
//   - queries.jsonl 由 -build 从仓库 Go 源码自动采样生成，ground truth 为「文件 + 函数」；
//   - **自动采样，非人工标注**，报告与输出均显式标注；
//   - 向量为 1024 维词袋哈希伪向量（非语义 embedding），只用于让 vector/rrf 路由可执行；
//   - fulltext/rrf/rrf+graph 直接调用 pkg/codeindex 的 Search；vector 单路由按
//     pkg/codeindex 中同一条 pgvector SQL 独立执行，便于拆解单路效果。
package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/pgindex"
)

const (
	benchTenantID  = "tenant-bench-retrieval"
	benchProjectID = "proj-bench-retrieval"
	defaultDSN     = "postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable"
)

// QueryCase 是一条自动采样的检索样本。label_source 固定为 auto-sampled。
type QueryCase struct {
	ID          string `json:"id"`
	Query       string `json:"query"`
	File        string `json:"file"`
	Symbol      string `json:"symbol"`
	ChunkType   string `json:"chunk_type"`
	LabelSource string `json:"label_source"`
	Note        string `json:"note"`
}

type hit struct {
	File      string
	Symbol    string
	StartLine int
}

type modeResult struct {
	name      string
	queries   [][]hit
	latencies []time.Duration
}

type modeMetrics struct {
	name       string
	recallFile map[int]float64
	recallSym  map[int]float64
	mrrFile    float64
	mrrSym     float64
	p50        time.Duration
	p95        time.Duration
}

func main() {
	var (
		buildFlag   = flag.Bool("build", false, "auto-sample queries from repository and rewrite queries.jsonl")
		queriesPath = flag.String("queries", "bench/retrieval/queries.jsonl", "query set path")
		dsnFlag     = flag.String("dsn", "", "postgres DSN (default: $HARNESS_TEST_PG_DSN or local pgvector)")
		kFlag       = flag.Int("k", 20, "top-K for Recall@K")
		distractors = flag.Int("distractors", 60, "number of distractor files indexed alongside ground-truth files")
		fullOnly    = flag.Bool("full-only", false, "index only ground-truth files (fast sanity run)")
	)
	flag.Parse()
	if *kFlag <= 0 {
		*kFlag = 20
	}

	root, err := findRepoRoot()
	if err != nil {
		fail("locate repo root: %v", err)
	}
	dsn := *dsnFlag
	if dsn == "" {
		dsn = os.Getenv("HARNESS_TEST_PG_DSN")
	}
	if dsn == "" {
		dsn = defaultDSN
	}

	var queries []QueryCase
	if *buildFlag {
		queries, err = buildQuerySet(root, 50)
		if err != nil {
			fail("auto-sample queries: %v", err)
		}
		if err := writeQuerySet(*queriesPath, queries); err != nil {
			fail("write %s: %v", *queriesPath, err)
		}
		fmt.Printf("queries.jsonl: wrote %d auto-sampled queries -> %s\n\n", len(queries), *queriesPath)
	} else {
		queries, err = readQuerySet(*queriesPath)
		if err != nil {
			fail("read %s: %v (run with -build to generate)", *queriesPath, err)
		}
	}
	if len(queries) == 0 {
		fail("query set is empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		fail("connect postgres (set -dsn or HARNESS_TEST_PG_DSN): %v", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		fail("migrate: %v", err)
	}

	writer := codeindex.NewCodeWriter(db, func(p string, src []byte) (codeindex.ParseResult, error) {
		return parser.NewRegistry().Parse(p, src), nil
	}, fakeEmbedder, nil)

	stats, err := indexCorpus(ctx, writer, db, root, queries, *distractors, *fullOnly)
	if err != nil {
		fail("index corpus: %v", err)
	}
	fmt.Printf("corpus: files=%d chunks=%d edges=%d index_total=%v (auto-sampled, vector=word-bag hashing, NOT semantic)\n\n",
		stats.files, stats.chunks, stats.edges, stats.duration)
	fmt.Printf("queries=%d  topK=%d  distractors=%d  labeling=auto-sampled (NOT manually labeled)\n\n",
		len(queries), *kFlag, *distractors)

	results, err := runModes(ctx, writer, db, root, queries, *kFlag)
	if err != nil {
		fail("run modes: %v", err)
	}
	modeMetricsList := make([]modeMetrics, 0, len(results))
	for _, res := range results {
		modeMetricsList = append(modeMetricsList, computeMetrics(res, queries))
	}
	printReport(modeMetricsList, queries, stats)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bench/retrieval: "+format+"\n", args...)
	os.Exit(1)
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 4; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd, nil
		}
		wd = filepath.Dir(wd)
	}
	return "", errors.New("go.mod not found")
}

// ---------------------------------------------------------------- query set

var identifierRE = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

var stopTokens = map[string]bool{
	"func": true, "return": true, "if": true, "else": true, "for": true, "range": true,
	"var": true, "const": true, "type": true, "struct": true, "interface": true,
	"error": true, "err": true, "string": true, "int": true, "int64": true, "bool": true,
	"true": true, "false": true, "nil": true, "make": true, "new": true, "map": true,
	"chan": true, "go": true, "defer": true, "select": true, "case": true, "switch": true,
	"package": true, "import": true, "ctx": true, "context": true, "self": true, "this": true,
	"fmt": true, "len": true, "append": true, "the": true, "and": true, "with": true,
}

type candidate struct {
	rel   string
	abs   string
	chunk codeindex.Chunk
	rank  string
}

// buildQuerySet 自动采样：解析仓库 Go 源码，挑 function/method chunk 作为 ground truth，
// query 由「函数名拆词 + 函数体内其它标识符」组成。
func buildQuerySet(root string, limit int) ([]QueryCase, error) {
	registry := parser.NewRegistry()
	var cands []candidate
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build", "bin", "tmp",
				"coverage", ".harness", "testdata", "frontend":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 256<<10 {
			return nil
		}
		parsed, err := registry.ParseFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		for _, chunk := range parsed.Chunks {
			if chunk.Symbol == "" || strings.Contains(chunk.Symbol, "#part") {
				continue
			}
			if chunk.ChunkType != codeindex.ChunkFunction && chunk.ChunkType != codeindex.ChunkMethod {
				continue
			}
			if strings.Count(chunk.Text, "\n") < 2 || len(chunk.Text) < 40 {
				continue
			}
			cands = append(cands, candidate{
				rel:   filepath.ToSlash(rel),
				abs:   path,
				chunk: chunk,
				rank:  sha1Hex(filepath.ToSlash(rel) + "|" + chunk.Symbol),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].rank < cands[j].rank })

	seen := map[string]bool{}
	perFile := map[string]int{}
	out := make([]QueryCase, 0, limit)
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		key := c.rel + "|" + c.chunk.Symbol
		if seen[key] || perFile[c.rel] >= 2 {
			continue
		}
		query := buildQuery(c.chunk.Symbol, c.chunk.Text)
		if len(strings.Fields(query)) < 2 {
			continue
		}
		seen[key] = true
		perFile[c.rel]++
		out = append(out, QueryCase{
			ID:          fmt.Sprintf("q%03d", len(out)+1),
			Query:       query,
			File:        c.rel,
			Symbol:      c.chunk.Symbol,
			ChunkType:   string(c.chunk.ChunkType),
			LabelSource: "auto-sampled:go-ast",
			Note:        "自动采样，非人工标注",
		})
	}
	if len(out) < limit {
		return nil, fmt.Errorf("only %d usable queries sampled, need %d", len(out), limit)
	}
	return out, nil
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func buildQuery(symbol, text string) string {
	nameTokens := splitIdentifier(symbol)
	inName := map[string]bool{}
	for _, tok := range nameTokens {
		inName[tok] = true
	}
	var extra []string
	seen := map[string]bool{}
	for _, tok := range identifierRE.FindAllString(text, -1) {
		lower := strings.ToLower(tok)
		if len(lower) < 3 || stopTokens[lower] || inName[lower] || seen[lower] {
			continue
		}
		seen[lower] = true
		extra = append(extra, lower)
		if len(extra) == 3 {
			break
		}
	}
	return strings.Join(append(nameTokens, extra...), " ")
}

// splitIdentifier 把 IndexFile / index_file / pkg.T.Code 拆成小写词。
func splitIdentifier(s string) []string {
	s = strings.ReplaceAll(s, ".", " ")
	var b strings.Builder
	var prevLower bool
	for _, r := range s {
		if unicode.IsUpper(r) && prevLower {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
	}
	parts := strings.FieldsFunc(b.String(), func(r rune) bool {
		return r == '_' || r == '-' || r == ' ' || r == '/'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || stopTokens[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

func writeQuerySet(path string, queries []QueryCase) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, q := range queries {
		line, err := json.Marshal(q)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return w.Flush()
}

func readQuerySet(path string) ([]QueryCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []QueryCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var q QueryCase
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			return nil, fmt.Errorf("decode line: %w", err)
		}
		out = append(out, q)
	}
	return out, sc.Err()
}

// ---------------------------------------------------------------- corpus

type corpusStats struct {
	files    int
	chunks   int
	edges    int
	duration time.Duration
}

func indexCorpus(ctx context.Context, writer *codeindex.CodeWriter, db *pgindex.DB, root string,
	queries []QueryCase, distractors int, fullOnly bool) (corpusStats, error) {
	var stats corpusStats
	if _, err := db.Exec(ctx, `DELETE FROM hlp_code.code_chunks WHERE tenant_id=$1 AND project_id=$2`, benchTenantID, benchProjectID); err != nil {
		return stats, err
	}
	if _, err := db.Exec(ctx, `DELETE FROM hlp_code.code_edges WHERE tenant_id=$1 AND project_id=$2`, benchTenantID, benchProjectID); err != nil {
		return stats, err
	}
	if _, err := db.Exec(ctx, `DELETE FROM hlp_code.indexed_files WHERE tenant_id=$1 AND project_id=$2`, benchTenantID, benchProjectID); err != nil {
		return stats, err
	}

	gtFiles := map[string]bool{}
	for _, q := range queries {
		gtFiles[q.File] = true
	}
	paths := make([]string, 0, len(gtFiles)+distractors)
	for rel := range gtFiles {
		paths = append(paths, filepath.Join(root, filepath.FromSlash(rel)))
	}
	if !fullOnly {
		extra := sampleDistractors(root, gtFiles, distractors)
		paths = append(paths, extra...)
	}
	sort.Strings(paths)

	start := time.Now()
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return stats, err
		}
		res, err := writer.IndexFile(ctx, benchTenantID, benchProjectID, p, src, time.Now().UTC(), false)
		if err != nil {
			return stats, fmt.Errorf("index %s: %w", p, err)
		}
		stats.files++
		stats.chunks += res.Chunks
		stats.edges += res.Edges
	}
	stats.duration = time.Since(start)
	return stats, nil
}

func sampleDistractors(root string, exclude map[string]bool, limit int) []string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build", "bin", "tmp",
				"coverage", ".harness", "testdata", "frontend":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if exclude[rel] {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return sha1Hex(files[i]) < sha1Hex(files[j]) })
	if len(files) > limit {
		files = files[:limit]
	}
	out := make([]string, 0, len(files))
	for _, rel := range files {
		out = append(out, filepath.Join(root, filepath.FromSlash(rel)))
	}
	return out
}

// ---------------------------------------------------------------- search modes

func runModes(ctx context.Context, writer *codeindex.CodeWriter, db *pgindex.DB, root string,
	queries []QueryCase, topK int) ([]modeResult, error) {
	modes := []modeResult{
		{name: "fulltext"},
		{name: "vector"},
		{name: "rrf"},
		{name: "rrf+graph"},
	}
	for i, q := range queries {
		// fulltext：无 embedder、无 graph。
		start := time.Now()
		hits, _, err := writer.Search(ctx, codeindex.SearchOptions{
			TenantID: benchTenantID, ProjectID: benchProjectID, Query: q.Query, TopK: topK,
		})
		if err != nil {
			return nil, fmt.Errorf("fulltext %s: %w", q.ID, err)
		}
		modes[0].queries = append(modes[0].queries, toHits(root, hits))
		modes[0].latencies = append(modes[0].latencies, time.Since(start))

		// vector：与 pkg/codeindex.vectorSearch 同一条 SQL 的单路由。
		start = time.Now()
		vecHits, err := vectorOnly(ctx, db, root, q.Query, topK)
		if err != nil {
			return nil, fmt.Errorf("vector %s: %w", q.ID, err)
		}
		modes[1].queries = append(modes[1].queries, vecHits)
		modes[1].latencies = append(modes[1].latencies, time.Since(start))

		// rrf：keyword + vector。
		start = time.Now()
		hits, _, err = writer.Search(ctx, codeindex.SearchOptions{
			TenantID: benchTenantID, ProjectID: benchProjectID, Query: q.Query, TopK: topK,
			Embedder: fakeEmbedder,
		})
		if err != nil {
			return nil, fmt.Errorf("rrf %s: %w", q.ID, err)
		}
		modes[2].queries = append(modes[2].queries, toHits(root, hits))
		modes[2].latencies = append(modes[2].latencies, time.Since(start))

		// rrf+graph：keyword + vector + graph。
		start = time.Now()
		hits, _, err = writer.Search(ctx, codeindex.SearchOptions{
			TenantID: benchTenantID, ProjectID: benchProjectID, Query: q.Query, TopK: topK,
			Embedder: fakeEmbedder, EnableGraph: true, RRFK: 60,
		})
		if err != nil {
			return nil, fmt.Errorf("rrf+graph %s: %w", q.ID, err)
		}
		modes[3].queries = append(modes[3].queries, toHits(root, hits))
		modes[3].latencies = append(modes[3].latencies, time.Since(start))
		_ = i
	}
	return modes, nil
}

func toHits(root string, hits []codeindex.SearchHit) []hit {
	out := make([]hit, 0, len(hits))
	for _, h := range hits {
		out = append(out, hit{File: relTo(root, h.FilePath), Symbol: h.Symbol, StartLine: h.StartLine})
	}
	return out
}

func vectorOnly(ctx context.Context, db *pgindex.DB, root, query string, limit int) ([]hit, error) {
	rows, err := db.Query(ctx, `
		SELECT file_path, COALESCE(symbol,''), start_line
		FROM hlp_code.code_chunks
		WHERE tenant_id=$1 AND project_id=$2 AND embedding IS NOT NULL
		ORDER BY embedding <=> $3::vector
		LIMIT $4`, benchTenantID, benchProjectID, vectorLiteral(fakeVector(query)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hit
	for rows.Next() {
		var h hit
		var file string
		if err := rows.Scan(&file, &h.Symbol, &h.StartLine); err != nil {
			return nil, err
		}
		h.File = relTo(root, file)
		out = append(out, h)
	}
	return out, rows.Err()
}

func relTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

// ---------------------------------------------------------------- metrics

func computeMetrics(res modeResult, queries []QueryCase) modeMetrics {
	m := modeMetrics{
		name:       res.name,
		recallFile: map[int]float64{},
		recallSym:  map[int]float64{},
	}
	ks := []int{5, 20}
	for _, k := range ks {
		var fileHits, symHits int
		for i, hits := range res.queries {
			if i >= len(queries) {
				break
			}
			if rankFile(hits, queries[i]) <= k {
				fileHits++
			}
			if rankSym(hits, queries[i]) <= k {
				symHits++
			}
		}
		n := float64(len(queries))
		m.recallFile[k] = float64(fileHits) / n
		m.recallSym[k] = float64(symHits) / n
	}
	var sumFile, sumSym float64
	for i, hits := range res.queries {
		if i >= len(queries) {
			break
		}
		if r := rankFile(hits, queries[i]); r > 0 {
			sumFile += 1 / float64(r)
		}
		if r := rankSym(hits, queries[i]); r > 0 {
			sumSym += 1 / float64(r)
		}
	}
	n := float64(len(queries))
	m.mrrFile = sumFile / n
	m.mrrSym = sumSym / n
	m.p50 = percentile(res.latencies, 0.50)
	m.p95 = percentile(res.latencies, 0.95)
	return m
}

func rankFile(hits []hit, q QueryCase) int {
	for i, h := range hits {
		if h.File == q.File {
			return i + 1
		}
	}
	return 0
}

func rankSym(hits []hit, q QueryCase) int {
	for i, h := range hits {
		if h.File == q.File && h.Symbol == q.Symbol {
			return i + 1
		}
	}
	return 0
}

func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ---------------------------------------------------------------- output

func printReport(ms []modeMetrics, queries []QueryCase, stats corpusStats) {
	fmt.Println("## 检索模式对比（自动采样，非人工标注）")
	fmt.Println()
	fmt.Printf("语料：%d 文件 / %d chunks / %d edges；query=%d；向量=1024 维词袋哈希伪向量（非语义 embedding）\n\n",
		stats.files, stats.chunks, stats.edges, len(queries))
	fmt.Println("| 模式 | Recall@5(文件) | Recall@20(文件) | Recall@20(函数) | MRR(文件) | MRR(函数) | P50 | P95 |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	byName := map[string]modeMetrics{}
	for _, m := range ms {
		byName[m.name] = m
		fmt.Printf("| %s | %.3f | %.3f | %.3f | %.3f | %.3f | %v | %v |\n",
			m.name, m.recallFile[5], m.recallFile[20], m.recallSym[20],
			m.mrrFile, m.mrrSym, m.p50.Round(time.Microsecond), m.p95.Round(time.Microsecond))
	}

	singleBestRecall := math.Max(byName["fulltext"].recallFile[20], byName["vector"].recallFile[20])
	singleBestMRR := math.Max(byName["fulltext"].mrrFile, byName["vector"].mrrFile)
	fmt.Println()
	fmt.Println("| 对比 | ΔRecall@20(文件) | ΔMRR(文件) |")
	fmt.Println("|---|---|---|")
	for _, name := range []string{"rrf", "rrf+graph"} {
		m := byName[name]
		fmt.Printf("| %s vs 最佳单路 | %+.1f%% | %+.1f%% |\n",
			name, pct(m.recallFile[20], singleBestRecall), pct(m.mrrFile, singleBestMRR))
	}

	fmt.Println()
	fmt.Println("### 门槛判定")
	fmt.Println()
	gate("Recall@20(文件) >= 0.75", byName["rrf+graph"].recallFile[20], 0.75, true)
	gate("Recall@20(文件) >= 0.75 (rrf)", byName["rrf"].recallFile[20], 0.75, true)
	gate("MRR(文件) >= 0.55 (fulltext)", byName["fulltext"].mrrFile, 0.55, true)
	gate("MRR(文件) >= 0.65 (rrf+graph)", byName["rrf+graph"].mrrFile, 0.65, true)
	gain := pct(byName["rrf+graph"].recallFile[20], singleBestRecall)
	fmt.Printf("- RRF 增益 >= +5%%：%s（rrf+graph Recall@20 相对最佳单路 %+.1f%%）\n", pass(gain >= 5), gain)
	fmt.Println()
	fmt.Println("注：K=5 的「记忆检索」门槛（Recall@5 >= 0.8）属于 T-P1-05 记忆召回，本表 Recall@5 仅供代码检索参考。")
	fmt.Println("注：queries.jsonl 由仓库代码自动采样生成，ground truth 为文件+函数，**非人工标注**。")
}

func gate(label string, got, want float64, greater bool) {
	ok := got >= want
	if !greater {
		ok = got <= want
	}
	fmt.Printf("- %s：%s（实测 %.3f）\n", label, pass(ok), got)
}

func pass(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func pct(v, base float64) float64 {
	if base == 0 {
		return 0
	}
	return (v - base) / base * 100
}

// ---------------------------------------------------------------- embedding

// fakeVector 与 pkg/codeindex 测试中的伪向量一致：词袋 hashing + L2 归一化。
func fakeVector(text string) []float32 {
	vec := make([]float32, 1024)
	for _, tok := range codeindex.IdentifierTokens(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(strings.ToLower(tok)))
		sum := h.Sum64()
		vec[int(sum%1024)] += 1
		vec[int((sum>>17)%1024)] += 0.5
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		vec[0] = 1
		return vec
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

func fakeEmbedder(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		out = append(out, fakeVector(text))
	}
	return out, nil
}

func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(fmt.Sprintf("%.6f", v))
	}
	b.WriteByte(']')
	return b.String()
}
