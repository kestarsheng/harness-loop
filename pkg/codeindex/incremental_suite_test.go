package codeindex_test

// 本文件是 T-P1-T3 的增量索引验收套件。
//
// 所有用例都连真实 PostgreSQL + pgvector 执行，不做任何内存替身：
//   - 未设置 HARNESS_TEST_PG_DSN 时，用例 t.Skip，并在 -v 输出中打印 "SKIPPED"；
//   - 设置 HARNESS_REQUIRE_PG=1 时，缺 DSN 直接 Fatal，杜绝「以为跑了其实 Skip」。
//
// 覆盖的 7 个验收点：
//  1. touch（mtime 变化、内容不变）不得写事务；
//  2. 改单个函数时整文件 chunk 全部替换；
//  3. 删除文件时清理 chunks / edges（含 target_file 反向边）与文件状态；
//  4. 事务内注入写入失败后整体回滚，旧索引仍可检索；
//  5. 同一文件并发 10 次索引，最终结果唯一；
//  6. 1s 内 5 次保存只产生 1 次索引任务；
//  7. 全量索引后增量只让变更文件参与。

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/pgindex"
)

const (
	suiteTenantID  = "tenant-code-suite"
	suiteProjectID = "proj-code-suite"

	suiteAlphaSource = `package sample

import "fmt"

func Alpha() { fmt.Println() }

func Beta() {}
`
	// Alpha 与 suiteAlphaSource 完全一致，只改 Beta，用于验证整文件替换。
	suiteBetaOnlyChangedSource = `package sample

import "fmt"

func Alpha() { fmt.Println() }

func Beta() {
	_ = 1
}
`
)

// requireSuiteDSN 返回真实 PG DSN；缺失时要么 Skip（默认），要么在
// HARNESS_REQUIRE_PG=1 时 Fatal，保证验收环境里不会静默跳过。
func requireSuiteDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		if os.Getenv("HARNESS_REQUIRE_PG") == "1" {
			t.Fatalf("HARNESS_TEST_PG_DSN is missing and HARNESS_REQUIRE_PG=1 forbids skipping integration tests")
		}
		t.Skip("HARNESS_TEST_PG_DSN not set -> SKIPPED (set it to run against real PostgreSQL)")
	}
	return dsn
}

type suiteEnv struct {
	db     *pgindex.DB
	writer *codeindex.CodeWriter
}

func setupSuite(t *testing.T) *suiteEnv {
	t.Helper()
	dsn := requireSuiteDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	registry := parser.NewRegistry()
	w := codeindex.NewCodeWriter(db, func(path string, src []byte) (codeindex.ParseResult, error) {
		return registry.Parse(path, src), nil
	}, nil, nil)
	t.Logf("REAL PG: integration suite running against %s (NOT skipped)", maskDSN(dsn))
	return &suiteEnv{db: db, writer: w}
}

func maskDSN(dsn string) string {
	if i := strings.Index(dsn, "@"); i >= 0 && i+1 < len(dsn) {
		return "***@" + dsn[i+1:]
	}
	return "***"
}

type suiteChunk struct {
	ID        string
	Symbol    string
	StartLine int
	Content   string
}

func suiteChunks(t *testing.T, env *suiteEnv, path string) []suiteChunk {
	t.Helper()
	rows, err := env.db.Query(context.Background(), `
		SELECT chunk_id::text, COALESCE(symbol,''), start_line, content
		FROM hlp_code.code_chunks
		WHERE tenant_id=$1 AND project_id=$2 AND file_path=$3
		ORDER BY start_line, symbol`, suiteTenantID, suiteProjectID, path)
	if err != nil {
		t.Fatalf("query chunks: %v", err)
	}
	defer rows.Close()
	var out []suiteChunk
	for rows.Next() {
		var c suiteChunk
		if err := rows.Scan(&c.ID, &c.Symbol, &c.StartLine, &c.Content); err != nil {
			t.Fatalf("scan chunk: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("chunks rows: %v", err)
	}
	return out
}

// suiteFileVersion 取 indexed_files 的 indexed_at 与行版本 xmin。
// xmin 变化说明该行被 UPDATE 过；用于证明 touch 没有写事务。
func suiteFileVersion(t *testing.T, env *suiteEnv, path string) (time.Time, uint32) {
	t.Helper()
	var indexedAt time.Time
	var xmin string
	err := env.db.QueryRow(context.Background(), `
		SELECT indexed_at, xmin::text
		FROM hlp_code.indexed_files
		WHERE tenant_id=$1 AND project_id=$2 AND file_path=$3`,
		suiteTenantID, suiteProjectID, path).Scan(&indexedAt, &xmin)
	if err != nil {
		t.Fatalf("query file version: %v", err)
	}
	var v uint32
	for _, r := range xmin {
		v = v*10 + uint32(r-'0')
	}
	return indexedAt, v
}

func suiteCleanup(t *testing.T, env *suiteEnv, paths ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, path := range paths {
			_ = env.writer.DeleteFile(ctx, suiteTenantID, suiteProjectID, path)
		}
	})
}

