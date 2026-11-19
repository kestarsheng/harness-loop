package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	agentapproval "agent-platform/pkg/agent/approval"
	"agent-platform/pkg/harnessloop/policy"
)

// ApprovalClientConfig configures the HTTP approval client that bridges the
// harness loop gate to agent-service's approval API.
type ApprovalClientConfig struct {
	Address        string
	RequestTimeout time.Duration
	Timeout        time.Duration
	PollInterval   time.Duration
	Approval       policy.ApprovalConfig
	Now            func() time.Time

	// TokenIssuer/TokenSink 是「批准真正生效」的下半场：批准后签发一次性
	// 令牌并注册到执行面（mcp-service）。只在跨进程部署时装配；未装配时保持
	// 老的「只回一个 allowed」行为（执行面会失败关闭）。
	TokenIssuer *policy.ApprovalTokenIssuer
	TokenSink   policy.ApprovalTokenSink
}

// ApprovalClient implements ToolApprovalGate over HTTP. The approval queue
// lives in agent-service (U17-A); this client only creates a request, polls
// its status and closes stale requests. It never allows a call on timeout,
// rejection or an unreachable service.
type ApprovalClient struct {
	baseURL      string
	http         *http.Client
	classifier   *policy.ApprovalGate
	timeout      time.Duration
	pollInterval time.Duration
	now          func() time.Time

	mu       sync.Mutex
	rejected map[string]policy.ApprovalOutcome
	keys     map[string]string
	// tickets 让「同一调用」（同租户/项目/任务/阶段/工具/参数）复用同一张
	// pending 工单。重复建单就是工单风暴的来源：模型多轮重试、预检与闸门
	// 各问一次，都会走到 Begin。
	tickets map[string]*pendingTicket

	issuer *policy.ApprovalTokenIssuer
	sink   policy.ApprovalTokenSink
}

// pendingTicket 是去重的占位：第一个 Begin 负责建单，其余并发调用等它建完
// 直接拿同一张工单，而不是各自再发一次 /approval/request。
type pendingTicket struct {
	ready  chan struct{}
	ticket *policy.ApprovalTicket
	err    error
}

