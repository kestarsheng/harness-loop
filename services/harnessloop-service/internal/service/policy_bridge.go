package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/policy"
)

// AuditWriter persists permission decisions into hlp_event.audit.
type AuditWriter interface {
	AppendToolDecision(ctx context.Context, rec policy.AuditRecord) error
}

// ToolCallInput is the runtime-side tool call shape shared by the gRPC
// PreflightTool handler and the stage runner.
type ToolCallInput struct {
	TenantID      string
	ProjectID     string
	TaskID        string
	Stage         string
	Attempt       int
	AgentID       string
	RoleName      string
	WorkspaceRoot string
	Tool          string
	Paths         []string
	Command       []string
	Arguments     []byte
	TenantPolicy  *policy.PolicySet
	RolePolicy    *policy.PolicySet
	TaskPolicy    *policy.PolicySet
}

type PreflightResult struct {
	Allowed          bool
	RequiresApproval bool
	ApprovalID       string
	Layer            policy.Layer
	RuleID           string
	Code             string
	Reason           string
	CacheHit         bool
}

// ToolApprovalGate is the gate contract used by the policy bridge. The
// in-process *policy.ApprovalGate implements it; the HTTP-backed client is the
// cross-process variant that talks to agent-service.
type ToolApprovalGate interface {
	Begin(ctx context.Context, req policy.Request, tags []string) (*policy.ApprovalTicket, error)
	Await(ctx context.Context, ticket *policy.ApprovalTicket) policy.ApprovalOutcome
	Authorize(ctx context.Context, req policy.Request, tags []string) policy.ApprovalOutcome
}

// PolicyBridge wires policy.Resolver (four-layer precedence), ApprovalGate and
// the audit sink into the runtime. policy.Engine is kept as the fallback path
// used when the feature flag is disabled.
type PolicyBridge struct {
	enabled  bool
	engine   *policy.Engine
	resolver *policy.Resolver
	writer   AuditWriter
	emitter  *orchestrator.PublisherEmitter
	gate     ToolApprovalGate

	lookup func(ctx context.Context, taskID string) (tenantID, projectID string, err error)
}

func NewPolicyBridge(enabled bool, engine *policy.Engine, writer AuditWriter, emitter *orchestrator.PublisherEmitter, gate ToolApprovalGate) *PolicyBridge {
	if engine == nil {
		engine = policy.NewEngine()
	}
	b := &PolicyBridge{enabled: enabled, engine: engine, writer: writer, emitter: emitter, gate: gate}
	resolver := policy.NewResolver(engine)
	resolver.SetAuditSink(b)
	if concrete, ok := gate.(*policy.ApprovalGate); ok && concrete != nil {
		resolver.SetApprovalGate(concrete)
	}
	b.resolver = resolver
	return b
}

func (b *PolicyBridge) Enabled() bool { return b != nil && b.enabled }

func (b *PolicyBridge) Resolver() *policy.Resolver {
	if b == nil {
		return nil
	}
	return b.resolver
}

func (b *PolicyBridge) SetTaskLookup(fn func(ctx context.Context, taskID string) (tenantID, projectID string, err error)) {
	if b != nil {
		b.lookup = fn
	}
}

// BumpPolicyVersion / BumpApprovalVersion invalidate cached decisions whenever
// the policy or approval configuration changes.
func (b *PolicyBridge) BumpPolicyVersion() int64 {
	if b == nil || b.resolver == nil {
		return 0
	}
	return b.resolver.BumpPolicyVersion()
}

func (b *PolicyBridge) BumpApprovalVersion() int64 {
	if b == nil || b.resolver == nil {
		return 0
	}
	return b.resolver.BumpApprovalVersion()
}

// Invalidate evicts both policy and approval cached decisions.
func (b *PolicyBridge) Invalidate() {
	b.BumpPolicyVersion()
	b.BumpApprovalVersion()
}