func suiteIDs(chunks []suiteChunk) map[string]bool {
	ids := make(map[string]bool, len(chunks))
	for _, c := range chunks {
		ids[c.ID] = true
	}
	return ids
}

func suiteSymbols(chunks []suiteChunk) []string {
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.Symbol+"#"+fmt.Sprint(c.StartLine))
	}
	sort.Strings(out)
	return out
}

// hashVector 产出确定性的 1024 维伪向量（词袋 hashing trick，L2 归一化）。
// 它不是语义 embedding，只用于让 vector / rrf 路由可被真实 SQL 执行。
func hashVector(text string) []float32 {
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

func suiteEmbedder(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		out = append(out, hashVector(text))
	}
	return out, nil
}

// TestSuiteDSNIsReal 明确宣告本次是「真跑了」还是「Skip 了」：
// 仅有 DSN 还不够，还要能连上、迁移过、关键表存在。
func TestSuiteDSNIsReal(t *testing.T) {
	env := setupSuite(t)
	ctx := context.Background()
	if err := env.db.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	var exists bool
	if err := env.db.QueryRow(ctx, `SELECT to_regclass('hlp_code.code_chunks') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("probe schema: %v", err)
	}
	if !exists {
		t.Fatal("hlp_code.code_chunks missing after migrate")
	}
	t.Logf("DSN guard: real PG connection verified, hlp_code schema present (integration tests will RUN, not skip)")
}

//  1. touch 不写事务：内容不变、mtime 前进，必须整体跳过，且 indexed_files 行
//     （indexed_at 与 xmin）与 chunks 行完全不动。
func TestSuiteTouchDoesNotWriteTransaction(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "touch.go", suiteAlphaSource)
	ctx := context.Background()
	suiteCleanup(t, env, path)

	if _, err := env.writer.IndexFile(ctx, suiteTenantID, suiteProjectID, path, []byte(suiteAlphaSource), time.Now().UTC(), false); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	beforeIDs := suiteChunks(t, env, path)
	beforeAt, beforeXmin := suiteFileVersion(t, env, path)
	if len(beforeIDs) != 3 {
		t.Fatalf("seed chunks = %d, want 3 (header+Alpha+Beta)", len(beforeIDs))
	}

	time.Sleep(20 * time.Millisecond)
	res, err := env.writer.IndexFile(ctx, suiteTenantID, suiteProjectID, path,
		[]byte(suiteAlphaSource), time.Now().UTC().Add(2*time.Second), false)
	if err != nil {
		t.Fatalf("touch index: %v", err)
	}
	if !res.Skipped || res.Chunks != 0 || res.Edges != 0 {
		t.Fatalf("touch must be skipped without writes: %+v", res)
	}

	afterAt, afterXmin := suiteFileVersion(t, env, path)
	if !afterAt.Equal(beforeAt) {
		t.Fatalf("touch rewrote indexed_at: %v -> %v", beforeAt, afterAt)
	}
	if afterXmin != beforeXmin {
		t.Fatalf("touch updated indexed_files row (xmin %d -> %d): transaction was written", beforeXmin, afterXmin)
	}
	if got := suiteChunks(t, env, path); len(got) != len(beforeIDs) {
		t.Fatalf("touch changed chunk count: %d -> %d", len(beforeIDs), len(got))
	} else {
		after := suiteIDs(got)
		for _, c := range beforeIDs {
			if !after[c.ID] {
				t.Fatalf("touch rewrote chunk %s", c.ID)
			}
		}
	}
}

//  2. 只改一个函数，整个文件的 chunk 全部重写（chunk_id 全换），
//     未改动函数的文本保持一致。
func TestSuiteSingleFunctionChangeReplacesWholeFile(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "whole.go", suiteAlphaSource)
	ctx := context.Background()
	suiteCleanup(t, env, path)

	if _, err := env.writer.IndexFile(ctx, suiteTenantID, suiteProjectID, path, []byte(suiteAlphaSource), time.Now().UTC(), false); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	old := suiteChunks(t, env, path)
	oldAlpha := ""
	for _, c := range old {
		if c.Symbol == "Alpha" {
			oldAlpha = c.Content
		}
	}
	if oldAlpha == "" {
		t.Fatal("seed index has no Alpha chunk")
	}

	res, err := env.writer.IndexFile(ctx, suiteTenantID, suiteProjectID, path,
		[]byte(suiteBetaOnlyChangedSource), time.Now().UTC(), false)
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if res.Skipped || res.Chunks != len(old) {
		t.Fatalf("changed file must be fully rewritten: %+v (old chunks %d)", res, len(old))
	}

	now := suiteChunks(t, env, path)
	if len(now) != len(old) {
		t.Fatalf("chunk count changed: %d -> %d", len(old), len(now))
	}
	oldSet := suiteIDs(old)
	nowSet := suiteIDs(now)
	for id := range oldSet {
		if nowSet[id] {
			t.Fatalf("chunk %s survived a whole-file rewrite", id)
		}
	}
	if got := suiteSymbols(now); strings.Join(got, ",") != strings.Join(suiteSymbols(old), ",") {
		t.Fatalf("symbol layout changed unexpectedly: %v", got)
	}
	for _, c := range now {
		if c.Symbol == "Alpha" && c.Content != oldAlpha {
			t.Fatalf("untouched Alpha content changed:\n%s\n---\n%s", oldAlpha, c.Content)
		}
	}
}

// 3. 删除文件要清理 chunks、edges（source_file 与 target_file 两个方向）与状态行。
func TestSuiteDeleteCleansEdges(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "del.go", suiteAlphaSource)
	ctx := context.Background()
	suiteCleanup(t, env, path)

	if _, err := env.writer.IndexFile(ctx, suiteTenantID, suiteProjectID, path, []byte(suiteAlphaSource), time.Now().UTC(), false); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	edges, err := env.writer.CountEdges(ctx, suiteTenantID, suiteProjectID, path)
	if err != nil || edges < 2 {
		t.Fatalf("seed edges = %d err=%v (want import + call)", edges, err)
	}

	// 模拟外部文件指向本文件的调用边，覆盖 DELETE 条件里的 target_file 分支。
	if _, err := env.db.Exec(ctx, `
		INSERT INTO hlp_code.code_edges
			(tenant_id, project_id, edge_type, source_symbol, target_symbol, source_file, target_file)
		VALUES ($1,$2,'call','ExternalCaller','Alpha','external.go',$3)`,
		suiteTenantID, suiteProjectID, path); err != nil {
		t.Fatalf("seed reverse edge: %v", err)
	}
	edges, err = env.writer.CountEdges(ctx, suiteTenantID, suiteProjectID, path)
	if err != nil || edges < 3 {
		t.Fatalf("reverse edge not visible: %d err=%v", edges, err)
	}

	if err := env.writer.DeleteFile(ctx, suiteTenantID, suiteProjectID, path); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, err := env.writer.CountChunks(ctx, suiteTenantID, suiteProjectID, path); err != nil || n != 0 {
		t.Fatalf("chunks after delete = %d err=%v", n, err)
	}
	if n, err := env.writer.CountEdges(ctx, suiteTenantID, suiteProjectID, path); err != nil || n != 0 {
		t.Fatalf("edges after delete = %d err=%v", n, err)
	}
	if state, err := env.writer.GetFileState(ctx, suiteTenantID, suiteProjectID, path); err != nil || state != nil {
		t.Fatalf("state after delete = %+v err=%v", state, err)
	}
}

//  4. 事务内写入失败（embedding 维度错误触发 pgvector 报错）后整体回滚：
//     旧 chunks 原样保留、状态仍是 indexed、旧索引仍可被检索。
func TestSuiteInjectedWriteFailureRollsBackKeepsOldIndex(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "rollback.go", suiteAlphaSource)
	ctx := context.Background()
	suiteCleanup(t, env, path)

	if _, err := env.writer.IndexFile(ctx, suiteTenantID, suiteProjectID, path, []byte(suiteAlphaSource), time.Now().UTC(), false); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	before := suiteChunks(t, env, path)

	// 注入 3 维向量：vector(1024) 列插入时报错，错误发生在事务内。
	broken := codeindex.NewCodeWriter(env.db, func(p string, src []byte) (codeindex.ParseResult, error) {
		registry := parser.NewRegistry()
		return registry.Parse(p, src), nil
	}, func(_ context.Context, texts []string) ([][]float32, error) {
		return [][]float32{{0.1, 0.2, 0.3}}, nil
	}, env.writer.Metrics())

	res, err := broken.IndexFile(ctx, suiteTenantID, suiteProjectID, path,
		[]byte(suiteBetaOnlyChangedSource), time.Now().UTC(), true)
	if err == nil {
		t.Fatalf("dimension mismatch must fail the transaction, got %+v", res)
	}
	if errors.Is(err, codeindex.ErrParseFailed) {
		t.Fatalf("failure must come from the write path, not parsing: %v", err)
	}
	if !strings.Contains(err.Error(), "insert chunk") {
		t.Fatalf("unexpected failure point: %v", err)
	}

	state, err := env.writer.GetFileState(ctx, suiteTenantID, suiteProjectID, path)
	if err != nil || state == nil {
		t.Fatalf("state: %v %v", state, err)
	}
	if state.Status != codeindex.StatusIndexed {
		t.Fatalf("rollback must preserve indexed status, got %+v", state)
	}
	after := suiteChunks(t, env, path)
	if len(after) != len(before) {
		t.Fatalf("rollback lost chunks: %d -> %d", len(before), len(after))
	}
	beforeSet := suiteIDs(before)
	for _, c := range after {
		if !beforeSet[c.ID] {
			t.Fatalf("rollback left a partial chunk %s", c.ID)
		}
	}

	hits, stats, err := env.writer.Search(ctx, codeindex.SearchOptions{
		TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "Alpha", TopK: 5,
	})
	if err != nil {
		t.Fatalf("search after rollback: %v", err)
	}
	if stats.KeywordHits == 0 || len(hits) == 0 {
		t.Fatalf("old index must stay searchable after rollback: hits=%d stats=%+v", len(hits), stats)
	}
	found := false
	for _, hit := range hits {
		if hit.FilePath == path && hit.Symbol == "Alpha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rolled-back file no longer returns its Alpha chunk: %+v", hits)
	}
}

// 5. 同一文件并发 10 次索引，最终每个 symbol 只有一份，没有重复行。
func TestSuiteConcurrent10IndexFinalUnique(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "concurrent.go", suiteAlphaSource)
	ctx := context.Background()
	suiteCleanup(t, env, path)

	const workers = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := env.writer.IndexFile(ctx, suiteTenantID, suiteProjectID, path,
				[]byte(suiteAlphaSource), time.Now().UTC(), true)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent index failed: %v", err)
		}
	}

	chunks := suiteChunks(t, env, path)
	if len(chunks) != 3 {
		t.Fatalf("concurrent indexing left %d chunks, want 3: %v", len(chunks), suiteSymbols(chunks))
	}
	seen := map[string]int{}
	for _, c := range chunks {
		seen[c.Symbol]++
	}
	for symbol, n := range seen {
		if n != 1 {
			t.Fatalf("symbol %q appears %d times after concurrent indexing", symbol, n)
		}
	}
	var files int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*) FROM hlp_code.indexed_files
		WHERE tenant_id=$1 AND project_id=$2 AND file_path=$3`,
		suiteTenantID, suiteProjectID, path).Scan(&files); err != nil {
		t.Fatalf("count indexed_files: %v", err)
	}
	if files != 1 {
		t.Fatalf("indexed_files rows = %d, want 1", files)
	}
}

