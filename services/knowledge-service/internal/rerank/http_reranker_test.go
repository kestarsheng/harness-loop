package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func rerankCandidates(n int) []Candidate {
	out := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Candidate{
			ChunkID: "c" + string(rune('a'+i)),
			Content: "content " + string(rune('a'+i)),
			Score:   1.0 - float64(i)*0.01,
		})
	}
	return out
}

func TestHTTPRerankReordersAndTruncates(t *testing.T) {
	var gotBody rerankRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode: %v", err)
		}
		// 让最后 5 个候选分数最高，验证 top20 -> top5 的截断发生在排序之后。
		results := []rerankItem{}
		for i := 0; i < len(gotBody.Documents); i++ {
			results = append(results, rerankItem{Index: i, RelevanceScore: float64(i)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer server.Close()

	r := NewHTTPReranker(server.URL, time.Second)
	candidates := rerankCandidates(20)
	out, err := r.Rerank(context.Background(), "query", candidates, 5)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("topM = %d, want 5", len(out))
	}
	if len(gotBody.Documents) != 20 {
		t.Fatalf("request documents = %d, want 20", len(gotBody.Documents))
	}
	if gotBody.Query != "query" || gotBody.TopN != 20 {
		t.Fatalf("request = %+v", gotBody)
	}
	for i := 0; i < len(out); i++ {
		if out[i].ChunkID != candidates[len(candidates)-1-i].ChunkID {
			t.Fatalf("order[%d] = %s, want %s", i, out[i].ChunkID, candidates[len(candidates)-1-i].ChunkID)
		}
		if out[i].RerankScore <= 0 {
			t.Fatalf("rerank score not set: %+v", out[i])
		}
	}
	if r.ID() != "http" {
		t.Fatalf("id = %s", r.ID())
	}
}

func TestHTTPRerankAcceptsBareArrayAndTEIField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"index":1,"score":0.9},{"index":0,"score":0.1}]`))
	}))
	defer server.Close()

	out, err := NewHTTPReranker(server.URL, time.Second).Rerank(context.Background(), "q", rerankCandidates(3), 2)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 2 || out[0].ChunkID != "cb" {
		t.Fatalf("order = %+v", out)
	}
	if out[0].RerankScore != 0.9 {
		t.Fatalf("score = %v", out[0].RerankScore)
	}
}

func TestHTTPRerankRetriesOnceOn5xx(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`[{"index":0,"relevance_score":1.0}]`))
	}))
	defer server.Close()

	out, err := NewHTTPReranker(server.URL, 2*time.Second).Rerank(context.Background(), "q", rerankCandidates(3), 3)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 3 || out[0].ChunkID != "ca" {
		t.Fatalf("out = %+v", out)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry)", got)
	}
}

func TestHTTPRerankDoesNotRetryOn4xx(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	if _, err := NewHTTPReranker(server.URL, time.Second).Rerank(context.Background(), "q", rerankCandidates(3), 3); err == nil {
		t.Fatal("4xx must fail")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestHTTPRerankTimeoutBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`[{"index":0,"relevance_score":1.0}]`))
	}))
	defer server.Close()

	r := NewHTTPReranker(server.URL, 60*time.Millisecond)
	start := time.Now()
	_, err := r.Rerank(context.Background(), "q", rerankCandidates(3), 3)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("timeout must fail so the caller can degrade")
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("timeout budget not enforced: %v", elapsed)
	}
}

func TestHTTPRerankErrors(t *testing.T) {
	if _, err := NewHTTPReranker("", time.Second).Rerank(context.Background(), "q", rerankCandidates(3), 3); err == nil {
		t.Fatal("missing endpoint must fail")
	}
	if out, err := NewHTTPReranker("http://127.0.0.1:1", time.Second).Rerank(context.Background(), "q", nil, 3); err != nil || out != nil {
		t.Fatalf("empty candidates must short-circuit: %v %v", out, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer server.Close()
	if _, err := NewHTTPReranker(server.URL, time.Second).Rerank(context.Background(), "q", rerankCandidates(3), 3); err == nil {
		t.Fatal("invalid payload must fail")
	}
}

func TestHTTPRerankKeepsCandidateSetOnPartialScores(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.2}]}`))
	}))
	defer server.Close()

	out, err := NewHTTPReranker(server.URL, time.Second).Rerank(context.Background(), "q", rerankCandidates(3), 3)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("candidate set must stay complete: %+v", out)
	}
	seen := map[string]bool{}
	for _, c := range out {
		seen[c.ChunkID] = true
	}
	for _, c := range rerankCandidates(3) {
		if !seen[c.ChunkID] {
			t.Fatalf("candidate %s dropped", c.ChunkID)
		}
	}
	if out[0].ChunkID != "ca" {
		t.Fatalf("scored candidate must rank first: %+v", out)
	}
	if out[len(out)-1].ChunkID != "cc" {
		t.Fatalf("unscored candidates must keep fusion order behind scored ones: %+v", out)
	}
}

func TestRerankMetricsSnapshot(t *testing.T) {
	m := NewMetrics()
	m.Record(100, false)
	m.Record(300, true)
	s := m.Snapshot()
	if s.Calls != 2 || s.Degraded != 1 || s.Failures != 1 || s.MaxMs != 300 {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.DegradedRate != 0.5 || s.AvgMs != 200 {
		t.Fatalf("derived = %+v", s)
	}
}
