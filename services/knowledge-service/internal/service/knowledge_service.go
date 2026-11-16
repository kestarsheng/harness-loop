// Package service provides business logic for knowledge service
package service

import (
	"context"
	"fmt"
	"time"

	"agent-platform/pkg/config"
	"agent-platform/pkg/llm"
	commonpb "agent-platform/pkg/pb/common"
	pb "agent-platform/pkg/pb/knowledge"
	"agent-platform/services/knowledge-service/internal/chunker"
	"agent-platform/services/knowledge-service/internal/indexing"
	"agent-platform/services/knowledge-service/internal/parser"
	"agent-platform/services/knowledge-service/internal/repository"
	"agent-platform/services/knowledge-service/internal/rerank"
)

// KnowledgeService provides knowledge management functionality
type KnowledgeService struct {
	pb.UnimplementedKnowledgeServiceServer
	llmClient llm.Client
	docRepo   *repository.DocumentRepository
	cfg       *config.Config
	chunker   *chunker.TokenChunker
	reranker  rerank.Reranker

	// rerank 相关：候选截断 20、返回 5（design 4.10.4），并记录耗时与降级率。
	rerankMetrics    *rerank.Metrics
	rerankCandidates int
	rerankTopM       int

	// indexer 非空时上传走异步索引（pending -> processing -> ready/failed）。
	indexer *indexing.Worker
}

const (
	defaultRerankCandidates = 20
	defaultRerankTopM       = 5
)

// NewKnowledgeService creates a new knowledge service
func NewKnowledgeService(llmClient llm.Client, docRepo *repository.DocumentRepository, cfg *config.Config) *KnowledgeService {
	return &KnowledgeService{
		llmClient:        llmClient,
		docRepo:          docRepo,
		cfg:              cfg,
		chunker:          chunker.NewTokenChunker(512, 50),
		rerankMetrics:    rerank.NewMetrics(),
		rerankCandidates: defaultRerankCandidates,
		rerankTopM:       defaultRerankTopM,
	}
}

// Upload handles file upload.
// worker 存在时走异步：立即返回 pending，解析/切分/向量化由后台完成；
// 未装配 worker（灰度或单机）时退回同步路径，保持旧行为。
func (s *KnowledgeService) Upload(ctx context.Context, req *pb.UploadRequest) (*pb.UploadResponse, error) {
	if err := repository.ValidateUpload(req.Filename, req.Content, 0); err != nil {
		return nil, err
	}

	chunkSize := int(req.ChunkSize)
	if chunkSize == 0 {
		chunkSize = 512
	}
	chunkOverlap := int(req.ChunkOverlap)
	if chunkOverlap == 0 {
		chunkOverlap = 50
	}

	if s.indexer != nil {
		docID, err := s.docRepo.SavePendingDocument(ctx, repository.SaveDocumentMeta{
			TenantID:      req.TenantId,
			KBID:          req.GetKbId(),
			Filename:      req.Filename,
			Status:        repository.StatusPending,
			SplitStrategy: req.ChunkStrategy,
			Overlap:       chunkOverlap,
		}, string(req.Content))
		if err != nil {
			return nil, fmt.Errorf("save document: %w", err)
		}
		job := indexing.Job{
			DocumentID:   docID,
			TenantID:     req.TenantId,
			KBID:         req.GetKbId(),
			Filename:     req.Filename,
			Content:      req.Content,
			ChunkSize:    chunkSize,
			ChunkOverlap: chunkOverlap,
		}
		if err := s.indexer.Enqueue(ctx, job); err != nil {
			_ = s.docRepo.UpdateDocumentStatus(ctx, docID, repository.StatusFailed, err.Error(), 0)
			return nil, fmt.Errorf("enqueue index job: %w", err)
		}
		return &pb.UploadResponse{
			DocumentId: docID,
			Filename:   req.Filename,
			Status:     repository.StatusPending,
		}, nil
	}

	// 同步模式：解析 → 切分 → 向量 → 落库，一步到 ready。
	content, err := parser.Parse(req.Filename, req.Content)
	if err != nil {
		return nil, fmt.Errorf("parse file: %w", err)
	}
	s.chunker = chunker.NewTokenChunker(chunkSize, chunkOverlap)
	chunks := s.chunker.Chunk(content)

	embeddings, err := s.embedChunks(ctx, chunks)
	if err != nil {
		return nil, err
	}

	docID, err := s.docRepo.SaveDocumentWithMeta(ctx, repository.SaveDocumentMeta{
		TenantID:      req.TenantId,
		KBID:          req.GetKbId(),
		Filename:      req.Filename,
		Status:        repository.StatusPending,
		SplitStrategy: req.ChunkStrategy,
		Overlap:       chunkOverlap,
	}, content, chunks, embeddings)
	if err != nil {
		return nil, fmt.Errorf("save document: %w", err)
	}
	if err := s.docRepo.UpdateDocumentStatus(ctx, docID, repository.StatusReady, "", 0); err != nil {
		return nil, fmt.Errorf("mark document ready: %w", err)
	}

	return &pb.UploadResponse{
		DocumentId: docID,
		Filename:   req.Filename,
		ChunkCount: int32(len(chunks)),
		Status:     repository.StatusReady,
	}, nil
}