// 6. 1s 内 5 次保存（同一文件）在 debounce 窗口内只产生 1 次索引任务。
func TestSuiteBurstSavesCollapseToOneTask(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "burst.go", suiteAlphaSource)
	suiteCleanup(t, env, path)

	var handlerCalls int32
	q := codeindex.NewQueue(2, func(p string) {
		atomic.AddInt32(&handlerCalls, 1)
		if _, err := env.writer.IndexFile(context.Background(), suiteTenantID, suiteProjectID, p,
			[]byte(suiteAlphaSource), time.Now().UTC(), false); err != nil {
			t.Errorf("handler index: %v", err)
		}
	})
	w := codeindex.NewWatcher(dir, q, codeindex.WatcherOptions{DebounceWindow: 120 * time.Millisecond, PollInterval: 0})
	defer w.Stop()

	burstStart := time.Now()
	for i := 0; i < 5; i++ {
		w.HandleEvent(path)
		time.Sleep(30 * time.Millisecond)
	}
	q.Wait()
	<-time.After(400 * time.Millisecond)
	if elapsed := time.Since(burstStart); elapsed > time.Second+500*time.Millisecond {
		t.Fatalf("burst took too long to settle: %v", elapsed)
	}

	if got := atomic.LoadInt32(&handlerCalls); got != 1 {
		t.Fatalf("5 saves within 1s produced %d index tasks, want 1", got)
	}
	if got := q.Processed(); got != 1 {
		t.Fatalf("queue processed = %d, want 1", got)
	}
	if n, err := env.writer.CountChunks(context.Background(), suiteTenantID, suiteProjectID, path); err != nil || n != 3 {
		t.Fatalf("chunks after burst = %d err=%v", n, err)
	}
}

