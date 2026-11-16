package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	pb "agent-platform/pkg/pb/knowledge"
	"agent-platform/services/knowledge-service/internal/repository"
	"agent-platform/services/knowledge-service/internal/rerank"
)

func searchResults(n int) []*pb.SearchResult {
	out := make([]*pb.SearchResult, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &pb.SearchResult{
			ChunkId:    "chunk-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			DocumentId: "doc-1",
			Content:    "content",
			Score:      1.0 - float64(i)*0.01,
		})
	}
	return out
}

func TestHTTPRerankDegradedMarksMetadata(t *testing.T) {
	svc := &KnowledgeService{
		reranker:         rerank.NewHTTPReranker("http://127.0.0.1:1/rerank", 80*time.Millisecond),
		rerankMetrics:    rerank.NewMetrics(),
		rerankCandidates: 20,
		rerankTopM:       5,
	}
	results := searchResults(8)
	out := svc.rerankResults(context.Background(), "query", results, 8)

	if len(out) != 5 {
		t.Fatalf("out = %d, want top5", len(out))
	}
	for i, r := range out {
		if r.Metadata["rerank"] != "degraded" {
			t.Fatalf("result %d metadata = %v, want rerank=degraded", i, r.Metadata)
		}
		if r.ChunkId != results[i].ChunkId {
			t.Fatalf("degraded path must keep fusion order: %s vs %s", r.ChunkId, results[i].ChunkId)
		}
	}
	stats := svc.RerankStats()
	if stats.Calls != 1 || stats.Degraded != 1 || stats.DegradedRate != 1 {
		t.Fatalf("metrics = %+v", stats)
	}
}

func TestHTTPRerankAppliedScoresAndTopM(t *testing.T) {
	var documents int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		documents = len(req.Documents)
		results := make([]map[string]any, 0, len(req.Documents))
		// 反向打分：最后一个候选得分最高。
		for i := 0; i < len(req.Documents); i++ {
			results = append(results, map[string]any{"index": i, "relevance_score": float64(i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer server.Close()

	svc := &KnowledgeService{
		reranker:         rerank.NewHTTPReranker(server.URL, time.Second),
		rerankMetrics:    rerank.NewMetrics(),
		rerankCandidates: 20,
		rerankTopM:       5,
	}
	results := searchResults(30)
	out := svc.rerankResults(context.Background(), "query", results, 5)

	if documents != 20 {
		t.Fatalf("candidates sent = %d, want 20", documents)
	}
	if len(out) != 5 {
		t.Fatalf("out = %d, want 5", len(out))
	}
	if out[0].ChunkId != results[19].ChunkId {
		t.Fatalf("best cross-encoder score must rank first: %s vs %s", out[0].ChunkId, results[19].ChunkId)
	}
	for _, r := range out {
		if r.Metadata["rerank"] != "applied" {
			t.Fatalf("metadata = %v", r.Metadata)
		}
		if r.Metadata["rerank_score"] == "" {
			t.Fatalf("rerank_score missing: %v", r.Metadata)
		}
	}
	if stats := svc.RerankStats(); stats.Calls != 1 || stats.Degraded != 0 {
		t.Fatalf("metrics = %+v", stats)
	}
}

func TestHTTPRerankNilRerankerKeepsFusionOrder(t *testing.T) {
	svc := &KnowledgeService{rerankMetrics: rerank.NewMetrics(), rerankCandidates: 20, rerankTopM: 5}
	results := searchResults(8)
	out := svc.rerankResults(context.Background(), "query", results, 8)
	if len(out) != 5 {
		t.Fatalf("out = %d", len(out))
	}
	for i, r := range out {
		if r.ChunkId != results[i].ChunkId {
			t.Fatalf("fusion order must be preserved")
		}
		if r.Metadata["rerank"] != "" {
			t.Fatalf("no reranker means no rerank mark: %v", r.Metadata)
		}
	}
}

func TestHTTPRerankLatencyP95(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`[{"index":0,"relevance_score":1}]`))
	}))
	defer server.Close()

	svc := &KnowledgeService{
		reranker:         rerank.NewHTTPReranker(server.URL, 2*time.Second),
		rerankMetrics:    rerank.NewMetrics(),
		rerankCandidates: 20,
		rerankTopM:       5,
	}
	const rounds = 20
	samples := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		_ = svc.rerankResults(context.Background(), "query", searchResults(20), 5)
		samples = append(samples, time.Since(start))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p95 := samples[len(samples)*95/100-1]
	if p95 > 2500*time.Millisecond {
		t.Fatalf("rerank P95 = %v, want <= 2.5s", p95)
	}
	t.Logf("rerank P95=%v min=%v max=%v rounds=%d", p95, samples[0], samples[len(samples)-1], rounds)
}

type fakeDocumentGetter struct {
	docs map[string]*repository.Document
}

func (f fakeDocumentGetter) GetDocument(_ context.Context, id, _ string) (*repository.Document, error) {
	if doc, ok := f.docs[id]; ok {
		return doc, nil
	}
	return nil, errDocumentMissing
}

var errDocumentMissing = &documentMissingError{}

type documentMissingError struct{}

func (e *documentMissingError) Error() string { return "document missing" }

func TestIndexWorkerFailedDocumentNotSearchable(t *testing.T) {
	results := []*pb.SearchResult{
		{ChunkId: "c1", DocumentId: "ready-doc"},
		{ChunkId: "c2", DocumentId: "failed-doc"},
		{ChunkId: "c3", DocumentId: "pending-doc"},
		{ChunkId: "c4", DocumentId: "processing-doc"},
		{ChunkId: "c5", DocumentId: "other-kb"},
		{ChunkId: "c6", DocumentId: "unknown-doc"},
	}
	getter := fakeDocumentGetter{docs: map[string]*repository.Document{
		"ready-doc":      {ID: "ready-doc", Status: repository.StatusReady, KBID: "kb1"},
		"failed-doc":     {ID: "failed-doc", Status: repository.StatusFailed, KBID: "kb1"},
		"pending-doc":    {ID: "pending-doc", Status: repository.StatusPending, KBID: "kb1"},
		"processing-doc": {ID: "processing-doc", Status: repository.StatusProcessing, KBID: "kb1"},
		"other-kb":       {ID: "other-kb", Status: repository.StatusReady, KBID: "kb2"},
	}}

	out := filterSearchableResults(context.Background(), getter, results, "t1", "kb1")
	if len(out) != 2 {
		t.Fatalf("kept %d results, want ready + unknown(fail-open): %+v", len(out), out)
	}
	if out[0].ChunkId != "c1" || out[1].ChunkId != "c6" {
		t.Fatalf("kept = %+v", out)
	}
	for _, status := range []string{repository.StatusFailed, repository.StatusPending, repository.StatusProcessing} {
		if repository.Searchable(status) {
			t.Fatalf("%s must never be searchable", status)
		}
	}
	if !repository.Searchable(repository.StatusReady) {
		t.Fatal("ready must be searchable")
	}
}
