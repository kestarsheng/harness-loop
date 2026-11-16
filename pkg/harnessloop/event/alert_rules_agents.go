package event

func evalAgentAlerts(in AlertInput) []Alert {
	out := []Alert{}

	if in.SubtaskCircuitOpen > 2 {
		out = append(out, Alert{
			Name: "subtask_circuit_open", Severity: AlertWarn,
			Detail: format("%d circuit openings", in.SubtaskCircuitOpen),
			Action: "inspect the class_key failures and the role tool set",
		})
	}
	if in.SubtaskQueueDepth > 40 && in.SubtaskQueueSustained {
		out = append(out, Alert{
			Name: "subtask_queue_backlog", Severity: AlertWarn,
			Detail: format("subtask queue depth %.0f sustained", in.SubtaskQueueDepth),
			Action: "check tenant quotas and parent task count; consider widening slots",
		})
	}
	if in.AggregationConflict > 0 {
		out = append(out, Alert{
			Name: "aggregation_conflict", Severity: AlertWarn,
			Detail: format("%d aggregation conflicts", in.AggregationConflict),
			Action: "check whether one plan splits same-path units",
		})
	}
	if in.SubtaskFailedTotal >= 10 && in.SubtaskTimeoutRatio > 0.3 {
		out = append(out, Alert{
			Name: "subtask_timeout_ratio", Severity: AlertWarn,
			Detail: format("timeout ratio %.2f over %d failures", in.SubtaskTimeoutRatio, in.SubtaskFailedTotal),
			Action: "check timeout config and model responsiveness",
		})
	}
	if in.ModelFallback > 10 {
		out = append(out, Alert{
			Name: "model_fallback_spike", Severity: AlertWarn,
			Detail: format("%d fallbacks", in.ModelFallback),
			Action: "check provider status and the fallback chain",
		})
	}
	if in.ModelUnavailable >= 2 {
		out = append(out, Alert{
			Name: "model_unavailable", Severity: AlertError,
			Detail: format("%d unavailable models", in.ModelUnavailable),
			Action: "switch the primary model or widen the fallback chain",
		})
	}
	if in.ModelCapabilityReject > 0 {
		out = append(out, Alert{
			Name: "model_capability_rejected", Severity: AlertError,
			Detail: format("%d capability rejections", in.ModelCapabilityReject),
			Action: "the stage override picked a model that lacks the required capability",
		})
	}
	return out
}
