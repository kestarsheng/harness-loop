package event

import "fmt"

type AlertSeverity string

const (
	AlertWarn  AlertSeverity = "warn"
	AlertError AlertSeverity = "error"
)

// AlertInput aggregates one evaluation window. Counts are window counts unless
// the field name says otherwise.
type AlertInput struct {
	AcceptanceFailureRatio float64
	AcceptanceSamples      int
	RetryExhausted         int
	EventLagP95Seconds     float64
	IndexQueueDepth        float64
	IndexQueueSustained    bool
	IndexFailedFiles       int
	CheckpointDrift        int
	CheckpointCorrupted    int
	PGConnectFailures      int
	CompactionDegraded     float64
	CompactionTotal        float64
	MemoryCorrupted        int
	MemoryCapacityWarning  int
	SubtaskCircuitOpen     int
	SubtaskQueueDepth      float64
	SubtaskQueueSustained  bool
	AggregationConflict    int
	SubtaskTimeoutRatio    float64
	SubtaskFailedTotal     int
	ModelFallback          int
	ModelUnavailable       int
	ModelCapabilityReject  int
	PolicyDenyRatio        float64
	SandboxEscape          int
	SelfModifyAttempts     int
	PolicyHashMismatch     int
	ApprovalPending        int
	ApprovalSustained      bool
	PolicyLatencyP95Ms     float64
}

// Alert is one triggered rule.
type Alert struct {
	Name     string        `json:"name"`
	Severity AlertSeverity `json:"severity"`
	Detail   string        `json:"detail"`
	Action   string        `json:"action"`
}

// EvaluateAlerts applies design 10.4. Rate-based rules require a minimum sample
// so a single event cannot page an operator; sustained conditions are supplied
// by the caller as booleans (the window is tracked outside this pure function).
func EvaluateAlerts(in AlertInput) []Alert {
	out := []Alert{}
	out = append(out, evalCoreAlerts(in)...)
	out = append(out, evalAgentAlerts(in)...)
	out = append(out, evalSecurityAlerts(in)...)
	return out
}

func format(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
