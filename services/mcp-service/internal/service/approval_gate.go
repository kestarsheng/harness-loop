package service

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"agent-platform/pkg/harnessloop/policy"
	pb "agent-platform/pkg/pb/mcp"
	"agent-platform/services/mcp-service/internal/audit"
)

// 执行面的审批闸门（T-P2-39）。
//
// 批准发生在 harnessloop，执行发生在这里；中间隔着 agent-service，而 agent-
// service 不持有任何密钥。所以「批准是否真的生效」只能由这里裁决：
//
//  1. harnessloop 批准后签发一次性令牌，通过 RegisterApprovalToken 注册到这里；
//  2. 调用到达时先按普通策略判：放行就放行（不需要令牌）；
//  3. 被 exec 白名单拒、且属于高危可批类时，等一枚能绑定这次调用的令牌；
//  4. 令牌到了就校验签名/绑定/有效期，一次性消费 nonce，再用 ExecApproved
//     重新判一次（此时白名单让位，沙箱硬线仍生效）；
//  5. 等不到/验不过/重复消费 → 拒绝并审计。
//
// 无密钥时整条链路关闭（第 3 步不会等），调用按原策略被拒——fail closed。

const approvalWaitEnv = "MCP_APPROVAL_WAIT_SECONDS"

func approvalWaitFromEnv() time.Duration {
	seconds := 180
	if raw := strings.TrimSpace(os.Getenv(approvalWaitEnv)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			seconds = parsed
		}
	}
	return time.Duration(seconds) * time.Second
}

// RegisterApprovalToken 接收 harnessloop 签发的一次性审批令牌。只验签与
// 有效期，绑定比对留到真正执行前（那时才知道要跑什么）。
func (s *MCPService) RegisterApprovalToken(ctx context.Context, req *pb.RegisterApprovalTokenRequest) (*pb.RegisterApprovalTokenResponse, error) {
	if s == nil || s.approvalTokens == nil {
		return &pb.RegisterApprovalTokenResponse{
			Accepted: false,
			Reason:   "approval token verification is not configured (" + policy.ApprovalTokenSecretEnv + ")",
		}, nil
	}
	claims, err := s.approvalTokens.Register(strings.TrimSpace(req.GetToken()))
	if err != nil {
		audit.Emit(audit.Record{
			Tool:       "approval_token",
			Decision:   audit.DecisionDeny,
			Code:       firstNonEmpty(policy.TokenErrorCode(err), policy.CodeApprovalTokenInvalid),
			Reason:     err.Error(),
			ApprovalID: claims.ApprovalID,
			TaskID:     claims.TaskID,
		})
		return &pb.RegisterApprovalTokenResponse{Accepted: false, Reason: err.Error()}, nil
	}
	audit.Emit(audit.Record{
		Tool:       "approval_token",
		Decision:   audit.DecisionAllow,
		Code:       "E_APPROVAL_TOKEN_REGISTERED",
		Reason:     "approval token registered",
		ApprovalID: claims.ApprovalID,
		TaskID:     claims.TaskID,
		Nonce:      claims.Nonce,
	})
	return &pb.RegisterApprovalTokenResponse{Accepted: true, Nonce: claims.Nonce}, nil
}