func NewApprovalClient(cfg ApprovalClientConfig) (*ApprovalClient, error) {
	address := strings.TrimSpace(cfg.Address)
	if address == "" {
		return nil, errors.New("approval client: address is required")
	}
	if !strings.HasPrefix(address, "http://") && !strings.HasPrefix(address, "https://") {
		address = "http://" + address
	}
	if cfg.Approval.TimeoutSeconds <= 0 {
		cfg.Approval = policy.DefaultApprovalConfig()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = time.Duration(cfg.Approval.TimeoutSeconds) * time.Second
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 10 * time.Second
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	classifier := policy.NewApprovalGate(cfg.Approval, nil)
	return &ApprovalClient{
		baseURL:      strings.TrimRight(address, "/"),
		http:         &http.Client{Timeout: requestTimeout},
		classifier:   classifier,
		timeout:      timeout,
		pollInterval: poll,
		now:          now,
		rejected:     make(map[string]policy.ApprovalOutcome),
		keys:         make(map[string]string),
		tickets:      make(map[string]*pendingTicket),
		issuer:       cfg.TokenIssuer,
		sink:         cfg.TokenSink,
	}, nil
}

// Begin classifies the call and creates a pending request in agent-service.
// A non-high-risk call returns (nil, nil): approval is not required.
func (c *ApprovalClient) Begin(ctx context.Context, req policy.Request, tags []string) (*policy.ApprovalTicket, error) {
	if c == nil {
		return nil, policy.ErrApprovalUnavailable
	}
	kind, ruleID := c.classifier.Classify(req.Tool, tags, req.Args)
	if kind == policy.HighRiskNone {
		return nil, nil
	}

	key := approvalRequestKey(req)
	c.mu.Lock()
	out, rejected := c.rejected[key]
	if rejected {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", policy.ErrApprovalRejected, out.RuleID)
	}
	if existing, ok := c.tickets[key]; ok {
		c.mu.Unlock()
		<-existing.ready
		if existing.err != nil {
			return nil, existing.err
		}
		// 复用而不新建：同一调用（同租户/项目/任务/阶段/工具/参数）只应
		// 有一张工单，否则模型每轮重试都会往审批队列里灌一张。
		log.Printf("[approval] reusing pending ticket %s for %s/%s", existing.ticket.ID, req.TaskID, req.Tool)
		return existing.ticket, nil
	}
	entry := &pendingTicket{ready: make(chan struct{})}
	c.tickets[key] = entry
	c.mu.Unlock()

	ticket, err := c.createTicket(ctx, req, key, kind, ruleID)

	c.mu.Lock()
	if err != nil {
		delete(c.tickets, key)
		entry.err = err
	} else {
		entry.ticket = ticket
		c.keys[ticket.ID] = key
	}
	close(entry.ready)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return ticket, nil
}

// createTicket 建单并返回带完整绑定的工单。绑定（task/tool/args_digest）就是
// 之后签发令牌时要钉死的东西，所以建单时一次算好。
func (c *ApprovalClient) createTicket(ctx context.Context, req policy.Request, key string, kind policy.HighRiskKind, ruleID string) (*policy.ApprovalTicket, error) {
	id, err := c.create(ctx, req, kind, ruleID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", policy.ErrApprovalUnavailable, err)
	}
	return &policy.ApprovalTicket{
		ID:          id,
		Kind:        kind,
		RuleID:      ruleID,
		Tool:        req.Tool,
		Stage:       req.Stage,
		RequestedAt: c.now(),
		ExpiresAt:   c.now().Add(c.timeout),
		TenantID:    req.TenantID,
		ProjectID:   req.ProjectID,
		TaskID:      req.TaskID,
		ArgsDigest:  policy.ArgumentsDigest(req.Args),
	}, nil
}

// Await polls agent-service until the human decision arrives. Timeout,
// cancellation and status errors all keep the call closed.
func (c *ApprovalClient) Await(ctx context.Context, ticket *policy.ApprovalTicket) policy.ApprovalOutcome {
	out := policy.ApprovalOutcome{
		Required:   true,
		Status:     agentapproval.StatusPending,
		ApprovalID: ticket.ID,
		Kind:       ticket.Kind,
		RuleID:     ticket.RuleID,
	}
	if c == nil {
		out.Code = policy.CodeApprovalUnavailable
		out.Err = policy.ErrApprovalUnavailable
		out.Reason = "approval client is not configured"
		return out
	}

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		status, err := c.status(ctx, ticket.ID)
		if err != nil {
			out.Code = policy.CodeApprovalUnavailable
			out.Err = policy.ErrApprovalUnavailable
			out.Reason = err.Error()
			return out
		}
		switch status {
		case agentapproval.StatusApproved:
			// 批准生效的收尾：签发一次性令牌并注册到执行面。注册失败就不
			// 放行——执行面没有令牌会照样拒，这里必须如实报错。
			if err := c.deliverToken(ctx, ticket); err != nil {
				out.Code = policy.CodeApprovalUnavailable
				out.Err = policy.ErrApprovalUnavailable
				out.Reason = "approval approved but token delivery failed: " + err.Error()
				c.forget(ticket.ID)
				return out
			}
			out.Allowed = true
			out.Status = agentapproval.StatusApproved
			out.Reason = "approval approved"
			c.forget(ticket.ID)
			return out
		case agentapproval.StatusRejected:
			out.Status = agentapproval.StatusRejected
			out.Code = policy.CodeApprovalRejected
			out.Err = policy.ErrApprovalRejected
			out.Reason = "approval rejected"
			c.rememberRejected(ticket, out)
			return out
		case agentapproval.StatusCancelled, agentapproval.StatusTimeout:
			out.Status = status
			if status == agentapproval.StatusTimeout {
				out.Code = policy.CodeApprovalTimeout
				out.Err = policy.ErrApprovalTimeout
			} else {
				out.Code = policy.CodeApprovalCancelled
				out.Err = policy.ErrApprovalCancelled
			}
			out.Reason = "approval " + string(status)
			c.forget(ticket.ID)
			return out
		}

		if !c.now().Before(ticket.ExpiresAt) {
			c.closeRemote(ticket.ID, "harness approval timed out; the tool call is not allowed")
			out.Status = agentapproval.StatusTimeout
			out.Code = policy.CodeApprovalTimeout
			out.Err = policy.ErrApprovalTimeout
			out.Reason = "approval timed out; the call is not allowed"
			c.forget(ticket.ID)
			return out
		}
		select {
		case <-ctx.Done():
			c.closeRemote(ticket.ID, "harness cancelled the approval request")
			out.Status = agentapproval.StatusCancelled
			out.Code = policy.CodeApprovalCancelled
			out.Err = policy.ErrApprovalCancelled
			out.Reason = "approval cancelled"
			c.forget(ticket.ID)
			return out
		case <-ticker.C:
		}
	}
}

