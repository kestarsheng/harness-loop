package service

import (
	"context"
	"errors"
	"time"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
)

var (
	ErrIndexDisabled  = errors.New("index service: code index is not configured")
	ErrTenantRequired = errors.New("index service: tenant_id and project_id are required")
)

type IndexService struct {
	writer *codeindex.CodeWriter
	graph  *GraphExpander
}

func NewIndexService(writer *codeindex.CodeWriter) *IndexService {
	return &IndexService{writer: writer, graph: NewGraphExpander(writer)}
}

type CodeSearchResult struct {
	Hits  []codeindex.SearchHit
	Stats codeindex.SearchStats
}

// SetGraphPolicy 调整图扩展第三路的规模与权重（默认 50 节点 / 每符号 5 / 权重 0.8）。
func (s *IndexService) SetGraphPolicy(policy GraphPolicy) {
	if s == nil {
		return
	}
	s.graph.SetPolicy(policy)
}

func (s *IndexService) GraphPolicy() GraphPolicy {
	if s == nil {
		return DefaultGraphPolicy()
	}
	return s.graph.Policy()
}

// ExpandGraph 提供显式的 1 跳扩展入口（供调试/评估用），
// 检索主链路仍由 Search 内部的 RRF 三路融合完成。
func (s *IndexService) ExpandGraph(ctx context.Context, tenantID, projectID string, hits []codeindex.SearchHit) (GraphExpansion, error) {
	if s == nil || s.writer == nil {
		return GraphExpansion{}, ErrIndexDisabled
	}
	return s.graph.ExpandFromHits(ctx, tenantID, projectID, hits)
}

func (s *IndexService) Search(ctx context.Context, tenantID, projectID, query string, topK int, embedder codeindex.EmbedFunc, enableGraph bool) (CodeSearchResult, error) {
	if s == nil || s.writer == nil {
		return CodeSearchResult{}, ErrIndexDisabled
	}
	if tenantID == "" || projectID == "" {
		return CodeSearchResult{}, ErrTenantRequired
	}
	opts := s.graph.Apply(codeindex.SearchOptions{
		TenantID: tenantID, ProjectID: projectID, Query: query,
		TopK: topK, Embedder: embedder,
	}, enableGraph)
	hits, stats, err := s.writer.Search(ctx, opts)
	if err != nil {
		return CodeSearchResult{}, err
	}
	return CodeSearchResult{Hits: hits, Stats: stats}, nil
}

func (s *IndexService) Index(ctx context.Context, tenantID, projectID, path string, src []byte, mtime time.Time, force bool) (codeindex.IndexResult, error) {
	if s == nil || s.writer == nil {
		return codeindex.IndexResult{}, ErrIndexDisabled
	}
	if tenantID == "" || projectID == "" {
		return codeindex.IndexResult{}, ErrTenantRequired
	}
	return s.writer.IndexFile(ctx, tenantID, projectID, path, src, mtime, force)
}

func (s *IndexService) Metrics() codeindex.MetricsSnapshot {
	if s == nil || s.writer == nil {
		return codeindex.MetricsSnapshot{}
	}
	return s.writer.Metrics().Snapshot()
}

// Capabilities 暴露解析能力级别（full/regex），用于索引状态与运维查询。
func (s *IndexService) Capabilities() map[string]string {
	out := make(map[string]string, len(parser.Languages))
	if s == nil || s.writer == nil {
		return out
	}
	for lang, capability := range parser.Capabilities() {
		out[lang] = string(capability)
	}
	return out
}
