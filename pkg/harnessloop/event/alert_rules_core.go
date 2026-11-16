package event

func evalCoreAlerts(in AlertInput) []Alert {
	out := []Alert{}

	if in.AcceptanceSamples >= 10 && in.AcceptanceFailureRatio > 0.6 {
		out = append(out, Alert{
			Name: "acceptance_failure_rate", Severity: AlertWarn,
			Detail: format("acceptance failure ratio %.2f over %d samples", in.AcceptanceFailureRatio, in.AcceptanceSamples),
			Action: "check acceptance criteria and model output",
		})
	}
	if in.RetryExhausted > 5 {
		out = append(out, Alert{
			Name: "retry_exhausted_spike", Severity: AlertError,
			Detail: format("%d retry exhaustions in the window", in.RetryExhausted),
			Action: "inspect error categories for dependency failures",
		})
	}
	if in.EventLagP95Seconds > 5 {
		out = append(out, Alert{
			Name: "event_lag", Severity: AlertWarn,
			Detail: format("event lag p95 %.1fs", in.EventLagP95Seconds),
			Action: "check postgres writes and redis fanout",
		})
	}
	if in.IndexQueueDepth > 500 && in.IndexQueueSustained {
		out = append(out, Alert{
			Name: "index_backlog", Severity: AlertWarn,
			Detail: format("index queue depth %.0f sustained", in.IndexQueueDepth),
			Action: "scale the indexer or reduce concurrency",
		})
	}
	if in.IndexFailedFiles > 20 {
		out = append(out, Alert{
			Name: "index_failures", Severity: AlertError,
			Detail: format("%d failed files", in.IndexFailedFiles),
			Action: "read last_error and rebuild the project index",
		})
	}
	if in.CheckpointDrift > 0 || in.CheckpointCorrupted > 0 {
		out = append(out, Alert{
			Name: "checkpoint_integrity", Severity: AlertError,
			Detail: format("drift=%d corrupted=%d", in.CheckpointDrift, in.CheckpointCorrupted),
			Action: "look for out-of-band workspace edits",
		})
	}
	if in.PGConnectFailures > 3 {
		out = append(out, Alert{
			Name: "postgres_unavailable", Severity: AlertError,
			Detail: format("%d connection failures", in.PGConnectFailures),
			Action: "degrade to read-only paths and notify on-call",
		})
	}
	if in.CompactionTotal > 0 && in.CompactionDegraded/in.CompactionTotal > 0.3 {
		out = append(out, Alert{
			Name: "compaction_degraded", Severity: AlertWarn,
			Detail: format("degraded ratio %.2f over %.0f compactions", in.CompactionDegraded/in.CompactionTotal, in.CompactionTotal),
			Action: "check the summarizer model and its timeout",
		})
	}
	if in.MemoryCorrupted > 0 {
		out = append(out, Alert{
			Name: "memory_corrupted", Severity: AlertError,
			Detail: format("%d corrupted memory records", in.MemoryCorrupted),
			Action: "back up immediately and investigate",
		})
	}
	if in.MemoryCapacityWarning > 0 {
		out = append(out, Alert{
			Name: "memory_capacity", Severity: AlertWarn,
			Detail: format("%d capacity warnings", in.MemoryCapacityWarning),
			Action: "prompt the user to clean up topics manually; never auto-delete",
		})
	}
	return out
}
