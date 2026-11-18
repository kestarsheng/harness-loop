package policy

import (
	"strings"

	"agent-platform/pkg/harnessloop/policy/sandbox"
)

// HighRiskClassifier 判定一次工具调用是否属于「必须人来看一眼」的高危类。
//
// 抽成独立类型的原因：判定顺序要改。改之前 exec 白名单（system 层）先把
// `rm` / `kubectl` 这类命令拒掉，ApprovalGate.Classify 根本轮不到执行，
// require_approval 于是永远不可达。现在 resolver 在 system 层给出
// E_EXEC_DENIED 时会再问一次分类器：能归到高危类的，改成走审批；归不进去的
// 仍然按原样硬拒。
type HighRiskClassifier struct {
	cfg ApprovalConfig
}

func NewHighRiskClassifier(cfg ApprovalConfig) *HighRiskClassifier {
	return &HighRiskClassifier{cfg: cfg}
}

func (c *HighRiskClassifier) Config() ApprovalConfig {
	if c == nil {
		return DefaultApprovalConfig()
	}
	return c.cfg
}

// Classify 返回高危类别与对应的 rule id。""/none 表示不需要审批。
func (c *HighRiskClassifier) Classify(tool string, tags []string, args ToolArgs) (HighRiskKind, string) {
	cfg := c.Config()
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
	if cfg.HighRisk.Delete && isDeleteInvocation(lowerTool, args.Paths, args.Command) && deleteScopeEligible(tags, args) {
		return HighRiskDelete, "policy.approval.high_risk.delete"
	}
	if cfg.HighRisk.Cost && cfg.CostThreshold > 0 && args.EstimatedCost > cfg.CostThreshold {
		return HighRiskCost, "policy.approval.high_risk.cost"
	}
	return HighRiskNone, ""
}

// deleteScopeEligible 回答「这次删除是否落在可以走审批的范围里」。
//
// 原来只有 write 标签的工具才算 delete 类，于是 run_command + `rm` 这种最典型
// 的破坏性调用压根进不了分类。现在 exec 类命令只要带命令实参就参与分类；
// 但越出工作区的目标（绝对路径 / .. 上跳 / shell 元字符）不在这里放行——
// 那类命令没有「人工批准后还能收场」的余地，必须留在 sandbox.exec 硬拒。
func deleteScopeEligible(tags []string, args ToolArgs) bool {
	if contains(tags, TagWrite) {
		return true
	}
	if len(args.Command) == 0 {
		return false
	}
	return !commandArgsEscape(args.Command)
}

// commandArgsEscape 判断命令是否指向工作区之外（绝对路径 / 上跳 / 元字符）。
// 空命令不参与判定，避免下标越界。
func commandArgsEscape(command []string) bool {
	if len(command) <= 1 {
		return false
	}
	return sandbox.HasEscapingArgs(command[1:])
}

// ApprovalRequiredByPolicy 报告 resolver 是否要把一次 system 层拒绝改判成审批。
// 只有「命令不在 exec 白名单」这一种拒绝可以被审批接管：阶段矩阵、路径逃逸、
// 未注册工具、自我提权都属于硬线，审批不能解锁。
func (c *HighRiskClassifier) ApprovalRequiredByPolicy(sys Decision, req Request, tags []string) (Decision, bool) {
	if sys.Code != CodeExecDenied {
		return Decision{}, false
	}
	if commandArgsEscape(req.Args.Command) {
		return Decision{}, false
	}
	kind, ruleID := c.Classify(req.Tool, tags, req.Args)
	if kind == HighRiskNone {
		return Decision{}, false
	}
	return Decision{
		Allowed:          false,
		Effect:           EffectRequireApproval,
		RequiresApproval: true,
		Layer:            LayerSystem,
		RuleID:           ruleID,
		Code:             CodeApprovalRequired,
		Reason: "command is outside the exec allowlist but high-risk (" + string(kind) + "); human approval required",
		Tool:             req.Tool,
		Stage:            req.Stage,
	}, true
}

// gate 侧保留原有方法签名，内部委托给分类器，避免出现两份判定逻辑。
func (g *ApprovalGate) Classify(tool string, tags []string, args ToolArgs) (HighRiskKind, string) {
	if g == nil {
		return HighRiskNone, ""
	}
	return NewHighRiskClassifier(g.cfg).Classify(tool, tags, args)
}