// 7. 全量索引后，增量只提交 mtime 变化的文件，未变更文件零写入。
func TestSuiteFullThenIncrementalOnlyChangedFile(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	pathA := writeFile(t, dir, "full_a.go", "package sample\n\nimport \"fmt\"\n\nfunc A() { fmt.Println() }\n")
	pathB := writeFile(t, dir, "full_b.go", "package sample\n\nfunc B() {}\n")
	pathC := writeFile(t, dir, "full_c.go", "package sample\n\nfunc C() {}\n")
	ctx := context.Background()
	suiteCleanup(t, env, pathA, pathB, pathC)

	// 1) 全量：目录内所有文件都进一次索引。
	for _, p := range []string{pathA, pathB, pathC} {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.writer.IndexFile(ctx, suiteTenantID, suiteProjectID, p, src, time.Now().UTC(), false); err != nil {
			t.Fatalf("full index %s: %v", p, err)
		}
	}
	fullA := suiteChunks(t, env, pathA)
	fullB := suiteChunks(t, env, pathB)
	fullC := suiteChunks(t, env, pathC)
	_, xminA := suiteFileVersion(t, env, pathA)
	_, xminC := suiteFileVersion(t, env, pathC)

	// 2) 建立 mtime 基线：第一次扫描把 3 个文件记进基线。
	w := codeindex.NewWatcher(dir, nil, codeindex.WatcherOptions{DebounceWindow: 60 * time.Millisecond, PollInterval: 0})
	defer w.Stop()
	if base := w.ScanMTime(); len(base) != 3 {
		t.Fatalf("baseline scan = %v, want 3 files", base)
	}
	if clean := w.ScanMTime(); len(clean) != 0 {
		t.Fatalf("unchanged rescan = %v, want empty", clean)
	}

	// 3) 只改 B 文件内容并显式推进 mtime。
	future := time.Now().Add(2 * time.Second)
	changedSource := "package sample\n\nfunc B() { _ = 2 }\n"
	if err := os.WriteFile(pathB, []byte(changedSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pathB, future, future); err != nil {
		t.Fatal(err)
	}
	changed := w.ScanMTime()
	if len(changed) != 1 || filepath.Base(changed[0]) != "full_b.go" {
		t.Fatalf("incremental scan = %v, want only full_b.go", changed)
	}

	// 4) 变更文件走真实 queue → handler → IndexFile 链路。
	var handled int32
	q := codeindex.NewQueue(2, func(p string) {
		atomic.AddInt32(&handled, 1)
		src, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return
		}
		if _, err := env.writer.IndexFile(context.Background(), suiteTenantID, suiteProjectID, p, src, future, false); err != nil {
			t.Errorf("incremental index: %v", err)
		}
	})
	if n := q.Submit(changed); n != 1 {
		t.Fatalf("queue accepted %d tasks, want 1", n)
	}
	q.Wait()
	if got := atomic.LoadInt32(&handled); got != 1 {
		t.Fatalf("handled %d files, want 1", got)
	}

	// 5) 未变更文件不受影响（chunk 集合与行版本都不变）。
	nowA := suiteChunks(t, env, pathA)
	nowC := suiteChunks(t, env, pathC)
	if !sameIDSet(fullA, nowA) {
		t.Fatalf("untouched file A was rewritten")
	}
	if !sameIDSet(fullC, nowC) {
		t.Fatalf("untouched file C was rewritten")
	}
	if _, nowXminA := suiteFileVersion(t, env, pathA); nowXminA != xminA {
		t.Fatalf("untouched file A had its state row updated")
	}
	if _, nowXminC := suiteFileVersion(t, env, pathC); nowXminC != xminC {
		t.Fatalf("untouched file C had its state row updated")
	}

	// 变更文件被整文件重写。
	nowB := suiteChunks(t, env, pathB)
	if len(nowB) != len(fullB) {
		t.Fatalf("changed file chunk count: %d -> %d", len(fullB), len(nowB))
	}
	oldB := suiteIDs(fullB)
	for _, c := range nowB {
		if oldB[c.ID] {
			t.Fatalf("changed file kept stale chunk %s", c.ID)
		}
	}
	if !strings.Contains(nowB[0].Content, "func B() { _ = 2 }") {
		t.Fatalf("changed file content not refreshed: %+v", nowB)
	}
}

