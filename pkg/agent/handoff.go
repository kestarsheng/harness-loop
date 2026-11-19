package agent

import (
	"fmt"
	"strings"
)

// HandoffPrefix is the prefix for handoff tool names
const HandoffPrefix = "transfer_to_"

// HandoffTool represents a handoff tool definition
type HandoffTool struct {
	TargetAgentID   string `json:"target_agent_id"`
	TargetAgentName string `json:"target_agent_name"`
	Description     string `json:"description"`
}

// NewHandoffTool creates a handoff tool for an agent
func NewHandoffTool(targetAgent *Agent) HandoffTool {
	return HandoffTool{
		TargetAgentID:   targetAgent.ID,
		TargetAgentName: targetAgent.Name,
		Description:     fmt.Sprintf("Transfer the conversation to %s. %s", targetAgent.Name, targetAgent.Description),
	}
}

// ToolName returns the tool name for this handoff
func (h HandoffTool) ToolName() string {
	return HandoffPrefix + h.TargetAgentID
}

// ToToolDefinition converts to LLM tool definition format
func (h HandoffTool) ToToolDefinition() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        h.ToolName(),
			"description": h.Description,
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
	}
}

// IsHandoffTool checks if a tool name is a handoff tool
func IsHandoffTool(toolName string) bool {
	return strings.HasPrefix(toolName, HandoffPrefix)
}

// ParseHandoffTarget extracts the target agent ID from a handoff tool name
func ParseHandoffTarget(toolName string) string {
	if !IsHandoffTool(toolName) {
		return ""
	}
	return strings.TrimPrefix(toolName, HandoffPrefix)
}

// HandoffResult represents the result of a handoff operation
type HandoffResult struct {
	// Type indicates the result type
	Type string `json:"type"` // "handoff", "response", "tool_call"

	// TargetAgent is the agent being handed off to
	TargetAgent string `json:"target_agent,omitempty"`

	// Content is the response content
	Content string `json:"content,omitempty"`

	// Variables to update in context
	Variables map[string]any `json:"variables,omitempty"`

	// Reason for the handoff
	Reason string `json:"reason,omitempty"`
}

// NewHandoffResult creates a handoff result
func NewHandoffResult(targetAgentID string) *HandoffResult {
	return &HandoffResult{
		Type:        "handoff",
		TargetAgent: targetAgentID,
		Variables:   make(map[string]any),
	}
}

// NewResponseResult creates a response result
func NewResponseResult(content string) *HandoffResult {
	return &HandoffResult{
		Type:      "response",
		Content:   content,
		Variables: make(map[string]any),
	}
}

// WithReason sets the reason for handoff
func (r *HandoffResult) WithReason(reason string) *HandoffResult {
	r.Reason = reason
	return r
}

// WithVariable sets a variable
func (r *HandoffResult) WithVariable(key string, value any) *HandoffResult {
	if r.Variables == nil {
		r.Variables = make(map[string]any)
	}
	r.Variables[key] = value
	return r
}

// BuildHandoffTools builds handoff tools for an agent.
//
// A handoff tool is only created for a target that (a) exists in the registry
// and (b) is not the agent itself. The self check is the hard guard against the
// regression where a misconfigured agent listing its own ID in Handoffs ended up
// with a transfer_to_<self> tool, then rejected its own call at validation time.
func BuildHandoffTools(agent *Agent, registry *Registry) []HandoffTool {
	if agent == nil {
		return nil
	}
	tools := make([]HandoffTool, 0, len(agent.Handoffs))
	seen := make(map[string]bool, len(agent.Handoffs))

	for _, targetID := range agent.Handoffs {
		if targetID == "" || targetID == agent.ID || seen[targetID] {
			continue
		}
		targetAgent := registry.Get(targetID)
		if targetAgent == nil {
			continue
		}
		seen[targetID] = true
		tools = append(tools, NewHandoffTool(targetAgent))
	}

	return tools
}

// BuildAllTools builds all tools for an agent (regular tools + handoff tools).
// Handoff tools come from BuildHandoffTools so the self-handoff exclusion is
// shared rather than duplicated at each call site.
func BuildAllTools(agent *Agent, registry *Registry, toolDefinitions map[string]any) []map[string]any {
	tools := make([]map[string]any, 0)
	seen := make(map[string]bool)

	// Add regular tools
	if agent != nil {
		for _, toolName := range agent.Tools {
			if seen[toolName] {
				continue
			}
			if def, ok := toolDefinitions[toolName]; ok {
				tools = append(tools, def.(map[string]any))
				seen[toolName] = true
			}
		}
	}

	// Add handoff tools
	for _, ht := range BuildHandoffTools(agent, registry) {
		name := ht.ToolName()
		if seen[name] {
			continue
		}
		tools = append(tools, ht.ToToolDefinition())
		seen[name] = true
	}

	return tools
}

// HandoffError is the single error type for a rejected handoff. The message is
// always stable and always carries both sides so logs read
// "agent A cannot handoff to B: <reason>".
type HandoffError struct {
	From   string
	To     string
	Reason string
}

