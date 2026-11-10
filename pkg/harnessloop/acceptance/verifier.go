package acceptance

import (
	"fmt"
	"time"
)

type NormalizeOptions struct {
	Stage         string
	Input         *Input
	ModelAdvice   string
	Confidence    float64
	DurationMs    int64
	PromptVersion string
	Model         string
}

type NormalizeResult struct {
	Verdict            Verdict
	RollbackTarget     string
	RollbackOverridden bool
	Issues             []string
}

func Finalize(v Verdict, opts NormalizeOptions) (NormalizeResult, error) {
	var issues []string
	res := NormalizeResult{}

	if v.AcceptanceID == "" && opts.Input != nil {
		v.AcceptanceID = opts.Input.AcceptanceID
	}
	if v.PromptVersion == "" {
		if opts.PromptVersion != "" {
			v.PromptVersion = opts.PromptVersion
		} else {
			v.PromptVersion = PromptVersion
		}
	}
	if v.InputDigest == "" && opts.Input != nil {
		v.InputDigest = opts.Input.Hash()
	}
	if v.DurationMs == 0 {
		v.DurationMs = opts.DurationMs
	}
	if v.Model == "" {
		v.Model = opts.Model
	}

	switch v.Verdict {
	case VerdictPass, VerdictFail, VerdictBlocked:
	case "":
		issues = append(issues, "verdict_missing")
		v.Verdict = VerdictFail
	default:
		issues = append(issues, "verdict_invalid")
		v.Verdict = VerdictFail
	}

	if v.Verdict == VerdictPass {
		if len(v.Evidence) == 0 {
			issues = append(issues, "pass_without_evidence")
			v.Verdict = VerdictFail
			v.Evidence = []Evidence{{Kind: "verdict", Ref: "acceptance", Excerpt: "pass without evidence downgraded to fail"}}
		} else if len(v.Unverified) > 0 || (opts.Input != nil && len(opts.Input.UnverifiedCriteria) > 0) {
			issues = append(issues, "pass_with_unverified_criteria")
			v.Verdict = VerdictBlocked
		}
	}

	if v.Verdict == VerdictFail && len(v.FailedCriteria) == 0 {
		issues = append(issues, "fail_without_failed_criteria")
		v.FailedCriteria = []FailedCriterion{{CriterionID: "unknown", Category: "unknown"}}
	}

	if v.Verdict == VerdictFail || v.Verdict == VerdictBlocked {
		target, overridden := RollbackFor(opts.Stage, v.FailedCriteria, opts.Confidence, opts.ModelAdvice)
		v.RollbackTarget = target
		res.RollbackTarget = target
		res.RollbackOverridden = overridden
		if overridden {
			issues = append(issues, "rollback_overridden")
		}
	}

	if v.VerifiedBy == "" {
		v.VerifiedBy = VerifiedByModel
	}
	if v.Confidence == 0 && opts.Confidence > 0 {
		v.Confidence = opts.Confidence
	}

	res.Verdict = v
	res.Issues = issues
	if err := v.Validate(); err != nil {
		return res, err
	}
	return res, nil
}

func (n NormalizeResult) AuditRecord(taskID, stage, attemptID string) AuditRecord {
	return AuditRecord{
		AcceptanceID:       n.Verdict.AcceptanceID,
		TaskID:             taskID,
		Stage:              stage,
		AttemptID:          attemptID,
		PromptVersion:      n.Verdict.PromptVersion,
		Model:              n.Verdict.Model,
		InputDigest:        n.Verdict.InputDigest,
		DurationMs:         n.Verdict.DurationMs,
		Verdict:            n.Verdict.Verdict,
		VerifiedBy:         n.Verdict.VerifiedBy,
		RollbackTarget:     n.RollbackTarget,
		RollbackOverridden: n.RollbackOverridden,
		Issues:             n.Issues,
		Evidence:           n.Verdict.Evidence,
		FailedCriteria:     n.Verdict.FailedCriteria,
		Unverified:         n.Verdict.Unverified,
		RecordedAt:         time.Now(),
	}
}

func (v Verdict) Summary() string {
	if v.Verdict == VerdictPass {
		return fmt.Sprintf("acceptance %s passed", v.AcceptanceID)
	}
	return fmt.Sprintf("acceptance %s %s: %s", v.AcceptanceID, v.Verdict, v.Reason)
}
