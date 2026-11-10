package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-platform/pkg/harnessloop/policy/sandbox"
)

type Layer string

const (
	LayerSystem Layer = "system"
	LayerTenant Layer = "tenant"
	LayerRole   Layer = "role"
	LayerTask   Layer = "task"
)

const (
	CodeToolDenied    = "E_TOOL_DENIED"
	CodePathEscape    = "E_PATH_ESCAPE"
	CodeExecDenied    = "E_EXEC_DENIED"
	CodeUnknownTool   = "E_UNKNOWN_TOOL"
	CodeUnknownStage  = "E_UNKNOWN_STAGE"
	CodeSelfElevation = "E_SELF_ELEVATION"
)

var errInvalidTool = errors.New("policy: tool name is required")

type ToolArgs struct {
	Paths   []string
	Command []string
}

type PolicySet struct {
	AllowTools []string
	DenyTools  []string
}

type Request struct {
	Tool            string
	Stage           string
	Args            ToolArgs
	WorkspaceRoot   string
	AllowedSubpaths []string
	Tenant          *PolicySet
	Role            *PolicySet
	Task            *PolicySet
}

type Decision struct {
	Allowed       bool
	Layer         Layer
	RuleID        string
	Code          string
	Reason        string
	Tool          string
	Stage         string
	RequestedPath string
}

func (d Decision) Error() string {
	return fmt.Sprintf("%s (%s layer=%s rule=%s tool=%s stage=%s path=%s): %s",
		d.Code, d.Code, d.Layer, d.RuleID, d.Tool, d.Stage, d.RequestedPath, d.Reason)
}

func (d Decision) AsError() error {
	if d.Allowed {
		return nil
	}
	return errors.New(d.Error())
}

type Engine struct {
	Matrix    Matrix
	Registry  *Registry
	ExecAllow sandbox.ExecAllowlist
}

func NewEngine() *Engine {
	return &Engine{Matrix: DefaultMatrix(), Registry: NewRegistry(), ExecAllow: sandbox.DefaultExecAllowlist()}
}

func (e *Engine) Decide(ctx context.Context, req Request) Decision {
	base := Decision{Tool: req.Tool, Stage: req.Stage}

	if req.Tool == "" {
		base.Layer = LayerSystem
		base.RuleID = "registry.unknown"
		base.Code = CodeUnknownTool
		base.Reason = "tool name is empty"
		return base
	}
	if e.Registry == nil || !e.Registry.Known(req.Tool) {
		base.Layer = LayerSystem
		base.RuleID = "registry.unknown"
		base.Code = CodeUnknownTool
		base.Reason = "tool is not registered (deny-by-default)"
		return base
	}

	rule, ok := e.Matrix.Rule(req.Stage)
	if !ok {
		base.Layer = LayerSystem
		base.RuleID = "stage_matrix." + req.Stage
		base.Code = CodeUnknownStage
		base.Reason = "unknown stage"
		return base
	}
	if rule.DenyAll {
		base.Layer = LayerSystem
		base.RuleID = "stage_matrix." + req.Stage
		base.Code = CodeToolDenied
		base.Reason = "stage denies all tool usage"
		return base
	}

	tags, _ := e.Registry.TagsFor(req.Tool)
	for _, tag := range tags {
		switch tag {
		case TagWrite:
			if !contains(rule.Write, req.Tool) {
				base.Layer = LayerSystem
				base.RuleID = "stage_matrix." + req.Stage + ".write"
				base.Code = CodeToolDenied
				base.Reason = "write tools are not allowed in this stage"
				return base
			}
		case TagRead:
			if !contains(rule.Read, req.Tool) {
				base.Layer = LayerSystem
				base.RuleID = "stage_matrix." + req.Stage + ".read"
				base.Code = CodeToolDenied
				base.Reason = "read tool is not allowed in this stage"
				return base
			}
		case TagExec:
			if len(rule.Exec) == 0 {
				base.Layer = LayerSystem
				base.RuleID = "stage_matrix." + req.Stage + ".exec"
				base.Code = CodeToolDenied
				base.Reason = "exec is not allowed in this stage"
				return base
			}
		case TagNetwork:
			base.Layer = LayerSystem
			base.RuleID = "stage_matrix." + req.Stage + ".network"
			base.Code = CodeToolDenied
			base.Reason = "network tools are denied in the harness loop"
			return base
		}
	}

	for _, layer := range []struct {
		name Layer
		set  *PolicySet
	}{
		{LayerTenant, req.Tenant},
		{LayerRole, req.Role},
		{LayerTask, req.Task},
	} {
		if layer.set == nil {
			continue
		}
		if contains(layer.set.DenyTools, req.Tool) {
			base.Layer = layer.name
			base.RuleID = string(layer.name) + ".deny"
			base.Code = CodeToolDenied
			base.Reason = "tool is explicitly denied at this layer"
			return base
		}
		if len(layer.set.AllowTools) > 0 && !contains(layer.set.AllowTools, req.Tool) {
			base.Layer = layer.name
			base.RuleID = string(layer.name) + ".allow"
			base.Code = CodeToolDenied
			base.Reason = "tool is not in this layer's allowlist"
			return base
		}
	}

	if len(req.Args.Paths) > 0 {
		hasWrite := false
		for _, tag := range tags {
			if tag == TagWrite {
				hasWrite = true
			}
		}
		if hasWrite {
			for _, p := range req.Args.Paths {
				if isSelfElevationPath(p) {
					base.Layer = LayerSystem
					base.RuleID = "policy.self_elevation"
					base.Code = CodeSelfElevation
					base.RequestedPath = p
					base.Reason = "agents must not modify policy or permission files"
					return base
				}
			}
		}
		if req.WorkspaceRoot == "" {
			base.Layer = LayerSystem
			base.RuleID = "sandbox.workspace"
			base.Code = CodePathEscape
			base.RequestedPath = req.Args.Paths[0]
			base.Reason = "workspace root is required for path arguments"
			return base
		}
		guard := sandbox.PathGuard{Root: req.WorkspaceRoot, AllowedSubpaths: req.AllowedSubpaths}
		if bad, err := guard.Check(req.Args.Paths); err != nil {
			base.Layer = LayerSystem
			base.RuleID = "sandbox.realpath"
			base.RequestedPath = bad
			base.Code = CodePathEscape
			base.Reason = err.Error()
			return base
		}
	}

	if len(req.Args.Command) > 0 {
		if err := e.ExecAllow.Check(req.Args.Command); err != nil {
			base.Layer = LayerSystem
			base.RuleID = "sandbox.exec"
			base.Code = CodeExecDenied
			base.Reason = err.Error()
			return base
		}
	}

	base.Allowed = true
	base.Layer = LayerSystem
	base.RuleID = "default.allow"
	base.Reason = "allowed"
	return base
}

var policyPathFragments = []string{
	"config/harness_loop/",
	".harness/policy/",
	".harness/config",
}

func isSelfElevationPath(p string) bool {
	norm := strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	for _, frag := range policyPathFragments {
		if strings.Contains(norm, frag) {
			return true
		}
	}
	return false
}

func (e *Engine) Guard(ctx context.Context, req Request, exec func() error) error {
	d := e.Decide(ctx, req)
	if !d.Allowed {
		return errors.New(d.Error())
	}
	return exec()
}
