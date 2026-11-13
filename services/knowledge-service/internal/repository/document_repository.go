// Package repository provides data access for knowledge service
package repository

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"agent-platform/pkg/mongodb"
	"agent-platform/pkg/qdrant"
	"agent-platform/services/knowledge-service/internal/search"
)

// Document represents a stored document
type Document struct {
	ID            string
	Filename      string
	Content       string
	ChunkCount    int
	Status        string
	TenantID      string
	KBID          string
	FailureReason string
	RetryCount    int
	Tags          []string
	PageCount     int
	SplitStrategy string
	Overlap       int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Chunk represents a document chunk
type Chunk struct {
	ID         string
	DocID      string
	KBID       string
	Content    string
	ChunkIndex int
	PageNo     int
	Section    string
	Score      float64
	Vector     []float64
}

// DocumentRepository manages documents and chunks
type DocumentRepository struct {
	mongo     *mongodb.Client
	qdrant    *qdrant.Client
	bm25      *search.BM25
	bm25Mu    sync.RWMutex
	bm25Init  bool
	chunkMeta map[string]chunkInfo
}

type chunkInfo struct {
	docID string
	kbID  string
}

// NewDocumentRepository creates a new document repository
func NewDocumentRepository(mongo *mongodb.Client, qdrant *qdrant.Client) *DocumentRepository {
	return &DocumentRepository{
		mongo:     mongo,
		qdrant:    qdrant,
		bm25:      search.NewBM25(),
		chunkMeta: make(map[string]chunkInfo),
	}
}

// SaveDocument saves a document and its chunks
func (r *DocumentRepository) SaveDocument(ctx context.Context, filename, content string, chunks []string, embeddings [][]float64, tenantID string) (string, error) {
	docID := uuid.New().String()

	// Save document metadata
	doc := &mongodb.Document{
		ID:        docID,
		Title:     filename,
		Content:   content,
		CreatedAt: time.Now(),
	}
	if err := r.mongo.InsertDocument(ctx, doc); err != nil {
		return "", fmt.Errorf("save document: %w", err)
	}

	// Save chunks to MongoDB and Qdrant
	var mongoChunks []mongodb.Chunk
	var qdrantPoints []qdrant.Point

	for i, chunk := range chunks {
		chunkID := uuid.New().String()

		mongoChunks = append(mongoChunks, mongodb.Chunk{
			ID:         chunkID,
			DocID:      docID,
			Content:    chunk,
			ChunkIndex: i,
			CreatedAt:  time.Now(),
		})

		qdrantPoints = append(qdrantPoints, qdrant.Point{
			ID:     chunkID,
			Vector: embeddings[i],
			Payload: map[string]interface{}{
				"doc_id":      docID,
				"content":     chunk,
				"chunk_index": i,
				"tenant_id":   tenantID,
			},
		})
	}

	// Batch insert chunks to MongoDB
	if err := r.mongo.InsertChunks(ctx, mongoChunks); err != nil {
		return "", fmt.Errorf("save chunks: %w", err)
	}

	// Batch insert to Qdrant
	if err := r.qdrant.Upsert(ctx, qdrantPoints); err != nil {
		return "", fmt.Errorf("save vectors: %w", err)
	}

	return docID, nil
}

// GetDocument gets a document by ID
func (r *DocumentRepository) GetDocument(ctx context.Context, id, tenantID string) (*Document, error) {
	doc, err := r.mongo.GetDocument(ctx, id)
	if err != nil {
		return nil, err
	}

	chunks, err := r.mongo.GetChunksByDocID(ctx, id)
	if err != nil {
		return nil, err
	}

	status, kbID, reason, retry, pageCount, overlap, tags, strategy := readDocMeta(doc.Metadata)
	return &Document{
		ID:            doc.ID,
		Filename:      doc.Title,
		Content:       doc.Content,
		ChunkCount:    len(chunks),
		Status:        status,
		TenantID:      tenantID,
		KBID:          kbID,
		FailureReason: reason,
		RetryCount:    retry,
		Tags:          tags,
		PageCount:     pageCount,
		SplitStrategy: strategy,
		Overlap:       overlap,
		CreatedAt:     doc.CreatedAt,
		UpdatedAt:     doc.UpdatedAt,
	}, nil
}

// ListDocuments lists documents
func (r *DocumentRepository) ListDocuments(ctx context.Context, tenantID, status, kbFilter string, page, pageSize int) ([]*Document, int64, error) {
	statusFilter := status
	docs, err := r.mongo.ListDocuments(ctx, int64(pageSize), int64((page-1)*pageSize))
	if err != nil {
		return nil, 0, err
	}

	total, err := r.mongo.CountDocuments(ctx)
	if err != nil {
		return nil, 0, err
	}

	var documents []*Document
	for _, doc := range docs {
		status, kbID, reason, retry, _, _, _, _ := readDocMeta(doc.Metadata)
		if statusFilter != "" && status != statusFilter {
			continue
		}
		if kbFilter != "" && !KBMatches(kbID, kbFilter) {
			continue
		}
		documents = append(documents, &Document{
			ID:            doc.ID,
			Filename:      doc.Title,
			Status:        status,
			TenantID:      tenantID,
			KBID:          kbID,
			FailureReason: reason,
			RetryCount:    retry,
			CreatedAt:     doc.CreatedAt,
			UpdatedAt:     doc.UpdatedAt,
		})
	}

	return documents, total, nil
}

// DeleteDocument deletes a document and its chunks
func (r *DocumentRepository) DeleteDocument(ctx context.Context, id, tenantID string) error {
	// Get chunks to delete from Qdrant
	chunks, err := r.mongo.GetChunksByDocID(ctx, id)
	if err != nil {
		return err
	}

	// Delete from Qdrant
	var chunkIDs []string
	for _, c := range chunks {
		chunkIDs = append(chunkIDs, c.ID)
	}
	if len(chunkIDs) > 0 {
		if err := r.qdrant.Delete(ctx, chunkIDs); err != nil {
			return fmt.Errorf("delete vectors: %w", err)
		}
	}

	// Delete from MongoDB
	if err := r.mongo.DeleteDocument(ctx, id); err != nil {
		return fmt.Errorf("delete document: %w", err)
	}

	// Keep the in-memory BM25 index consistent: a deleted document must never
	// be returned from keyword search.
	for _, c := range chunks {
		r.RemoveBM25Document(c.ID)
	}

	return nil
}

// SearchVector performs vector search
func (r *DocumentRepository) SearchVector(ctx context.Context, vector []float64, topK int, tenantID string, scoreThreshold float64) ([]qdrant.SearchResult, error) {
	req := &qdrant.SearchRequest{
		Vector:         vector,
		Limit:          topK,
		WithPayload:    true,
		ScoreThreshold: scoreThreshold,
	}

	if tenantID != "" {
		req.Filter = map[string]interface{}{
			"must": []map[string]interface{}{
				{
					"key":   "tenant_id",
					"match": map[string]interface{}{"value": tenantID},
				},
			},
		}
	}

	return r.qdrant.Search(ctx, req)
}

// SearchBM25 performs BM25 text search with real BM25 algorithm
func (r *DocumentRepository) SearchBM25(ctx context.Context, query string, topK int, tenantID string) ([]Chunk, error) {
	r.bm25Mu.Lock()
	defer r.bm25Mu.Unlock()

	// Initialize BM25 index if not done
	if !r.bm25Init {
		// Load all chunks from MongoDB to build BM25 index
		chunks, err := r.mongo.GetAllChunks(ctx)
		if err != nil {
			// Fallback to simple search if BM25 initialization fails
			return r.searchBM25Fallback(ctx, query, topK)
		}

		// Build BM25 index
		r.bm25.Clear()
		r.chunkMeta = make(map[string]chunkInfo, len(chunks))
		for _, c := range chunks {
			kb := ""
			if c.Metadata != nil {
				if v, ok := c.Metadata["kb_id"].(string); ok {
					kb = v
				}
			}
			r.chunkMeta[c.ID] = chunkInfo{docID: c.DocID, kbID: kb}
			r.bm25.AddDocument(c.ID, c.Content)
		}
		r.bm25Init = true
	}

	// Perform BM25 search
	results := r.bm25.Search(query, topK)

	// Convert results to chunks
	var chunks []Chunk
	for _, res := range results {
		info := r.chunkMeta[res.ID]
		chunks = append(chunks, Chunk{
			ID:      res.ID,
			DocID:   info.docID,
			KBID:    info.kbID,
			Content: res.Content,
			Score:   res.Score,
		})
	}

	return chunks, nil
}

// searchBM25Fallback is a fallback when BM25 index cannot be built
func (r *DocumentRepository) searchBM25Fallback(ctx context.Context, query string, topK int) ([]Chunk, error) {
	chunks, err := r.mongo.SearchBM25(ctx, query, topK)
	if err != nil {
		return nil, err
	}

	var result []Chunk
	for _, c := range chunks {
		result = append(result, Chunk{
			ID:      c.ID,
			DocID:   c.DocID,
			Content: c.Content,
		})
	}

	return result, nil
}

// RefreshBM25Index rebuilds the BM25 index
func (r *DocumentRepository) RefreshBM25Index(ctx context.Context) error {
	r.bm25Mu.Lock()
	defer r.bm25Mu.Unlock()

	chunks, err := r.mongo.GetAllChunks(ctx)
	if err != nil {
		return fmt.Errorf("load chunks for BM25: %w", err)
	}

	r.bm25.Clear()
	for _, c := range chunks {
		r.bm25.AddDocument(c.ID, c.Content)
	}
	r.bm25Init = true

	return nil
}

func readDocMeta(meta map[string]interface{}) (status, kbID, reason string, retry, pageCount, overlap int, tags []string, strategy string) {
	status = StatusReady
	if meta == nil {
		return
	}
	if v, ok := meta["status"].(string); ok && v != "" {
		status = v
	}
	if v, ok := meta["kb_id"].(string); ok {
		kbID = v
	}
	if v, ok := meta["failure_reason"].(string); ok {
		reason = v
	}
	switch v := meta["retry_count"].(type) {
	case int:
		retry = v
	case int32:
		retry = int(v)
	case int64:
		retry = int(v)
	}
	switch v := meta["page_count"].(type) {
	case int:
		pageCount = v
	case int32:
		pageCount = int(v)
	}
	switch v := meta["overlap"].(type) {
	case int:
		overlap = v
	case int32:
		overlap = int(v)
	}
	if v, ok := meta["split_strategy"].(string); ok {
		strategy = v
	}
	if v, ok := meta["tags"].(interface{ String() string }); ok {
		_ = v
	}
	return
}

// SaveDocumentWithMeta saves a document with explicit state and kb metadata.
func (r *DocumentRepository) SaveDocumentWithMeta(ctx context.Context, meta SaveDocumentMeta, content string, chunks []string, embeddings [][]float64) (string, error) {
	docID, err := r.SaveDocument(ctx, meta.Filename, content, chunks, embeddings, meta.TenantID)
	if err != nil {
		return "", err
	}
	status := meta.Status
	if status == "" {
		status = StatusReady
	}
	metadata := map[string]interface{}{
		"status":         status,
		"kb_id":          meta.KBID,
		"split_strategy": meta.SplitStrategy,
		"overlap":        meta.Overlap,
		"page_count":     meta.PageCount,
	}
	if len(meta.Tags) > 0 {
		metadata["tags"] = meta.Tags
	}
	if err := r.mongo.UpdateDocumentMetadata(ctx, docID, metadata); err != nil {
		return "", err
	}
	return docID, nil
}

type SaveDocumentMeta struct {
	TenantID      string
	KBID          string
	Filename      string
	Status        string
	Tags          []string
	PageCount     int
	SplitStrategy string
	Overlap       int
}

// UpdateDocumentStatus updates the indexing state machine fields.
func (r *DocumentRepository) UpdateDocumentStatus(ctx context.Context, docID, status, reason string, retryCount int) error {
	if !ValidStatus(status) {
		return &UploadError{Code: "E_INVALID_STATUS", Msg: status}
	}
	return r.mongo.UpdateDocumentMetadata(ctx, docID, map[string]interface{}{
		"status":         status,
		"failure_reason": reason,
		"retry_count":    retryCount,
	})
}

// RemoveBM25Document keeps the in-memory index in sync after deletion.
func (r *DocumentRepository) RemoveBM25Document(chunkID string) {
	r.bm25Mu.Lock()
	defer r.bm25Mu.Unlock()
	if r.bm25.RemoveDocument(chunkID) {
		delete(r.chunkMeta, chunkID)
	}
}