// Preflight is the non-blocking decision used by Handler.PreflightTool. A
// require_approval result creates a pending ticket and returns immediately so
// one human decision never blocks unrelated tasks.
func (b *PolicyBridge) Preflight(ctx context.Context, in ToolCallInput) PreflightResult {
	if b == nil {
		return PreflightResult{Allowed: false, Code: policy.CodeToolDenied, Reason: "policy bridge is not configured"}
	}
	in = in.normalize()
	req := b.request(in)
	if b.enabled {
		if res, cross := b.crossTenant(ctx, in); cross {
			return res
		}
		d := b.resolver.Decide(ctx, req)
		res := resultFromDecision(d)
		if d.EffectiveEffect() == policy.EffectRequireApproval {
			res.RequiresApproval = true
			res.Allowed = false
			approvalID, code := b.beginApproval(ctx, req, b.toolTags(req.Tool))
			res.ApprovalID = approvalID
			if code != "" {
				res.Code = code
			}
		}
		return res
	}
	d := b.engine.Decide(ctx, req)
	return resultFromDecision(d)
}

// AuthorizeTool is the blocking gate used inside a stage. It only blocks the
// task that made the call; every task runs in its own goroutine. Timeout or a
// rejected approval never releases the call.
func (b *PolicyBridge) AuthorizeTool(ctx context.Context, in ToolCallInput) error {
	if b == nil {
		return nil
	}
	in = in.normalize()
	req := b.request(in)
	if !b.enabled {
		d := b.engine.Decide(ctx, req)
		if d.Allowed {
			return nil
		}
		return d.AsError()
	}
	if res, cross := b.crossTenant(ctx, in); cross {
		return fmt.Errorf("%s: %s", res.Code, res.Reason)
	}
	d := b.resolver.Decide(ctx, req)
	switch d.EffectiveEffect() {
	case policy.EffectAllow:
		return nil
	case policy.EffectRequireApproval:
		if b.gate == nil {
			return fmt.Errorf("%s: approval service unavailable", policy.CodeApprovalUnavailable)
		}
		outcome := b.gate.Authorize(ctx, req, b.toolTags(req.Tool))
		if outcome.Allowed {
			return nil
		}
		code := outcome.Code
		if code == "" {
			code = policy.CodeApprovalRejected
		}
		return fmt.Errorf("%s: %s", code, outcome.Reason)
	default:
		return d.AsError()
	}
}

// Write implements policy.AuditSink. The same normalised rule id/layer is used
// for the audit row and for the tool.denied / approval.requested event, which
// is what REQ-166 requires.
func (b *PolicyBridge) Write(ctx context.Context, rec policy.AuditRecord) error {
	if rec.RuleID == "" {
		rec.RuleID = "policy.rule.unspecified"
	}
	if rec.Layer == "" {
		rec.Layer = policy.LayerSystem
	}
	if b.writer != nil {
		_ = b.writer.AppendToolDecision(ctx, rec)
	}
	switch rec.Decision {
	case policy.EffectDeny:
		b.emit(ctx, rec, event.ToolDenied, event.SeverityWarn)
	case policy.EffectRequireApproval:
		b.emit(ctx, rec, event.ApprovalRequested, event.SeverityWarn)
	}
	return nil
}

func (b *PolicyBridge) emit(ctx context.Context, rec policy.AuditRecord, typ event.EventType, severity event.Severity) {
	if b.emitter == nil {
		return
	}
	emitter := b.emitter.For(orchestrator.EmitContext{
		TenantID: rec.TenantID, ProjectID: rec.ProjectID, TaskID: rec.TaskID,
		Stage: rec.Stage, Attempt: rec.Attempt,
	})
	_ = emitter.EmitType(ctx, orchestrator.EmitContext{}, typ, severity, map[string]any{
		"tool":     rec.Tool,
		"decision": string(rec.Decision),
		"rule_id":  rec.RuleID,
		"layer":    string(rec.Layer),
		"code":     rec.Code,
	})
}

func (b *PolicyBridge) beginApproval(ctx context.Context, req policy.Request, tags []string) (string, string) {
	if b.gate == nil {
		return "", policy.CodeApprovalUnavailable
	}
	ticket, err := b.gate.Begin(ctx, req, tags)
	if err != nil {
		return "", approvalErrorCode(err)
	}
	if ticket == nil {
		// The resolver declared require_approval but the call is not a
		// high-risk class the gate can hold. Keep it pending/denied rather
		// than silently allowing it.
		return "", policy.CodeApprovalRequired
	}
	return ticket.ID, ""
}