func sameIDSet(a, b []suiteChunk) bool {
	if len(a) != len(b) {
		return false
	}
	set := suiteIDs(a)
	for _, c := range b {
		if !set[c.ID] {
			return false
		}
	}
	return true
}

// 以下用例覆盖 search.go 的三条路由与边界分支，同时验证全量索引后的检索可用性。

func seedSearchCorpus(t *testing.T, env *suiteEnv) (string, string) {
	t.Helper()
	dir := t.TempDir()
	pathA := writeFile(t, dir, "search_a.go", `package sample

import "fmt"

func Alpha() {
	helper()
	fmt.Println()
}
`)
	pathB := writeFile(t, dir, "search_b.go", `package sample

func helper() {}
`)
	suiteCleanup(t, env, pathA, pathB)
	ctx := context.Background()
	embedded := codeindex.NewCodeWriter(env.db, func(p string, s []byte) (codeindex.ParseResult, error) {
		return parser.NewRegistry().Parse(p, s), nil
	}, suiteEmbedder, env.writer.Metrics())
	for _, p := range []string{pathA, pathB} {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := embedded.IndexFile(ctx, suiteTenantID, suiteProjectID, p, src, time.Now().UTC(), false); err != nil {
			t.Fatalf("index %s: %v", p, err)
		}
	}
	return pathA, pathB
}

