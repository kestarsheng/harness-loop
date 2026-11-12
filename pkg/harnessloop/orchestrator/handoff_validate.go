package orchestrator

import (
	"context"
	"encoding/json"
	"strings"

	"agent-platform/pkg/harnessloop/compaction"
	"agent-platform/pkg/harnessloop/subtask"
)

// HandoffRequest carries the parent identity a handoff must match. The
// validator never trusts tenant/project fields on the contract itself.
type HandoffRequest struct {
	ParentTaskID string
	TenantID     string
	ProjectID    string
	Stage        string
}

// HandoffValidator enforces HandoffContract/v1 (REQ-131/132/137) and reuses the
// T-P1-22 role/tool registry for recipient permissions.
type HandoffValidator struct {
	roles   *subtask.Registry
	emitter Emitter
}

func NewHandoffValidator(roles *subtask.Registry, emitter Emitter) *HandoffValidator {
	return &HandoffValidator{roles: roles, emitter: emitter}
}

// Validate checks structural completeness, the five-element summary, the
// unfinished list, tenant/project isolation and recipient tool permissions.
func (v *HandoffValidator) Validate(ctx context.Context, h *HandoffContract, req HandoffRequest) error {
	if h == nil {
		return newError(CodeHandoffIncomplete, "handoff contract is nil")
	}
	if strings.TrimSpace(h.HandoffID) == "" || strings.TrimSpace(h.ParentTaskID) == "" {
		return newError(CodeHandoffIncomplete, "handoff_id and parent_task_id are required")
	}
	if req.ParentTaskID != "" && h.ParentTaskID != req.ParentTaskID {
		return newError(CodeHandoffIncomplete, "handoff parent_task_id %q does not match request %q", h.ParentTaskID, req.ParentTaskID)
	}
	if strings.TrimSpace(h.From.AgentID) == "" || strings.TrimSpace(h.From.Role) == "" || strings.TrimSpace(h.From.SubtaskID) == "" {
		return newError(CodeHandoffIncomplete, "from agent_id/role/subtask_id are required")
	}
	if strings.TrimSpace(h.To.AgentID) == "" || strings.TrimSpace(h.To.Role) == "" {
		return newError(CodeHandoffIncomplete, "to agent_id/role are required")
	}
	if len(h.Artifacts) == 0 {
		return newError(CodeHandoffIncomplete, "artifacts must not be empty")
	}
	for _, artifact := range h.Artifacts {
		if strings.TrimSpace(artifact.Path) == "" || strings.TrimSpace(artifact.Hash) == "" {
			return newError(CodeHandoffIncomplete, "artifact %q requires both path and hash", artifact.Path)
		}
		if err := validateSandboxPath("", artifact.Path); err != nil {
			return newError(CodeHandoffIncomplete, "artifact path %q is outside the workspace: %v", artifact.Path, err)
		}
	}
	if err := validateFiveElements(h.ContextSummary); err != nil {
		return err
	}
	if len(h.Unfinished) == 0 {
		return newError(CodeHandoffIncomplete, "unfinished list must not be empty")
	}
	for _, item := range h.Unfinished {
		if strings.TrimSpace(item.Item) == "" {
			return newError(CodeHandoffIncomplete, "unfinished item is empty")
		}
	}
	if strings.TrimSpace(h.Constraints.Role) == "" {
		return newError(CodeHandoffIncomplete, "constraints.role is required")
	}
	if len(h.Constraints.Acceptance) == 0 {
		return newError(CodeHandoffIncomplete, "constraints.acceptance is required")
	}
	if h.TenantID != req.TenantID || h.ProjectID != req.ProjectID {
		if v.emitter != nil {
			if err := emitSecurityViolation(ctx, v.emitter, h.ParentTaskID, h.SubtaskID, h.HandoffID,
				CodeCrossTenantDenied, "handoff crosses tenant or project boundary"); err != nil {
				return err
			}
		}
		return newError(CodeCrossTenantDenied,
			"handoff tenant/project (%s/%s) does not match parent (%s/%s)",
			h.TenantID, h.ProjectID, req.TenantID, req.ProjectID)
	}
	return v.validatePermissions(ctx, h, req)
}

func validateFiveElements(summary ContextSummary) error {
	payload, err := json.Marshal(map[string]string{
		"goal":        summary.GoalRef,
		"done":        summary.Done,
		"todo":        summary.Todo,
		"constraints": summary.Constraints,
		"background":  summary.Background,
	})
	if err != nil {
		return newError(CodeHandoffIncomplete, "encode context summary: %v", err)
	}
	if _, err := compaction.ParseSummary(string(payload)); err != nil {
		return newError(CodeHandoffIncomplete, "context_summary is incomplete: %v", err)
	}
	return nil
}

func (v *HandoffValidator) validatePermissions(ctx context.Context, h *HandoffContract, req HandoffRequest) error {
	if v.roles == nil {
		return nil
	}
	stage := req.Stage
	if stage == "" {
		stage = "c"
	}
	binding, err := v.roles.Resolve(ctx, h.To.AgentID, stage)
	if err != nil {
		return err
	}
	if !containsString(binding.Roles, h.To.Role) {
		return newError(subtask.CodeAgentRoleUndeclared,
			"agent %s does not declare handoff role %q", h.To.AgentID, h.To.Role)
	}
	if h.Constraints.Role != h.To.Role {
		return newError(subtask.CodeAgentRoleUndeclared,
			"handoff constraints.role %q does not match receiver role %q", h.Constraints.Role, h.To.Role)
	}
	for _, tool := range h.Constraints.Tools {
		if err := v.roles.CheckTool(ctx, h.To.AgentID, stage, tool); err != nil {
			return err
		}
	}
	return nil
}

func emitSecurityViolation(ctx context.Context, emitter Emitter, parentTaskID, subtaskID, handoffID, code, detail string) error {
	return emit(ctx, emitter, Event{
		Type:         EventSecurityViolation,
		Severity:     SeverityError,
		ParentTaskID: parentTaskID,
		SubtaskID:    subtaskID,
		HandoffID:    handoffID,
		Payload: map[string]any{
			"code":   code,
			"detail": detail,
		},
	})
}
