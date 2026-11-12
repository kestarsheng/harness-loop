package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"agent-platform/pkg/harnessloop/policy"
	pb "agent-platform/pkg/pb/mcp"
)

// ToolPolicyContext is carried from agent-service through CallToolRequest's
// tool_config. It gives the MCP layer the stage/task/workspace facts that the
// proto request does not have.
type ToolPolicyContext struct {
	Stage         string `json:"stage,omitempty"`
	TenantID      string `json:"tenant_id,omitempty"`
	ProjectID     string `json:"project_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	WorkspaceRoot string `json:"workspace_root,omitempty"`
}

// ToolPolicyInput is the normalised call facts used by the MCP-level policy
// gate.
type ToolPolicyInput struct {
	Tool          string
	Stage         string
	TenantID      string
	ProjectID     string
	TaskID        string
	AgentID       string
	WorkspaceRoot string
	Paths         []string
	Command       []string
	Tags          []string
}

// ToolPolicyResult is the decision returned to CallTool.
type ToolPolicyResult struct {
	Allowed bool
	Code    string
	RuleID  string
	Layer   policy.Layer
	Reason  string
}

// ToolPolicy is the single MCP-side enforcement point. Implementations must be
// fail-closed: an error or malformed context never allows the tool call.
type ToolPolicy interface {
	Authorize(ctx context.Context, in ToolPolicyInput) ToolPolicyResult
}

// HarnessToolPolicy evaluates the same policy.Engine rules as the harness
// loop. Calls without a stage belong to non-harness callers (chat/gateway) and
// keep the legacy behaviour.
type HarnessToolPolicy struct {
	engine     *policy.Engine
	mu         sync.RWMutex
	registered map[string][]string
}

func NewHarnessToolPolicy(engine *policy.Engine) *HarnessToolPolicy {
	if engine == nil {
		engine = policy.NewEngine()
	}
	if engine.Registry == nil {
		engine.Registry = policy.NewRegistry()
	}
	return &HarnessToolPolicy{engine: engine, registered: make(map[string][]string)}
}

func (p *HarnessToolPolicy) Authorize(ctx context.Context, in ToolPolicyInput) ToolPolicyResult {
	if p == nil || p.engine == nil {
		return ToolPolicyResult{Allowed: false, Code: policy.CodeToolDenied, Reason: "tool policy is not configured"}
	}
	if strings.TrimSpace(in.Stage) == "" {
		return ToolPolicyResult{Allowed: true, Reason: "non-stage caller"}
	}

	tags := normalizePermissionTags(in.Tags)
	toolName := harnessToolName(in.Tool, tags)
	p.register(toolName, tags)

	decision := p.engine.Decide(ctx, policy.Request{
		Tool:          toolName,
		Stage:         in.Stage,
		WorkspaceRoot: in.WorkspaceRoot,
		Args:          policy.ToolArgs{Paths: in.Paths, Command: in.Command},
		TenantID:      in.TenantID,
		ProjectID:     in.ProjectID,
		TaskID:        in.TaskID,
		AgentID:       in.AgentID,
		RoleName:      in.AgentID,
	})
	if decision.Allowed {
		return ToolPolicyResult{Allowed: true, Code: decision.Code, RuleID: decision.RuleID, Layer: decision.Layer, Reason: decision.Reason}
	}
	return ToolPolicyResult{
		Allowed: false,
		Code:    firstNonEmpty(decision.Code, policy.CodeToolDenied),
		RuleID:  decision.RuleID,
		Layer:   decision.Layer,
		Reason:  decision.Reason,
	}
}

func (p *HarnessToolPolicy) register(tool string, tags []string) {
	p.mu.RLock()
	existing, ok := p.registered[tool]
	p.mu.RUnlock()
	if ok && equalStrings(existing, tags) {
		return
	}
	if err := p.engine.Registry.Register(tool, tags); err != nil {
		return
	}
	p.mu.Lock()
	p.registered[tool] = append([]string(nil), tags...)
	p.mu.Unlock()
}

// normalizePermissionTags maps MCP permission tags onto harness policy tags.
func normalizePermissionTags(tags []string) []string {
	set := make(map[string]bool, len(tags))
	for _, tag := range tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "read", "fs.read":
			set[policy.TagRead] = true
		case "write", "fs.write":
			set[policy.TagWrite] = true
		case "net.write":
			set[policy.TagWrite] = true
			set[policy.TagNetwork] = true
		case "exec":
			set[policy.TagExec] = true
		case "network", "net.read":
			set[policy.TagNetwork] = true
		}
	}
	out := make([]string, 0, len(set))
	for _, tag := range []string{policy.TagRead, policy.TagWrite, policy.TagExec, policy.TagNetwork} {
		if set[tag] {
			out = append(out, tag)
		}
	}
	return out
}

// harnessToolName maps MCP tool names onto the stage matrix vocabulary.
func harnessToolName(tool string, tags []string) string {
	switch tool {
	case "file_read":
		return "read_file"
	case "file_write":
		return "write_file"
	case "code_execute":
		return "run_command"
	}
	switch {
	case containsTag(tags, policy.TagWrite):
		return "write_file"
	case containsTag(tags, policy.TagExec):
		return "run_command"
	case containsTag(tags, policy.TagRead):
		return "read_file"
	}
	return tool
}

func containsTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// policyContextFromToolConfig extracts the reserved policy context from the
// CallToolRequest.tool_config JSON. Both a top-level and an extra-nested key
// are accepted so callers do not depend on one encoding.
func policyContextFromToolConfig(raw string) ToolPolicyContext {
	if strings.TrimSpace(raw) == "" {
		return ToolPolicyContext{}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return ToolPolicyContext{}
	}
	if ctxRaw, ok := top["__policy_context__"]; ok {
		var pc ToolPolicyContext
		if err := json.Unmarshal(ctxRaw, &pc); err == nil {
			return pc
		}
	}
	if extraRaw, ok := top["extra"]; ok {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(extraRaw, &extra); err == nil {
			if ctxRaw, ok := extra["__policy_context__"]; ok {
				var pc ToolPolicyContext
				if err := json.Unmarshal(ctxRaw, &pc); err == nil {
					return pc
				}
			}
		}
	}
	return ToolPolicyContext{}
}

func (s *MCPService) SetToolPolicy(p ToolPolicy) {
	s.mu.Lock()
	s.toolPolicy = p
	s.mu.Unlock()
}

func (s *MCPService) toolPolicyInput(req *pb.CallToolRequest) ToolPolicyInput {
	pc := policyContextFromToolConfig(req.GetToolConfig())
	if pc.TenantID == "" {
		pc.TenantID = req.GetTenantId()
	}
	paths, command := parseToolArguments(req.GetArguments())
	return ToolPolicyInput{
		Tool:          req.GetName(),
		Stage:         pc.Stage,
		TenantID:      pc.TenantID,
		ProjectID:     pc.ProjectID,
		TaskID:        pc.TaskID,
		AgentID:       pc.AgentID,
		WorkspaceRoot: pc.WorkspaceRoot,
		Paths:         paths,
		Command:       command,
		Tags:          s.permissionTagsFor(req.GetName()),
	}
}

func (s *MCPService) permissionTagsFor(tool string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.tools[tool]; ok {
		return append([]string(nil), t.PermissionTags...)
	}
	if entry, ok := s.remoteTools[tool]; ok {
		return append([]string(nil), entry.Tool.PermissionTags...)
	}
	return nil
}

func parseToolArguments(raw string) ([]string, []string) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, nil
	}
	return argumentList(args, "paths", "path", "file_path", "files"),
		argumentList(args, "command", "cmd", "argv")
}

func argumentList(args map[string]any, keys ...string) []string {
	for _, key := range keys {
		value, ok := args[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if typed != "" {
				return []string{typed}
			}
		case []any:
			out := make([]string, 0, len(typed))
			for _, item := range typed {
				if s, ok := item.(string); ok && s != "" {
					out = append(out, s)
				}
			}
			return out
		case []string:
			return typed
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
