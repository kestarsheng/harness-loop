package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"agent-platform/pkg/agent"
	"agent-platform/pkg/harnessloop/policy"
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

// agentApprovalWaitEnv 是 agent-service 给执行面的等待预算：批准是异步的，
// 执行面在预算内等一枚 harnessloop 签发的一次性令牌。
const agentApprovalWaitEnv = "AGENT_APPROVAL_WAIT_SECONDS"

func agentApprovalWaitSeconds() int {
	const fallback = 180
	if raw := strings.TrimSpace(os.Getenv(agentApprovalWaitEnv)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			return parsed
		}
	}
	return fallback
}

// annotateApprovalBinding 给 exec 调用补上审批绑定。
//
// agent-service 不持有签发密钥，也不替执行面做判定：它只把「这次调用是哪
// 一个任务、哪个工具、哪份参数摘要」原样传给 mcp-service，并给出等待预算。
// 令牌本身由 harnessloop 直接注册到 mcp；这里透传的是绑定指纹。
func annotateApprovalBinding(cfg *agent.ToolSpecificConfig, toolName string, arguments json.RawMessage) {
	if cfg == nil {
		return
	}
	harnessTool := harnessToolName(toolName)
	if harnessTool != "run_command" {
		return
	}
	section, ok := cfg.Extra["__policy_context__"].(map[string]any)
	if !ok {
		return
	}
	taskID, _ := section["task_id"].(string)
	if strings.TrimSpace(taskID) == "" {
		return
	}
	digest := policy.ArgumentsDigest(policy.CanonicalArgsFromJSON(arguments))
	section["approval_ref"] = policy.ApprovalRef(taskID, harnessTool, digest)
	section["approval_wait_seconds"] = agentApprovalWaitSeconds()
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
