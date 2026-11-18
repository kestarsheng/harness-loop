package service

import (
	"fmt"
	"net"
	"strings"
)

// Severity is how a startup finding should be treated by the process.
type Severity string

const (
	// SeverityFatal means the service must refuse to serve: the configuration
	// cannot possibly work, and starting anyway only moves the failure to the
	// first request (where it looks like a crash).
	SeverityFatal Severity = "fatal"
	// SeverityWarn means the service can serve but a capability is dark. A
	// capability that silently does nothing is worse than one that is off, so
	// every warn is printed on startup instead of being discovered later.
	SeverityWarn Severity = "warn"
)

// Finding is one line of the startup self-check.
type Finding struct {
	Name     string
	Severity Severity
	Detail   string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s[%s]: %s", f.Severity, f.Name, f.Detail)
}

// SelfCheckInput is the assembled state of the process right before it starts
// serving. It is a plain struct so the checks stay testable without a live
// process, a database or a socket.
type SelfCheckInput struct {
	ListenAddress  string
	AgentTarget    string
	MetricsAddress string

	OrchestratorEnabled bool
	SubtaskExecutor     bool
	SubtaskRepo         bool
	StageRunner         bool

	PolicyLayersEnabled bool
	ApprovalGate        bool
	MemoryRecall        bool
}

// StartupSelfCheck validates the wiring of the P2 paths before the socket is
// opened (T-P2-35 point 5).
//
// 这组检查存在的理由不是"多一层保险"，而是三类真实故障都曾经只在运行期暴露：
// 监听地址与 agent target 撞车（自己拨自己）、metrics 与 gRPC 抢同一个端口
// （第二个 ListenAndServe 的错误被吞掉）、编排开着但执行器是 nil（子任务永远
// 不跑，父任务却按"计划完成"收口）。它们全部可以在 Listen 之前判定。
func StartupSelfCheck(in SelfCheckInput) []Finding {
	findings := make([]Finding, 0, 6)

	listen := strings.TrimSpace(in.ListenAddress)
	if listen == "" {
		findings = append(findings, Finding{"listen_address", SeverityFatal, "listen address is empty"})
	} else if _, _, err := net.SplitHostPort(listen); err != nil {
		findings = append(findings, Finding{"listen_address", SeverityFatal,
			fmt.Sprintf("%q is not a host:port address: %v", listen, err)})
	}

	// 自己拨自己：agent target 与监听地址相同时，子任务执行器会把请求打回
	// harnessloop 自己的 gRPC 端口，表现是子任务永久超时，而不是报错。
	if listen != "" && strings.EqualFold(listen, strings.TrimSpace(in.AgentTarget)) {
		findings = append(findings, Finding{"agent_target", SeverityFatal,
			fmt.Sprintf("agent target %q is the service's own listen address", in.AgentTarget)})
	}

	// metrics 与 gRPC 抢端口：Registry.Serve 在 goroutine 里吞掉 ListenAndServe
	// 的错误，抢输的一方只是"没有 /metrics"，很难被发现。
	metrics := strings.TrimSpace(in.MetricsAddress)
	if metrics != "" && !strings.EqualFold(metrics, "off") {
		if _, mPort, mErr := net.SplitHostPort(metrics); mErr == nil {
			_, lPort, lErr := net.SplitHostPort(listen)
			if lErr == nil && mPort == lPort {
				findings = append(findings, Finding{"metrics_address", SeverityFatal,
					fmt.Sprintf("metrics %q collides with the gRPC listen port %q", metrics, listen)})
			}
		}
	}

	if in.OrchestratorEnabled {
		if !in.SubtaskExecutor {
			findings = append(findings, Finding{"orchestrator_executor", SeverityWarn,
				"orchestrator is on but no subtask executor is wired (agent-service unavailable): subtasks will not run"})
		}
		if !in.StageRunner {
			findings = append(findings, Finding{"orchestrator_stages", SeverityWarn,
				"orchestrator is on but the stage runner is missing: a/b stage output cannot be planned"})
		}
		if !in.SubtaskRepo {
			findings = append(findings, Finding{"orchestrator_subtask_repo", SeverityWarn,
				"orchestrator is on but subtask persistence is off: dashboards and queries will see zero subtasks"})
		}
	}

	if in.PolicyLayersEnabled && !in.ApprovalGate {
		findings = append(findings, Finding{"policy_approval_gate", SeverityWarn,
			"policy layers are on but no approval gate is configured: high-risk calls stay pending forever"})
	}

	return findings
}

// FatalFindings returns only the findings that must block startup.
func FatalFindings(findings []Finding) []Finding {
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if f.Severity == SeverityFatal {
			out = append(out, f)
		}
	}
	return out
}

// FormatFindings renders findings as one log line so the startup banner carries
// the whole verdict instead of scattering it over several lines.
func FormatFindings(findings []Finding) string {
	if len(findings) == 0 {
		return "selfcheck: all checks passed"
	}
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, f.String())
	}
	return "selfcheck: " + strings.Join(parts, "; ")
}
