package rerank

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agent-platform/pkg/llm"
)

type LLMReranker struct {
	Client  llm.Client
	Model   string
	Timeout time.Duration
}

func NewLLMReranker(client llm.Client, model string) *LLMReranker {
	return &LLMReranker{Client: client, Model: model, Timeout: 3 * time.Second}
}

func (r *LLMReranker) ID() string { return "llm" }

type scored struct {
	ID    int     `json:"id"`
	Score float64 `json:"score"`
}

func (r *LLMReranker) Rerank(ctx context.Context, query string, candidates []Candidate, topM int) ([]Candidate, error) {
	if r.Client == nil {
		return nil, fmt.Errorf("rerank: llm client is nil")
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var b strings.Builder
	b.WriteString("Query: ")
	b.WriteString(query)
	b.WriteString("\nCandidates:\n")
	for i, c := range candidates {
		b.WriteString(fmt.Sprintf("[%d] %s\n", i, truncate(c.Content, 300)))
	}
	b.WriteString("\nScore each candidate for relevance from 0 to 10. Output ONLY a JSON array like [{\"id\":0,\"score\":8.5}]. No prose.")

	resp, err := r.Client.Chat(runCtx, &llm.ChatRequest{
		Messages:     []llm.Message{{Role: "user", Content: b.String()}},
		Model:        r.Model,
		Temperature:  0,
		SystemPrompt: "You are a strict reranker. Return valid JSON only.",
	})
	if err != nil {
		return nil, fmt.Errorf("rerank: llm call: %w", err)
	}
	scores, err := parseScores(resp.Content)
	if err != nil {
		return nil, err
	}

	reranked := make([]Candidate, len(candidates))
	copy(reranked, candidates)
	for _, s := range scores {
		if s.ID >= 0 && s.ID < len(reranked) {
			reranked[s.ID].RerankScore = s.Score
		}
	}
	sort.SliceStable(reranked, func(i, j int) bool {
		if reranked[i].RerankScore != reranked[j].RerankScore {
			return reranked[i].RerankScore > reranked[j].RerankScore
		}
		return reranked[i].Score > reranked[j].Score
	})
	return top(reranked, topM), nil
}

func parseScores(content string) ([]scored, error) {
	start := strings.Index(content, "[")
	end := strings.LastIndex(content, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("rerank: output is not a json array")
	}
	var out []scored
	if err := json.Unmarshal([]byte(content[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("rerank: invalid json: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("rerank: empty score list")
	}
	return out, nil
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
