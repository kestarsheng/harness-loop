package policy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	agentapproval "agent-platform/pkg/agent/approval"
)

const (
	CodeApprovalRequired    = "E_APPROVAL_REQUIRED"
	CodeApprovalRejected    = "E_APPROVAL_REJECTED"
	CodeApprovalTimeout     = "E_APPROVAL_TIMEOUT"
	CodeApprovalCancelled   = "E_APPROVAL_CANCELLED"
	CodeApprovalUnavailable = "E_APPROVAL_UNAVAILABLE"
)

var (
	ErrApprovalRejected    = errors.New("policy: approval rejected; retry is not allowed")
	ErrApprovalTimeout     = errors.New("policy: approval timed out; the call is not allowed")
	ErrApprovalCancelled   = errors.New("policy: approval cancelled")
	ErrApprovalUnavailable = errors.New("policy: approval service unavailable; hold the call as pending")
)

type HighRiskKind string

const (
	HighRiskNone         HighRiskKind = ""
	HighRiskDelete       HighRiskKind = "delete"
	HighRiskNetworkWrite HighRiskKind = "network_write"
	HighRiskDeploy       HighRiskKind = "deploy"
	HighRiskCost         HighRiskKind = "cost"
)

type HighRiskConfig struct {
	Delete       bool `yaml:"delete"`
	NetworkWrite bool `yaml:"network_write"`
	Deploy       bool `yaml:"deploy"`
	Cost         bool `yaml:"cost"`
}

type ApprovalConfig struct {
	Version            int            `yaml:"version"`
	Enabled            bool           `yaml:"enabled"`
	TimeoutSeconds     int            `yaml:"timeout_seconds"`
	CostThreshold      float64        `yaml:"cost_threshold"`
	TimeoutAutoApprove bool           `yaml:"timeout_auto_approve"`
	HighRisk           HighRiskConfig `yaml:"high_risk"`
}

func DefaultApprovalConfig() ApprovalConfig {
	return ApprovalConfig{
		Version:        1,
		Enabled:        true,
		TimeoutSeconds: 300,
		CostThreshold:  0.10,
		HighRisk: HighRiskConfig{
			Delete:       true,
			NetworkWrite: true,
			Deploy:       true,
			Cost:         true,
		},
	}
}

func (c ApprovalConfig) Validate() error {
	if c.TimeoutSeconds <= 0 || c.TimeoutSeconds > 86400 {
		return fmt.Errorf("policy: approval timeout_seconds must be within (0, 86400], got %d", c.TimeoutSeconds)
	}
	if c.TimeoutAutoApprove {
		return errors.New("policy: approval timeout_auto_approve is forbidden; timeout must never auto-approve")
	}
	if c.CostThreshold < 0 {
		return fmt.Errorf("policy: approval cost_threshold must be >= 0, got %v", c.CostThreshold)
	}
	return nil
}