func TestSuiteSearchModesAndStats(t *testing.T) {
	env := setupSuite(t)
	pathA, _ := seedSearchCorpus(t, env)
	ctx := context.Background()

	// fulltext（无 embedder）
	hits, stats, err := env.writer.Search(ctx, codeindex.SearchOptions{
		TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "helper", TopK: 10,
	})
	if err != nil {
		t.Fatalf("fulltext search: %v", err)
	}
	if stats.KeywordHits == 0 || stats.VectorHits != 0 || stats.Merged == 0 || len(hits) == 0 {
		t.Fatalf("fulltext stats = %+v hits=%d", stats, len(hits))
	}
	for _, hit := range hits {
		if len(hit.Sources) != 1 || hit.Sources[0] != "keyword" {
			t.Fatalf("fulltext-only hit must come from keyword: %+v", hit.Sources)
		}
	}

	// keyword + vector
	hits, stats, err = env.writer.Search(ctx, codeindex.SearchOptions{
		TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "helper", TopK: 10,
		Embedder: suiteEmbedder,
	})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if stats.KeywordHits == 0 || stats.VectorHits == 0 || stats.Merged == 0 {
		t.Fatalf("hybrid stats = %+v", stats)
	}
	sawVectorSource := false
	for _, hit := range hits {
		for _, src := range hit.Sources {
			if src == "vector" {
				sawVectorSource = true
			}
		}
	}
	if !sawVectorSource {
		t.Fatalf("hybrid hits must retain vector provenance: %+v", hits)
	}
	if stats.LatencyMs < 0 {
		t.Fatalf("latency = %d", stats.LatencyMs)
	}

	// keyword + vector + graph
	hits, stats, err = env.writer.Search(ctx, codeindex.SearchOptions{
		TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "helper", TopK: 10,
		Embedder: suiteEmbedder, EnableGraph: true, RRFK: 60,
	})
	if err != nil {
		t.Fatalf("graph search: %v", err)
	}
	if stats.GraphHits == 0 || stats.Merged == 0 {
		t.Fatalf("graph stats = %+v", stats)
	}
	foundGraph := false
	for _, hit := range hits {
		for _, src := range hit.Sources {
			if src == "graph" {
				foundGraph = true
			}
		}
		if hit.Symbol == "Alpha" && hit.FilePath != pathA {
			t.Fatalf("graph hit resolved to wrong file: %+v", hit)
		}
	}
	if !foundGraph {
		t.Fatalf("graph route contributed no visible hit: %+v", hits)
	}
}

func TestSuiteSearchEdgeCases(t *testing.T) {
	env := setupSuite(t)
	seedSearchCorpus(t, env)
	ctx := context.Background()

	// 空查询：三条路由都不应执行，也不应报错。
	hits, stats, err := env.writer.Search(ctx, codeindex.SearchOptions{
		TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "   ", TopK: 5, Embedder: suiteEmbedder, EnableGraph: true,
	})
	if err != nil {
		t.Fatalf("empty query: %v", err)
	}
	if len(hits) != 0 || stats.KeywordHits != 0 || stats.VectorHits != 0 || stats.GraphHits != 0 {
		t.Fatalf("empty query must produce nothing: hits=%d stats=%+v", len(hits), stats)
	}

	// TopK 越界回落到 10。
	for _, topK := range []int{0, -1, 101} {
		got, _, err := env.writer.Search(ctx, codeindex.SearchOptions{
			TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "helper", TopK: topK,
		})
		if err != nil {
			t.Fatalf("topK=%d: %v", topK, err)
		}
		if len(got) > 10 {
			t.Fatalf("topK=%d returned %d hits, want <=10", topK, len(got))
		}
	}

	// 跨租户隔离。
	got, stats, err := env.writer.Search(ctx, codeindex.SearchOptions{
		TenantID: "tenant-other", ProjectID: suiteProjectID, Query: "helper", TopK: 10,
	})
	if err != nil {
		t.Fatalf("cross tenant: %v", err)
	}
	if len(got) != 0 || stats.Merged != 0 {
		t.Fatalf("cross tenant leaked hits: %d", len(got))
	}

	// embedder 失败必须降级为 keyword-only，不向上抛错。
	got, stats, err = env.writer.Search(ctx, codeindex.SearchOptions{
		TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "helper", TopK: 10,
		Embedder: func(context.Context, []string) ([][]float32, error) {
			return nil, errors.New("embedding backend down")
		},
	})
	if err != nil {
		t.Fatalf("embedder failure must degrade: %v", err)
	}
	if stats.VectorHits != 0 || stats.KeywordHits == 0 || len(got) == 0 {
		t.Fatalf("degraded stats = %+v hits=%d", stats, len(got))
	}
}

