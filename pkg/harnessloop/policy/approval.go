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
	key         string
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
}

func NewApprovalGate(cfg ApprovalConfig, manager *agentapproval.ApprovalFlowManager) *ApprovalGate {
	return &ApprovalGate{
		cfg:          cfg,
		manager:      manager,
		now:          time.Now,
		pollInterval: 50 * time.Millisecond,
		rejected:     make(map[string]ApprovalOutcome),
	}
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

func (g *ApprovalGate) Classify(tool string, tags []string, args ToolArgs) (HighRiskKind, string) {
	cfg := g.cfg
	if !cfg.Enabled {
		return HighRiskNone, ""
	}
	lowerTool := strings.ToLower(tool)
	if cfg.HighRisk.Deploy && isDeployInvocation(lowerTool, args.Command) {
		return HighRiskDeploy, "policy.approval.high_risk.deploy"
	}
	if cfg.HighRisk.NetworkWrite && contains(tags, TagNetwork) && isWriteMethod(args.NetworkMethod) {
		return HighRiskNetworkWrite, "policy.approval.high_risk.network_write"
	}
	if cfg.HighRisk.Delete && contains(tags, TagWrite) && isDeleteInvocation(lowerTool, args.Paths, args.Command) {
		return HighRiskDelete, "policy.approval.high_risk.delete"
	}
	if cfg.HighRisk.Cost && cfg.CostThreshold > 0 && args.EstimatedCost > cfg.CostThreshold {
		return HighRiskCost, "policy.approval.high_risk.cost"
	}
	return HighRiskNone, ""
}

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

	return &ApprovalTicket{
		ID:          created.ID,
		Kind:        kind,
		RuleID:      ruleID,
		Tool:        req.Tool,
		Stage:       req.Stage,
		RequestedAt: g.now(),
		ExpiresAt:   g.now().Add(time.Duration(g.cfg.TimeoutSeconds) * time.Second),
		key:         key,
	}, nil
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
			out.Allowed = true
			out.Status = agentapproval.StatusApproved
			out.Reason = "approval approved"
			return out
		case agentapproval.StatusRejected:
			out.Status = agentapproval.StatusRejected
			out.Code = CodeApprovalRejected
			out.Err = ErrApprovalRejected
			out.Reason = "approval rejected"
			g.rememberRejected(ticket, out)
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
			return out
		}

		if !g.now().Before(ticket.ExpiresAt) {
			_ = g.manager.ExpireRequest(ctx, ticket.ID)
			out.Status = agentapproval.StatusTimeout
			out.Code = CodeApprovalTimeout
			out.Err = ErrApprovalTimeout
			out.Reason = "approval timed out; the call is not allowed"
			return out
		}

		select {
		case <-ctx.Done():
			_ = g.manager.CancelRequest(context.Background(), ticket.ID, "")
			out.Status = agentapproval.StatusCancelled
			out.Code = CodeApprovalCancelled
			out.Err = ErrApprovalCancelled
			out.Reason = "approval cancelled"
			return out
		case <-ticker.C:
		}
	}
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