// SetIndexWorker 装配异步索引 worker；传 nil 表示退回同步上传。
func (s *KnowledgeService) SetIndexWorker(worker *indexing.Worker) { s.indexer = worker }

// Indexer 让 KnowledgeService 直接充当 indexing.Indexer。
func (s *KnowledgeService) Index(ctx context.Context, job indexing.Job) error {
	if s.docRepo == nil {
		return fmt.Errorf("index: document repository is not configured")
	}
	content, err := parser.Parse(job.Filename, job.Content)
	if err != nil {
		return fmt.Errorf("parse file: %w", err)
	}
	chunkSize := job.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}
	overlap := job.ChunkOverlap
	if overlap <= 0 {
		overlap = 50
	}
	chunks := chunker.NewTokenChunker(chunkSize, overlap).Chunk(content)
	embeddings, err := s.embedChunks(ctx, chunks)
	if err != nil {
		return err
	}
	return s.docRepo.SaveChunks(ctx, job.DocumentID, job.TenantID, chunks, embeddings)
}

func (s *KnowledgeService) embedChunks(ctx context.Context, chunks []string) ([][]float64, error) {
	var embeddings [][]float64
	for i := 0; i < len(chunks); i += 20 {
		end := i + 20
		if end > len(chunks) {
			end = len(chunks)
		}
		batchEmbeddings, err := s.llmClient.EmbedBatch(ctx, chunks[i:end])
		if err != nil {
			return nil, fmt.Errorf("generate embeddings: %w", err)
		}
		embeddings = append(embeddings, batchEmbeddings...)
	}
	return embeddings, nil
}

// GetDocument gets a document
func (s *KnowledgeService) GetDocument(ctx context.Context, req *pb.GetDocumentRequest) (*pb.Document, error) {
	doc, err := s.docRepo.GetDocument(ctx, req.Id, req.TenantId)
	if err != nil {
		return nil, err
	}

	return &pb.Document{
		Id:         doc.ID,
		Filename:   doc.Filename,
		Content:    doc.Content,
		ChunkCount: int32(doc.ChunkCount),
		Status:     doc.Status,
		CreatedAt:  doc.CreatedAt.Unix(),
		UpdatedAt:  doc.UpdatedAt.Unix(),
	}, nil
}

