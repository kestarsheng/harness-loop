package acceptance

import (
	"fmt"
	"strings"
)

const (
	PromptVersion = "acceptance.v1"

	CodeContextIncomplete    = "E_ACCEPTANCE_CONTEXT_INCOMPLETE"
	CodeAggregationMissing   = "E_AGGREGATION_MISSING"
	CodeExecutionHistoryLeak = "E_EXECUTION_HISTORY_LEAK"
	CodeInvalidVerdict       = "E_INVALID_ACCEPTANCE_VERDICT"
	CodeInvalidRollback      = "E_INVALID_ROLLBACK_TARGET"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Criterion struct {
	ID       string `json:"criterion_id"`
	Type     string `json:"type"`
	Expr     string `json:"expr"`
	Required bool   `json:"required"`
}

type Artifact struct {
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
	Excerpt string `json:"excerpt,omitempty"`
}

type CheckResult struct {
	CriterionID string `json:"criterion_id"`
	Type        string `json:"type"`
	Passed      bool   `json:"passed"`
	Output      string `json:"output,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
}

type Constraints struct {
	WorkspaceRoot         string   `json:"workspace_root"`
	AllowedPaths          []string `json:"allowed_paths,omitempty"`
	ToolWhitelist         []string `json:"tool_whitelist"`
	BudgetRemainingTokens int64    `json:"budget_remaining_tokens"`
}

type Evidence struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Excerpt string `json:"excerpt,omitempty"`
}

type FailedCriterion struct {
	CriterionID string `json:"criterion_id"`
	Category    string `json:"category"`
}

type PreviousVerdict struct {
	AcceptanceID   string            `json:"acceptance_id"`
	Verdict        string            `json:"verdict"`
	Reason         string            `json:"reason"`
	Evidence       []Evidence        `json:"evidence"`
	FailedCriteria []FailedCriterion `json:"failed_criteria,omitempty"`
	RollbackTarget string            `json:"rollback_target,omitempty"`
}

type AggregationReport struct {
	ReportID       string     `json:"report_id"`
	AcceptedCount  int        `json:"accepted_count"`
	FailedCount    int        `json:"failed_count"`
	CancelledCount int        `json:"cancelled_count"`
	Conflicts      []string   `json:"conflicts,omitempty"`
	Artifacts      []Artifact `json:"artifacts,omitempty"`
}

type Input struct {
	AcceptanceID       string
	TaskID             string
	TenantID           string
	ProjectID          string
	Stage              string
	AttemptID          string
	TraceID            string
	StageSpanID        string
	Goal               string
	GoalHash           string
	Criteria           []Criterion
	Artifacts          []Artifact
	Checks             []CheckResult
	Constraints        Constraints
	Previous           *PreviousVerdict
	Aggregation        *AggregationReport
	SystemPrompt       string
	PromptVersion      string
	Messages           []Message
	Tools              []string
	MemoryNamespace    string
	UnverifiedCriteria []string
}

func (in *Input) Validate() error {
	var missing []string

	if strings.TrimSpace(in.Goal) == "" {
		missing = append(missing, "goal")
	}
	if strings.TrimSpace(in.GoalHash) == "" {
		missing = append(missing, "goal_hash")
	}
	if len(in.Criteria) == 0 {
		missing = append(missing, "stage_criteria")
	}
	if in.Stage == "c" || in.Stage == "d" {
		if len(in.Artifacts) == 0 {
			missing = append(missing, "artifacts")
		}
	}
	if len(in.Constraints.ToolWhitelist) == 0 {
		missing = append(missing, "tool_whitelist")
	}
	if strings.TrimSpace(in.Constraints.WorkspaceRoot) == "" {
		missing = append(missing, "workspace_root")
	}
	if in.Stage == "d" && in.Aggregation == nil {
		return &ContextError{Code: CodeAggregationMissing, Missing: []string{"aggregation_report"}}
	}
	if len(missing) > 0 {
		return &ContextError{Code: CodeContextIncomplete, Missing: missing}
	}
	return nil
}

type ContextError struct {
	Code    string
	Missing []string
	Msg     string
}

func (e *ContextError) Error() string {
	if len(e.Missing) > 0 {
		return fmt.Sprintf("%s: missing %s", e.Code, strings.Join(e.Missing, ", "))
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

const (
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictBlocked = "blocked"

	VerifiedByProgram = "program"
	VerifiedByModel   = "model"
	VerifiedByMixed   = "mixed"
)

type Verdict struct {
	AcceptanceID   string            `json:"acceptance_id"`
	Verdict        string            `json:"verdict"`
	Reason         string            `json:"reason"`
	Evidence       []Evidence        `json:"evidence"`
	FailedCriteria []FailedCriterion `json:"failed_criteria,omitempty"`
	Unverified     []string          `json:"unverified,omitempty"`
	RollbackTarget string            `json:"rollback_target,omitempty"`
	VerifiedBy     string            `json:"verified_by"`
	Confidence     float64           `json:"confidence"`
	DurationMs     int64             `json:"duration_ms"`
	PromptVersion  string            `json:"prompt_version"`
	Model          string            `json:"model,omitempty"`
	InputDigest    string            `json:"input_digest"`
}

var rollbackCategories = map[string]string{
	"impl_gap":        "c",
	"test_gap":        "c",
	"plan_gap":        "b",
	"requirement_gap": "b",
	"unknown":         "c",
}

func (v *Verdict) Validate() error {
	if v.AcceptanceID == "" {
		return &ContextError{Code: CodeInvalidVerdict, Msg: "acceptance_id is required"}
	}
	switch v.Verdict {
	case VerdictPass, VerdictFail, VerdictBlocked:
	default:
		return &ContextError{Code: CodeInvalidVerdict, Msg: fmt.Sprintf("verdict %q is invalid", v.Verdict)}
	}
	if strings.TrimSpace(v.Reason) == "" {
		return &ContextError{Code: CodeInvalidVerdict, Msg: "reason is required"}
	}
	if len(v.Evidence) == 0 {
		return &ContextError{Code: CodeInvalidVerdict, Msg: "evidence requires at least one item"}
	}
	switch v.VerifiedBy {
	case VerifiedByProgram, VerifiedByModel, VerifiedByMixed:
	default:
		return &ContextError{Code: CodeInvalidVerdict, Msg: fmt.Sprintf("verified_by %q is invalid", v.VerifiedBy)}
	}
	if v.PromptVersion == "" {
		return &ContextError{Code: CodeInvalidVerdict, Msg: "prompt_version is required"}
	}
	if v.InputDigest == "" {
		return &ContextError{Code: CodeInvalidVerdict, Msg: "input_digest is required"}
	}
	if v.Verdict == VerdictFail && v.RollbackTarget != "" {
		if !validRollback(v.RollbackTarget) {
			return &ContextError{Code: CodeInvalidRollback, Msg: fmt.Sprintf("rollback_target %q is invalid", v.RollbackTarget)}
		}
	}
	return nil
}

func validRollback(s string) bool {
	return s == "a" || s == "b" || s == "c" || s == "d"
}

func SuggestedRollback(category string) string {
	if r, ok := rollbackCategories[category]; ok {
		return r
	}
	return "c"
}