func (b *PolicyBridge) crossTenant(ctx context.Context, in ToolCallInput) (PreflightResult, bool) {
	if b.lookup == nil || in.TaskID == "" || in.TenantID == "" {
		return PreflightResult{}, false
	}
	tenantID, _, err := b.lookup(ctx, in.TaskID)
	if err != nil || tenantID == "" {
		return PreflightResult{}, false
	}
	if tenantID == in.TenantID {
		return PreflightResult{}, false
	}
	rec := policy.AuditRecord{
		TS: time.Now().UTC(), TenantID: in.TenantID, ProjectID: in.ProjectID, TaskID: in.TaskID,
		Stage: in.Stage, Attempt: in.Attempt, Role: in.RoleName, AgentID: in.AgentID,
		Tool: in.Tool, Decision: policy.EffectDeny, RuleID: "security.cross_tenant",
		Layer: policy.LayerSystem, Code: "E_CROSS_TENANT_DENIED",
	}
	_ = b.Write(ctx, rec)
	if b.emitter != nil {
		em := b.emitter.For(orchestrator.EmitContext{
			TenantID: in.TenantID, ProjectID: in.ProjectID, TaskID: in.TaskID, Stage: in.Stage, Attempt: in.Attempt,
		})
		_ = em.EmitType(ctx, orchestrator.EmitContext{}, event.SecurityViolation, event.SeverityError, map[string]any{
			"reason": "cross tenant tool access denied", "expected_tenant": tenantID,
			"task_id": in.TaskID, "tool": in.Tool, "rule_id": "security.cross_tenant",
		})
	}
	return PreflightResult{
		Allowed: false, Layer: policy.LayerSystem, RuleID: "security.cross_tenant",
		Code: "E_CROSS_TENANT_DENIED", Reason: "task belongs to another tenant",
	}, true
}

// normalize fills Paths/Command from the raw tool arguments when the caller did
// not pre-parse them.
func (in ToolCallInput) normalize() ToolCallInput {
	if len(in.Arguments) == 0 || (len(in.Paths) > 0 && len(in.Command) > 0) {
		return in
	}
	var raw map[string]any
	if err := json.Unmarshal(in.Arguments, &raw); err != nil {
		return in
	}
	if len(in.Paths) == 0 {
		in.Paths = stringList(raw, "paths", "path", "file_path", "files")
	}
	if len(in.Command) == 0 {
		in.Command = stringList(raw, "command", "cmd", "argv")
	}
	return in
}

func stringList(raw map[string]any, keys ...string) []string {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			if val != "" {
				return []string{val}
			}
		case []any:
			out := make([]string, 0, len(val))
			for _, item := range val {
				if s, ok := item.(string); ok && s != "" {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}

// toolTags returns the registry tags of a tool so the approval gate can
// classify delete / network-write classes. Without tags only name-based
// deploy detection would work.
func (b *PolicyBridge) toolTags(tool string) []string {
	if b == nil || b.engine == nil || b.engine.Registry == nil {
		return nil
	}
	tags, _ := b.engine.Registry.TagsFor(tool)
	return tags
}

func (b *PolicyBridge) request(in ToolCallInput) policy.Request {
	in = in.normalize()
	return policy.Request{
		Tool:          in.Tool,
		Stage:         in.Stage,
		WorkspaceRoot: in.WorkspaceRoot,
		Args: policy.ToolArgs{
			Paths:   in.Paths,
			Command: in.Command,
		},
		Tenant:    in.TenantPolicy,
		Role:      in.RolePolicy,
		Task:      in.TaskPolicy,
		TenantID:  in.TenantID,
		ProjectID: in.ProjectID,
		TaskID:    in.TaskID,
		RoleName:  in.RoleName,
		AgentID:   in.AgentID,
		Attempt:   in.Attempt,
	}
}

func resultFromDecision(d policy.Decision) PreflightResult {
	return PreflightResult{
		Allowed:    d.Allowed,
		Layer:      d.Layer,
		RuleID:     d.RuleID,
		Code:       d.Code,
		Reason:     d.Reason,
		CacheHit:   d.CacheHit,
		ApprovalID: "",
	}
}

func approvalErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case isApprovalRejected(err):
		return policy.CodeApprovalRejected
	case isApprovalUnavailable(err):
		return policy.CodeApprovalUnavailable
	default:
		return policy.CodeApprovalRequired
	}
}

func isApprovalRejected(err error) bool    { return errors.Is(err, policy.ErrApprovalRejected) }
func isApprovalUnavailable(err error) bool { return errors.Is(err, policy.ErrApprovalUnavailable) }