// ListDocuments lists documents
func (s *KnowledgeService) ListDocuments(ctx context.Context, req *pb.ListDocumentsRequest) (*pb.ListDocumentsResponse, error) {
	page := int(req.Pagination.GetPage())
	pageSize := int(req.Pagination.GetPageSize())
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = 20
	}

	docs, total, err := s.docRepo.ListDocuments(ctx, req.TenantId, req.Status, req.GetKbId(), page, pageSize)
	if err != nil {
		return nil, err
	}

	resp := &pb.ListDocumentsResponse{
		Pagination: &commonpb.PaginationResponse{
			Total:    int32(total),
			Page:     int32(page),
			PageSize: int32(pageSize),
		},
	}

	for _, doc := range docs {
		resp.Documents = append(resp.Documents, &pb.Document{
			Id:         doc.ID,
			Filename:   doc.Filename,
			ChunkCount: int32(doc.ChunkCount),
			Status:     doc.Status,
			CreatedAt:  doc.CreatedAt.Unix(),
		})
	}

	return resp, nil
}

// DeleteDocument deletes a document
func (s *KnowledgeService) DeleteDocument(ctx context.Context, req *pb.DeleteDocumentRequest) (*commonpb.Empty, error) {
	if err := s.docRepo.DeleteDocument(ctx, req.Id, req.TenantId); err != nil {
		return nil, err
	}
	return &commonpb.Empty{}, nil
}

// Search performs search
func (s *KnowledgeService) Search(ctx context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	startTime := time.Now()

	// Generate query embedding
	queryEmbedding, err := s.llmClient.Embed(ctx, req.Query)
	if err != nil {
		return nil, fmt.Errorf("generate query embedding: %w", err)
	}

	topK := int(req.TopK)
	if topK == 0 {
		topK = 10
	}

	var results []*pb.SearchResult

	switch req.SearchType {
	case "bm25":
		// BM25 search
		chunks, err := s.docRepo.SearchBM25(ctx, req.Query, topK, req.TenantId)
		if err != nil {
			return nil, err
		}
		for i, c := range chunks {
			results = append(results, &pb.SearchResult{
				ChunkId:    c.ID,
				DocumentId: c.DocID,
				Content:    c.Content,
				Score:      c.Score,
			})
			_ = i
		}

	case "hybrid":
		// Hybrid search (RRF fusion)
		results, err = s.hybridSearch(ctx, queryEmbedding, req.Query, topK, req.TenantId)
		if err != nil {
			return nil, err
		}

	default:
		// Vector search
		searchResults, err := s.docRepo.SearchVector(ctx, queryEmbedding, topK, req.TenantId, req.ScoreThreshold)
		if err != nil {
			return nil, err
		}
		for _, r := range searchResults {
			content, _ := r.Payload["content"].(string)
			docID, _ := r.Payload["doc_id"].(string)
			results = append(results, &pb.SearchResult{
				ChunkId:    r.ID,
				DocumentId: docID,
				Content:    content,
				Score:      r.Score,
			})
		}
	}

	// 统一收口：先剔除不可检索文档（failed/pending/processing），
	// 再做 cross-encoder 重排（失败降级并标 rerank=degraded），最后补来源引用。
	results = s.filterSearchable(ctx, results, req.TenantId, req.GetKbId())
	results = s.rerankResults(ctx, req.Query, results, topK)
	results = s.attachSources(ctx, results, req.TenantId)

	return &pb.SearchResponse{
		Results:   results,
		Total:     int64(len(results)),
		LatencyMs: time.Since(startTime).Milliseconds(),
	}, nil
}

// SearchStream performs streaming search
func (s *KnowledgeService) SearchStream(req *pb.SearchRequest, stream pb.KnowledgeService_SearchStreamServer) error {
	resp, err := s.Search(stream.Context(), req)
	if err != nil {
		return err
	}

	for _, result := range resp.Results {
		if err := stream.Send(result); err != nil {
			return err
		}
	}

	return nil
}

