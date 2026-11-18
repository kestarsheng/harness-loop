package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/policy"
)

// TaskPolicySink 是任务级策略集的注入点：runtime 在拿到任务行之后调用。
type TaskPolicySink interface {
	SetTaskPolicySet(taskID string, set *policy.PolicySet)
}

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
	layers   *policy.LayerStore

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
		// 解析器与闸门共用同一份高危配置，否则会出现「闸门不建工单、解析器
		// 却判 require_approval」——那种组合下调用永远批不出来。
		resolver.SetApprovalConfig(concrete.Config())
	}
	b.resolver = resolver
	b.loadLayerSets()
	registerDefaultTaskPolicySink(b)
	return b
}

// loadLayerSets 装配 tenant/role 两层（task 层还可以由任务快照覆盖）。
//
// 装配必须发生在桥自己身上：stage_runner 只知道 tenant_id/agent_id/task_id，
// 让它去查配置就等于每个调用点都要重复一遍装配逻辑，而「生产代码从不注入」
// 正是这么来的。配置缺失时降级成空 store（只剩 system 层），行为与改动前一致。
func (b *PolicyBridge) loadLayerSets() {
	path := resolveLayerSetPath()
	store, err := policy.LoadLayerStore(path)
	if err != nil {
		log.Printf("[policy] layer sets unavailable, system layer only: path=%q err=%v", path, err)
	}
	b.layers = store
	if b.resolver != nil {
		b.resolver.SetLayerStore(store)
	}
	if tenants, roles, tasks := store.Len(); tenants+roles+tasks > 0 {
		log.Printf("[policy] layer sets loaded: path=%s tenants=%v roles=%v tasks=%d",
			store.Path(), store.Keys("tenant"), store.Keys("role"), tasks)
	}
}

