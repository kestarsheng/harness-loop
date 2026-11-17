package codeindex_test

// 本文件覆盖 embedding 维度守卫的「不依赖数据库」部分：
//   - pgvector 列类型 -> 维度的解析；
//   - 768 维模型写 1536 维索引必须在写入前被拒绝（E_EMBEDDING_DIM_MISMATCH），
//     而且拒绝发生在调用 embedding 服务之前、事务开启之前；
//   - 守卫未启用时行为与接入前一致（不校验）。
//
// 依赖真实 PostgreSQL 的端到端路径见 embedding_guard_suite_test.go。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/harnessloop/modelregistry"
)

// guardRegistry 生成只含一个 embedding 模型的注册表（enforce_dim 与模型维度一致，
// 否则 modelregistry 在加载期就会整份拒绝）。
func guardRegistry(t *testing.T, dim int) *modelregistry.Registry {
	t.Helper()
	body := fmt.Sprintf(`version: 1
models:
  - id: chat
    provider: test
    kind: chat
    context_window: 8192
    max_output_tokens: 1024
    price_in_per_1k: 0.001
    price_out_per_1k: 0.002
    status: available
  - id: embed
    provider: test
    kind: embedding
    context_window: 8192
    max_output_tokens: 1
    embedding_dim: %d
    price_in_per_1k: 0.0001
    price_out_per_1k: 0
    status: available
defaults:
  task_model: chat
  strategy: balanced
  fallback: [chat]
embedding:
  memory_model: embed
  code_model: embed
  enforce_dim: %d
`, dim, dim)
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	reg, err := modelregistry.Load(path)
	if err != nil {
		t.Fatalf("load registry(%d): %v", dim, err)
	}
	return reg
}

func guardParse(path string, src []byte) (codeindex.ParseResult, error) {
	return parser.NewRegistry().Parse(path, src), nil
}

// fixedEmbedder 返回固定维度向量，并记录被调用次数。
func fixedEmbedder(dim int, calls *int32) codeindex.EmbedFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		atomic.AddInt32(calls, 1)
		out := make([][]float32, 0, len(texts))
		for range texts {
			out = append(out, make([]float32, dim))
		}
		return out, nil
	}
}

func TestParseVectorDim(t *testing.T) {
	cases := []struct {
		columnType string
		want       int
	}{
		{"vector(1024)", 1024},
		{"vector(1536)", 1536},
		{"vector(768)", 768},
		{"vector", 0},
		{"text", 0},
		{"", 0},
		{"vector(abc)", 0},
		{"vector(0)", 0},
	}
	for _, c := range cases {
		if got := codeindex.ParseVectorDim(c.columnType); got != c.want {
			t.Fatalf("ParseVectorDim(%q) = %d, want %d", c.columnType, got, c.want)
		}
	}
}

// 守卫未启用：不产生任何校验，行为与接入前一致。
func TestEmbeddingGuardDisabledByDefault(t *testing.T) {
	w := codeindex.NewCodeWriter(nil, guardParse, nil, nil)
	if _, ok := w.EmbeddingGuard(); ok {
		t.Fatal("fresh writer must have no embedding guard")
	}
	w.SetEmbeddingGuard(codeindex.EmbeddingGuard{ModelID: "embed"}) // 缺 Registry
	if _, ok := w.EmbeddingGuard(); ok {
		t.Fatal("guard without registry must stay disabled")
	}
	w.SetEmbeddingGuard(codeindex.EmbeddingGuard{Registry: guardRegistry(t, 768)}) // 缺 ModelID
	if _, ok := w.EmbeddingGuard(); ok {
		t.Fatal("guard without model id must stay disabled")
	}
}

