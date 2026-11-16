package metrics

import (
	"time"
)

func (h *Harness) Subtask(status, role string, d time.Duration, parallelSet string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricSubtasksTotal, "Subtask terminal state count.",
		map[string]string{"status": status, "role": role}).Inc()
	h.Reg.Histogram(MetricSubtaskDurationSeconds, "Subtask duration in seconds.",
		map[string]string{"role": role, "parallel_set_id": parallelSet}, DefaultBuckets).Observe(d.Seconds())
}

func (h *Harness) SubtaskQueueDepth(tenantID string, depth int) {
	if h == nil {
		return
	}
	h.Reg.Gauge(MetricSubtaskQueueDepth, "Queued subtasks per tenant.", map[string]string{"tenant_id": tenantID}).Set(float64(depth))
}

func (h *Harness) SubtaskCircuitOpen(classKey string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricSubtaskCircuitOpenTotal, "Subtask circuit breaker openings.", map[string]string{"class_key": classKey}).Inc()
}

func (h *Harness) SchedulerAlloc(parentTaskID, kind string, value float64) {
	if h == nil {
		return
	}
	h.Reg.Gauge(MetricSchedulerAlloc, "Fair scheduler allocation view.",
		map[string]string{"parent_task_id": parentTaskID, "kind": kind}).Set(value)
}

func (h *Harness) Handoff(status, fromRole, toRole string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricHandoffTotal, "Handoff count by status.",
		map[string]string{"status": status, "from_role": fromRole, "to_role": toRole}).Inc()
}

func (h *Harness) AggregationConflict(kind string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricAggregationConflict, "Aggregation conflicts.", map[string]string{"kind": kind}).Inc()
}