// Authorize is Begin + Await, the blocking contract used inside a stage.
func (c *ApprovalClient) Authorize(ctx context.Context, req policy.Request, tags []string) policy.ApprovalOutcome {
	ticket, err := c.Begin(ctx, req, tags)
	if err != nil {
		kind, ruleID := c.classifier.Classify(req.Tool, tags, req.Args)
		out := policy.ApprovalOutcome{
			Required: true,
			Status:   agentapproval.StatusPending,
			Kind:     kind,
			RuleID:   ruleID,
			Err:      err,
			Reason:   err.Error(),
		}
		switch {
		case errors.Is(err, policy.ErrApprovalRejected):
			out.Code = policy.CodeApprovalRejected
			out.Status = agentapproval.StatusRejected
		case errors.Is(err, policy.ErrApprovalUnavailable):
			out.Code = policy.CodeApprovalUnavailable
		default:
			out.Code = policy.CodeApprovalRequired
		}
		return out
	}
	if ticket == nil {
		return policy.ApprovalOutcome{Allowed: true, Reason: "approval not required"}
	}
	return c.Await(ctx, ticket)
}

// Status exposes a single request's state (used by the handler and tests).
func (c *ApprovalClient) Status(ctx context.Context, approvalID string) (agentapproval.ApprovalStatus, error) {
	return c.status(ctx, approvalID)
}

