package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// HTTPReranker 适配外部 cross-encoder 服务（bge-reranker / Cohere / Jina / TEI 的 /rerank 形态）。
//
// 设计约束（design 4.10.4、REQ-091）：
//   - 输入 top 20、输出 top 5，由调用方通过 topM 控制；
//   - 超时预算独立于主链路，失败由调用方降级为融合顺序并标 rerank=degraded；
//   - 请求路径最多尝试 2 次（首次 + 1 次重试），不做长退避重试。
type HTTPReranker struct {
	URL     string
	Model   string
	APIKey  string
	Timeout time.Duration
	Client  *http.Client
	// MaxAttempts 是单次 Rerank 的最大尝试次数，默认 2。
	MaxAttempts int
}

// DefaultRerankTimeout 是 cross-encoder 的超时预算（服务 P95 目标 ≤ 2.5s，含检索）。
const DefaultRerankTimeout = 1200 * time.Millisecond

func NewHTTPReranker(url string, timeout time.Duration) *HTTPReranker {
	if timeout <= 0 {
		timeout = DefaultRerankTimeout
	}
	return &HTTPReranker{URL: strings.TrimSpace(url), Timeout: timeout, MaxAttempts: 2}
}

func (r *HTTPReranker) ID() string { return "http" }

type rerankRequest struct {
	Model     string   `json:"model,omitempty"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankItem struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
	Score          float64 `json:"score"`
}

type rerankResponse struct {
	Results []rerankItem `json:"results"`
}

func (r *HTTPReranker) Rerank(ctx context.Context, query string, candidates []Candidate, topM int) ([]Candidate, error) {
	if r == nil || r.URL == "" {
		return nil, fmt.Errorf("rerank: http endpoint is not configured")
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultRerankTimeout
	}
	attempts := r.MaxAttempts
	if attempts <= 0 {
		attempts = 2
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	documents := make([]string, 0, len(candidates))
	for _, c := range candidates {
		documents = append(documents, truncate(c.Content, 1200))
	}
	body, err := json.Marshal(rerankRequest{Model: r.Model, Query: query, Documents: documents, TopN: len(candidates)})
	if err != nil {
		return nil, fmt.Errorf("rerank: encode request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		items, err := r.doRerank(runCtx, body)
		if err == nil {
			return applyScores(candidates, items, topM), nil
		}
		lastErr = err
		var permanent *permanentError
		if errors.As(err, &permanent) || runCtx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// permanentError 标记不该重试的失败（4xx、响应格式错误）。
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }

func permanent(err error) error { return &permanentError{err: err} }

func (r *HTTPReranker) doRerank(ctx context.Context, body []byte) ([]rerankItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rerank: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.APIKey)
	}

	client := r.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: http call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("rerank: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// 4xx 属于确定性失败（URL/鉴权/请求体），不重试；5xx 可重试一次。
		err := fmt.Errorf("rerank: status %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, permanent(err)
		}
		return nil, err
	}
	items, err := parseRerankResponse(payload)
	if err != nil {
		return nil, permanent(err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("rerank: empty score list")
	}
	return items, nil
}

// parseRerankResponse 兼容两种主流返回：{"results":[...]} 与裸数组 [...],
// 每项接受 relevance_score（Cohere/Jina）或 score（TEI）。
func parseRerankResponse(payload []byte) ([]rerankItem, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("rerank: empty response body")
	}
	switch trimmed[0] {
	case '[':
		var items []rerankItem
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("rerank: invalid json array: %w", err)
		}
		return items, nil
	case '{':
		var obj rerankResponse
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, fmt.Errorf("rerank: invalid json object: %w", err)
		}
		if obj.Results == nil {
			return nil, fmt.Errorf("rerank: response has no results")
		}
		return obj.Results, nil
	}
	return nil, fmt.Errorf("rerank: unexpected response payload")
}

// applyScores 按 cross-encoder 分数重排，保持候选集合完整、排序确定：
// relevance desc -> 原融合分 desc -> 原顺序 asc。
func applyScores(candidates []Candidate, items []rerankItem, topM int) []Candidate {
	out := make([]Candidate, len(candidates))
	copy(out, candidates)
	for _, item := range items {
		if item.Index < 0 || item.Index >= len(out) {
			continue
		}
		score := item.RelevanceScore
		if score == 0 {
			score = item.Score
		}
		out[item.Index].RerankScore = score
	}
	index := make(map[string]int, len(out))
	for i, c := range out {
		index[c.ChunkID] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RerankScore != out[j].RerankScore {
			return out[i].RerankScore > out[j].RerankScore
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return index[out[i].ChunkID] < index[out[j].ChunkID]
	})
	return top(out, topM)
}