// 核心用例：768 维模型 + 1536 维索引 -> E_EMBEDDING_DIM_MISMATCH，
// 且拒绝发生在调用 embedder 之前（embedCalls == 0）、事务开启之前（db 为 nil 也不会 panic）。
func TestEmbeddingGuardRejectsModelIndexMismatchBeforeEmbedding(t *testing.T) {
	var calls int32
	w := codeindex.NewCodeWriter(nil, guardParse, fixedEmbedder(768, &calls), nil)
	w.SetEmbeddingGuard(codeindex.EmbeddingGuard{
		Registry: guardRegistry(t, 768),
		ModelID:  "embed",
		IndexDim: 1536, // 索引列是 1536 维，模型只会产出 768 维
	})

	src := []byte("package sample\n\nfunc Alpha() {}\n")
	_, err := w.IndexFileWith(context.Background(), "t-guard", "p-guard", "guard.go", src, time.Now().UTC(),
		codeindex.IndexOptions{SkipStateLookup: true})
	if err == nil {
		t.Fatal("768-dim model writing a 1536-dim index must be rejected")
	}
	if !codeindex.IsEmbeddingDimMismatch(err) {
		t.Fatalf("expected EmbeddingDimError, got %T: %v", err, err)
	}
	var dimErr *modelregistry.EmbeddingDimError
	if !errors.As(err, &dimErr) {
		t.Fatalf("errors.As must reach EmbeddingDimError: %v", err)
	}
	if dimErr.ModelDim != 768 || dimErr.IndexDim != 1536 || dimErr.ModelID != "embed" {
		t.Fatalf("dimension error = %+v", dimErr)
	}
	if !strings.Contains(err.Error(), modelregistry.CodeEmbeddingDimMismatch) {
		t.Fatalf("error = %v, want code %s", err, modelregistry.CodeEmbeddingDimMismatch)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("embedder must not be called when the target dimension is already wrong, got %d calls", got)
	}
}

// 目标维度对得上，但 embedding 服务返回了错长度的向量：同样在写入前拒绝。
func TestEmbeddingGuardRejectsWrongVectorLength(t *testing.T) {
	var calls int32
	w := codeindex.NewCodeWriter(nil, guardParse, fixedEmbedder(1536, &calls), nil)
	w.SetEmbeddingGuard(codeindex.EmbeddingGuard{
		Registry: guardRegistry(t, 768),
		ModelID:  "embed",
		IndexDim: 768,
	})

	src := []byte("package sample\n\nfunc Alpha() {}\n")
	_, err := w.IndexFileWith(context.Background(), "t-guard", "p-guard", "guard.go", src, time.Now().UTC(),
		codeindex.IndexOptions{SkipStateLookup: true})
	if !codeindex.IsEmbeddingDimMismatch(err) {
		t.Fatalf("expected dimension mismatch for a 1536-length vector, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Fatal("embedder should have been called before the vector length check")
	}
}

// 维度一致时必须放行：守卫放行后会继续走到事务阶段，
// 这里用 nil db 必然 panic 来证明「不是被守卫拦住的」。
// 连真库的放行路径见 embedding_guard_suite_test.go。
func TestEmbeddingGuardAllowsMatchingDim(t *testing.T) {
	var calls int32
	w := codeindex.NewCodeWriter(nil, guardParse, fixedEmbedder(768, &calls), nil)
	w.SetEmbeddingGuard(codeindex.EmbeddingGuard{
		Registry: guardRegistry(t, 768),
		ModelID:  "embed",
		IndexDim: 768,
	})

	src := []byte("package sample\n\nfunc Alpha() {}\n")
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("guard passed, indexing continued into the tx layer and panicked on nil db: %v", r)
			}
		}()
		_, err := w.IndexFileWith(context.Background(), "t-guard", "p-guard", "guard.go", src, time.Now().UTC(),
			codeindex.IndexOptions{SkipStateLookup: true})
		if codeindex.IsEmbeddingDimMismatch(err) {
			t.Fatalf("matching dimension must pass the guard: %v", err)
		}
	}()
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Fatal("embedder must run when dimensions match")
	}
}
