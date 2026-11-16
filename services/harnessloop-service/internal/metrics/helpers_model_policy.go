package metrics

import (
	"strconv"
	"time"
)

func (h *Harness) ModelCall(stage, model string, tokensIn, tokensOut int64, cost float64, tenant, project string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricModelCallsTotal, "Model calls per stage.", map[string]string{"stage": stage, "model": model}).Inc()
	h.Reg.Counter(MetricModelTokensTotal, "Model tokens per direction.", map[string]string{"stage": stage, "model": model, "direction": "in"}).Add(float64(tokensIn))
	h.Reg.Counter(MetricModelTokensTotal, "Model tokens per direction.", map[string]string{"stage": stage, "model": model, "direction": "out"}).Add(float64(tokensOut))
	h.Reg.Counter(MetricModelCostTotal, "Model cost in USD.",
		map[string]string{"tenant": tenant, "project": project, "stage": stage, "model": model}).Add(cost)
}

func (h *Harness) ModelFallback(primary, fallback, trigger string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricModelFallbackTotal, "Model fallback count.",
		map[string]string{"primary": primary, "fallback": fallback, "trigger": trigger}).Inc()
}

func (h *Harness) ModelSelectLatency(d time.Duration) {
	if h == nil {
		return
	}
	h.Reg.Histogram(MetricModelSelectLatencyMs, "Model selection latency in milliseconds.",
		nil, []float64{1, 5, 10, 25, 50, 100}).Observe(float64(d.Milliseconds()))
}

func (h *Harness) ModelUnavailable(model, provider string, count int) {
	if h == nil {
		return
	}
	h.Reg.Gauge(MetricModelUnavailable, "Unavailable models.", map[string]string{"model": model, "provider": provider}).Set(float64(count))
}

func (h *Harness) PolicyDecision(decision, ruleID, layer string, latency time.Duration, cacheHit bool) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricPolicyDecisionsTotal, "Policy decisions.",
		map[string]string{"decision": decision, "rule_id": ruleID, "layer": layer}).Inc()
	h.Reg.Histogram(MetricPolicyDecisionLatencyMs, "Policy decision latency in milliseconds.",
		map[string]string{"hit": strconv.FormatBool(cacheHit)}, []float64{0.1, 0.5, 1, 2, 5, 10, 20, 50}).Observe(float64(latency.Microseconds()) / 1000)
}

func (h *Harness) PolicyCacheHitRatio(ratio float64) {
	if h == nil {
		return
	}
	h.Reg.Gauge(MetricPolicyCacheHitRatio, "Policy decision cache hit ratio.", nil).Set(ratio)
}

func (h *Harness) SandboxEscapeAttempt(kind string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricSandboxEscapeTotal, "Sandbox escape attempts.", map[string]string{"kind": kind}).Inc()
}

func (h *Harness) ExecDenied(reason string) {
	if h == nil {
		return
	}
	h.Reg.Counter(MetricExecDeniedTotal, "Exec denials by reason.", map[string]string{"reason": reason}).Inc()
}

func (h *Harness) ApprovalPending(priority string, count int) {
	if h == nil {
		return
	}
	h.Reg.Gauge(MetricApprovalPending, "Pending approvals.", map[string]string{"priority": priority}).Set(float64(count))
}
