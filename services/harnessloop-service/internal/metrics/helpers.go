package metrics

import (
	"strconv"
	"time"
)

func (h *Harness) TaskTerminal(status string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricTasksTotal, "Task terminal state count.", map[string]string{"status": status}).Inc()
}

func (h *Harness) StageDuration(stage string, attempt int, d time.Duration) {
	if h == nil {
		return
	}
	h.Reg.Histogram(MetricStageDurationSeconds, "Stage wall clock duration in seconds.",
		map[string]string{"stage": stage, "attempt": strconv.Itoa(attempt)}, DefaultBuckets).Observe(d.Seconds())
}

func (h *Harness) Acceptance(stage, verdict, verifiedBy string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricAcceptanceTotal, "Acceptance verdict count.",
		map[string]string{"stage": stage, "verdict": verdict, "verified_by": verifiedBy}).Inc()
}

func (h *Harness) Error(category, stage string, retryable bool) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricErrorsTotal, "Classified error count.",
		map[string]string{"category": category, "stage": stage, "retryable": strconv.FormatBool(retryable)}).Inc()
}

func (h *Harness) Retry(category, stage string, times int) {
	if h == nil {
		return
	}
	h.Reg.Histogram(MetricRetryCount, "Retries per stage.",
		map[string]string{"category": category, "stage": stage}, []float64{0, 1, 2, 3, 4, 5}).Observe(float64(times))
}

func (h *Harness) Checkpoint(kind string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricCheckpointTotal, "Checkpoint behaviour count.", map[string]string{"kind": kind}).Inc()
}

func (h *Harness) Compaction(reason string, degraded bool) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricCompactionTotal, "Compaction trigger count.",
		map[string]string{"reason": reason, "degraded": strconv.FormatBool(degraded)}).Inc()
}

func (h *Harness) CompactionRatio(strategyID string, ratio float64) {
	if h == nil {
		return
	}
	h.Reg.Histogram(MetricCompactionRatio, "Context compaction ratio.",
		map[string]string{"strategy_id": strategyID}, []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9}).Observe(ratio)
}

func (h *Harness) EventLag(d time.Duration) {
	if h == nil {
		return
	}
	h.Reg.Histogram(MetricEventLagSeconds, "Event produced-to-queryable lag in seconds.",
		nil, []float64{0.1, 0.5, 1, 2, 5, 10, 30}).Observe(d.Seconds())
}

func (h *Harness) Recall(kind string, latency time.Duration, route string, hit bool) {
	if h == nil {
		return
	}
	h.Reg.Histogram(MetricRecallLatencyMs, "Recall latency in milliseconds.",
		map[string]string{"kind": kind}, []float64{5, 10, 25, 50, 100, 250, 500, 1000}).Observe(float64(latency.Milliseconds()))
	if hit {
		h.Reg.Counter(MetricRecallHitTotal, "Recall hits by route.", map[string]string{"kind": kind, "route": route}).Inc()
	}
}

func (h *Harness) InjectRatio(stage string, ratio float64) {
	if h == nil {
		return
	}
	h.Reg.Gauge(MetricInjectTokensRatio, "Injected memory tokens as a fraction of the window.", map[string]string{"stage": stage}).Set(ratio)
}

func (h *Harness) IndexQueueDepth(projectID string, depth int) {
	if h == nil {
		return
	}
	h.Reg.Gauge(MetricIndexQueueDepth, "Indexer queue depth.", map[string]string{"project_id": projectID}).Set(float64(depth))
}

func (h *Harness) IndexDuration(mode string, d time.Duration) {
	if h == nil {
		return
	}
	h.Reg.Histogram(MetricIndexDurationSeconds, "Index build duration in seconds.",
		map[string]string{"mode": mode}, DefaultBuckets).Observe(d.Seconds())
}

// EventPublishFailed 记录一次「事件落库失败、已进缓冲等待重投」。eventType 与
// reason 都是低基数标签（reason 见 event.FailureReason）。
func (h *Harness) EventPublishFailed(eventType, reason string) {
	if h == nil {
		return
	}
	if eventType == "" {
		eventType = "unknown"
	}
	if reason == "" {
		reason = "sink_error"
	}
	h.Reg.Counter(MetricEventPublishFailedTotal, "Event publish failures after buffering.",
		map[string]string{"event_type": eventType, "reason": reason}).Inc()
}

// EventSeqConflict 记录一次 seq 分配撞键（正常路径为 0，滚动升级期间才会出现）。
func (h *Harness) EventSeqConflict(source string) {
	if h == nil {
		return
	}
	if source == "" {
		source = "event_repo"
	}
	h.Reg.Counter(MetricEventSeqConflictTotal, "Event seq allocation conflicts.",
		map[string]string{"source": source}).Inc()
}