// resolveLayerSetPath 依次尝试环境变量、部署目录、仓库配置目录。
func resolveLayerSetPath() string {
	if p := strings.TrimSpace(os.Getenv("HARNESSLOOP_POLICY_LAYERS_FILE")); p != "" {
		return p
	}
	for _, candidate := range []string{
		"config/harness_loop/policy_layers.yaml",
		"pkg/config/harness_loop/policy_layers.yaml",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "config/harness_loop/policy_layers.yaml"
}

// SetLayerStore 允许测试/联调换掉装配来源（例如直接用内存 store）。
func (b *PolicyBridge) SetLayerStore(store *policy.LayerStore) {
	if b == nil {
		return
	}
	b.layers = store
	if b.resolver != nil {
		b.resolver.SetLayerStore(store)
	}
}

// SetTaskPolicySet 注入任务级策略集。runtime 从 hlp_task.task_runs.policy_snapshot
// 读出来就调这里——任务级策略天然属于存储而不是部署配置。
func (b *PolicyBridge) SetTaskPolicySet(taskID string, set *policy.PolicySet) {
	if b == nil || taskID == "" || set == nil {
		return
	}
	if b.layers == nil {
		b.layers = policy.NewLayerStore()
	}
	if err := b.layers.PutTaskSnapshot(taskID, set); err != nil {
		log.Printf("[policy] task policy rejected: task_id=%s err=%v", taskID, err)
		return
	}
	// 任务层变了，缓存里的决策必须立刻失效，否则同一任务会继续用旧判定。
	b.BumpPolicyVersion()
}

// defaultTaskPolicySink 是「runtime 找不到显式注入点」时的回落路径。
//
// Runtime 与 PolicyBridge 都在 service 包，但装配点在 cmd/main.go（本期文件域
// 之外），所以桥在构造时把自己登记成默认注入目标：生产路径不需要任何额外
// 接线就能让任务快照里的策略集生效。
var defaultTaskPolicySink atomic.Pointer[PolicyBridge]

func registerDefaultTaskPolicySink(b *PolicyBridge) {
	if b != nil {
		defaultTaskPolicySink.Store(b)
	}
}

// DefaultTaskPolicySink 返回当前进程登记的默认注入目标（可能为 nil）。
func DefaultTaskPolicySink() TaskPolicySink {
	b := defaultTaskPolicySink.Load()
	if b == nil {
		return nil
	}
	return b
}

// clearDefaultTaskPolicySink 给测试收尾用，避免用例之间互相污染。
func clearDefaultTaskPolicySink(b *PolicyBridge) {
	if b != nil && defaultTaskPolicySink.CompareAndSwap(b, nil) {
		return
	}
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
		// 这里刻意不用 gate.Authorize 一步走完：那个快捷路径在「策略层要求审批
		// 但闸门判定不属于高危类」时会返回 allowed=true，require_approval 就变成
		// 了 allow 的别名。分开走 Begin/Await 才能守住「没有工单就不放行」。
		ticket, err := b.gate.Begin(ctx, req, b.toolTags(req.Tool))
		if err != nil {
			code := approvalErrorCode(err)
			return fmt.Errorf("%s: %s", code, err.Error())
		}
		if ticket == nil {
			return fmt.Errorf("%s: rule %s requires approval but no approval ticket could be issued",
				policy.CodeApprovalRequired, d.RuleID)
		}
		outcome := b.gate.Await(ctx, ticket)
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
	if rec.PolicySource == "" {
		rec.PolicySource = "system:engine"
	}
	// hlp_event.audit 的 payload 由 repository 按固定键组装，policy_source 只能
	// 从 args_summary 这条既有通道落库（payload->'args_summary'->>'policy_source'）。
	// 不改 repository 是为了守住本期文件域；查询侧按同一条路径取即可。
	if rec.ArgsSummary == nil {
		rec.ArgsSummary = map[string]any{}
	}
	rec.ArgsSummary["policy_source"] = rec.PolicySource
	if b.writer != nil {
		if err := b.writer.AppendToolDecision(ctx, rec); err != nil {
			// 审计面写失败不能连带吞掉事件面：记日志后继续发事件，人至少还能
			// 从事件流看到这次拒绝（反过来则什么都看不到）。
			log.Printf("[policy] audit append failed: task_id=%s rule=%s decision=%s err=%v",
				rec.TaskID, rec.RuleID, rec.Decision, err)
		}
	}
	switch rec.Decision {
	case policy.EffectDeny:
		b.emit(ctx, rec, event.ToolDenied, event.SeverityWarn)
	case policy.EffectRequireApproval:
		b.emit(ctx, rec, event.ApprovalRequested, event.SeverityWarn)
	}
	return nil
}

// emit 发一条决策事件。失败不得静默：tool.denied 曾经就是这样丢的，
// 于是「审计有记录、事件流没有」两面对不上。
func (b *PolicyBridge) emit(ctx context.Context, rec policy.AuditRecord, typ event.EventType, severity event.Severity) {
	if b.emitter == nil {
		return
	}
	emitter := b.emitter.For(orchestrator.EmitContext{
		TenantID: rec.TenantID, ProjectID: rec.ProjectID, TaskID: rec.TaskID,
		Stage: rec.Stage, Attempt: rec.Attempt,
	})
	reportEmitFailure(rec.TaskID, typ, emitter.EmitType(ctx, orchestrator.EmitContext{}, typ, severity, map[string]any{
		"tool":          rec.Tool,
		"decision":      string(rec.Decision),
		"rule_id":       rec.RuleID,
		"layer":         string(rec.Layer),
		"code":          rec.Code,
		"policy_source": rec.PolicySource,
	}))
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
	if err := b.Write(ctx, rec); err != nil {
		reportEmitFailure(in.TaskID, event.ToolDenied, err)
	}
	if b.emitter != nil {
		em := b.emitter.For(orchestrator.EmitContext{
			TenantID: in.TenantID, ProjectID: in.ProjectID, TaskID: in.TaskID, Stage: in.Stage, Attempt: in.Attempt,
		})
		reportEmitFailure(in.TaskID, event.SecurityViolation, em.EmitType(ctx, orchestrator.EmitContext{}, event.SecurityViolation, event.SeverityError, map[string]any{
			"reason": "cross tenant tool access denied", "expected_tenant": tenantID,
			"task_id": in.TaskID, "tool": in.Tool, "rule_id": "security.cross_tenant",
		}))
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
		in.Command = commandList(raw, "command", "cmd", "argv")
	}
	return in
}

// commandList 把 exec 参数还原成 argv。
//
// agent 侧把命令当整串发过来（"rm -f scratch/probe.txt"），MCP 执行时用
// strings.Fields 拆开跑；如果策略层拿整串去判，白名单看到的是「一个带斜杠的
// 可执行名」，Classify 看到的是「工具名里没有 rm」——两边都判错，审批永远不可达。
// 这里按 MCP 的同一套拆分规则还原，策略判的就是真正要跑的东西。
func commandList(raw map[string]any, keys ...string) []string {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			if fields := strings.Fields(val); len(fields) > 0 {
				return fields
			}
		case []any:
			out := make([]string, 0, len(val))
			for _, item := range val {
				if s, ok := item.(string); ok && s != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
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
