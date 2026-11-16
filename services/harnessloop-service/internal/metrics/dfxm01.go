package metrics

const (
	MetricTasksTotal              = "hlp_tasks_total"
	MetricStageDurationSeconds    = "hlp_stage_duration_seconds"
	MetricAcceptanceTotal         = "hlp_acceptance_total"
	MetricErrorsTotal             = "hlp_errors_total"
	MetricRetryCount              = "hlp_retry_count"
	MetricCheckpointTotal         = "hlp_checkpoint_total"
	MetricCompactionTotal         = "hlp_compaction_total"
	MetricCompactionRatio         = "hlp_compaction_ratio"
	MetricIndexQueueDepth         = "hlp_index_queue_depth"
	MetricIndexDurationSeconds    = "hlp_index_duration_seconds"
	MetricRecallLatencyMs         = "hlp_recall_latency_ms"
	MetricRecallHitTotal          = "hlp_recall_hit_total"
	MetricEventLagSeconds         = "hlp_event_lag_seconds"
	MetricInjectTokensRatio       = "hlp_inject_tokens_ratio"
	MetricSubtasksTotal           = "hlp_subtasks_total"
	MetricSubtaskDurationSeconds  = "hlp_subtask_duration_seconds"
	MetricSubtaskQueueDepth       = "hlp_subtask_queue_depth"
	MetricSubtaskCircuitOpenTotal = "hlp_subtask_circuit_open_total"
	MetricSchedulerAlloc          = "hlp_scheduler_alloc"
	MetricHandoffTotal            = "hlp_handoff_total"
	MetricAggregationConflict     = "hlp_aggregation_conflict_total"
	MetricModelCallsTotal         = "hlp_model_calls_total"
	MetricModelTokensTotal        = "hlp_model_tokens_total"
	MetricModelCostTotal          = "hlp_model_cost_total"
	MetricModelFallbackTotal      = "hlp_model_fallback_total"
	MetricModelSelectLatencyMs    = "hlp_model_select_latency_ms"
	MetricModelUnavailable        = "hlp_model_unavailable"
	MetricPolicyDecisionsTotal    = "hlp_policy_decisions_total"
	MetricPolicyDecisionLatencyMs = "hlp_policy_decision_latency_ms"
	MetricPolicyCacheHitRatio     = "hlp_policy_cache_hit_ratio"
	MetricSandboxEscapeTotal      = "hlp_sandbox_escape_attempts_total"
	MetricExecDeniedTotal         = "hlp_exec_denied_total"
	MetricApprovalPending         = "hlp_approval_pending"
)

// DefaultBuckets are reused across duration/latency histograms.
var DefaultBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

// Harness wraps the registry with typed helpers. A nil *Harness is a no-op so
// call sites stay free of nil checks.
type Harness struct {
	Reg *Registry
}

// NewHarness creates the registry and pre-registers the full DFX-M-01 surface.
func NewHarness() *Harness {
	h := &Harness{Reg: NewRegistry()}
	h.preRegister()
	return h
}

func (h *Harness) reg() *Registry {
	if h == nil {
		return nil
	}
	return h.Reg
}

