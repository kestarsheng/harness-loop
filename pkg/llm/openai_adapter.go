package llm

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
	"github.com/openai/openai-go/shared/constant"
)

// requestTimeout preserves the legacy 5-minute per-call timeout. The SDK
// applies it per attempt, and retries are disabled so one logical call maps to
// exactly one HTTP request, as before.
const requestTimeout = 300 * time.Second

// newSDKClient builds the underlying openai-go client for any
// OpenAI-compatible endpoint.
func newSDKClient(cfg Config) openai.Client {
	// The gateway's TLS handshake can exceed the 10s net/http default on slow
	// links; the overall per-call deadline below still bounds the request.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = 30 * time.Second

	return openai.NewClient(
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(normalizeBaseURL(cfg.BaseURL)),
		option.WithHTTPClient(&http.Client{Transport: transport}),
		option.WithRequestTimeout(requestTimeout),
		option.WithMaxRetries(0),
	)
}

// normalizeBaseURL keeps the SDK's URL resolution from dropping a path prefix
// such as /v1 when the configured URL has no trailing slash.
func normalizeBaseURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/"
}

// buildChatParams converts a ChatRequest into SDK params. System prompt, model
// override, max-token fallback and tool definitions follow the legacy client.
//
// stream_options is only attached for streaming calls: the gateway rejects a
// request that sets stream_options while stream=false.
func buildChatParams(cfg Config, req *ChatRequest, streaming bool) openai.ChatCompletionNewParams {
	effectiveModel := cfg.Model
	if req.Model != "" {
		effectiveModel = req.Model
	}

	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, openai.SystemMessage(req.SystemPrompt))
	}
	for _, m := range req.Messages {
		messages = append(messages, toMessageParam(m, cfg.EmitToolCallMessages))
	}

	params := openai.ChatCompletionNewParams{
		Model:    effectiveModel,
		Messages: messages,
	}
	if streaming {
		// Ask for the trailing usage frame on streaming responses. Providers that
		// ignore stream_options simply omit the frame; consumers then fall back to
		// their estimate instead of inventing a real usage value.
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: param.NewOpt(true),
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = cfg.MaxTokens
	}
	if maxTokens > 0 {
		params.MaxTokens = param.NewOpt(int64(maxTokens))
	}
	if req.Temperature > 0 {
		params.Temperature = param.NewOpt(req.Temperature)
	}

	if len(req.Tools) > 0 {
		params.Tools = make([]openai.ChatCompletionToolParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			params.Tools = append(params.Tools, openai.ChatCompletionToolParam{
				Type: functionType(t.Type),
				Function: shared.FunctionDefinitionParam{
					Name:        t.Function.Name,
					Description: param.NewOpt(t.Function.Description),
					Parameters:  shared.FunctionParameters(t.Function.Parameters),
				},
			})
		}
	}

	return params
}

// toMessageParam maps an internal message onto the SDK message union. Tool
// fields are emitted only when emitToolCallMessages is enabled; otherwise the
// legacy body is preserved, including a tool message without tool_call_id.
func toMessageParam(m Message, emitToolCallMessages bool) openai.ChatCompletionMessageParamUnion {
	switch m.Role {
	case "system":
		return openai.SystemMessage(m.Content)
	case "assistant":
		return assistantParam(m, emitToolCallMessages)
	case "tool":
		return toolMessageParam(m, emitToolCallMessages)
	default:
		return openai.UserMessage(m.Content)
	}
}