func TestSuiteEmbedBatchChunks(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package sample\n\nimport \"fmt\"\n\n")
	for i := 0; i < 70; i++ {
		fmt.Fprintf(&b, "func F%02d() { fmt.Println(%d) }\n", i, i)
	}
	src := b.String()
	path := writeFile(t, dir, "many.go", src)
	ctx := context.Background()
	suiteCleanup(t, env, path)

	var (
		batches []int
		mu      sync.Mutex
	)
	w := codeindex.NewCodeWriter(env.db, func(p string, s []byte) (codeindex.ParseResult, error) {
		return parser.NewRegistry().Parse(p, s), nil
	}, func(_ context.Context, texts []string) ([][]float32, error) {
		mu.Lock()
		batches = append(batches, len(texts))
		mu.Unlock()
		out := make([][]float32, 0, len(texts))
		for _, text := range texts {
			out = append(out, hashVector(text))
		}
		return out, nil
	}, nil)

	res, err := w.IndexFile(ctx, suiteTenantID, suiteProjectID, path, []byte(src), time.Now().UTC(), false)
	if err != nil {
		t.Fatalf("index many chunks: %v", err)
	}
	if res.Chunks != 71 {
		t.Fatalf("chunks = %d, want 71 (header + 70 funcs)", res.Chunks)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 2 || batches[0] != 64 || batches[1] != 7 {
		t.Fatalf("embed batches = %v, want [64 7]", batches)
	}
}