func LoadApprovalConfig(path string) (ApprovalConfig, error) {
	cfg := DefaultApprovalConfig()
	if path == "" {
		return cfg, cfg.Validate()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	var file struct {
		Version  int            `yaml:"version"`
		Approval ApprovalConfig `yaml:"approval"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return cfg, err
	}
	file.Approval.Version = file.Version
	if err := file.Approval.Validate(); err != nil {
		return cfg, err
	}
	return file.Approval, nil
}

type ApprovalTicket struct {
	ID          string
	Kind        HighRiskKind
	RuleID      string
	Tool        string
	Stage       string
	RequestedAt time.Time
	ExpiresAt   time.Time

	// 下面四个字段只用于签发一次性审批令牌：令牌必须钉死「哪个任务、哪次
	// 调用（参数摘要）、哪张工单」，少一个就能被别的调用复用。
	TenantID   string
	ProjectID  string
	TaskID     string
	ArgsDigest string

	key string
}

type ApprovalOutcome struct {
	Required   bool
	Allowed    bool
	Status     agentapproval.ApprovalStatus
	ApprovalID string
	Kind       HighRiskKind
	RuleID     string
	Code       string
	Reason     string
	Err        error
}

type ApprovalGate struct {
	mu           sync.Mutex
	cfg          ApprovalConfig
	manager      *agentapproval.ApprovalFlowManager
	now          func() time.Time
	pollInterval time.Duration
	alert        func(string)
	rejected     map[string]ApprovalOutcome
	requests     int

	// tickets 按调用指纹复用 pending 工单。同一次调用被重复检查（重试、预检
	// 与闸门各问一次）时不能再建一张工单——否则一次 rm 会刷出一屏工单。
	// 只在 gate 自己管理工单时启用（manager 非空）。
	tickets map[string]*ApprovalTicket

	issuer *ApprovalTokenIssuer
	sink   ApprovalTokenSink
}

func NewApprovalGate(cfg ApprovalConfig, manager *agentapproval.ApprovalFlowManager) *ApprovalGate {
	return &ApprovalGate{
		cfg:          cfg,
		manager:      manager,
		now:          time.Now,
		pollInterval: 50 * time.Millisecond,
		rejected:     make(map[string]ApprovalOutcome),
		tickets:      make(map[string]*ApprovalTicket),
	}
}

// SetTokenDelivery 挂上令牌签发器与投递端。两者都非空时，批准后会把一次性
// 令牌送到执行面；缺一不可，否则宁可不下发（执行面会失败关闭）。
func (g *ApprovalGate) SetTokenDelivery(issuer *ApprovalTokenIssuer, sink ApprovalTokenSink) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.issuer = issuer
	g.sink = sink
	g.mu.Unlock()
}

func (g *ApprovalGate) Config() ApprovalConfig {
	return g.cfg
}

func (g *ApprovalGate) SetClock(now func() time.Time) {
	if now != nil {
		g.now = now
	}
}

func (g *ApprovalGate) SetPollInterval(d time.Duration) {
	if d > 0 {
		g.pollInterval = d
	}
}

func (g *ApprovalGate) SetAlertHook(fn func(string)) {
	g.alert = fn
}

func (g *ApprovalGate) RequestCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests
}

// Classify 见 classify.go：抽出去是为了让 resolver 能在 system 层拒绝之前
// 也问一次「这算不算高危」，判定顺序改了但判定逻辑只有一份。

func (g *ApprovalGate) Begin(ctx context.Context, req Request, tags []string) (*ApprovalTicket, error) {
	kind, ruleID := g.Classify(req.Tool, tags, req.Args)
	if kind == HighRiskNone {
		return nil, nil
	}

	key := approvalKey(req)
	g.mu.Lock()
	if out, ok := g.rejected[key]; ok {
		g.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrApprovalRejected, out.RuleID)
	}
	if existing, ok := g.tickets[key]; ok {
		if g.now().Before(existing.ExpiresAt) {
			// 同一调用（同任务、同工具、同参数）已经有工单在等人批复：
			// 直接复用，不再建第二张。
			g.mu.Unlock()
			return existing, nil
		}
		delete(g.tickets, key)
	}
	manager := g.manager
	g.mu.Unlock()

	if manager == nil {
		g.notify("policy: approval service unavailable for high-risk tool " + req.Tool)
		return nil, ErrApprovalUnavailable
	}

	created, err := manager.CreateRequest(ctx, &agentapproval.ApprovalRequest{
		Type:           agentapproval.ApprovalTypeToolCall,
		Priority:       agentapproval.PriorityHigh,
		AgentID:        approvalAgent(req),
		SessionID:      req.TaskID,
		Description:    fmt.Sprintf("high-risk tool %s (%s) requires approval", req.Tool, kind),
		RiskLevel:      "high",
		RiskReason:     "policy.high_risk:" + string(kind),
		AutoApprove:    false,
		TimeoutSeconds: g.cfg.TimeoutSeconds,
		Metadata: map[string]interface{}{
			"rule_id":    ruleID,
			"kind":       string(kind),
			"tool":       req.Tool,
			"stage":      req.Stage,
			"tenant_id":  req.TenantID,
			"project_id": req.ProjectID,
			"task_id":    req.TaskID,
			"subtask_id": req.SubtaskID,
		},
	})
	if err != nil {
		g.notify("policy: failed to create approval request for " + req.Tool + ": " + err.Error())
		return nil, ErrApprovalUnavailable
	}

	g.mu.Lock()
	g.requests++
	g.mu.Unlock()

	ticket := &ApprovalTicket{
		ID:          created.ID,
		Kind:        kind,
		RuleID:      ruleID,
		Tool:        req.Tool,
		Stage:       req.Stage,
		RequestedAt: g.now(),
		ExpiresAt:   g.now().Add(time.Duration(g.cfg.TimeoutSeconds) * time.Second),
		TenantID:    req.TenantID,
		ProjectID:   req.ProjectID,
		TaskID:      req.TaskID,
		ArgsDigest:  ArgumentsDigest(req.Args),
		key:         key,
	}
	g.mu.Lock()
	g.tickets[key] = ticket
	g.mu.Unlock()
	return ticket, nil
}

func (g *ApprovalGate) Await(ctx context.Context, ticket *ApprovalTicket) ApprovalOutcome {
	out := ApprovalOutcome{
		Required:   true,
		Status:     agentapproval.StatusPending,
		ApprovalID: ticket.ID,
		Kind:       ticket.Kind,
		RuleID:     ticket.RuleID,
	}

	if g.manager == nil {
		out.Code = CodeApprovalUnavailable
		out.Err = ErrApprovalUnavailable
		out.Reason = "approval service unavailable"
		return out
	}

	ticker := time.NewTicker(g.pollInterval)
	defer ticker.Stop()

	for {
		st, err := g.status(ticket.ID)
		if err != nil {
			out.Code = CodeApprovalUnavailable
			out.Err = ErrApprovalUnavailable
			out.Reason = err.Error()
			g.notify("policy: approval status unavailable for " + ticket.ID + ": " + err.Error())
			return out
		}
		switch st {
		case agentapproval.StatusApproved:
			// 批准生效的最后一步：把「这一次调用」签成一次性令牌送到执行面。
			// 送不到就不算放行——执行面没有令牌会照样拒绝，这里必须先把
			// 失败说清楚，而不是返回 allowed 让调用端以为能跑。
			if err := g.deliverToken(ctx, ticket); err != nil {
				out.Status = agentapproval.StatusApproved
				out.Code = CodeApprovalUnavailable
				out.Err = ErrApprovalUnavailable
				out.Reason = "approval approved but token delivery failed: " + err.Error()
				g.notify("policy: approval token delivery failed for " + ticket.ID + ": " + err.Error())
				g.forgetTicket(ticket)
				return out
			}
			out.Allowed = true
			out.Status = agentapproval.StatusApproved
			out.Reason = "approval approved"
			g.forgetTicket(ticket)
			return out
		case agentapproval.StatusRejected:
			out.Status = agentapproval.StatusRejected
			out.Code = CodeApprovalRejected
			out.Err = ErrApprovalRejected
			out.Reason = "approval rejected"
			g.rememberRejected(ticket, out)
			g.forgetTicket(ticket)
			return out
		case agentapproval.StatusCancelled, agentapproval.StatusTimeout:
			out.Status = st
			if st == agentapproval.StatusTimeout {
				out.Code = CodeApprovalTimeout
				out.Err = ErrApprovalTimeout
			} else {
				out.Code = CodeApprovalCancelled
				out.Err = ErrApprovalCancelled
			}
			out.Reason = "approval " + string(st)
			g.forgetTicket(ticket)
			return out
		}

		if !g.now().Before(ticket.ExpiresAt) {
			_ = g.manager.ExpireRequest(ctx, ticket.ID)
			out.Status = agentapproval.StatusTimeout
			out.Code = CodeApprovalTimeout
			out.Err = ErrApprovalTimeout
			out.Reason = "approval timed out; the call is not allowed"
			g.forgetTicket(ticket)
			return out
		}

		select {
		case <-ctx.Done():
			_ = g.manager.CancelRequest(context.Background(), ticket.ID, "")
			out.Status = agentapproval.StatusCancelled
			out.Code = CodeApprovalCancelled
			out.Err = ErrApprovalCancelled
			out.Reason = "approval cancelled"
			g.forgetTicket(ticket)
			return out
		case <-ticker.C:
		}
	}
}

// deliverToken 给一次已批准的调用签一次性令牌并投递。未装配签发器/投递端时
// 什么都不做（离线单元测试与旧行为），装配了就必须成功。
func (g *ApprovalGate) deliverToken(ctx context.Context, ticket *ApprovalTicket) error {
	g.mu.Lock()
	issuer, sink := g.issuer, g.sink
	g.mu.Unlock()
	if issuer == nil || sink == nil || ticket == nil {
		return nil
	}
	token, _, err := issueTicketToken(issuer, ticket)
	if err != nil {
		return err
	}
	return RegisterApprovalTokenWithRetry(ctx, sink, token)
}

func (g *ApprovalGate) forgetTicket(ticket *ApprovalTicket) {
	if ticket == nil {
		return
	}
	g.mu.Lock()
	if cur, ok := g.tickets[ticket.key]; ok && cur.ID == ticket.ID {
		delete(g.tickets, ticket.key)
	}
	g.mu.Unlock()
}

// issueTicketToken 把工单钉成令牌载荷。gate 与 HTTP ApprovalClient 共用同一
// 份载荷构造，避免两边签出不同绑定。
func issueTicketToken(issuer *ApprovalTokenIssuer, ticket *ApprovalTicket) (string, ApprovalTokenClaims, error) {
	return issuer.Issue(ApprovalTokenClaims{
		TenantID:   ticket.TenantID,
		ProjectID:  ticket.ProjectID,
		TaskID:     ticket.TaskID,
		Tool:       ticket.Tool,
		Stage:      ticket.Stage,
		ArgsDigest: ticket.ArgsDigest,
		ApprovalID: ticket.ID,
	})
}

func (g *ApprovalGate) Authorize(ctx context.Context, req Request, tags []string) ApprovalOutcome {
	ticket, err := g.Begin(ctx, req, tags)
	if err != nil {
		out := ApprovalOutcome{
			Required: true,
			Status:   agentapproval.StatusPending,
			Err:      err,
			Reason:   err.Error(),
		}
		switch {
		case errors.Is(err, ErrApprovalRejected):
			out.Code = CodeApprovalRejected
			out.Status = agentapproval.StatusRejected
			out.Kind, out.RuleID = g.Classify(req.Tool, tags, req.Args)
		case errors.Is(err, ErrApprovalUnavailable):
			out.Code = CodeApprovalUnavailable
		default:
			out.Code = CodeApprovalRequired
		}
		return out
	}
	if ticket == nil {
		return ApprovalOutcome{Allowed: true, Reason: "approval not required"}
	}
	return g.Await(ctx, ticket)
}

func (g *ApprovalGate) Status(ctx context.Context, approvalID string) (agentapproval.ApprovalStatus, error) {
	return g.status(approvalID)
}

func (g *ApprovalGate) Cancel(ctx context.Context, approvalID string) error {
	if g.manager == nil {
		return ErrApprovalUnavailable
	}
	return g.manager.CancelRequest(ctx, approvalID, "")
}

func (g *ApprovalGate) status(approvalID string) (agentapproval.ApprovalStatus, error) {
	if g.manager == nil {
		return agentapproval.StatusPending, ErrApprovalUnavailable
	}
	req, err := g.manager.GetRequest(approvalID)
	if err != nil {
		return agentapproval.StatusPending, ErrApprovalUnavailable
	}
	return req.Status, nil
}

func (g *ApprovalGate) rememberRejected(ticket *ApprovalTicket, out ApprovalOutcome) {
	g.mu.Lock()
	g.rejected[ticket.key] = out
	g.mu.Unlock()
}

func (g *ApprovalGate) notify(reason string) {
	if g.alert != nil {
		g.alert(reason)
	}
}

func approvalKey(req Request) string {
	return strings.Join([]string{
		req.TenantID,
		req.ProjectID,
		req.TaskID,
		req.Stage,
		req.Tool,
		strings.Join(req.Args.Paths, "\x00"),
		strings.Join(req.Args.Command, "\x00"),
		req.Args.NetworkMethod,
		fmt.Sprintf("%.6f", req.Args.EstimatedCost),
	}, "|")
}

func approvalAgent(req Request) string {
	if req.AgentID != "" {
		return req.AgentID
	}
	if req.RoleName != "" {
		return req.RoleName
	}
	return "harness-loop"
}

func isWriteMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

func isDeployInvocation(tool string, command []string) bool {
	if strings.HasPrefix(tool, "deploy") || strings.HasPrefix(tool, "release") {
		return true
	}
	if len(command) >= 2 && command[0] == "kubectl" && command[1] == "apply" {
		return true
	}
	return false
}

func isDeleteInvocation(tool string, paths, command []string) bool {
	for _, marker := range []string{"delete", "remove", "unlink", "rmdir"} {
		if strings.Contains(tool, marker) {
			return true
		}
	}
	if len(command) > 0 {
		switch strings.ToLower(command[0]) {
		case "rm", "del", "rmdir", "remove-item":
			return true
		}
	}
	for _, p := range paths {
		if strings.HasSuffix(strings.ToLower(p), ".delete") {
			return true
		}
	}
	return false
}
