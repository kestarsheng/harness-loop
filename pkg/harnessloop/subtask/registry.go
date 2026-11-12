package subtask

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agent-platform/pkg/harnessloop/policy"
)

// Error codes returned by the role/tool registry.
const (
	CodeAgentRoleUndeclared = "E_AGENT_ROLE_UNDECLARED"
	CodeToolNotPermitted    = "E_TOOL_NOT_PERMITTED"
	CodeToolTagMissing      = "E_TOOL_TAG_MISSING"
	CodeUnknownStage        = "E_UNKNOWN_STAGE"
)

// CodedError carries a stable error code so callers and tests can assert on it.
type CodedError struct {
	Code   string
	Detail string
}

func (e *CodedError) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

func coded(code, format string, args ...any) error {
	return &CodedError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// AgentCatalog queries the agent-service for declared roles and tools. It is
// remote by contract: the registry never persists a local copy.
type AgentCatalog interface {
	AgentRoles(ctx context.Context, agentID string) ([]string, error)
	AgentTools(ctx context.Context, agentID string) ([]string, error)
}

// ToolTagProvider returns the permission tags declared for a tool. An empty
// result means the tool is untagged and must be rejected.
type ToolTagProvider interface {
	ToolPermissionTags(ctx context.Context, tool string) ([]string, error)
}

// RoleBinding is the resolved capability set of one agent under one stage.
type RoleBinding struct {
	AgentID       string
	Roles         []string
	DeclaredTools []string
	GrantedTools  []string
	DeniedTools   []string
	MissingTags   []string
}

// Registry resolves agent roles against the stage tool matrix.
type Registry struct {
	catalog AgentCatalog
	matrix  policy.Matrix
	tags    ToolTagProvider
}

// NewRegistry builds a registry. tags may be nil, in which case tag validation
// is skipped.
func NewRegistry(catalog AgentCatalog, matrix policy.Matrix, tags ToolTagProvider) *Registry {
	return &Registry{catalog: catalog, matrix: matrix, tags: tags}
}

// Resolve returns the tools an agent may use in the given stage. Tools outside
// the stage matrix are reported as denied; tools without permission tags are
// reported separately and never granted.
func (r *Registry) Resolve(ctx context.Context, agentID, stage string) (*RoleBinding, error) {
	if r == nil || r.catalog == nil {
		return nil, coded(CodeAgentRoleUndeclared, "agent catalog is not configured")
	}
	if strings.TrimSpace(agentID) == "" {
		return nil, coded(CodeAgentRoleUndeclared, "agent id is empty")
	}

	roles, err := r.catalog.AgentRoles(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("resolve roles for %s: %w", agentID, err)
	}
	if len(roles) == 0 {
		return nil, coded(CodeAgentRoleUndeclared, "agent %s declares no role", agentID)
	}

	tools, err := r.catalog.AgentTools(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("resolve tools for %s: %w", agentID, err)
	}

	rule, ok := r.matrix.Rule(stage)
	if !ok {
		return nil, coded(CodeUnknownStage, "stage %q is not defined in the tool matrix", stage)
	}

	binding := &RoleBinding{
		AgentID:       agentID,
		Roles:         dedupe(roles),
		DeclaredTools: dedupe(tools),
	}

	if rule.DenyAll {
		binding.DeniedTools = append(binding.DeniedTools, binding.DeclaredTools...)
		return binding, nil
	}

	allowed := make(map[string]struct{}, len(rule.Read)+len(rule.Write)+len(rule.Exec))
	for _, list := range [][]string{rule.Read, rule.Write, rule.Exec} {
		for _, name := range list {
			allowed[name] = struct{}{}
		}
	}

	for _, tool := range binding.DeclaredTools {
		if _, ok := allowed[tool]; !ok {
			binding.DeniedTools = append(binding.DeniedTools, tool)
			continue
		}
		if r.tags != nil {
			tags, err := r.tags.ToolPermissionTags(ctx, tool)
			if err != nil {
				return nil, fmt.Errorf("resolve permission tags for %s: %w", tool, err)
			}
			if len(tags) == 0 {
				binding.MissingTags = append(binding.MissingTags, tool)
				continue
			}
		}
		binding.GrantedTools = append(binding.GrantedTools, tool)
	}

	sort.Strings(binding.DeniedTools)
	sort.Strings(binding.MissingTags)
	return binding, nil
}

// CheckTool rejects a tool that the agent did not declare or the stage forbids.
func (r *Registry) CheckTool(ctx context.Context, agentID, stage, tool string) error {
	binding, err := r.Resolve(ctx, agentID, stage)
	if err != nil {
		return err
	}
	for _, granted := range binding.GrantedTools {
		if granted == tool {
			return nil
		}
	}
	for _, missing := range binding.MissingTags {
		if missing == tool {
			return coded(CodeToolTagMissing, "tool %q declares no permission tags", tool)
		}
	}
	return coded(CodeToolNotPermitted, "tool %q is not permitted for agent %s in stage %s", tool, agentID, stage)
}

// ValidateToolTags rejects tools that are registered without permission tags.
func (r *Registry) ValidateToolTags(ctx context.Context, tool string) error {
	if r == nil || r.tags == nil {
		return nil
	}
	tags, err := r.tags.ToolPermissionTags(ctx, tool)
	if err != nil {
		return fmt.Errorf("validate permission tags for %s: %w", tool, err)
	}
	if len(tags) == 0 {
		return coded(CodeToolTagMissing, "tool %q declares no permission tags", tool)
	}
	return nil
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}