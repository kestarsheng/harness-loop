package agent

import (
	"agent-platform/pkg/agent/approval"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// StreamEventType defines the type of streaming event
type StreamEventType string

const (
	// EventThink is emitted when the agent starts thinking (LLM call begins)
	EventThink StreamEventType = "think"
	// EventToken is emitted for each token from the LLM
	EventToken StreamEventType = "token"
	// EventToolStart is emitted before a tool is executed
	EventToolStart StreamEventType = "tool_start"
	// EventToolResult is emitted after a tool completes
	EventToolResult StreamEventType = "tool_result"
	// EventHandoff is emitted when control transfers to another agent
	EventHandoff StreamEventType = "handoff"
	// EventFinal is emitted when the agent loop completes
	EventFinal StreamEventType = "final"
	// EventError is emitted on errors
	EventError StreamEventType = "error"
	// EventApproval is emitted when a tool call requires human approval.
	// The agent blocks on WaitForApproval until the user submits a decision
	// via the approval API. Carries the approval request id so the frontend
	// can show an inline approval prompt and call /approval/approve|reject.
	EventApproval StreamEventType = "approval"
)

// ApprovalEvent carries the info needed for a frontend to render an
// in-chat approval prompt and submit a decision back via the approval API.
type ApprovalEvent struct {
	RequestID string `json:"request_id"`
	ToolName  string `json:"tool_name"`
	Reason    string `json:"reason,omitempty"`
}

// StreamEvent represents a single streaming event from the engine
type StreamEvent struct {
	Type       StreamEventType `json:"type"`
	AgentID    string          `json:"agent_id,omitempty"`
	AgentName  string          `json:"agent_name,omitempty"`
	Content    string          `json:"content,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolArgs   json.RawMessage `json:"tool_args,omitempty"`
	ToolResult string          `json:"tool_result,omitempty"`
	Step       int             `json:"step,omitempty"`
	Tokens     int             `json:"tokens,omitempty"`
	Done       bool            `json:"done,omitempty"`
	Error      string          `json:"error,omitempty"`
	// Approval is set when Type == EventApproval.
	Approval *ApprovalEvent `json:"approval,omitempty"`
}

// StreamCallback is called for each streaming event
type StreamCallback func(event StreamEvent)

// Stage-loop context variables. The harness loop (harnessloop-service) owns the
// stage policy matrix and publishes the tool subset a stage may use; the engine
// only consumes it. Keeping the policy in the caller means the engine never
// hard-codes which stage allows which tool (DFX-M-05 可配置化).
const (
	// StageToolListVar carries the comma-separated MCP tool names the current
	// stage is allowed to call.
	StageToolListVar = "__stage_tools__"
	// StageRequiresToolCallVar is "true" when a stage may not finish without at
	// least one real tool call.
	StageRequiresToolCallVar = "__requires_tool_call__"
	// stageFinishTool is the tool the model must call to close a stage.
	stageFinishTool = "stage_finish"
	// stageNudgeLimit caps how many times the loop re-prompts a model that tries
	// to finish a stage without calling any tool. Bounded so a stubborn model
	// cannot burn the whole step budget.
	stageNudgeLimit = 2
)

// ErrStageToolCallRequired reports that a stage finished without the mandatory
// tool call. It is returned instead of accepting the finish, so a "completed"
// stage always has at least one real tool decision behind it.
var ErrStageToolCallRequired = errors.New("stage requires at least one tool call before finish")

// LLMStreamingClient extends LLMClient with streaming capability
type LLMStreamingClient interface {
	LLMClient
	// ChatStream calls the LLM and returns a channel of incremental tokens
	ChatStream(ctx context.Context, req *LLMRequest) (<-chan LLMStreamChunk, error)
}

// TokenUsage mirrors the provider's usage block for one LLM call.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// LLMStreamChunk represents an incremental token from the LLM
type LLMStreamChunk struct {
	Content       string      `json:"content"`
	ToolCall      *ToolCall   `json:"tool_call,omitempty"`
	ToolCallIndex int         `json:"tool_call_index,omitempty"` // stream index for aggregating multi-tool-call deltas
	Usage         *TokenUsage `json:"usage,omitempty"`
	Done          bool        `json:"done"`
	Error         error       `json:"error,omitempty"`
}

// RunStream executes the agent loop with real-time event streaming
// The callback is invoked for each event as it happens.
// The final ExecutionResult is still returned at the end.
func (e *Engine) RunStream(ctx context.Context, req *ExecutionRequest, callback StreamCallback) (*ExecutionResult, error) {
	// Create execution context
	execCtx := NewExecutionContext(req.SessionID)
	execCtx.TenantID = req.TenantID
	execCtx.UserID = req.UserID
	execCtx.EntryAgent = req.EntryAgent
	execCtx.StageMode = req.StageMode
	execCtx.Goal = req.Goal
	execCtx.SuccessCriteria = req.SuccessCriteria
	execCtx.MarkRunning()

	// Set initial variables
	for k, v := range req.ContextVars {
		execCtx.SetVariable(k, v)
	}

	// Set system prompt override from Prompt Management
	if req.SystemPromptOverride != "" {
		execCtx.SystemPromptOverride = req.SystemPromptOverride
	}

	// Add user message
	execCtx.AddMessage("user", req.Message)

	// Get entry agent
	entryAgentID := req.EntryAgent
	if entryAgentID == "" {
		defaultAgent := e.registry.GetDefault()
		if defaultAgent == nil {
			return nil, ErrNoDefaultAgent
		}
		entryAgentID = defaultAgent.ID
	}

	// Start with entry agent
	currentAgent := e.registry.Get(entryAgentID)
	if currentAgent == nil {
		return nil, fmt.Errorf("entry agent %s not found", entryAgentID)
	}

	execCtx.CurrentAgent = currentAgent.ID

	// Save context
	if e.store != nil {
		if err := e.store.Save(ctx, execCtx); err != nil {
			return nil, fmt.Errorf("save context: %w", err)
		}
	}

	// Execute agent loop with streaming
	result, err := e.executeLoopStream(ctx, execCtx, currentAgent, req.Message, callback)

	// Save final context
	if e.store != nil {
		e.store.Save(ctx, execCtx)
	}

	return result, err
}

// executeLoopStream runs the agent loop with real-time event streaming
func (e *Engine) executeLoopStream(ctx context.Context, execCtx *ExecutionContext, startAgent *Agent, initialMessage string, callback StreamCallback) (*ExecutionResult, error) {
	currentAgent := startAgent

	// Check if the LLM client supports streaming
	_, hasStreaming := e.llmClient.(LLMStreamingClient)

	// Stage mode bookkeeping: how many tools the loop really executed (handoff
	// and stage_finish do not count) and how often we already nudged the model
	// to use one.
	toolCallsMade := 0
	nudges := 0

	for step := 0; step < e.config.MaxSteps; step++ {
		stepStart := time.Now()
		execCtx.StepCount = step + 1

		// In stage mode the running agent is narrowed to the tools the current
		// stage may use and its handoffs are removed: one stage is one agent's
		// job, and a handoff would emit no tool_call chunk for the gate.
		stepAgent := currentAgent
		if execCtx.StageMode {
			stepAgent = stageAgentOverlay(currentAgent, execCtx)
		}

		// Build messages for this agent
		messages := e.buildAgentMessages(ctx, stepAgent, execCtx)
		if execCtx.StageMode {
			messages = applyStageDirective(messages, execCtx)
		}

		// Build tools for this agent
		tools, err := e.buildAgentTools(ctx, stepAgent)
		if err == nil && execCtx.StageMode {
			tools = appendStageFinishTool(ctx, e.tools, tools)
		}
		if err != nil {
			callback(StreamEvent{Type: EventError, Error: fmt.Sprintf("build tools: %v", err)})
			return nil, fmt.Errorf("build tools: %w", err)
		}

		// Build LLM request
		llmReq := &LLMRequest{
			Messages:    messages,
			Tools:       tools,
			Model:       currentAgent.Model,
			MaxTokens:   currentAgent.MaxTokens,
			Temperature: currentAgent.Temperature,
		}

		// Emit think event
		callback(StreamEvent{
			Type:      EventThink,
			AgentID:   currentAgent.ID,
			AgentName: currentAgent.Name,
			Step:      step + 1,
		})

		var llmResp *LLMResponse

		if hasStreaming {
			// Use real streaming LLM call. Stage mode always aggregates streamed
			// tool_call deltas: a stage without tool_calls has nothing to gate,
			// which is exactly the gap this loop closes.
			llmResp, err = e.callLLMStreamingMode(ctx, llmReq, callback, currentAgent.ID, currentAgent.Name, step+1, execCtx.StageMode)
		} else {
			// Fallback to non-streaming
			llmResp, err = e.llmClient.Chat(ctx, llmReq)
		}

		if err != nil {
			callback(StreamEvent{Type: EventError, Error: fmt.Sprintf("LLM call failed: %v", err)})
			execCtx.MarkError(err.Error())
			return e.buildResult(execCtx, ""), fmt.Errorf("llm call: %w", err)
		}

		// Update token counts
		execCtx.AddTokens(llmResp.TotalTokens)
		execCtx.AddCost(llmResp.Cost)

		// Check for tool calls
		if len(llmResp.ToolCalls) == 0 {
			// No tool calls — we're done, unless stage mode requires a stage_finish call.
			execCtx.AddMessage("assistant", llmResp.Content)
			if execCtx.StageMode {
				if args, ok := ParseStageFinishProtocol(llmResp.Content); ok {
					// A stage that promised a tool call must actually make one:
					// the harness policy gate only sees real tool_call chunks.
					// Re-prompt a bounded number of times, then fail loudly
					// instead of accepting a finish with no evidence behind it.
					if stageRequiresToolCall(execCtx) && toolCallsMade == 0 {
						if nudges < stageNudgeLimit {
							nudges++
							execCtx.AddMessage("user", stageToolNudgeText(execCtx))
							continue
						}
						execCtx.MarkError(ErrStageToolCallRequired.Error())
						callback(StreamEvent{
							Type:    EventError,
							AgentID: currentAgent.ID,
							Error:   ErrStageToolCallRequired.Error(),
						})
						return e.buildResult(execCtx, llmResp.Content), ErrStageToolCallRequired
					}
					callback(StreamEvent{
						Type: EventToolStart, AgentID: currentAgent.ID, AgentName: currentAgent.Name,
						ToolName: stageFinishTool, ToolArgs: args,
					})
					callback(StreamEvent{
						Type: EventToolResult, AgentID: currentAgent.ID, AgentName: currentAgent.Name,
						ToolName: stageFinishTool, ToolResult: `{"status":"finished"}`,
					})
					return e.buildResult(execCtx, llmResp.Content), nil
				}
				execCtx.MarkError(ErrStageNotFinished.Error())
				callback(StreamEvent{
					Type:    EventError,
					AgentID: currentAgent.ID,
					Error:   ErrStageNotFinished.Error(),
				})
				return e.buildResult(execCtx, llmResp.Content), ErrStageNotFinished
			}
			execCtx.MarkCompleted()

			callback(StreamEvent{
				Type:      EventFinal,
				AgentID:   currentAgent.ID,
				AgentName: currentAgent.Name,
				Content:   llmResp.Content,
				Tokens:    execCtx.TotalTokens,
				Done:      true,
			})

			return e.buildResult(execCtx, llmResp.Content), nil
		}

		// Record the assistant turn before its tool results so the history
		// keeps valid tool_call/tool_result pairing.
		execCtx.AddAssistantToolCallMessage(llmResp.Content, llmResp.ToolCalls)

		// Process tool calls
		for _, tc := range llmResp.ToolCalls {
			// stage_finish closes the stage: emit the call so the harness sees
			// the finish payload on the stream, pair the result in history, and
			// return. Without this branch a streamed stage_finish would be
			// executed as a tool and the loop would run to the step limit.
			if execCtx.StageMode && tc.Name == stageFinishTool {
				callback(StreamEvent{
					Type: EventToolStart, AgentID: currentAgent.ID, AgentName: currentAgent.Name,
					ToolName: stageFinishTool, ToolArgs: tc.Arguments, Step: step + 1,
				})
				if !validStageFinishArgs(tc.Arguments) {
					// Malformed payload: tell the model instead of handing the
					// harness an unparseable finish (which fails the attempt).
					msg := `stage_finish 参数不合法：必须包含 attempt_id、stage(a-d)、summary 三个非空字段`
					callback(StreamEvent{
						Type: EventToolResult, AgentID: currentAgent.ID, AgentName: currentAgent.Name,
						ToolName: stageFinishTool, ToolResult: msg, Step: step + 1,
					})
					execCtx.AddToolMessage(tc.Name, tc.ID, msg)
					tc.Status = "error"
					tc.Result = msg
					execCtx.RecordToolCall(tc)
					continue
				}
				callback(StreamEvent{
					Type: EventToolResult, AgentID: currentAgent.ID, AgentName: currentAgent.Name,
					ToolName: stageFinishTool, ToolResult: `{"status":"finished"}`, Step: step + 1,
				})
				execCtx.AddToolMessage(tc.Name, tc.ID, `{"status":"finished"}`)
				tc.Status = "completed"
				tc.Result = `{"status":"finished"}`
				execCtx.RecordToolCall(tc)
				return e.buildResult(execCtx, llmResp.Content), nil
			}

			// Check if it's a handoff
			if IsHandoffTool(tc.Name) {
				targetID := ParseHandoffTarget(tc.Name)

				// Validate handoff
				if err := ValidateHandoff(currentAgent, targetID, e.registry); err != nil {
					callback(StreamEvent{Type: EventError, Error: err.Error()})
					execCtx.MarkError(err.Error())
					return e.buildResult(execCtx, ""), err
				}

				// Record handoff
				record := AgentExecutionRecord{
					AgentID:     currentAgent.ID,
					AgentName:   currentAgent.Name,
					Thought:     llmResp.Content,
					Action:      "handoff",
					Result:      fmt.Sprintf("Transferred to %s", targetID),
					HandoffTo:   targetID,
					TokensUsed:  llmResp.TotalTokens,
					StartedAt:   stepStart,
					CompletedAt: time.Now(),
					Duration:    time.Since(stepStart).Milliseconds(),
				}
				execCtx.AddAgentRecord(record)

				// Emit handoff event
				callback(StreamEvent{
					Type:      EventHandoff,
					AgentID:   currentAgent.ID,
					AgentName: currentAgent.Name,
					Content:   fmt.Sprintf("Transferring to %s", targetID),
					Step:      step + 1,
				})

				// Switch to target agent
				currentAgent = e.registry.Get(targetID)
				execCtx.SetCurrentAgent(targetID)

				// Save context
				if e.store != nil {
					e.store.Save(ctx, execCtx)
				}

				break
			}

			// ★ Approval check: does this tool require human approval?
			if e.ruleEngine != nil && e.approvalManager != nil {
				needsApproval, matchedRule := e.ruleEngine.NeedsApproval(currentAgent.ID, tc.Name, nil)
				if needsApproval && matchedRule != nil && !matchedRule.AutoApprove {
					// Emit approval pending event
					callback(StreamEvent{
						Type:      EventToolStart,
						AgentID:   currentAgent.ID,
						AgentName: currentAgent.Name,
						ToolName:  tc.Name,
						Content:   fmt.Sprintf("⏳ 等待审批: 工具 %s 需要人工确认 (风险: %s)", tc.Name, matchedRule.RiskThreshold),
						Step:      step + 1,
					})

					approvalReq := &approval.ApprovalRequest{
						Type:        approval.ApprovalTypeToolCall,
						Priority:    approval.PriorityMedium,
						AgentID:     currentAgent.ID,
						SessionID:   execCtx.SessionID,
						Description: fmt.Sprintf("Agent %s requests to execute tool: %s", currentAgent.Name, tc.Name),
						Details: map[string]interface{}{
							"tool_name":  tc.Name,
							"arguments":  string(tc.Arguments),
							"risk_level": matchedRule.RiskThreshold,
						},
						RiskLevel:      matchedRule.RiskThreshold,
						RiskReason:     fmt.Sprintf("Tool %s requires approval (risk: %s)", tc.Name, matchedRule.RiskThreshold),
						TimeoutSeconds: matchedRule.TimeoutSeconds,
						AutoApprove:    false,
					}

					createdReq, err := e.approvalManager.CreateRequest(ctx, approvalReq)
					if err != nil {
						callback(StreamEvent{Type: EventError, Error: fmt.Sprintf("approval request failed: %v", err)})
						execCtx.AddToolMessage(tc.Name, tc.ID, fmt.Sprintf("Approval request failed: %v", err))
						continue
					}

					// Emit an approval event carrying the request id so the
					// frontend can render an in-chat approval prompt and submit
					// a decision via /approval/approve|reject. The engine then
					// blocks on WaitForApproval until that decision arrives.
					callback(StreamEvent{
						Type:      EventApproval,
						AgentID:   currentAgent.ID,
						AgentName: currentAgent.Name,
						ToolName:  tc.Name,
						Step:      step + 1,
						Approval: &ApprovalEvent{
							RequestID: createdReq.ID,
							ToolName:  tc.Name,
							Reason:    matchedRule.RiskThreshold,
						},
					})

					decision, err := e.approvalManager.WaitForApproval(ctx, createdReq.ID)
					if err != nil {
						callback(StreamEvent{Type: EventToolResult, AgentID: currentAgent.ID, AgentName: currentAgent.Name, ToolName: tc.Name, ToolResult: fmt.Sprintf("审批超时/过期: %v", err)})
						execCtx.AddToolMessage(tc.Name, tc.ID, fmt.Sprintf("Approval timeout: %v", err))
						continue
					}

					if decision.Decision != approval.StatusApproved {
						callback(StreamEvent{Type: EventToolResult, AgentID: currentAgent.ID, AgentName: currentAgent.Name, ToolName: tc.Name, ToolResult: fmt.Sprintf("❌ 工具被拒绝: %s", decision.Reason)})
						execCtx.AddToolMessage(tc.Name, tc.ID, fmt.Sprintf("Rejected: %s", decision.Reason))
						record := AgentExecutionRecord{
							AgentID:     currentAgent.ID,
							AgentName:   currentAgent.Name,
							Thought:     llmResp.Content,
							Action:      tc.Name,
							Arguments:   string(tc.Arguments),
							Result:      fmt.Sprintf("Rejected: %s", decision.Reason),
							TokensUsed:  llmResp.TotalTokens,
							StartedAt:   stepStart,
							CompletedAt: time.Now(),
							Duration:    time.Since(stepStart).Milliseconds(),
						}
						execCtx.AddAgentRecord(record)
						continue
					}

					// Approved
					callback(StreamEvent{
						Type:      EventToolResult,
						AgentID:   currentAgent.ID,
						AgentName: currentAgent.Name,
						ToolName:  tc.Name,
						Content:   fmt.Sprintf("✅ 工具 %s 已批准，开始执行", tc.Name),
						Step:      step + 1,
					})
					if decision.ModifiedParams != nil {
						if modifiedArgs, err := json.Marshal(decision.ModifiedParams); err == nil {
							tc.Arguments = modifiedArgs
						}
					}
				}
			}

			// Emit tool start event
			callback(StreamEvent{
				Type:      EventToolStart,
				AgentID:   currentAgent.ID,
				AgentName: currentAgent.Name,
				ToolName:  tc.Name,
				ToolArgs:  tc.Arguments,
				Step:      step + 1,
			})

			// Execute regular tool
			toolStart := time.Now()

			// Get tool config for current agent
			var toolCfg *ToolSpecificConfig
			if currentAgent.ToolConfig != nil {
				if cfg, ok := currentAgent.ToolConfig[tc.Name]; ok {
					toolCfg = &cfg
				}
			}

			// Bind fine-grained browser primitives to the current session so they
			// share one browser page across calls (navigate -> click -> extract).
			// injectSessionID returns a copy; the agent's ToolConfig is untouched.
			if isFineGrainedBrowserTool(tc.Name) {
				toolCfg = injectSessionID(toolCfg, execCtx.SessionID)
			}

			result, err := e.tools.Execute(ctx, tc.Name, tc.Arguments, toolCfg)
			toolDuration := time.Since(toolStart).Milliseconds()

			var recordResult string
			var status string
			if err != nil {
				recordResult = fmt.Sprintf("Error: %v", err)
				status = "error"
			} else {
				recordResult = result
				status = "completed"
			}

			// Emit tool result event
			callback(StreamEvent{
				Type:       EventToolResult,
				AgentID:    currentAgent.ID,
				AgentName:  currentAgent.Name,
				ToolName:   tc.Name,
				ToolResult: recordResult,
				Step:       step + 1,
			})

			// Record tool execution
			tc.Status = status
			tc.Result = result
			execCtx.RecordToolCall(tc)
			toolCallsMade++

			// Add to message history
			execCtx.AddToolMessage(tc.Name, tc.ID, result)

			// Record in agent history
			record := AgentExecutionRecord{
				AgentID:     currentAgent.ID,
				AgentName:   currentAgent.Name,
				Thought:     llmResp.Content,
				Action:      tc.Name,
				Arguments:   string(tc.Arguments),
				Result:      recordResult,
				TokensUsed:  llmResp.TotalTokens,
				StartedAt:   stepStart,
				CompletedAt: time.Now(),
				Duration:    toolDuration,
			}
			execCtx.AddAgentRecord(record)
		}

		// Save context after each step
		if e.store != nil {
			e.store.Save(ctx, execCtx)
		}
	}

	// Max steps reached
	execCtx.MarkError("maximum steps reached")
	callback(StreamEvent{Type: EventError, Error: "maximum steps reached"})
	return e.buildResult(execCtx, "Maximum execution steps reached. Please try a simpler request."), ErrMaxStepsReached
}

// callLLMStreaming calls the LLM with streaming and forwards tokens via callback.
// It collects the full response and returns an LLMResponse just like the non-streaming version.
func (e *Engine) callLLMStreaming(ctx context.Context, req *LLMRequest, callback StreamCallback, agentID, agentName string, step int) (*LLMResponse, error) {
	return e.callLLMStreamingMode(ctx, req, callback, agentID, agentName, step, false)
}

// callLLMStreamingMode is callLLMStreaming with an explicit override for
// tool-call aggregation. Stage mode forces it on so a stage can never silently
// lose the model's tool_call deltas when AGENT_STREAM_TOOL_CALLS is unset.
func (e *Engine) callLLMStreamingMode(ctx context.Context, req *LLMRequest, callback StreamCallback, agentID, agentName string, step int, forceToolCalls bool) (*LLMResponse, error) {
	streamingClient, ok := e.llmClient.(LLMStreamingClient)
	if !ok {
		// Fallback to non-streaming
		return e.llmClient.Chat(ctx, req)
	}

	// Convert LLMRequest to the format expected by the streaming client
	llmReq := &LLMRequest{
		Messages:    req.Messages,
		Tools:       req.Tools,
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}

	ch, err := streamingClient.ChatStream(ctx, llmReq)
	if err != nil {
		return nil, fmt.Errorf("start streaming: %w", err)
	}

	// Collect the full response while forwarding tokens
	var fullContent string
	var totalTokens int
	var usage *TokenUsage
	var toolAcc *streamToolCallAccumulator
	if e.config.StreamToolCalls || forceToolCalls {
		toolAcc = newStreamToolCallAccumulator()
	}

	for chunk := range ch {
		if chunk.Error != nil {
			return nil, chunk.Error
		}
		if chunk.Done {
			break
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.Content != "" {
			fullContent += chunk.Content
			callback(StreamEvent{
				Type:      EventToken,
				AgentID:   agentID,
				AgentName: agentName,
				Content:   chunk.Content,
				Step:      step,
			})
		}
		if toolAcc != nil && chunk.ToolCall != nil {
			toolAcc.add(chunk.ToolCall, chunk.ToolCallIndex)
		}
	}

	var toolCalls []ToolCall
	if toolAcc != nil {
		toolCalls, err = toolAcc.result()
		if err != nil {
			return nil, err
		}
	}

	// Prefer the provider's usage block; the character heuristic is only a
	// fallback for gateways that ignore stream_options.include_usage.
	if usage != nil && usage.TotalTokens > 0 {
		totalTokens = usage.TotalTokens
	} else {
		totalTokens = len(fullContent) / 4
	}

	return &LLMResponse{
		Content:     fullContent,
		ToolCalls:   toolCalls,
		TotalTokens: totalTokens,
		Cost:        0, // cost tracking happens at the service layer
		StopReason:  "stop",
	}, nil
}

// ErrStreamToolCallIncomplete indicates that streamed tool_call deltas could
// not be assembled into a complete, parseable tool call. The engine returns it
// instead of handing a truncated JSON payload to the tool executor.
var ErrStreamToolCallIncomplete = errors.New("stream tool call incomplete")

// streamedToolCall accumulates the deltas of one tool call.
type streamedToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

// streamToolCallAccumulator merges OpenAI-style tool_call deltas. Deltas are
// keyed by ToolCallIndex; id and function.name are taken from the first
// non-empty delta and argument fragments are concatenated in arrival order.
// Arrival order of indices does not matter because the final result is sorted
// by index. A delta with a missing index (zero value) is appended to index 0,
// except when it carries a different non-empty id, which starts a new call.
type streamToolCallAccumulator struct {
	byIndex  map[int]*streamedToolCall
	order    []int
	maxIndex int
}

func newStreamToolCallAccumulator() *streamToolCallAccumulator {
	return &streamToolCallAccumulator{byIndex: make(map[int]*streamedToolCall)}
}

func (a *streamToolCallAccumulator) add(delta *ToolCall, index int) {
	if delta == nil {
		return
	}
	idx := a.resolveIndex(delta, index)
	state, ok := a.byIndex[idx]
	if !ok {
		state = &streamedToolCall{}
		a.byIndex[idx] = state
		a.order = append(a.order, idx)
		if idx > a.maxIndex {
			a.maxIndex = idx
		}
	}
	if state.id == "" {
		state.id = delta.ID
	}
	if state.name == "" {
		state.name = delta.Name
	}
	state.arguments.WriteString(string(delta.Arguments))
}

// resolveIndex maps a zero index to the call it belongs to. Gateways that omit
// the index field send zero for every delta, which is indistinguishable from a
// real index 0. A different non-empty id means a new parallel call.
func (a *streamToolCallAccumulator) resolveIndex(delta *ToolCall, index int) int {
	if index > 0 {
		return index
	}
	if delta.ID != "" {
		if state, ok := a.byIndex[0]; ok && state.id != "" && state.id != delta.ID {
			return a.maxIndex + 1
		}
	}
	return 0
}

// result validates and returns the aggregated calls ordered by index. It fails
// loudly rather than returning a partially assembled JSON payload.
func (a *streamToolCallAccumulator) result() ([]ToolCall, error) {
	if len(a.order) == 0 {
		return nil, nil
	}
	indices := append([]int(nil), a.order...)
	sort.Ints(indices)
	calls := make([]ToolCall, 0, len(indices))
	for _, idx := range indices {
		state := a.byIndex[idx]
		args := strings.TrimSpace(state.arguments.String())
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return nil, fmt.Errorf("%w: index %d arguments are not valid JSON", ErrStreamToolCallIncomplete, idx)
		}
		if state.name == "" {
			return nil, fmt.Errorf("%w: index %d has arguments but no function name", ErrStreamToolCallIncomplete, idx)
		}
		id := state.id
		if id == "" {
			id = fmt.Sprintf("call_stream_%d", idx)
		}
		calls = append(calls, ToolCall{
			ID:        id,
			Name:      state.name,
			Arguments: json.RawMessage(args),
			Status:    "pending",
		})
	}
	return calls, nil
}

// Stage-loop helpers. The harness loop publishes the stage policy as context
// variables; these helpers turn that into (a) the tool subset offered to the
// model and (b) prompt text that makes the tool loop, not the text protocol,
// the primary way a stage finishes.

// stageToolLabel maps an MCP tool name onto the name the harness policy and the
// stage matrix use, so the model sees the same vocabulary the gate enforces.
func stageToolLabel(name string) string {
	switch name {
	case "file_read":
		return "read_file"
	case "file_write":
		return "write_file"
	case "code_execute":
		return "run_command"
	}
	return name
}

// stageAgentOverlay returns a per-step copy of the agent narrowed to the tools
// the current stage may use. Handoffs are dropped: one stage is one agent's job,
// and a handoff emits no tool_call chunk for the policy gate to decide on.
func stageAgentOverlay(base *Agent, execCtx *ExecutionContext) *Agent {
	if base == nil {
		return base
	}
	overlay := *base
	overlay.Handoffs = nil

	allowed := stageToolNames(execCtx)
	if len(allowed) == 0 {
		return &overlay
	}

	seen := make(map[string]bool, len(base.Tools)+len(allowed))
	merged := make([]string, 0, len(base.Tools)+len(allowed))
	for _, name := range base.Tools {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		merged = append(merged, name)
	}
	for _, name := range allowed {
		if seen[name] {
			continue
		}
		seen[name] = true
		merged = append(merged, name)
	}
	overlay.Tools = merged
	return &overlay
}

// stageToolNames decodes the stage tool subset published by the harness loop.
func stageToolNames(execCtx *ExecutionContext) []string {
	raw, ok := execCtx.Variables[StageToolListVar]
	if !ok {
		return nil
	}
	list, ok := raw.(string)
	if !ok || strings.TrimSpace(list) == "" {
		return nil
	}
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// stageRequiresToolCall reports whether this stage may not finish without a
// real tool call.
func stageRequiresToolCall(execCtx *ExecutionContext) bool {
	raw, ok := execCtx.Variables[StageRequiresToolCallVar]
	if !ok {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// stageLetter returns the current stage letter (a-d), or "" when unknown.
func stageLetter(execCtx *ExecutionContext) string {
	raw, ok := execCtx.Variables["__stage__"]
	if !ok {
		return ""
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// stageDirectiveText is appended to the system prompt. System-level guidance is
// what the model actually follows (a user message alone is often ignored).
func stageDirectiveText(execCtx *ExecutionContext) string {
	var sb strings.Builder
	sb.WriteString("STAGE TOOL LOOP: this stage is only finished through the stage_finish tool call after you have used at least one tool.")
	if names := stageToolNames(execCtx); len(names) > 0 {
		labels := make([]string, 0, len(names))
		for _, name := range names {
			labels = append(labels, fmt.Sprintf("%s (%s)", stageToolLabel(name), name))
		}
		sb.WriteString("\nTools available in this stage: " + strings.Join(labels, ", ") + ".")
		sb.WriteString("\nCall them with the arguments their schema declares before drawing any conclusion; do not describe what you would do instead of doing it.")
	} else {
		sb.WriteString("\nNo workspace tool is granted for this stage: answer from the stage message and finish with stage_finish.")
	}
	if stageRequiresToolCall(execCtx) {
		sb.WriteString("\nThis stage must contain at least one real tool call; a finish without one is rejected and retried.")
	}
	sb.WriteString("\nIf the stage message asks for a machine-readable output (e.g. the b-stage SubtaskPlan JSON), that output must appear verbatim in the assistant content: stage_finish only closes the stage, it does not carry the deliverable.")
	if stage := stageLetter(execCtx); stage != "" {
		sb.WriteString("\nCurrent stage: " + stage + ".")
	}
	return sb.String()
}

// applyStageDirective rewrites the stage-mode guidance in the outgoing message
// list: the directive is appended to the system prompt, and the trailing
// text-only reminder built by buildAgentMessages is replaced with a tool-first
// one. The replace (not append) keeps a single, unambiguous instruction last.
func applyStageDirective(messages []Message, execCtx *ExecutionContext) []Message {
	if len(messages) == 0 {
		return messages
	}
	for i := range messages {
		if messages[i].Role == "system" {
			messages[i].Content += "\n\n" + stageDirectiveText(execCtx)
			break
		}
	}
	last := len(messages) - 1
	if messages[last].Role == "user" && strings.Contains(messages[last].Content, StageFinishMarker) {
		messages[last] = Message{Role: "user", Content: stageReminderText(execCtx)}
	}
	return messages
}

// stageReminderText replaces the legacy "output one line only" reminder, which
// pushed the model away from tool calls and straight into the text protocol.
func stageReminderText(execCtx *ExecutionContext) string {
	var sb strings.Builder
	sb.WriteString("结束本阶段前必须真实调用工具取得证据")
	if names := stageToolNames(execCtx); len(names) > 0 {
		labels := make([]string, 0, len(names))
		for _, name := range names {
			labels = append(labels, fmt.Sprintf("%s(%s)", stageToolLabel(name), name))
		}
		sb.WriteString("（本阶段可用：" + strings.Join(labels, "、") + "）")
	}
	sb.WriteString("，然后调用 stage_finish 工具结束：\n")
	sb.WriteString(`{"attempt_id":"<stage message 中的 attempt_id>","stage":"<a|b|c|d>","summary":"<一句话概括>"}`)
	sb.WriteString("\n若本阶段要求正文产出（例如 b 阶段的 SubtaskPlan JSON），必须先把该产出原样写在正文里，" +
		"再调用 stage_finish；summary 不能代替正文产出。")
	sb.WriteString("只有在确实无法发出工具调用时，才允许退回一行 " + StageFinishMarker + " 加同样的 JSON；" +
		"只输出自然语言、不带任何结束信号会被视为未完成并重试。")
	return sb.String()
}

// stageToolNudgeText is injected as a user turn when the model tries to finish
// a stage without having called any tool.
func stageToolNudgeText(execCtx *ExecutionContext) string {
	msg := "你还没有调用任何工具。本阶段必须先真实调用工具"
	if names := stageToolNames(execCtx); len(names) > 0 {
		labels := make([]string, 0, len(names))
		for _, name := range names {
			labels = append(labels, fmt.Sprintf("%s(%s)", stageToolLabel(name), name))
		}
		msg += "（可用：" + strings.Join(labels, "、") + "）"
	}
	return msg + "，再调用 stage_finish 结束。"
}

// validStageFinishArgs checks a streamed stage_finish payload before the engine
// accepts it, so a typo surfaces as a tool error the model can fix instead of an
// unparseable finish that fails the whole attempt.
func validStageFinishArgs(args json.RawMessage) bool {
	var payload stageFinishProtocol
	if err := json.Unmarshal(args, &payload); err != nil {
		return false
	}
	payload.Stage = strings.ToLower(strings.TrimSpace(payload.Stage))
	if strings.TrimSpace(payload.AttemptID) == "" || strings.TrimSpace(payload.Summary) == "" {
		return false
	}
	return len(payload.Stage) == 1 && strings.Contains("abcde", payload.Stage)
}