func (s *KnowledgeService) hybridSearch(ctx context.Context, queryEmbedding []float64, query string, topK int, tenantID string) ([]*pb.SearchResult, error) {
	// Vector search
	vectorResults, err := s.docRepo.SearchVector(ctx, queryEmbedding, topK*2, tenantID, 0)
	if err != nil {
		return nil, err
	}

	// BM25 search
	bm25Chunks, err := s.docRepo.SearchBM25(ctx, query, topK*2, tenantID)
	if err != nil {
		return nil, err
	}

	// RRF fusion
	scores := make(map[string]float64)
	contentMap := make(map[string]string)
	docIDMap := make(map[string]string)

	k := 60.0
	for i, r := range vectorResults {
		content, _ := r.Payload["content"].(string)
		docID, _ := r.Payload["doc_id"].(string)
		scores[r.ID] += 1.0 / (k + float64(i+1))
		contentMap[r.ID] = content
		docIDMap[r.ID] = docID
	}

	for i, c := range bm25Chunks {
		scores[c.ID] += 1.0 / (k + float64(i+1))
		contentMap[c.ID] = c.Content
		docIDMap[c.ID] = c.DocID
	}

	// Sort by score
	type result struct {
		id    string
		score float64
	}
	var sorted []result
	for id, score := range scores {
		sorted = append(sorted, result{id: id, score: score})
	}

	// Simple sort
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].score > sorted[i].score {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	// Build results
	var results []*pb.SearchResult
	for i := 0; i < len(sorted) && i < topK; i++ {
		r := sorted[i]
		results = append(results, &pb.SearchResult{
			ChunkId:    r.id,
			DocumentId: docIDMap[r.id],
			Content:    contentMap[r.id],
			Score:      r.score,
		})
	}

	return results, nil
}

// Helper function for min
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// filterSearchable 丢弃非 ready 或 kb 不匹配的命中。
func (s *KnowledgeService) filterSearchable(ctx context.Context, results []*pb.SearchResult, tenantID, kbID string) []*pb.SearchResult {
	if s.docRepo == nil {
		return results
	}
	return filterSearchableResults(ctx, s.docRepo, results, tenantID, kbID)
}

// documentGetter 抽出状态查询，便于用假实现验证「failed 不进检索」。
type documentGetter interface {
	GetDocument(ctx context.Context, id, tenantID string) (*repository.Document, error)
}

