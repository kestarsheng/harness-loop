package codeindex_test

// embedding 维度守卫的端到端用例（连真实 PostgreSQL + pgvector）。
//
// 与 embedding_guard_test.go 的分工：那边证明「不依赖库也能在写入前拦住」，
// 这里证明「守卫能从真实列定义里读出索引维度，并且在真事务里不留下任何部分写入」。
//
// 未设置 HARNESS_TEST_PG_DSN 时整组 Skip（与增量索引套件口径一致）。

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/harnessloop/modelregistry"
)

const guardSuiteSource = `package sample

import "fmt"

func Guarded() { fmt.Println("guarded") }
`

func TestSuiteEmbeddingGuardEndToEnd(t *testing.T) {
	env := setupSuite(t)
	ctx := context.Background()

	dir := t.TempDir()
	path := writeFile(t, dir, "guarded.go", guardSuiteSource)
	suiteCleanup(t, env, path)

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 1) 768 维模型 vs 真实索引列（DDL 是 vector(1024)）：必须拒绝。
	var calls int32
	mismatched := codeindex.NewCodeWriter(env.db, func(p string, s []byte) (codeindex.ParseResult, error) {
		return parser.NewRegistry().Parse(p, s), nil
	}, func(_ context.Context, texts []string) ([][]float32, error) {
		atomic.AddInt32(&calls, 1)
		out := make([][]float32, 0, len(texts))
		for range texts {
			out = append(out, make([]float32, 768))
		}
		return out, nil
	}, env.writer.Metrics())
	// IndexDim 留 0：让守卫自己从 hlp_code.code_chunks.embedding 的列定义里探测。
	mismatched.SetEmbeddingGuard(codeindex.EmbeddingGuard{
		Registry: guardRegistry(t, 768),
		ModelID:  "embed",
	})

	_, err = mismatched.IndexFile(ctx, suiteTenantID, suiteProjectID, path, src, time.Now().UTC(), false)
	if err == nil {
		t.Fatal("768-dim model writing the 1024-dim index must be rejected")
	}
	var dimErr *modelregistry.EmbeddingDimError
	if !errors.As(err, &dimErr) {
		t.Fatalf("expected EmbeddingDimError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), modelregistry.CodeEmbeddingDimMismatch) {
		t.Fatalf("error = %v, want code %s", err, modelregistry.CodeEmbeddingDimMismatch)
	}
	if dimErr.ModelDim != 768 || dimErr.IndexDim != 1024 {
		t.Fatalf("dimension error = %+v (index dim should be probed from the vector(1024) column)", dimErr)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("embedder must not be called for a doomed write, got %d calls", got)
	}
	if n, cerr := mismatched.CountChunks(ctx, suiteTenantID, suiteProjectID, path); cerr != nil || n != 0 {
		t.Fatalf("rejected write must not leave chunks: n=%d err=%v", n, cerr)
	}
	var states int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*) FROM hlp_code.indexed_files
		WHERE tenant_id=$1 AND project_id=$2 AND file_path=$3`,
		suiteTenantID, suiteProjectID, path).Scan(&states); err != nil {
		t.Fatalf("count indexed_files: %v", err)
	}
	if states != 0 {
		t.Fatalf("rejected write must not touch indexed_files, got %d rows", states)
	}

	// 2) 1024 维模型 vs 同一个索引列：放行并真正落库（顺便证明列维度是被探测出来的，
	//    若探测失败 IndexDim=0，ValidateEmbeddingWrite 会把 1024 也判为不匹配）。
	matched := codeindex.NewCodeWriter(env.db, func(p string, s []byte) (codeindex.ParseResult, error) {
		return parser.NewRegistry().Parse(p, s), nil
	}, suiteEmbedder, env.writer.Metrics())
	matched.SetEmbeddingGuard(codeindex.EmbeddingGuard{
		Registry: guardRegistry(t, 1024),
		ModelID:  "embed",
	})
	if _, err := matched.IndexFile(ctx, suiteTenantID, suiteProjectID, path, src, time.Now().UTC(), false); err != nil {
		t.Fatalf("matching dimension must be indexed: %v", err)
	}
	if n, err := matched.CountChunks(ctx, suiteTenantID, suiteProjectID, path); err != nil || n == 0 {
		t.Fatalf("matching dimension should write chunks: n=%d err=%v", n, err)
	}

	// 3) 查询侧：768 维查询向量打 1024 维索引必须报错，而不是静默退化成 keyword-only。
	//    matched 上挂的是 1024 维模型守卫，所以 768 维查询向量应被拒绝。
	_, _, err = matched.Search(ctx, codeindex.SearchOptions{
		TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "Guarded", TopK: 5,
		Embedder: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, 0, len(texts))
			for range texts {
				out = append(out, make([]float32, 768))
			}
			return out, nil
		},
	})
	if !codeindex.IsEmbeddingDimMismatch(err) {
		t.Fatalf("query vector with the wrong dimension must be rejected: %v", err)
	}

	// 4) 查询侧放行：维度一致时向量路仍可用。
	hits, stats, err := matched.Search(ctx, codeindex.SearchOptions{
		TenantID: suiteTenantID, ProjectID: suiteProjectID, Query: "Guarded", TopK: 5,
		Embedder: suiteEmbedder,
	})
	if err != nil {
		t.Fatalf("guard must pass matching query vectors: %v", err)
	}
	if stats.VectorHits == 0 || len(hits) == 0 {
		t.Fatalf("matching query vector should hit the vector route: stats=%+v hits=%d", stats, len(hits))
	}
}
