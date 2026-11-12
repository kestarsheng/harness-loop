package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"agent-platform/pkg/agent"
)

// toolPolicyContext is the per-request policy context forwarded from the
// harness loop to the MCP layer. It is carried on the Go context and merged
// into CallToolRequest.tool_config by toolAdapter.
type toolPolicyContext struct {
	Stage         string
	TenantID      string
	ProjectID     string
	TaskID        string
	AgentID       string
	WorkspaceRoot string
}

type toolPolicyContextKey struct{}

func toolPolicyContextFromVars(vars map[string]any, tenantID string) toolPolicyContext {
	pc := toolPolicyContext{
		Stage:         stringContextVar(vars, "__stage__"),
		ProjectID:     stringContextVar(vars, "__project_id__"),
		TaskID:        stringContextVar(vars, "__task_id__"),
		AgentID:       stringContextVar(vars, "__agent_id__"),
		WorkspaceRoot: stringContextVar(vars, "__workspace_root__"),
		TenantID:      firstNonEmptyString(tenantID, stringContextVar(vars, "__tenant_id__")),
	}
	return pc
}

func (pc toolPolicyContext) present() bool {
	return strings.TrimSpace(pc.Stage) != ""
}

func withToolPolicyContext(ctx context.Context, pc toolPolicyContext) context.Context {
	if !pc.present() {
		return ctx
	}
	return context.WithValue(ctx, toolPolicyContextKey{}, pc)
}

func toolPolicyContextFrom(ctx context.Context) (toolPolicyContext, bool) {
	pc, ok := ctx.Value(toolPolicyContextKey{}).(toolPolicyContext)
	if !ok || !pc.present() {
		return toolPolicyContext{}, false
	}
	return pc, true
}

// mergeToolPolicyContext clones the tool config and injects the reserved
// policy context, so concurrent runs never mutate an agent's shared config.
func mergeToolPolicyContext(cfg *agent.ToolSpecificConfig, pc toolPolicyContext) *agent.ToolSpecificConfig {
	var out agent.ToolSpecificConfig
	if cfg != nil {
		out = *cfg
	}
	extra := make(map[string]any, len(out.Extra)+1)
	for k, v := range out.Extra {
		extra[k] = v
	}
	extra["__policy_context__"] = map[string]any{
		"stage":          pc.Stage,
		"tenant_id":      pc.TenantID,
		"project_id":     pc.ProjectID,
		"task_id":        pc.TaskID,
		"agent_id":       pc.AgentID,
		"workspace_root": pc.WorkspaceRoot,
	}
	out.Extra = extra
	return &out
}

func stringContextVar(vars map[string]any, key string) string {
	value, ok := vars[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return fmt.Sprintf("%v", typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