func (h *Harness) preRegister() {
	for _, status := range []string{"completed", "aborted", "failed"} {
		h.Reg.Counter(MetricTasksTotal, "Task terminal state count.", map[string]string{"status": status})
	}
	for _, stage := range []string{"a", "b", "c", "d", "e"} {
		for _, attempt := range []string{"1", "2", "3"} {
			h.Reg.Histogram(MetricStageDurationSeconds, "Stage wall clock duration in seconds.", map[string]string{"stage": stage, "attempt": attempt}, DefaultBuckets)
		}
		h.Reg.Gauge(MetricInjectTokensRatio, "Injected memory tokens as a fraction of the window.", map[string]string{"stage": stage})
	}
	for _, stage := range []string{"a", "b", "c", "d"} {
		for _, verdict := range []string{"pass", "fail", "blocked"} {
			for _, verifiedBy := range []string{"program", "model", "mixed"} {
				h.Reg.Counter(MetricAcceptanceTotal, "Acceptance verdict count.", map[string]string{"stage": stage, "verdict": verdict, "verified_by": verifiedBy})
			}
		}
		h.Reg.Counter(MetricModelCallsTotal, "Model calls per stage.", map[string]string{"stage": stage, "model": "default"})
		for _, direction := range []string{"in", "out"} {
			h.Reg.Counter(MetricModelTokensTotal, "Model tokens per direction.", map[string]string{"stage": stage, "model": "default", "direction": direction})
		}
	}
	for _, category := range []string{"network", "timeout", "quota", "content", "no_finish", "invalid_tool_result", "system"} {
		for _, retryable := range []string{"true", "false"} {
			h.Reg.Counter(MetricErrorsTotal, "Classified error count.", map[string]string{"category": category, "stage": "a", "retryable": retryable})
		}
		h.Reg.Histogram(MetricRetryCount, "Retries per stage.", map[string]string{"category": category, "stage": "a"}, []float64{0, 1, 2, 3, 4, 5})
	}
	for _, kind := range []string{"saved", "restored", "drift", "corrupted"} {
		h.Reg.Counter(MetricCheckpointTotal, "Checkpoint behaviour count.", map[string]string{"kind": kind})
	}
	for _, reason := range []string{"threshold", "manual", "degraded"} {
		for _, degraded := range []string{"true", "false"} {
			h.Reg.Counter(MetricCompactionTotal, "Compaction trigger count.", map[string]string{"reason": reason, "degraded": degraded})
		}
	}
	h.Reg.Histogram(MetricCompactionRatio, "Context compaction ratio.", map[string]string{"strategy_id": "template"}, []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9})
	h.Reg.Gauge(MetricIndexQueueDepth, "Indexer queue depth.", map[string]string{"project_id": "platform"})
	for _, mode := range []string{"incremental", "full"} {
		h.Reg.Histogram(MetricIndexDurationSeconds, "Index build duration in seconds.", map[string]string{"mode": mode}, DefaultBuckets)
	}
	for _, kind := range []string{"code", "memory", "knowledge"} {
		h.Reg.Histogram(MetricRecallLatencyMs, "Recall latency in milliseconds.", map[string]string{"kind": kind}, []float64{5, 10, 25, 50, 100, 250, 500, 1000})
		for _, route := range []string{"fulltext", "vector", "graph"} {
			h.Reg.Counter(MetricRecallHitTotal, "Recall hits by route.", map[string]string{"kind": kind, "route": route})
		}
	}
	h.Reg.Histogram(MetricEventLagSeconds, "Event produced-to-queryable lag in seconds.", nil, []float64{0.1, 0.5, 1, 2, 5, 10, 30})
	for _, status := range []string{"pending", "running", "completed", "failed", "aborted", "skipped"} {
		for _, role := range []string{"planner", "executor", "verifier", "general"} {
			h.Reg.Counter(MetricSubtasksTotal, "Subtask terminal state count.", map[string]string{"status": status, "role": role})
		}
	}
	for _, role := range []string{"planner", "executor", "verifier", "general"} {
		h.Reg.Histogram(MetricSubtaskDurationSeconds, "Subtask duration in seconds.", map[string]string{"role": role, "parallel_set_id": "none"}, DefaultBuckets)
	}
	h.Reg.Gauge(MetricSubtaskQueueDepth, "Queued subtasks per tenant.", map[string]string{"tenant_id": "platform"})
	h.Reg.Counter(MetricSubtaskCircuitOpenTotal, "Subtask circuit breaker openings.", map[string]string{"class_key": "default"})
	for _, kind := range []string{"slots", "tokens"} {
		h.Reg.Gauge(MetricSchedulerAlloc, "Fair scheduler allocation view.", map[string]string{"parent_task_id": "platform", "kind": kind})
	}
	for _, status := range []string{"requested", "accepted", "completed", "rejected", "failed"} {
		h.Reg.Counter(MetricHandoffTotal, "Handoff count by status.", map[string]string{"status": status, "from_role": "planner", "to_role": "executor"})
	}
	for _, kind := range []string{"path_conflict", "contract_mismatch", "duplicate"} {
		h.Reg.Counter(MetricAggregationConflict, "Aggregation conflicts.", map[string]string{"kind": kind})
	}
	h.Reg.Counter(MetricModelCostTotal, "Model cost in USD.", map[string]string{"tenant": "platform", "project": "platform", "stage": "a", "model": "default"})
	h.Reg.Counter(MetricModelFallbackTotal, "Model fallback count.", map[string]string{"primary": "default", "fallback": "default", "trigger": "unavailable"})
	h.Reg.Histogram(MetricModelSelectLatencyMs, "Model selection latency in milliseconds.", nil, []float64{1, 5, 10, 25, 50, 100})
	h.Reg.Gauge(MetricModelUnavailable, "Unavailable models.", map[string]string{"model": "default", "provider": "default"})
	for _, decision := range []string{"allow", "deny", "require_approval"} {
		h.Reg.Counter(MetricPolicyDecisionsTotal, "Policy decisions.", map[string]string{"decision": decision, "rule_id": "none", "layer": "system"})
	}
	for _, hit := range []string{"true", "false"} {
		h.Reg.Histogram(MetricPolicyDecisionLatencyMs, "Policy decision latency in milliseconds.", map[string]string{"hit": hit}, []float64{0.1, 0.5, 1, 2, 5, 10, 20, 50})
	}
	h.Reg.Gauge(MetricPolicyCacheHitRatio, "Policy decision cache hit ratio.", nil)
	for _, kind := range []string{"relative", "abs", "symlink", "unc", "encode"} {
		h.Reg.Counter(MetricSandboxEscapeTotal, "Sandbox escape attempts.", map[string]string{"kind": kind})
	}
	for _, reason := range []string{"stage_matrix", "allowlist", "path", "command"} {
		h.Reg.Counter(MetricExecDeniedTotal, "Exec denials by reason.", map[string]string{"reason": reason})
	}
	for _, priority := range []string{"low", "normal", "high"} {
		h.Reg.Gauge(MetricApprovalPending, "Pending approvals.", map[string]string{"priority": priority})
	}
}
