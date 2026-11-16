package event

import "testing"

func TestEvaluateHealthHealthy(t *testing.T) {
	got := EvaluateHealth(HealthInput{
		AcceptancePassRate: 0.95,
		RetryRate:          0.05,
		StageDwellSeconds:  120,
		ErrorDensity:       0.5,
	}, DefaultHealthPolicy())
	if got.Grade != "healthy" || got.Score < 70 {
		t.Fatalf("healthy task scored %+v", got)
	}
	if len(got.Reasons) != 0 {
		t.Fatalf("healthy task must not carry reasons: %v", got.Reasons)
	}
}

func TestEvaluateHealthDegraded(t *testing.T) {
	got := EvaluateHealth(HealthInput{
		AcceptancePassRate: 0.3,
		RetryRate:          0.8,
		StageDwellSeconds:  7200,
		ErrorDensity:       18,
	}, DefaultHealthPolicy())
	if got.Grade == "healthy" || got.Score >= 70 {
		t.Fatalf("failing task must not be healthy: %+v", got)
	}
	if len(got.Reasons) == 0 {
		t.Fatal("degraded task must explain why")
	}
}

func TestAlertAcceptanceFailureRateNeedsSamples(t *testing.T) {
	if alerts := EvaluateAlerts(AlertInput{AcceptanceFailureRatio: 0.9, AcceptanceSamples: 4}); len(alerts) != 0 {
		t.Fatalf("small sample must not alert: %+v", alerts)
	}
	alerts := EvaluateAlerts(AlertInput{AcceptanceFailureRatio: 0.9, AcceptanceSamples: 40})
	if len(alerts) != 1 || alerts[0].Name != "acceptance_failure_rate" {
		t.Fatalf("alerts = %+v", alerts)
	}
}

func TestAlertCoreRules(t *testing.T) {
	alerts := EvaluateAlerts(AlertInput{
		RetryExhausted:        6,
		EventLagP95Seconds:    6,
		IndexQueueDepth:       600,
		IndexQueueSustained:   true,
		IndexFailedFiles:      21,
		CheckpointDrift:       1,
		PGConnectFailures:     4,
		MemoryCapacityWarning: 1,
	})
	want := map[string]bool{
		"retry_exhausted_spike": true, "event_lag": true, "index_backlog": true,
		"index_failures": true, "checkpoint_integrity": true, "postgres_unavailable": true,
		"memory_capacity": true,
	}
	for _, a := range alerts {
		delete(want, a.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing alerts: %v (got %+v)", want, alerts)
	}
}

func TestAlertAgentRules(t *testing.T) {
	alerts := EvaluateAlerts(AlertInput{
		SubtaskCircuitOpen:    3,
		SubtaskQueueDepth:     50,
		SubtaskQueueSustained: true,
		AggregationConflict:   1,
		SubtaskTimeoutRatio:   0.4,
		SubtaskFailedTotal:    12,
		ModelFallback:         11,
		ModelUnavailable:      2,
		ModelCapabilityReject: 1,
	})
	want := map[string]bool{
		"subtask_circuit_open": true, "subtask_queue_backlog": true,
		"aggregation_conflict": true, "subtask_timeout_ratio": true,
		"model_fallback_spike": true, "model_unavailable": true,
		"model_capability_rejected": true,
	}
	for _, a := range alerts {
		delete(want, a.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing alerts: %v", want)
	}
}

func TestAlertSecurityRules(t *testing.T) {
	alerts := EvaluateAlerts(AlertInput{
		PolicyDenyRatio:    0.3,
		SandboxEscape:      1,
		PolicyHashMismatch: 1,
		ApprovalPending:    6,
		ApprovalSustained:  true,
		PolicyLatencyP95Ms: 25,
	})
	severity := map[string]AlertSeverity{}
	for _, a := range alerts {
		severity[a.Name] = a.Severity
	}
	for _, name := range []string{"sandbox_escape_attempt", "self_privilege_escalation"} {
		if severity[name] != AlertError {
			t.Fatalf("%s severity = %q, want error", name, severity[name])
		}
	}
	for _, name := range []string{"policy_deny_spike", "approval_backlog", "policy_latency_regression"} {
		if severity[name] != AlertWarn {
			t.Fatalf("%s severity = %q, want warn", name, severity[name])
		}
	}
}
