package compaction

import (
	"fmt"
	"sort"
	"time"
)

const (
	VariantTemplateV1 = "template_v1"
	VariantTemplateV2 = "template_v2"
)

type CompactionRun struct {
	RunID                    string    `json:"run_id"`
	TenantID                 string    `json:"tenant_id"`
	ProjectID                string    `json:"project_id"`
	TaskID                   string    `json:"task_id"`
	Variant                  string    `json:"variant"`
	StrategyID               string    `json:"strategy_id"`
	PromptVersion            string    `json:"prompt_version"`
	TriggerReason            string    `json:"trigger_reason"`
	TokensBefore             int       `json:"tokens_before"`
	TokensAfter              int       `json:"tokens_after"`
	Ratio                    float64   `json:"ratio"`
	PreservedRate            float64   `json:"preserved_rate"`
	DownstreamAcceptanceRate float64   `json:"downstream_acceptance_rate"`
	Degraded                 bool      `json:"degraded"`
	DegradeLevel             string    `json:"degrade_level,omitempty"`
	OutputRef                string    `json:"output_ref,omitempty"`
	DurationMs               int64     `json:"duration_ms"`
	CreatedAt                time.Time `json:"created_at"`
}

func (r CompactionRun) Validate() error {
	if r.TaskID == "" || r.Variant == "" || r.PromptVersion == "" {
		return fmt.Errorf("compaction: run requires task_id/variant/prompt_version")
	}
	if r.TokensBefore < 0 || r.TokensAfter < 0 {
		return fmt.Errorf("compaction: token counts must be non-negative")
	}
	if r.Degraded && r.DegradeLevel == "" {
		return fmt.Errorf("compaction: degraded run must carry degrade_level")
	}
	if r.DegradeLevel != "" && r.DegradeLevel != DegradeLevelL1 && r.DegradeLevel != DegradeLevelL2 {
		return fmt.Errorf("compaction: invalid degrade_level %q", r.DegradeLevel)
	}
	return nil
}

func BuildRun(taskID, tenantID, projectID, variant string, promptVersion TemplateVersion, trigger string,
	tokensBefore, tokensAfter int, preservedRate, acceptanceRate float64,
	degraded bool, degradeLevel, outputRef string, durationMs int64) (CompactionRun, error) {
	if trigger == "" {
		trigger = ReasonManual
	}
	run := CompactionRun{
		RunID:                    fmt.Sprintf("cmp-%s-%d", taskID, time.Now().UnixNano()),
		TenantID:                 tenantID,
		ProjectID:                projectID,
		TaskID:                   taskID,
		Variant:                  variant,
		StrategyID:               variant,
		PromptVersion:            string(promptVersion),
		TriggerReason:            trigger,
		TokensBefore:             tokensBefore,
		TokensAfter:              tokensAfter,
		Ratio:                    ratio(tokensBefore, tokensAfter),
		PreservedRate:            preservedRate,
		DownstreamAcceptanceRate: acceptanceRate,
		Degraded:                 degraded,
		DegradeLevel:             degradeLevel,
		OutputRef:                outputRef,
		DurationMs:               durationMs,
		CreatedAt:                time.Now().UTC(),
	}
	if err := run.Validate(); err != nil {
		return CompactionRun{}, err
	}
	return run, nil
}

type VariantSummary struct {
	Variant          string  `json:"variant"`
	Count            int     `json:"count"`
	AvgRatio         float64 `json:"avg_ratio"`
	AvgPreservedRate float64 `json:"avg_preserved_rate"`
	AvgAcceptance    float64 `json:"avg_downstream_acceptance"`
	DegradedCount    int     `json:"degraded_count"`
}

func SummarizeRuns(runs []CompactionRun) []VariantSummary {
	byVariant := map[string][]CompactionRun{}
	for _, run := range runs {
		byVariant[run.Variant] = append(byVariant[run.Variant], run)
	}
	out := make([]VariantSummary, 0, len(byVariant))
	for variant, group := range byVariant {
		summary := VariantSummary{Variant: variant, Count: len(group)}
		for _, run := range group {
			summary.AvgRatio += run.Ratio
			summary.AvgPreservedRate += run.PreservedRate
			summary.AvgAcceptance += run.DownstreamAcceptanceRate
			if run.Degraded {
				summary.DegradedCount++
			}
		}
		n := float64(len(group))
		summary.AvgRatio /= n
		summary.AvgPreservedRate /= n
		summary.AvgAcceptance /= n
		out = append(out, summary)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Variant < out[j].Variant })
	return out
}

type Comparison struct {
	Left, Right     VariantSummary
	RatioDelta      float64
	PreservedDelta  float64
	AcceptanceDelta float64
	Winner          string
}

func CompareVariants(left, right VariantSummary) Comparison {
	comparison := Comparison{
		Left:            left,
		Right:           right,
		RatioDelta:      left.AvgRatio - right.AvgRatio,
		PreservedDelta:  left.AvgPreservedRate - right.AvgPreservedRate,
		AcceptanceDelta: left.AvgAcceptance - right.AvgAcceptance,
	}
	switch {
	case comparison.AcceptanceDelta > 0:
		comparison.Winner = left.Variant
	case comparison.AcceptanceDelta < 0:
		comparison.Winner = right.Variant
	case comparison.PreservedDelta > 0:
		comparison.Winner = left.Variant
	case comparison.PreservedDelta < 0:
		comparison.Winner = right.Variant
	case comparison.RatioDelta < 0:
		comparison.Winner = left.Variant
	case comparison.RatioDelta > 0:
		comparison.Winner = right.Variant
	default:
		comparison.Winner = "tie"
	}
	return comparison
}

func CompactionEventPayload(run CompactionRun) map[string]any {
	payload := map[string]any{
		"trigger_reason": run.TriggerReason,
		"tokens_before":  run.TokensBefore,
		"tokens_after":   run.TokensAfter,
		"ratio":          run.Ratio,
		"duration_ms":    run.DurationMs,
		"degraded":       run.Degraded,
		"strategy_id":    run.StrategyID,
		"prompt_version": run.PromptVersion,
	}
	if run.DegradeLevel != "" {
		payload["degrade_level"] = run.DegradeLevel
	}
	if run.OutputRef != "" {
		payload["output_ref"] = run.OutputRef
	}
	return payload
}

func IsEngineFailureDegraded(enabled bool, engineErr error) bool {
	if !enabled {
		return false
	}
	return engineErr != nil
}
