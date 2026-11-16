package event

// HealthInput holds the signals used by REQ-110 health scoring. Ratios are
// 0..1; durations are seconds.
type HealthInput struct {
	AcceptancePassRate float64
	RetryRate          float64
	StageDwellSeconds  float64
	ErrorDensity       float64
}

// HealthPolicy makes the scoring configurable so the rule can be tuned without
// a code change.
type HealthPolicy struct {
	WarnScore        int
	CriticalScore    int
	DwellThreshold   float64
	DwellCeiling     float64
	ErrorDensityCeil float64
}

// DefaultHealthPolicy matches design 7.2: acceptance pass rate dominates,
// retries and dwell time are secondary, error density is a penalty.
func DefaultHealthPolicy() HealthPolicy {
	return HealthPolicy{
		WarnScore:        70,
		CriticalScore:    40,
		DwellThreshold:   1800,
		DwellCeiling:     7200,
		ErrorDensityCeil: 20,
	}
}

// HealthScore is the computed health of one agent.
type HealthScore struct {
	Score   int      `json:"score"`
	Grade   string   `json:"grade"`
	Reasons []string `json:"reasons"`
}

// EvaluateHealth computes a 0..100 score. A task with a high retry rate and a
// low acceptance pass rate must score clearly below a healthy task.
func EvaluateHealth(in HealthInput, p HealthPolicy) HealthScore {
	pass := clamp01(in.AcceptancePassRate)
	retry := clamp01(in.RetryRate)
	density := clamp01(in.ErrorDensity / p.ErrorDensityCeil)
	dwell := 0.0
	if p.DwellCeiling > p.DwellThreshold {
		dwell = clamp01((in.StageDwellSeconds - p.DwellThreshold) / (p.DwellCeiling - p.DwellThreshold))
	}

	score := int(100 * (0.5*pass + 0.2*(1-retry) + 0.15*(1-dwell) + 0.15*(1-density)))
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	out := HealthScore{Score: score, Grade: grade(score, p), Reasons: []string{}}
	if pass < 0.5 {
		out.Reasons = append(out.Reasons, "acceptance pass rate below 0.5")
	}
	if retry > 0.4 {
		out.Reasons = append(out.Reasons, "retry rate above 0.4")
	}
	if dwell > 0 {
		out.Reasons = append(out.Reasons, "stage dwell time above threshold")
	}
	if density > 0.5 {
		out.Reasons = append(out.Reasons, "error density above half of the ceiling")
	}
	return out
}

func grade(score int, p HealthPolicy) string {
	switch {
	case score >= p.WarnScore:
		return "healthy"
	case score >= p.CriticalScore:
		return "degraded"
	default:
		return "critical"
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
