package codeindex

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agent-platform/pkg/harnessloop/modelregistry"
)

// 本文件是 codeindex 与 modelregistry 之间的 embedding 维度守卫。
//
// 起因：索引表 hlp_code.code_chunks.embedding 是定长 vector 列（DDL 为 vector(1024)），
// 而写入方向量来自外部 embedder。若注册表里的模型维度与索引列维度不一致，
// pgvector 会在 INSERT 阶段报 "expected N dimensions, not M"——那时事务已经
// 开了、chunk 已经删了，错误既不好定位，也容易留下部分写入。
//
// 做法：把模型维度检查提到「产生向量之前」与「落库之前」两个点，
// 统一复用 modelregistry 的 ValidateEmbeddingWrite / ValidateEmbeddingVector，
// 失败即返回 E_EMBEDDING_DIM_MISMATCH，且不进事务、不碰任何已有行。

// DefaultEmbeddingTable 是探测索引向量列维度时使用的表。
const DefaultEmbeddingTable = "hlp_code.code_chunks"

// EmbeddingGuard 把本 writer 的写入/查询向量钉到注册表里的 embedding 模型上。
//
// 零值（Registry 为 nil 或 ModelID 为空）表示未启用守卫，行为与接入前一致；
// 调用方通过 SetEmbeddingGuard 显式开启，避免对既有调用方造成静默行为变更。
type EmbeddingGuard struct {
	// Registry 是模型注册表（modelregistry.Load / DefaultRegistry 的产物）。
	Registry *modelregistry.Registry
	// ModelID 是实际产生向量的 embedding 模型 ID。
	ModelID string
	// IndexDim 是目标索引声明的向量维度。
	// <=0 时按 Table 的 embedding 列定义探测一次并缓存，探测失败则退化为
	// 「只比对实际向量长度」，不阻断索引。
	IndexDim int
	// Table 是探测 IndexDim 用的表名，空串取 DefaultEmbeddingTable。
	Table string
}

// Enabled 报告守卫是否生效。
func (g EmbeddingGuard) Enabled() bool { return g.Registry != nil && g.ModelID != "" }

// SetEmbeddingGuard 开启（或替换）维度守卫；传零值即关闭。
// 更换守卫会清掉已缓存的索引维度，下次使用时重新探测。
func (w *CodeWriter) SetEmbeddingGuard(g EmbeddingGuard) {
	if w == nil {
		return
	}
	w.guardMu.Lock()
	defer w.guardMu.Unlock()
	w.guard = g
	w.guardDim = 0
	w.guardResolved = false
}

// EmbeddingGuard 返回当前守卫（未启用时 ok=false）。
func (w *CodeWriter) EmbeddingGuard() (EmbeddingGuard, bool) {
	if w == nil {
		return EmbeddingGuard{}, false
	}
	w.guardMu.Lock()
	defer w.guardMu.Unlock()
	if !w.guard.Enabled() {
		return EmbeddingGuard{}, false
	}
	return w.guard, true
}

// IsEmbeddingDimMismatch 报告 err 是否是维度守卫的拒绝结果
// （modelregistry.EmbeddingDimError，错误串以 E_EMBEDDING_DIM_MISMATCH 开头）。
func IsEmbeddingDimMismatch(err error) bool {
	var dimErr *modelregistry.EmbeddingDimError
	return errors.As(err, &dimErr)
}

// indexDim 返回目标索引的向量维度；0 表示未知（跳过「模型维度 vs 索引维度」比对）。
func (w *CodeWriter) indexDim(ctx context.Context) int {
	w.guardMu.Lock()
	defer w.guardMu.Unlock()
	if w.guardResolved {
		return w.guardDim
	}
	w.guardResolved = true
	if w.guard.IndexDim > 0 {
		w.guardDim = w.guard.IndexDim
		return w.guardDim
	}
	w.guardDim = w.probeIndexDim(ctx, w.guard.Table)
	return w.guardDim
}

// probeIndexDim 读列的完整类型（format_type 会把 vector(1024) 原样打出来），
// 这样既避开 typmod 的编码细节，也能识别无长度约束的裸 vector。
func (w *CodeWriter) probeIndexDim(ctx context.Context, table string) int {
	if w.db == nil {
		return 0
	}
	if strings.TrimSpace(table) == "" {
		table = DefaultEmbeddingTable
	}
	var columnType string
	err := w.db.QueryRow(ctx, `
		SELECT format_type(atttypid, atttypmod)
		FROM pg_attribute
		WHERE attrelid = $1::regclass AND attname = 'embedding' AND NOT attisdropped`,
		table).Scan(&columnType)
	if err != nil {
		return 0
	}
	return ParseVectorDim(columnType)
}

// ParseVectorDim 从 format_type 输出里解析 pgvector 维度：
// "vector(1024)" -> 1024；"vector" / 其他类型 -> 0（表示无约束或未知）。
func ParseVectorDim(columnType string) int {
	open := strings.IndexByte(columnType, '(')
	closeAt := strings.IndexByte(columnType, ')')
	if open < 0 || closeAt <= open {
		return 0
	}
	dim, err := strconv.Atoi(strings.TrimSpace(columnType[open+1 : closeAt]))
	if err != nil || dim <= 0 {
		return 0
	}
	return dim
}

// validateEmbeddingTarget 校验「模型维度 vs 索引维度」。
// 必须在调用 embedder 之前跑：维度不合就不该为一次注定失败的写入付 embedding 的钱。
func (w *CodeWriter) validateEmbeddingTarget(ctx context.Context) error {
	guard, ok := w.EmbeddingGuard()
	if !ok {
		return nil
	}
	dim := w.indexDim(ctx)
	if err := guard.Registry.ValidateEmbeddingWrite(guard.ModelID, dim); err != nil {
		return wrapEmbeddingGuardError("index write", err)
	}
	return nil
}

// validateEmbeddingVectors 校验 embedding 服务真正返回的向量长度。
// 放在事务之前：一批向量里有任何一条长度不对，整批都不写。
func (w *CodeWriter) validateEmbeddingVectors(vectors [][]float32) error {
	guard, ok := w.EmbeddingGuard()
	if !ok {
		return nil
	}
	for i, vec := range vectors {
		if len(vec) == 0 {
			continue // NULL embedding 是既有的降级路径，不归守卫管
		}
		if err := guard.Registry.ValidateEmbeddingVector(guard.ModelID, vec); err != nil {
			return wrapEmbeddingGuardError(fmt.Sprintf("vector[%d]", i), err)
		}
	}
	return nil
}

// validateEmbeddingQuery 校验查询向量：维度不符时向量路必然查不到东西，
// 与其静默返回空集，不如直接报错让上层看见。
func (w *CodeWriter) validateEmbeddingQuery(vec []float32) error {
	guard, ok := w.EmbeddingGuard()
	if !ok {
		return nil
	}
	if err := guard.Registry.ValidateEmbeddingVector(guard.ModelID, vec); err != nil {
		return wrapEmbeddingGuardError("search query", err)
	}
	return nil
}

func wrapEmbeddingGuardError(stage string, err error) error {
	return fmt.Errorf("codeindex: embedding guard rejected %s: %w", stage, err)
}
