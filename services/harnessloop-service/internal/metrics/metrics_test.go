package metrics

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestDFXM01SurfaceRegistered(t *testing.T) {
	h := NewHarness()
	text := h.Reg.Render()
	want := []string{
		MetricTasksTotal, MetricStageDurationSeconds, MetricAcceptanceTotal,
		MetricErrorsTotal, MetricRetryCount, MetricCheckpointTotal,
		MetricCompactionTotal, MetricCompactionRatio, MetricIndexQueueDepth,
		MetricIndexDurationSeconds, MetricRecallLatencyMs, MetricRecallHitTotal,
		MetricEventLagSeconds, MetricInjectTokensRatio, MetricSubtasksTotal,
		MetricSubtaskDurationSeconds, MetricSubtaskQueueDepth, MetricSubtaskCircuitOpenTotal,
		MetricSchedulerAlloc, MetricHandoffTotal, MetricAggregationConflict,
		MetricModelCallsTotal, MetricModelTokensTotal, MetricModelCostTotal,
		MetricModelFallbackTotal, MetricModelSelectLatencyMs, MetricModelUnavailable,
		MetricPolicyDecisionsTotal, MetricPolicyDecisionLatencyMs, MetricPolicyCacheHitRatio,
		MetricSandboxEscapeTotal, MetricExecDeniedTotal, MetricApprovalPending,
	}
	for _, name := range want {
		if !strings.Contains(text, "# TYPE "+name+" ") {
			t.Fatalf("metric %s missing from /metrics output", name)
		}
	}
	if len(want) != 33 {
		t.Fatalf("DFX-M-01 set changed: %d metrics", len(want))
	}
}

func TestCounterGaugeHistogramSemantics(t *testing.T) {
	h := NewHarness()
	h.TaskTerminal("completed")
	h.TaskTerminal("completed")
	if got, _ := h.Reg.Find(MetricTasksTotal, map[string]string{"status": "completed"}); got != 2 {
		t.Fatalf("tasks_total = %v", got)
	}

	h.InjectRatio("a", 0.25)
	if got, _ := h.Reg.Find(MetricInjectTokensRatio, map[string]string{"stage": "a"}); got != 0.25 {
		t.Fatalf("inject ratio = %v", got)
	}

	h.Reg.Histogram("test_hist", "test", map[string]string{"k": "v"}, []float64{1, 5}).Observe(0.5)
	h.Reg.Histogram("test_hist", "test", map[string]string{"k": "v"}, []float64{1, 5}).Observe(3)
	text := h.Reg.Render()
	for _, line := range []string{
		`test_hist_bucket{k="v",le="1"} 1`,
		`test_hist_bucket{k="v",le="5"} 2`,
		`test_hist_bucket{k="v",le="+Inf"} 2`,
		`test_hist_count{k="v"} 2`,
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("histogram output missing %q:\n%s", line, text)
		}
	}
}

func TestMetricsHandler(t *testing.T) {
	h := NewHarness()
	rec := httptest.NewRecorder()
	h.Reg.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), MetricApprovalPending) {
		t.Fatal("metrics body missing DFX-M-01 metric")
	}
}

func TestMetricsConcurrentWrites(t *testing.T) {
	h := NewHarness()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.TaskTerminal("failed")
				h.StageDuration("a", idx, 1000000)
				h.PolicyDecision("deny", "rule-x", "role", 1000, false)
			}
		}(i)
	}
	wg.Wait()
	if got, _ := h.Reg.Find(MetricTasksTotal, map[string]string{"status": "failed"}); got != 800 {
		t.Fatalf("failed tasks = %v", got)
	}
}

func BenchmarkRender(b *testing.B) {
	h := NewHarness()
	for i := 0; i < 1000; i++ {
		h.TaskTerminal("completed")
		h.StageDuration("a", 1, 1500000000)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = h.Reg.Render()
	}
}
