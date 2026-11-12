package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const CodePolicyLoosening = "E_POLICY_LOOSENING"

var ErrPolicyLoosening = errors.New("policy: tenant policy may not loosen platform policy without approval")

type Resolver struct {
	engine          *Engine
	cache           *DecisionCache
	audit           AuditSink
	approval        *ApprovalGate
	policyVersion   atomic.Int64
	approvalVersion atomic.Int64

	mu                sync.RWMutex
	looseningApprover func(tenantID, policyHash string) (bool, string)
}

func NewResolver(engine *Engine) *Resolver {
	if engine == nil {
		engine = NewEngine()
	}
	return &Resolver{
		engine: engine,
		cache:  NewDecisionCache(10000, 60*time.Second),
	}
}

func (r *Resolver) Engine() *Engine {
	return r.engine
}

func (r *Resolver) SetCache(c *DecisionCache) {
	r.cache = c
}

func (r *Resolver) SetAuditSink(sink AuditSink) {
	r.audit = sink
}

func (r *Resolver) SetApprovalGate(gate *ApprovalGate) {
	r.approval = gate
}

func (r *Resolver) ApprovalGate() *ApprovalGate {
	return r.approval
}

func (r *Resolver) SetTenantLooseningApprover(fn func(tenantID, policyHash string) (bool, string)) {
	r.mu.Lock()
	r.looseningApprover = fn
	r.mu.Unlock()
}

func (r *Resolver) BumpPolicyVersion() int64 {
	return r.policyVersion.Add(1)
}

func (r *Resolver) BumpApprovalVersion() int64 {
	return r.approvalVersion.Add(1)
}

func (r *Resolver) PolicyVersion() int64 {
	return r.policyVersion.Load()
}

func (r *Resolver) ApprovalVersion() int64 {
	return r.approvalVersion.Load()
}

func (r *Resolver) Decide(ctx context.Context, req Request) Decision {
	start := time.Now()
	tags := r.tagsFor(req.Tool)

	if r.cacheable(req, tags) && r.cache != nil {
		key := DecisionKey(req, r.PolicyVersion(), r.ApprovalVersion())
		if cached, ok := r.cache.Get(key); ok {
			cached.CacheHit = true
			cached.DurationUS = time.Since(start).Microseconds()
			r.writeAudit(ctx, req, tags, cached)
			return cached
		}
		d := r.resolve(ctx, req, tags)
		r.cache.Put(key, d)
		d.DurationUS = time.Since(start).Microseconds()
		r.writeAudit(ctx, req, tags, d)
		return d
	}

	d := r.resolve(ctx, req, tags)
	d.DurationUS = time.Since(start).Microseconds()
	r.writeAudit(ctx, req, tags, d)
	return d
}

func (r *Resolver) resolve(ctx context.Context, req Request, tags []string) Decision {
	sys := r.engine.SystemDecide(ctx, req)
	if !sys.Allowed {
		sys.Effect = EffectDeny
		return sys
	}

	var pending *Decision
	var earliest *Decision
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
		decisions := layerDecisions(layer.set, layer.name, req.Tool, tags, req.Stage)
		for i := range decisions {
			if decisions[i].Effect == EffectDeny {
				return decisions[i]
			}
		}
		for i := range decisions {
			if decisions[i].RequiresApproval && pending == nil {
				cp := decisions[i]
				pending = &cp
			}
		}
		if earliest == nil {
			for i := range decisions {
				if decisions[i].Effect == EffectAllow && !decisions[i].RequiresApproval {
					cp := decisions[i]
					earliest = &cp
					break
				}
			}
		}
	}

	if pending != nil {
		out := *pending
		out.Allowed = false
		out.RequiresApproval = true
		out.Effect = EffectRequireApproval
		if out.Code == "" {
			out.Code = CodeApprovalRequired
		}
		if out.Reason == "" {
			out.Reason = "call requires human approval"
		}
		return out
	}

	if req.Role != nil && layerDeclaresAllow(req.Role) && !layerAllows(req.Role, req.Tool, tags, req.Stage) {
		return deny(req, LayerRole, "policy.role.no_match", "role does not declare an allow rule for this tool")
	}
	if req.Task != nil && layerDeclaresAllow(req.Task) && !layerAllows(req.Task, req.Tool, tags, req.Stage) {
		return deny(req, LayerTask, "policy.task.no_match", "task does not declare an allow rule for this tool")
	}

	out := Decision{Allowed: true, Effect: EffectAllow, Tool: req.Tool, Stage: req.Stage}
	if earliest != nil {
		out.Layer = earliest.Layer
		out.RuleID = earliest.RuleID
		out.Reason = earliest.Reason
	} else {
		out.Layer = sys.Layer
		out.RuleID = sys.RuleID
		out.Reason = sys.Reason
	}
	return out
}

