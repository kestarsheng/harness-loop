// Package llm provides LLM client implementations
package llm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go"
)

// Config holds LLM client configuration
type Config struct {
	Provider       string
	APIKey         string
	BaseURL        string
	Model          string
	EmbeddingModel string
	MaxTokens      int
	// EmitToolCallMessages sends standard OpenAI tool_calls (assistant) and
	// tool_call_id (tool) fields. Default false keeps the legacy request body
	// byte-for-byte unchanged, because not every OpenAI-compatible gateway
	// accepts these fields.
	EmitToolCallMessages bool
}

// Client is the LLM client interface
type Client interface {
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req *ChatRequest) (<-chan ChatStreamChunk, error)
	Embed(ctx context.Context, text string) ([]float64, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float64, error)
}

// Message represents a chat message
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolDefinition represents a tool definition
type ToolDefinition struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction represents a tool function
type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ToolCall represents a tool call
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction represents a tool call function
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatRequest represents a chat request
type ChatRequest struct {
	Messages     []Message        `json:"messages"`
	Model        string           `json:"model"`
	MaxTokens    int              `json:"max_tokens,omitempty"`
	Temperature  float64          `json:"temperature,omitempty"`
	Tools        []ToolDefinition `json:"tools,omitempty"`
	Stream       bool             `json:"stream,omitempty"`
	SystemPrompt string           `json:"-"`
}

// ChatResponse represents a chat response
type ChatResponse struct {
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	TotalTokens  int        `json:"total_tokens"`
	PromptTokens int        `json:"prompt_tokens"`
	FinishReason string     `json:"finish_reason"`
	Cost         float64    `json:"cost"`
}

// ChatStreamChunk represents a streaming chat chunk
type ChatStreamChunk struct {
	Content       string    `json:"content"`
	ToolCall      *ToolCall `json:"tool_call,omitempty"`
	ToolCallIndex int       `json:"tool_call_index,omitempty"` // stream index for aggregating multi-tool-call deltas
	Done          bool      `json:"done"`
	Error         error     `json:"error,omitempty"`
}

// CallMetrics records metrics for a single LLM call
type CallMetrics struct {
	Model       string    `json:"model"`
	TotalTokens int       `json:"total_tokens"`
	Cost        float64   `json:"cost"`
	LatencyMs   int64     `json:"latency_ms"`
	Success     bool      `json:"success"`
	Error       string    `json:"error,omitempty"`
	Caller      string    `json:"caller"` // engine, chat, reflection, eval, memory, mcp
	Timestamp   time.Time `json:"timestamp"`
}

// MetricsCallback is invoked after each LLM call completes
type MetricsCallback func(ctx context.Context, metrics *CallMetrics)

// metricsClient wraps a Client and records per-call metrics
type metricsClient struct {
	inner  Client
	cb     MetricsCallback
	caller string
}

// NewMetricsClient wraps a Client with metrics collection.
// If cb is nil, the wrapper is a no-op and inner is returned directly.
func NewMetricsClient(inner Client, cb MetricsCallback, caller string) Client {
	if cb == nil {
		return inner
	}
	return &metricsClient{inner: inner, cb: cb, caller: caller}
}

func (m *metricsClient) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	start := time.Now()
	resp, err := m.inner.Chat(ctx, req)
	latency := time.Since(start).Milliseconds()

	metrics := &CallMetrics{
		Model:     req.Model,
		LatencyMs: latency,
		Success:   err == nil,
		Caller:    m.caller,
		Timestamp: time.Now(),
	}
	if err != nil {
		metrics.Error = err.Error()
	}
	if resp != nil {
		metrics.TotalTokens = resp.TotalTokens
		metrics.Cost = resp.Cost
		if metrics.Model == "" {
			metrics.Model = "unknown"
		}
	}
	m.cb(ctx, metrics)

	return resp, err
}

func (m *metricsClient) ChatStream(ctx context.Context, req *ChatRequest) (<-chan ChatStreamChunk, error) {
	start := time.Now()
	ch, err := m.inner.ChatStream(ctx, req)
	latency := time.Since(start).Milliseconds()

	metrics := &CallMetrics{
		Model:     req.Model,
		LatencyMs: latency,
		Success:   err == nil,
		Caller:    m.caller,
		Timestamp: time.Now(),
	}
	if err != nil {
		metrics.Error = err.Error()
	}
	m.cb(ctx, metrics)

	return ch, err
}

func (m *metricsClient) Embed(ctx context.Context, text string) ([]float64, error) {
	start := time.Now()
	result, err := m.inner.Embed(ctx, text)
	latency := time.Since(start).Milliseconds()

	metrics := &CallMetrics{
		Model:     "embedding",
		LatencyMs: latency,
		Success:   err == nil,
		Caller:    m.caller,
		Timestamp: time.Now(),
	}
	if err != nil {
		metrics.Error = err.Error()
	}
	m.cb(ctx, metrics)

	return result, err
}

func (m *metricsClient) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	start := time.Now()
	result, err := m.inner.EmbedBatch(ctx, texts)
	latency := time.Since(start).Milliseconds()

	metrics := &CallMetrics{
		Model:     "embedding",
		LatencyMs: latency,
		Success:   err == nil,
		Caller:    m.caller,
		Timestamp: time.Now(),
	}
	if err != nil {
		metrics.Error = err.Error()
	}
	m.cb(ctx, metrics)

	return result, err
}

// OpenAIClient implements Client for OpenAI-compatible APIs on top of
// github.com/openai/openai-go.
type OpenAIClient struct {
	config Config
	sdk    openai.Client
}

// NewOpenAIClient creates a new OpenAI client
func NewOpenAIClient(cfg Config) *OpenAIClient {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 4096
	}
	return &OpenAIClient{
		config: cfg,
		sdk:    newSDKClient(cfg),
	}
}

// Chat sends a chat request
func (c *OpenAIClient) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	return chatOnce(ctx, &c.sdk, c.config, req, c.calculateCost)
}

// ChatStream sends a streaming chat request. The returned channel always ends
// with either Done or Error.
func (c *OpenAIClient) ChatStream(ctx context.Context, req *ChatRequest) (<-chan ChatStreamChunk, error) {
	return chatStream(ctx, &c.sdk, c.config, req), nil
}

// Embed generates embeddings for a single text
func (c *OpenAIClient) Embed(ctx context.Context, text string) ([]float64, error) {
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
func (c *OpenAIClient) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	return embedBatch(ctx, &c.sdk, c.config, texts)
}

func (c *OpenAIClient) calculateCost(tokens int) float64 {
	// GPT-4 pricing: $0.03/1K input, $0.06/1K output
	// Simplified: assume $0.04/1K tokens average
	return float64(tokens) * 0.00004
}

// NewClient creates a new LLM client based on provider
func NewClient(cfg Config) (Client, error) {
	switch cfg.Provider {
	case "openai":
		return NewOpenAIClient(cfg), nil
	case "dashscope":
		return NewDashScopeClient(cfg), nil
	default:
		// Default to DashScope if using DashScope-compatible base URL
		if strings.Contains(cfg.BaseURL, "dashscope") {
			return NewDashScopeClient(cfg), nil
		}
		return NewOpenAIClient(cfg), nil
	}
}