func (c *ApprovalClient) create(ctx context.Context, req policy.Request, kind policy.HighRiskKind, ruleID string) (string, error) {
	body := map[string]any{
		"type":            string(agentapproval.ApprovalTypeToolCall),
		"priority":        string(agentapproval.PriorityHigh),
		"agent_id":        approvalAgentID(req),
		"session_id":      req.TaskID,
		"description":     fmt.Sprintf("high-risk tool %s (%s) requires approval", req.Tool, kind),
		"risk_level":      "high",
		"risk_reason":     "policy.high_risk:" + string(kind),
		"auto_approve":    false,
		"timeout_seconds": int(c.timeout / time.Second),
		"metadata": map[string]any{
			"rule_id":    ruleID,
			"kind":       string(kind),
			"tool":       req.Tool,
			"stage":      req.Stage,
			"tenant_id":  req.TenantID,
			"project_id": req.ProjectID,
			"task_id":    req.TaskID,
			"subtask_id": req.SubtaskID,
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/approval/request", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, err := decodeApprovalResponse(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || payload.Code != 0 {
		return "", fmt.Errorf("approval request rejected: status=%d code=%d message=%s", resp.StatusCode, payload.Code, payload.Message)
	}
	if payload.Data.RequestID == "" {
		return "", errors.New("approval request response has no request_id")
	}
	return payload.Data.RequestID, nil
}

func (c *ApprovalClient) status(ctx context.Context, approvalID string) (agentapproval.ApprovalStatus, error) {
	if approvalID == "" {
		return "", errors.New("approval id is empty")
	}
	url := c.baseURL + "/approval/status?request_id=" + url.QueryEscape(approvalID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, err := decodeApprovalResponse(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || payload.Code != 0 {
		return "", fmt.Errorf("approval status unavailable: status=%d code=%d message=%s", resp.StatusCode, payload.Code, payload.Message)
	}
	return parseApprovalStatus(payload.Data.Status)
}

// closeRemote marks a timed-out or cancelled request terminal in agent-service
// so a stale pending row does not linger in the operator queue.
func (c *ApprovalClient) closeRemote(approvalID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _ := json.Marshal(map[string]any{
		"request_id": approvalID,
		"user_id":    "harness-loop-gate",
		"reason":     reason,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/approval/reject", bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// deliverToken 给已批准的调用签一次性令牌并注册到执行面。未装配签发器/执行面
// 时保持旧行为（不发令牌）；装配了就必须成功。
func (c *ApprovalClient) deliverToken(ctx context.Context, ticket *policy.ApprovalTicket) error {
	if c == nil || c.issuer == nil || c.sink == nil || ticket == nil {
		return nil
	}
	token, _, err := c.issuer.Issue(policy.ApprovalTokenClaims{
		TenantID:   ticket.TenantID,
		ProjectID:  ticket.ProjectID,
		TaskID:     ticket.TaskID,
		Tool:       ticket.Tool,
		Stage:      ticket.Stage,
		ArgsDigest: ticket.ArgsDigest,
		ApprovalID: ticket.ID,
	})
	if err != nil {
		return err
	}
	if err := policy.RegisterApprovalTokenWithRetry(ctx, c.sink, token); err != nil {
		return err
	}
	log.Printf("[approval] token issued and delivered for approval %s (task=%s tool=%s)", ticket.ID, ticket.TaskID, ticket.Tool)
	return nil
}

func (c *ApprovalClient) rememberRejected(ticket *policy.ApprovalTicket, out policy.ApprovalOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key, ok := c.keys[ticket.ID]
	if !ok {
		key = approvalTicketKey(ticket)
	}
	c.rejected[key] = out
	delete(c.keys, ticket.ID)
	delete(c.tickets, key)
}

func (c *ApprovalClient) forget(approvalID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key, ok := c.keys[approvalID]
	delete(c.keys, approvalID)
	if ok {
		delete(c.tickets, key)
	}
}

type approvalResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		RequestID       string `json:"request_id"`
		Status          string `json:"status"`
		DecidedBy       string `json:"decided_by"`
		RejectionReason string `json:"rejection_reason"`
	} `json:"data"`
}

func decodeApprovalResponse(r io.Reader) (approvalResponse, error) {
	var out approvalResponse
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		return out, fmt.Errorf("decode approval response: %w", err)
	}
	return out, nil
}

func parseApprovalStatus(raw string) (agentapproval.ApprovalStatus, error) {
	status := agentapproval.ApprovalStatus(strings.ToLower(strings.TrimSpace(raw)))
	switch status {
	case agentapproval.StatusPending, agentapproval.StatusApproved, agentapproval.StatusRejected,
		agentapproval.StatusTimeout, agentapproval.StatusCancelled, agentapproval.StatusEscalated:
		return status, nil
	default:
		return "", fmt.Errorf("unknown approval status %q", raw)
	}
}

func approvalRequestKey(req policy.Request) string {
	return strings.Join([]string{
		req.TenantID, req.ProjectID, req.TaskID, req.Stage, req.Tool,
		strings.Join(req.Args.Paths, "\x00"),
		strings.Join(req.Args.Command, "\x00"),
		req.Args.NetworkMethod,
		fmt.Sprintf("%.6f", req.Args.EstimatedCost),
	}, "|")
}

func approvalTicketKey(ticket *policy.ApprovalTicket) string {
	return strings.Join([]string{
		ticket.ID, ticket.Tool, ticket.Stage, ticket.RuleID,
	}, "|")
}

func approvalAgentID(req policy.Request) string {
	if req.AgentID != "" {
		return req.AgentID
	}
	if req.RoleName != "" {
		return req.RoleName
	}
	return "harness-loop"
}
