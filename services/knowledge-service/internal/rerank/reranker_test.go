package rerank

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-platform/pkg/llm"
)

type stubLLM struct {
	content string
	err     error
	delay   time.Duration
}

func (s *stubLLM) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	return &llm.ChatResponse{Content: s.content}, nil
}

func (s *stubLLM) ChatStream(ctx context.Context, req *llm.ChatRequest) (<-chan llm.ChatStreamChunk, error) {
	return nil, errors.New("not implemented")
}

func (s *stubLLM) Embed(ctx context.Context, text string) ([]float64, error) {
	return nil, errors.New("not implemented")
}

func (s *stubLLM) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	return nil, errors.New("not implemented")
}

func candidates() []Candidate {
	return []Candidate{
		{ChunkID: "c1", Content: "alpha", Score: 0.9},
		{ChunkID: "c2", Content: "beta", Score: 0.8},
		{ChunkID: "c3", Content: "gamma", Score: 0.7},
	}
}

func TestRerankNoopKeepsFusionOrder(t *testing.T) {
	out, err := (Noop{}).Rerank(context.Background(), "q", candidates(), 2)
	if err != nil {
		t.Fatalf("noop: %v", err)
	}
	if len(out) != 2 || out[0].ChunkID != "c1" || out[1].ChunkID != "c2" {
		t.Fatalf("unexpected order: %+v", out)
	}
	if (Noop{}).ID() != "noop" {
		t.Fatal("noop id")
	}
}

func TestRerankLLMReordersWithoutChangingSet(t *testing.T) {
	stub := &stubLLM{content: `[{"id":2,"score":9.1},{"id":0,"score":5.0},{"id":1,"score":1.0}]`}
	r := NewLLMReranker(stub, "qwen-plus")
	out, err := r.Rerank(context.Background(), "gamma", candidates(), 3)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("candidates = %d", len(out))
	}
	if out[0].ChunkID != "c3" || out[1].ChunkID != "c1" || out[2].ChunkID != "c2" {
		t.Fatalf("order = %v", []string{out[0].ChunkID, out[1].ChunkID, out[2].ChunkID})
	}
	if out[0].RerankScore != 9.1 {
		t.Fatalf("rerank score = %v", out[0].RerankScore)
	}
	seen := map[string]bool{}
	for _, c := range out {
		seen[c.ChunkID] = true
	}
	for _, c := range candidates() {
		if !seen[c.ChunkID] {
			t.Fatalf("candidate %s dropped", c.ChunkID)
		}
	}
}

func TestRerankLLMTopMLimit(t *testing.T) {
	stub := &stubLLM{content: `[{"id":1,"score":7},{"id":0,"score":3},{"id":2,"score":2}]`}
	out, err := NewLLMReranker(stub, "m").Rerank(context.Background(), "q", candidates(), 2)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 2 || out[0].ChunkID != "c2" {
		t.Fatalf("topM not applied: %+v", out)
	}
}

func TestRerankLLMRejectsInvalidOutput(t *testing.T) {
	cases := []string{"not json", `[]`, `{"id":1}`, `[{"id":"x"}]`}
	for _, content := range cases {
		r := NewLLMReranker(&stubLLM{content: content}, "m")
		if _, err := r.Rerank(context.Background(), "q", candidates(), 3); err == nil {
			t.Fatalf("invalid output %q must fail", content)
		}
	}
}

func TestRerankLLMTimeout(t *testing.T) {
	r := NewLLMReranker(&stubLLM{content: `[{"id":0,"score":1}]`, delay: 100 * time.Millisecond}, "m")
	r.Timeout = 10 * time.Millisecond
	_, err := r.Rerank(context.Background(), "q", candidates(), 3)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestRerankLLMErrors(t *testing.T) {
	if _, err := (&LLMReranker{}).Rerank(context.Background(), "q", candidates(), 3); err == nil {
		t.Fatal("nil client must fail")
	}
	out, err := NewLLMReranker(&stubLLM{content: "[]"}, "m").Rerank(context.Background(), "q", nil, 3)
	if err != nil || out != nil {
		t.Fatalf("empty candidates must short-circuit: %v %v", out, err)
	}
	r := NewLLMReranker(&stubLLM{err: errors.New("boom")}, "m")
	if _, err := r.Rerank(context.Background(), "q", candidates(), 3); err == nil {
		t.Fatal("llm error must propagate")
	}
}