func assistantParam(m Message, emitToolCallMessages bool) openai.ChatCompletionMessageParamUnion {
	p := openai.ChatCompletionAssistantMessageParam{}
	if m.Content != "" {
		p.Content.OfString = param.NewOpt(m.Content)
	}
	if emitToolCallMessages && len(m.ToolCalls) > 0 {
		p.ToolCalls = make([]openai.ChatCompletionMessageToolCallParam, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			p.ToolCalls = append(p.ToolCalls, openai.ChatCompletionMessageToolCallParam{
				ID:   tc.ID,
				Type: functionType(tc.Type),
				Function: openai.ChatCompletionMessageToolCallFunctionParam{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &p}
}

func toolMessageParam(m Message, emitToolCallMessages bool) openai.ChatCompletionMessageParamUnion {
	if emitToolCallMessages {
		return openai.ToolMessage(m.Content, m.ToolCallID)
	}
	// Legacy body: role "tool" plus content but no tool_call_id. The SDK marks
	// tool_call_id as required, so drop it explicitly.
	p := openai.ChatCompletionToolMessageParam{}
	if m.Content != "" {
		p.Content.OfString = param.NewOpt(m.Content)
	}
	p.SetExtraFields(map[string]any{"tool_call_id": param.Omit})
	return openai.ChatCompletionMessageParamUnion{OfTool: &p}
}

func functionType(t string) constant.Function {
	if t == "" {
		return "function"
	}
	return constant.Function(t)
}

// normalizeFinishReason maps provider-specific values onto the four standard
// OpenAI finish reasons while leaving unknown vendor values intact.
func normalizeFinishReason(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "null":
		return ""
	case "stop", "end_turn", "eos":
		return "stop"
	case "length", "max_tokens":
		return "length"
	case "tool_calls", "tool_call", "function_call":
		return "tool_calls"
	case "content_filter", "safety":
		return "content_filter"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

// normalizeToolCall fills the only supported tool type when a provider omits
// it and keeps id/name/arguments as-is.
func normalizeToolCall(tc ToolCall) ToolCall {
	if tc.Type == "" {
		tc.Type = "function"
	}
	return tc
}

// chatOnce performs one non-streaming chat call through the SDK.
func chatOnce(ctx context.Context, sdk *openai.Client, cfg Config, req *ChatRequest, cost func(int) float64) (*ChatResponse, error) {
	completion, err := sdk.Chat.Completions.New(ctx, buildChatParams(cfg, req, false))
	if err != nil {
		return nil, classifyError(ctx, err)
	}

	result := &ChatResponse{
		TotalTokens:  int(completion.Usage.TotalTokens),
		PromptTokens: int(completion.Usage.PromptTokens),
	}
	if len(completion.Choices) > 0 {
		choice := completion.Choices[0]
		result.Content = choice.Message.Content
		result.FinishReason = normalizeFinishReason(choice.FinishReason)
		for _, tc := range choice.Message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, normalizeToolCall(ToolCall{
				ID:   tc.ID,
				Type: string(tc.Type),
				Function: ToolCallFunction{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			}))
		}
	}
	result.Cost = cost(result.TotalTokens)
	return result, nil
}

// chatStream performs one streaming chat call through the SDK. The returned
// channel always ends with either Done or Error.
func chatStream(ctx context.Context, sdk *openai.Client, cfg Config, req *ChatRequest) <-chan ChatStreamChunk {
	ch := make(chan ChatStreamChunk, 100)
	go func() {
		defer close(ch)

		stream := sdk.Chat.Completions.NewStreaming(ctx, buildChatParams(cfg, req, true))
		defer stream.Close()

		for stream.Next() {
			chunk := stream.Current()
			// The usage frame has no choices, so read it before the empty-choices
			// skip. Some providers omit it entirely.
			if u := chunk.Usage; u.TotalTokens > 0 || u.PromptTokens > 0 || u.CompletionTokens > 0 {
				ch <- ChatStreamChunk{Usage: &Usage{
					PromptTokens:     int(u.PromptTokens),
					CompletionTokens: int(u.CompletionTokens),
					TotalTokens:      int(u.TotalTokens),
				}}
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				ch <- ChatStreamChunk{Content: delta.Content}
			}
			for _, tc := range delta.ToolCalls {
				call := normalizeToolCall(ToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: ToolCallFunction{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
				ch <- ChatStreamChunk{
					ToolCall:      &call,
					ToolCallIndex: int(tc.Index),
				}
			}
		}

		if err := stream.Err(); err != nil {
			ch <- ChatStreamChunk{Error: classifyError(ctx, err)}
			return
		}
		ch <- ChatStreamChunk{Done: true}
	}()
	return ch
}

// embedBatch performs one embeddings call through the SDK.
func embedBatch(ctx context.Context, sdk *openai.Client, cfg Config, texts []string) ([][]float64, error) {
	model := cfg.EmbeddingModel
	if model == "" {
		model = "text-embedding-3-small"
	}
	if len(texts) == 0 {
		return [][]float64{}, nil
	}

	res, err := sdk.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: model,
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
	})
	if err != nil {
		return nil, classifyError(ctx, err)
	}

	results := make([][]float64, len(res.Data))
	for _, d := range res.Data {
		if d.Index >= 0 && int(d.Index) < len(results) {
			results[d.Index] = d.Embedding
		}
	}
	return results, nil
}
