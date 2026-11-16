package rerank

import "sync/atomic"

// Metrics 记录 rerank 调用量、降级次数与耗时（DFX-M-01 的 rerank 系列）。
// 用 atomic 计数，可被并发检索请求安全调用。
type Metrics struct {
	calls    int64
	degraded int64
	failures int64
	totalMs  int64
	maxMs    int64
}

func NewMetrics() *Metrics { return &Metrics{} }

// Record 记录一次 rerank（含被降级的那次）。
func (m *Metrics) Record(durationMs int64, degraded bool) {
	if m == nil {
		return
	}
	atomic.AddInt64(&m.calls, 1)
	atomic.AddInt64(&m.totalMs, durationMs)
	if degraded {
		atomic.AddInt64(&m.degraded, 1)
		atomic.AddInt64(&m.failures, 1)
	}
	for {
		current := atomic.LoadInt64(&m.maxMs)
		if durationMs <= current || atomic.CompareAndSwapInt64(&m.maxMs, current, durationMs) {
			break
		}
	}
}

type MetricsSnapshot struct {
	Calls        int64
	Degraded     int64
	Failures     int64
	AvgMs        float64
	MaxMs        int64
	DegradedRate float64
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	calls := atomic.LoadInt64(&m.calls)
	degraded := atomic.LoadInt64(&m.degraded)
	snapshot := MetricsSnapshot{
		Calls:    calls,
		Degraded: degraded,
		Failures: atomic.LoadInt64(&m.failures),
		MaxMs:    atomic.LoadInt64(&m.maxMs),
	}
	if calls > 0 {
		snapshot.AvgMs = float64(atomic.LoadInt64(&m.totalMs)) / float64(calls)
		snapshot.DegradedRate = float64(degraded) / float64(calls)
	}
	return snapshot
}
