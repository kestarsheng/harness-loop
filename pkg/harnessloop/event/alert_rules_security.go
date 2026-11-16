package event

func evalSecurityAlerts(in AlertInput) []Alert {
	out := []Alert{}

	if in.PolicyDenyRatio > 0.2 {
		out = append(out, Alert{
			Name: "policy_deny_spike", Severity: AlertWarn,
			Detail: format("deny ratio %.2f", in.PolicyDenyRatio),
			Action: "check the stage matrix and allowlist against the plan",
		})
	}
	if in.SandboxEscape > 0 {
		out = append(out, Alert{
			Name: "sandbox_escape_attempt", Severity: AlertError,
			Detail: format("%d escape attempts", in.SandboxEscape),
			Action: "security incident: inspect the agent and prompts, abort the task if needed",
		})
	}
	if in.SelfModifyAttempts > 0 || in.PolicyHashMismatch > 0 {
		out = append(out, Alert{
			Name: "self_privilege_escalation", Severity: AlertError,
			Detail: format("self-modify=%d policy-hash-mismatch=%d", in.SelfModifyAttempts, in.PolicyHashMismatch),
			Action: "abort the task immediately and trace the audit trail",
		})
	}
	if in.ApprovalPending > 5 && in.ApprovalSustained {
		out = append(out, Alert{
			Name: "approval_backlog", Severity: AlertWarn,
			Detail: format("%d pending approvals sustained", in.ApprovalPending),
			Action: "notify approvers; consider adjusting high-risk rule thresholds",
		})
	}
	if in.PolicyLatencyP95Ms > 20 {
		out = append(out, Alert{
			Name: "policy_latency_regression", Severity: AlertWarn,
			Detail: format("policy decision p95 %.1fms", in.PolicyLatencyP95Ms),
			Action: "check rule count and decision cache hit ratio",
		})
	}
	return out
}