func TestSuiteEmbedFailureDegradesToNullVector(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "novec.go", suiteAlphaSource)
	ctx := context.Background()
	suiteCleanup(t, env, path)

	w := codeindex.NewCodeWriter(env.db, func(p string, s []byte) (codeindex.ParseResult, error) {
		return parser.NewRegistry().Parse(p, s), nil
	}, func(context.Context, []string) ([][]float32, error) {
		return nil, errors.New("embedding unavailable")
	}, nil)

	res, err := w.IndexFile(ctx, suiteTenantID, suiteProjectID, path, []byte(suiteAlphaSource), time.Now().UTC(), false)
	if err != nil {
		t.Fatalf("index without vectors: %v", err)
	}
	if res.Chunks != 3 {
		t.Fatalf("chunks = %d, want 3", res.Chunks)
	}
	var withEmbedding int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*) FROM hlp_code.code_chunks
		WHERE tenant_id=$1 AND project_id=$2 AND file_path=$3 AND embedding IS NOT NULL`,
		suiteTenantID, suiteProjectID, path).Scan(&withEmbedding); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if withEmbedding != 0 {
		t.Fatalf("degraded indexing stored %d embeddings, want 0", withEmbedding)
	}
	_, stats, err := w.Search(ctx, codeindex.SearchOptions{
		TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "Alpha", TopK: 5, Embedder: suiteEmbedder,
	})
	if err != nil {
		t.Fatalf("search null-vector index: %v", err)
	}
	if stats.VectorHits != 0 || stats.KeywordHits == 0 {
		t.Fatalf("null-vector search stats = %+v", stats)
	}
}

func TestSuiteWatcherRenameEnqueuesBothPaths(t *testing.T) {
	dir := t.TempDir()
	w := codeindex.NewWatcher(dir, nil, codeindex.WatcherOptions{DebounceWindow: time.Minute, PollInterval: 0})
	defer w.Stop()

	w.HandleRename(filepath.Join(dir, "old.go"), filepath.Join(dir, "new.go"))
	if got := w.Debouncer().Pending(); got != 2 {
		t.Fatalf("rename pending = %d, want 2 (old + new)", got)
	}
	w.HandleRename(filepath.Join(dir, ".git", "config"), filepath.Join(dir, "node_modules", "x.js"))
	if got := w.Debouncer().Pending(); got != 2 {
		t.Fatalf("ignored rename changed pending: %d", got)
	}
}

// TestSuiteWatcherStartIndexesRealWrite 走真实 fsnotify + 轮询双通道：
// 新文件落盘后，watcher 必须把它交给 queue 并完成索引（同一路径去重）。
func TestSuiteWatcherStartIndexesRealWrite(t *testing.T) {
	env := setupSuite(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "watch_new.go")
	suiteCleanup(t, env, path)

	var handled int32
	q := codeindex.NewQueue(2, func(p string) {
		atomic.AddInt32(&handled, 1)
		src, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return
		}
		if _, err := env.writer.IndexFile(context.Background(), suiteTenantID, suiteProjectID, p, src, time.Now().UTC(), false); err != nil {
			t.Errorf("watcher index %s: %v", p, err)
		}
	})
	w := codeindex.NewWatcher(dir, q, codeindex.WatcherOptions{
		DebounceWindow: 100 * time.Millisecond,
		PollInterval:   80 * time.Millisecond,
	})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	defer w.Stop()

	if err := os.WriteFile(path, []byte(suiteAlphaSource), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	q.Wait()
	if got := atomic.LoadInt32(&handled); got == 0 {
		t.Fatal("watcher never dispatched the new file")
	}
	if n, err := env.writer.CountChunks(context.Background(), suiteTenantID, suiteProjectID, path); err != nil || n != 3 {
		t.Fatalf("watched file chunks = %d err=%v", n, err)
	}
}

// TestSuiteDeleteFileOnClosedDB 覆盖删除路径的失败返回：连接池关闭时必须报错而不是静默成功。
func TestSuiteDeleteFileOnClosedDB(t *testing.T) {
	dsn := requireSuiteDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.Close()
	w := codeindex.NewCodeWriter(db, nil, nil, nil)
	err = w.DeleteFile(ctx, suiteTenantID, suiteProjectID, "closed.go")
	if err == nil {
		t.Fatal("DeleteFile on a closed pool must fail")
	}
	if !strings.Contains(err.Error(), "begin delete") {
		t.Fatalf("unexpected delete failure: %v", err)
	}
}

// TestSuiteIndexLatencyBudget 留档 T-P1-T3 的延迟门槛：
//   - 单文件 P95 ≤ 2s；
//   - 100 文件逐文件索引 P95 ≤ 60s。
//
// 数据来自仓库自身 .go 文件（自动采样，排除 *_test.go 与生成目录，≤256KB，
// 按仓库绝对路径排序取前 100，可复现），向量为 1024 维词袋哈希伪向量。
func TestSuiteIndexLatencyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("latency budget skipped in -short mode")
	}
	env := setupSuite(t)
	files := sampleRepoGoFiles(t, 100)
	if len(files) < 100 {
		t.Fatalf("sampled %d go files, need 100", len(files))
	}

	const (
		latTenant  = "tenant-lat-suite"
		latProject = "proj-lat-suite"
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, table := range []string{"code_chunks", "code_edges", "indexed_files"} {
			if _, err := env.db.Exec(ctx, "DELETE FROM hlp_code."+table+" WHERE tenant_id=$1 AND project_id=$2", latTenant, latProject); err != nil {
				t.Logf("cleanup %s: %v", table, err)
			}
		}
	})

	embedded := codeindex.NewCodeWriter(env.db, func(p string, s []byte) (codeindex.ParseResult, error) {
		return parser.NewRegistry().Parse(p, s), nil
	}, suiteEmbedder, nil)

	latencies := make([]time.Duration, 0, len(files))
	batchStart := time.Now()
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		res, err := embedded.IndexFile(context.Background(), latTenant, latProject, p, src, time.Now().UTC(), false)
		if err != nil {
			t.Fatalf("index %s: %v", p, err)
		}
		latencies = append(latencies, res.Duration)
	}
	batch := time.Since(batchStart)

	singleP95 := percentile(latencies[:20], 0.95)
	batchP95 := percentile(latencies, 0.95)
	p50 := percentile(latencies, 0.50)
	t.Logf("INDEX_LATENCY single_p95=%v (n=20) batch_p95=%v (n=%d) batch_p50=%v batch_total=%v",
		singleP95, batchP95, len(latencies), p50, batch)

	if singleP95 > 2*time.Second {
		t.Fatalf("single-file P95 = %v, budget 2s", singleP95)
	}
	if batchP95 > 60*time.Second {
		t.Fatalf("100-file P95 = %v, budget 60s", batchP95)
	}
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

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		wd = filepath.Dir(wd)
	}
	t.Fatal("repo root (go.mod) not found")
	return ""
}

// sampleRepoGoFiles 自动采样仓库 Go 文件；排序保证同一提交上可复现。
func sampleRepoGoFiles(t *testing.T, limit int) []string {
	t.Helper()
	root := repoRoot(t)
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true,
		"build": true, "bin": true, "tmp": true, "coverage": true, ".harness": true,
		"testdata": true, "frontend": true,
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
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
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	sort.Strings(files)
	if len(files) > limit {
		files = files[:limit]
	}
	return files
}