// filterSearchableResults 丢弃非 ready 或 kb 不匹配的命中。
// 状态查询失败时保留结果（fail-open）：宁可多给一条，也不因文档库抖动清空检索。
func filterSearchableResults(ctx context.Context, getter documentGetter, results []*pb.SearchResult, tenantID, kbID string) []*pb.SearchResult {
	if len(results) == 0 {
		return results
	}
	cache := make(map[string]*repository.Document, 8)
	out := make([]*pb.SearchResult, 0, len(results))
	for _, r := range results {
		doc, ok := cache[r.DocumentId]
		if !ok {
			loaded, err := getter.GetDocument(ctx, r.DocumentId, tenantID)
			if err != nil || loaded == nil {
				cache[r.DocumentId] = nil
				out = append(out, r)
				continue
			}
			cache[r.DocumentId] = loaded
			doc = loaded
		}
		if doc == nil {
			out = append(out, r)
			continue
		}
		if !repository.Searchable(doc.Status) {
			continue
		}
		if !repository.KBMatches(doc.KBID, kbID) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (s *KnowledgeService) SetReranker(r rerank.Reranker) {
	s.reranker = r
}

// SetRerankLimits 调整 top N -> top M（默认 20 -> 5）。
func (s *KnowledgeService) SetRerankLimits(candidates, topM int) {
	if candidates > 0 {
		s.rerankCandidates = candidates
	}
	if topM > 0 {
		s.rerankTopM = topM
	}
}

// RerankStats 暴露 rerank 调用量/降级率/耗时，供指标采集。
func (s *KnowledgeService) RerankStats() rerank.MetricsSnapshot { return s.rerankMetrics.Snapshot() }

// rerankResults 对融合结果做 top 20 -> top 5 重排。
// 任何失败（超时、5xx、协议不符）都降级为融合顺序，并在 metadata 标 rerank=degraded。
func (s *KnowledgeService) rerankResults(ctx context.Context, query string, results []*pb.SearchResult, topK int) []*pb.SearchResult {
	if len(results) == 0 {
		return results
	}
	candidatesLimit := s.rerankCandidates
	if candidatesLimit <= 0 {
		candidatesLimit = defaultRerankCandidates
	}
	if candidatesLimit > len(results) {
		candidatesLimit = len(results)
	}
	limit := s.rerankTopM
	if limit <= 0 {
		limit = defaultRerankTopM
	}
	if topK > 0 && topK < limit {
		limit = topK
	}
	if limit > candidatesLimit {
		limit = candidatesLimit
	}

	candidatesIn := results[:candidatesLimit]
	if s.reranker == nil {
		return candidatesIn[:limit]
	}

	candidates := make([]rerank.Candidate, 0, len(candidatesIn))
	for _, r := range candidatesIn {
		candidates = append(candidates, rerank.Candidate{
			ChunkID:    r.ChunkId,
			DocumentID: r.DocumentId,
			Content:    r.Content,
			Score:      r.Score,
		})
	}
	started := time.Now()
	reranked, err := s.reranker.Rerank(ctx, query, candidates, limit)
	s.rerankMetrics.Record(time.Since(started).Milliseconds(), err != nil)
	if err != nil {
		for _, r := range candidatesIn {
			markRerank(r, "degraded", 0)
		}
		return candidatesIn[:limit]
	}

	out := make([]*pb.SearchResult, 0, len(reranked))
	for _, c := range reranked {
		for _, r := range candidatesIn {
			if r.ChunkId == c.ChunkID {
				markRerank(r, "applied", c.RerankScore)
				out = append(out, r)
				break
			}
		}
	}
	if len(out) == 0 {
		for _, r := range candidatesIn {
			markRerank(r, "degraded", 0)
		}
		return candidatesIn[:limit]
	}
	return out
}

func markRerank(result *pb.SearchResult, state string, score float64) {
	if result.Metadata == nil {
		result.Metadata = map[string]string{}
	}
	result.Metadata["rerank"] = state
	if score > 0 {
		result.Metadata["rerank_score"] = fmt.Sprintf("%.2f", score)
	}
}

func (s *KnowledgeService) attachSources(ctx context.Context, results []*pb.SearchResult, tenantID string) []*pb.SearchResult {
	cache := make(map[string]*repository.Document, 4)
	for _, r := range results {
		doc, ok := cache[r.DocumentId]
		if !ok {
			loaded, err := s.docRepo.GetDocument(ctx, r.DocumentId, tenantID)
			if err != nil || loaded == nil {
				continue
			}
			cache[r.DocumentId] = loaded
			doc = loaded
		}
		ref := &pb.SourceRef{
			ChunkId:      r.ChunkId,
			DocumentId:   r.DocumentId,
			DocumentName: doc.Filename,
			Score:        r.Score,
			Snippet:      truncateSnippet(r.Content, 200),
			Uri:          fmt.Sprintf("/api/v2/knowledge/documents/%s", r.DocumentId),
		}
		if r.Metadata != nil {
			if v := r.Metadata["rerank_score"]; v != "" {
				var score float64
				fmt.Sscanf(v, "%f", &score)
				ref.RerankScore = score
			}
			if v := r.Metadata["page_no"]; v != "" {
				var page int
				fmt.Sscanf(v, "%d", &page)
				ref.PageNo = int32(page)
			}
			ref.Section = r.Metadata["section"]
		}
		r.Sources = append(r.Sources, ref)
	}
	return results
}

func truncateSnippet(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
