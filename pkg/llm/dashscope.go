// Package llm provides LLM client implementations
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/openai/openai-go"
)

// DashScopeClient implements Client for Alibaba DashScope API (通义千问).
// Chat and streaming go through the OpenAI-compatible endpoint via the SDK;
// embeddings keep the native DashScope endpoint, which the SDK cannot target.
type DashScopeClient struct {
	config Config
	sdk    openai.Client
	// httpClient is used only by the native embedding endpoint.
	httpClient *http.Client
}

// NewDashScopeClient creates a new DashScope client
func NewDashScopeClient(cfg Config) *DashScopeClient {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 4096
	}
	return &DashScopeClient{
		config: cfg,
		sdk:    newSDKClient(cfg),
		httpClient: &http.Client{
			Timeout: 300 * time.Second, // 5 分钟，浏览器 Agent 需要更长超时
		},
	}
}

// Chat sends a chat request to DashScope
func (c *DashScopeClient) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	return chatOnce(ctx, &c.sdk, c.config, req, c.calculateCost)
}

// ChatStream sends a streaming chat request to DashScope
func (c *DashScopeClient) ChatStream(ctx context.Context, req *ChatRequest) (<-chan ChatStreamChunk, error) {
	return chatStream(ctx, &c.sdk, c.config, req), nil
}

// Embed generates embeddings using DashScope embedding model
func (c *DashScopeClient) Embed(ctx context.Context, text string) ([]float64, error) {
	results, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return results[0], nil
}

// EmbedBatch generates embeddings for multiple texts
func (c *DashScopeClient) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	// Use the configured embedding model
	model := c.config.Model
	if c.config.EmbeddingModel != "" {
		model = c.config.EmbeddingModel
	}

	fmt.Printf("DEBUG EmbedBatch: model=%s, texts=%v\n", model, texts)

	// DashScope embedding API uses different endpoint
	// https://dashscope.aliyuncs.com/api/v1/services/embeddings/text-embedding/text-embedding
	embedURL := "https://dashscope.aliyuncs.com/api/v1/services/embeddings/text-embedding/text-embedding"

	req := map[string]interface{}{
		"model": model,
		"input": map[string]interface{}{
			"texts": texts,
		},
		"parameters": map[string]interface{}{
			"text_type": "query",
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		embedURL,
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.config.APIKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	fmt.Printf("DEBUG EmbedBatch: status=%d, response=%s\n", resp.StatusCode, string(respBody))

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	// DashScope embedding response format
	// API returns: {"output":{"embeddings":[{"embedding":[...],"text_index":0}]}}
	var embedResp struct {
		Output struct {
			Embeddings []struct {
				Embedding []float64 `json:"embedding"`
				TextIndex int       `json:"text_index"`
			} `json:"embeddings"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}

	if err := json.Unmarshal(respBody, &embedResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if embedResp.Code != "" && embedResp.Code != "Success" {
		return nil, fmt.Errorf("API error: %s", embedResp.Message)
	}

	results := make([][]float64, len(texts))
	for _, r := range embedResp.Output.Embeddings {
		if r.TextIndex < len(results) {
			results[r.TextIndex] = r.Embedding
		}
	}

	return results, nil
}

// calculateCost calculates cost based on DashScope pricing
func (c *DashScopeClient) calculateCost(tokens int) float64 {
	// Qwen pricing: approximately $0.002/1K tokens for input + output
	// This is an approximation
	return float64(tokens) * 0.000002
}