func (e *HandoffError) Error() string {
	from, to := e.From, e.To
	if from == "" {
		from = "<unknown>"
	}
	if to == "" {
		to = "<unknown>"
	}
	if e.Reason == "" {
		return fmt.Sprintf("agent %s cannot handoff to %s", from, to)
	}
	return fmt.Sprintf("agent %s cannot handoff to %s: %s", from, to, e.Reason)
}

// Unwrap lets errors.Is(err, ErrInvalidHandoff) keep working for callers that
// only care about the class of failure.
func (e *HandoffError) Unwrap() error { return ErrInvalidHandoff }

func newHandoffError(from, to, reason string) error {
	return &HandoffError{From: from, To: to, Reason: reason}
}

// MaxHandoffHopsDefault bounds one handoff chain (A→B→C→D). It exists as a
// last-resort stop for graphs the visited-set check cannot catch.
const MaxHandoffHopsDefault = 3

// HandoffGuard tracks the agents visited during one execution so that cycles
// (A→B→A) and self handoffs (B→B) are rejected before any agent switch.
type HandoffGuard struct {
	maxHops int
	current string
	hops    int
	visited map[string]bool
}

// NewHandoffGuard creates a guard seeded with the entry agent.
func NewHandoffGuard(startAgentID string, maxHops int) *HandoffGuard {
	if maxHops <= 0 {
		maxHops = MaxHandoffHopsDefault
	}
	g := &HandoffGuard{
		maxHops: maxHops,
		current: startAgentID,
		visited: make(map[string]bool, maxHops+1),
	}
	if startAgentID != "" {
		g.visited[startAgentID] = true
	}
	return g
}

// NewHandoffGuardFromContext rebuilds a guard from an execution context, so a
// resumed run cannot walk back into an agent the original run already visited.
// Both sides of every recorded handoff are marked visited; the currently
// running agent is then pinned as the guard's current position.
func NewHandoffGuardFromContext(execCtx *ExecutionContext, startAgentID string, maxHops int) *HandoffGuard {
	g := NewHandoffGuard(startAgentID, maxHops)
	if execCtx == nil {
		return g
	}
	for _, rec := range execCtx.AgentHistory {
		if rec.HandoffTo == "" {
			continue
		}
		if rec.AgentID != "" {
			g.visited[rec.AgentID] = true
		}
		g.Record(rec.HandoffTo)
	}
	if startAgentID != "" {
		g.current = startAgentID
	}
	return g
}

// Current returns the agent the guard considers current.
func (g *HandoffGuard) Current() string {
	if g == nil {
		return ""
	}
	return g.current
}

// Hops returns the number of handoffs already taken.
func (g *HandoffGuard) Hops() int {
	if g == nil {
		return 0
	}
	return g.hops
}

// Check rejects a handoff to toAgentID before it is executed.
func (g *HandoffGuard) Check(toAgentID string) error {
	from := g.Current()
	switch {
	case from != "" && from == toAgentID:
		return newHandoffError(from, toAgentID, "self handoff is not allowed")
	case g.visited[toAgentID]:
		return newHandoffError(from, toAgentID, "cycle detected: agent already visited in this execution")
	case g.Hops() >= g.maxHops:
		return newHandoffError(from, toAgentID, fmt.Sprintf("handoff hop limit exceeded (max %d)", g.maxHops))
	}
	return nil
}

// Record marks a completed handoff to toAgentID and advances the guard.
func (g *HandoffGuard) Record(toAgentID string) {
	if g == nil || toAgentID == "" {
		return
	}
	if !g.visited[toAgentID] {
		g.visited[toAgentID] = true
		g.hops++
	}
	g.current = toAgentID
}

// ValidateHandoff validates a handoff request statically (existence, self,
// declared target). Errors are always *HandoffError with from/to attached.
func ValidateHandoff(fromAgent *Agent, toAgentID string, registry *Registry) error {
	fromID := ""
	if fromAgent != nil {
		fromID = fromAgent.ID
	}
	if fromAgent == nil {
		return newHandoffError(fromID, toAgentID, "source agent is nil")
	}
	if toAgentID == "" {
		return newHandoffError(fromID, toAgentID, "target agent is empty")
	}
	// from == to is always rejected, even when the agent lists itself in
	// Handoffs: a self handoff can never make progress and loops forever.
	if fromID == toAgentID {
		return newHandoffError(fromID, toAgentID, "self handoff is not allowed")
	}
	if registry == nil || !registry.Exists(toAgentID) {
		return newHandoffError(fromID, toAgentID, "target agent not found")
	}
	if !fromAgent.CanHandoffTo(toAgentID) {
		return newHandoffError(fromID, toAgentID, "target agent is not declared in source agent handoffs")
	}
	return nil
}

// ValidateHandoffStep validates a handoff statically and then against the
// visited set / hop budget of the current execution.
func ValidateHandoffStep(guard *HandoffGuard, fromAgent *Agent, toAgentID string, registry *Registry) error {
	if err := ValidateHandoff(fromAgent, toAgentID, registry); err != nil {
		return err
	}
	if guard != nil {
		if err := guard.Check(toAgentID); err != nil {
			return err
		}
	}
	return nil
}