func (r *Resolver) ValidateTenantPolicy(tenantID string, platform, tenant PolicySet) error {
	denied := make(map[string]bool)
	for _, tool := range platform.DenyTools {
		denied[tool] = true
	}
	for _, rule := range platform.Rules {
		if rule.Effect == EffectDeny && rule.Tool != "" {
			denied[rule.Tool] = true
		}
	}

	loosened := make(map[string]bool)
	for _, tool := range tenant.AllowTools {
		if denied[tool] {
			loosened[tool] = true
		}
	}
	for _, rule := range tenant.Rules {
		if rule.Effect == EffectAllow && denied[rule.Tool] {
			loosened[rule.Tool] = true
		}
	}
	if len(loosened) == 0 {
		return nil
	}

	hash := tenantPolicyHash(platform, tenant)
	if r.looseningApproved(tenantID, hash) {
		return nil
	}
	names := make([]string, 0, len(loosened))
	for tool := range loosened {
		names = append(names, tool)
	}
	return fmt.Errorf("%w (code=%s, tenant=%s): %s", ErrPolicyLoosening, CodePolicyLoosening, tenantID, strings.Join(names, ","))
}

func (r *Resolver) looseningApproved(tenantID, policyHash string) bool {
	r.mu.RLock()
	fn := r.looseningApprover
	r.mu.RUnlock()
	if fn == nil {
		return false
	}
	approved, _ := fn(tenantID, policyHash)
	return approved
}

func (r *Resolver) cacheable(req Request, tags []string) bool {
	if contains(tags, TagWrite) {
		return false
	}
	return r.engine.Registry != nil && r.engine.Registry.Known(req.Tool)
}

func (r *Resolver) tagsFor(tool string) []string {
	if r.engine.Registry == nil {
		return nil
	}
	tags, _ := r.engine.Registry.TagsFor(tool)
	return tags
}

func (r *Resolver) writeAudit(ctx context.Context, req Request, tags []string, d Decision) {
	if r.audit == nil {
		return
	}
	_ = r.audit.Write(ctx, d.AuditRecord(req, tags, time.Duration(d.DurationUS)*time.Microsecond))
}

func layerDecisions(set *PolicySet, layer Layer, tool string, tags []string, stage string) []Decision {
	base := Decision{Tool: tool, Stage: stage, Layer: layer}
	var out []Decision

	if contains(set.DenyTools, tool) {
		d := base
		d.Effect = EffectDeny
		d.RuleID = string(layer) + ".deny"
		d.Code = CodeToolDenied
		d.Reason = "tool is explicitly denied at this layer"
		out = append(out, d)
	}

	if len(set.AllowTools) > 0 {
		d := base
		d.RuleID = string(layer) + ".allow"
		if contains(set.AllowTools, tool) {
			d.Effect = EffectAllow
			d.Reason = "tool is in this layer's allowlist"
		} else {
			d.Effect = EffectDeny
			d.RuleID = "policy." + string(layer) + ".no_match"
			d.Code = CodeToolDenied
			d.Reason = "tool is not in this layer's allowlist"
		}
		out = append(out, d)
	}

	for _, rule := range set.Rules {
		if !ruleMatches(rule, tool, tags, stage) {
			continue
		}
		d := base
		d.RuleID = rule.ID
		if d.RuleID == "" {
			d.RuleID = string(layer) + ".rule"
		}
		switch rule.Effect {
		case EffectAllow, EffectRequireApproval:
			d.Effect = EffectAllow
			d.Reason = "allowed by rule"
			if rule.RequiresApproval || rule.Effect == EffectRequireApproval {
				d.RequiresApproval = true
				d.Code = CodeApprovalRequired
				d.Reason = "rule requires human approval"
			}
		default:
			d.Effect = EffectDeny
			d.Code = CodeToolDenied
			d.Reason = "denied by rule"
		}
		out = append(out, d)
	}

	return out
}

func layerDeclaresAllow(set *PolicySet) bool {
	if len(set.AllowTools) > 0 {
		return true
	}
	for _, rule := range set.Rules {
		if rule.Effect == EffectAllow || rule.Effect == EffectRequireApproval {
			return true
		}
	}
	return false
}

func layerAllows(set *PolicySet, tool string, tags []string, stage string) bool {
	if contains(set.AllowTools, tool) {
		return true
	}
	for _, rule := range set.Rules {
		if (rule.Effect == EffectAllow || rule.Effect == EffectRequireApproval) && ruleMatches(rule, tool, tags, stage) {
			return true
		}
	}
	return false
}

func ruleMatches(rule Rule, tool string, tags []string, stage string) bool {
	if rule.Tool != "" && !matchTool(rule.Tool, tool) {
		return false
	}
	if len(rule.Stages) > 0 && !contains(rule.Stages, stage) {
		return false
	}
	if len(rule.Tags) > 0 {
		matched := false
		for _, tag := range rule.Tags {
			if contains(tags, tag) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func matchTool(pattern, tool string) bool {
	if pattern == tool {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(tool, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

func deny(req Request, layer Layer, ruleID, reason string) Decision {
	return Decision{
		Allowed: false,
		Effect:  EffectDeny,
		Layer:   layer,
		RuleID:  ruleID,
		Code:    CodeToolDenied,
		Reason:  reason,
		Tool:    req.Tool,
		Stage:   req.Stage,
	}
}

func tenantPolicyHash(platform, tenant PolicySet) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%v|%v|%v|%v", platform.DenyTools, tenant.AllowTools, tenant.DenyTools, tenant.Rules)))
	return hex.EncodeToString(sum[:])
}