// authorizeWithApproval 是 CallTool 的策略入口：先按普通规则判，只有「高危
// 且可批」的拒绝才会去等令牌。返回最终决策与（若用到）令牌 claims。
func (s *MCPService) authorizeWithApproval(ctx context.Context, req *pb.CallToolRequest, in ToolPolicyInput) (ToolPolicyResult, *policy.ApprovalTokenClaims) {
	if s == nil {
		return ToolPolicyResult{Allowed: false, Code: policy.CodeToolDenied, Reason: "mcp service is not configured"}, nil
	}
	s.mu.RLock()
	toolPolicy := s.toolPolicy
	s.mu.RUnlock()
	if toolPolicy == nil {
		return ToolPolicyResult{Allowed: true, Reason: "tool policy is not configured"}, nil
	}

	decision := toolPolicy.Authorize(ctx, in)
	if decision.Allowed {
		return decision, nil
	}

	claims, gateErr := s.awaitApprovalToken(ctx, req, in, decision)
	if gateErr != nil {
		code := firstNonEmpty(policy.TokenErrorCode(gateErr), policy.CodeApprovalTokenMissing)
		return ToolPolicyResult{
			Allowed: false,
			Code:    code,
			RuleID:  decision.RuleID,
			Layer:   decision.Layer,
			Reason:  gateErr.Error(),
		}, nil
	}
	if claims == nil {
		return decision, nil
	}

	// 令牌已消费：用 ExecApproved 重判一次。白名单让位，硬线照旧。
	approvedIn := in
	approvedIn.ExecApproved = true
	final := toolPolicy.Authorize(ctx, approvedIn)
	audit.Emit(audit.Record{
		Tool:          in.Tool,
		Decision:      audit.DecisionAllow,
		Code:          "E_APPROVAL_TOKEN_CONSUMED",
		Reason:        "one-time approval token verified and consumed",
		WorkspaceRoot: in.WorkspaceRoot,
		TaskID:        in.TaskID,
		ApprovalID:    claims.ApprovalID,
		ApprovalRef:   approvalRefFor(in),
		Nonce:         claims.Nonce,
	})
	return final, claims
}

// awaitApprovalToken 判定这次拒绝是否值得等、等多久，并消费一枚能绑定它的
// 令牌。返回 nil claims 表示「不该等」（按原决策拒绝）。
func (s *MCPService) awaitApprovalToken(ctx context.Context, req *pb.CallToolRequest, in ToolPolicyInput, decision ToolPolicyResult) (*policy.ApprovalTokenClaims, error) {
	if s.approvalTokens == nil || s.approvalWait <= 0 {
		return nil, nil
	}
	if strings.TrimSpace(in.Stage) == "" || strings.TrimSpace(in.TaskID) == "" {
		// 非 stage 调用没有审批通道，按原策略拒绝。
		return nil, nil
	}
	if decision.Code != policy.CodeExecDenied {
		// 只对「exec 白名单拒绝」开放审批；路径逃逸、跨租户、未知工具等
		// 硬线不因为批准而放行。
		return nil, nil
	}

	pc := policyContextFromToolConfig(req.GetToolConfig())
	toolName := harnessToolName(in.Tool, in.Tags)
	args := policy.CanonicalArgsFromJSON([]byte(req.GetArguments()))
	classifier := policy.NewApprovalGate(policy.DefaultApprovalConfig(), nil)
	if kind, _ := classifier.Classify(toolName, in.Tags, args); kind == policy.HighRiskNone {
		return nil, nil
	}

	wait := s.approvalWait
	if pc.ApprovalWaitSeconds > 0 {
		wait = time.Duration(pc.ApprovalWaitSeconds) * time.Second
	}
	if wait > 15*time.Minute {
		wait = 15 * time.Minute
	}

	expected := policy.ApprovalTokenExpectation{
		TenantID:   in.TenantID,
		ProjectID:  in.ProjectID,
		TaskID:     in.TaskID,
		Tool:       toolName,
		ArgsDigest: policy.ArgumentsDigest(args),
		ApprovalID: pc.ApprovalID,
	}
	claims, err := s.approvalTokens.WaitFor(ctx, expected, wait)
	if err != nil {
		// 审计由 CallTool 统一写（同一条拒绝只留一条痕），这里只回报错误。
		return nil, err
	}

	expected.ApprovalID = claims.ApprovalID
	consumed, err := s.approvalTokens.Consume(expected)
	if err != nil {
		return nil, err
	}
	return &consumed, nil
}

func approvalRefFor(in ToolPolicyInput) string {
	args := policy.ToolArgs{Paths: in.Paths, Command: in.Command}
	return policy.ApprovalRef(in.TaskID, harnessToolName(in.Tool, in.Tags), policy.ArgumentsDigest(args))
}

// setApprovalStore 供测试注入固定时钟/自定义 store。
func (s *MCPService) setApprovalStore(store *ApprovalTokenStore, wait time.Duration) {
	s.mu.Lock()
	s.approvalTokens = store
	s.approvalWait = wait
	s.mu.Unlock()
}
