package service

import (
	"context"
	"errors"
	"time"

	"agent-platform/pkg/codeindex"
)

var (
	ErrIndexDisabled  = errors.New("index service: code index is not configured")
	ErrTenantRequired = errors.New("index service: tenant_id and project_id are required")
)

type IndexService struct {
	writer *codeindex.CodeWriter
}

func NewIndexService(writer *codeindex.CodeWriter) *IndexService {
	return &IndexService{writer: writer}
}

type CodeSearchResult struct {
	Hits  []codeindex.SearchHit
	Stats codeindex.SearchStats
}

func (s *IndexService) Search(ctx context.Context, tenantID, projectID, query string, topK int, embedder codeindex.EmbedFunc, enableGraph bool) (CodeSearchResult, error) {
	if s == nil || s.writer == nil {
		return CodeSearchResult{}, ErrIndexDisabled
	}
	if tenantID == "" || projectID == "" {
		return CodeSearchResult{}, ErrTenantRequired
	}
	hits, stats, err := s.writer.Search(ctx, codeindex.SearchOptions{
		TenantID: tenantID, ProjectID: projectID, Query: query,
		TopK: topK, EnableGraph: enableGraph, Embedder: embedder,
	})
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
