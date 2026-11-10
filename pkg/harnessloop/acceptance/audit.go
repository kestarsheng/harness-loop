package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agent-platform/pkg/harnessloop/acceptance/checks"
)

const CodeReadOnlyViolation = "E_ACCEPTANCE_READ_ONLY"

var AcceptanceReadOnlyTools = []string{"read_file", "list_dir", "grep", "run_command"}

var writeToolNames = map[string]bool{
	"write_file":  true,
	"edit_file":   true,
	"delete":      true,
	"move":        true,
	"rename":      true,
	"copy":        true,
	"apply_patch": true,
	"mkdir":       true,
}

type AuditRecord struct {
	AcceptanceID       string
	TaskID             string
	Stage              string
	AttemptID          string
	PromptVersion      string
	Model              string
	InputDigest        string
	TokensIn           int
	TokensOut          int
	DurationMs         int64
	Verdict            string
	VerifiedBy         string
	RollbackTarget     string
	RollbackOverridden bool
	Issues             []string
	Evidence           []Evidence
	FailedCriteria     []FailedCriterion
	Unverified         []string
	CheckerResults     []checks.Result
	RecordedAt         time.Time
}

func (r AuditRecord) EventType() string {
	switch r.Verdict {
	case VerdictPass:
		return "acceptance.passed"
	case VerdictBlocked:
		return "acceptance.invalid"
	default:
		return "acceptance.failed"
	}
}

func (r AuditRecord) Payload() map[string]any {
	evidence := make([]map[string]any, 0, len(r.Evidence))
	for _, e := range r.Evidence {
		evidence = append(evidence, map[string]any{"kind": e.Kind, "ref": e.Ref, "excerpt": e.Excerpt})
	}
	failed := make([]map[string]any, 0, len(r.FailedCriteria))
	for _, f := range r.FailedCriteria {
		failed = append(failed, map[string]any{"criterion_id": f.CriterionID, "category": f.Category})
	}
	return map[string]any{
		"acceptance_id":       r.AcceptanceID,
		"task_id":             r.TaskID,
		"stage":               r.Stage,
		"attempt_id":          r.AttemptID,
		"prompt_version":      r.PromptVersion,
		"model":               r.Model,
		"input_digest":        r.InputDigest,
		"tokens_in":           r.TokensIn,
		"tokens_out":          r.TokensOut,
		"duration_ms":         r.DurationMs,
		"verdict":             r.Verdict,
		"verified_by":         r.VerifiedBy,
		"rollback_target":     r.RollbackTarget,
		"rollback_overridden": r.RollbackOverridden,
		"issues":              r.Issues,
		"evidence":            evidence,
		"failed_criteria":     failed,
		"unverified":          r.Unverified,
		"checker_results":     r.CheckerResults,
	}
}

func IsWriteTool(name string) bool { return writeToolNames[name] }

func GuardTool(tool string) error {
	if IsWriteTool(tool) {
		return &ContextError{Code: CodeReadOnlyViolation, Msg: fmt.Sprintf("acceptance is read-only, tool %q is not allowed", tool)}
	}
	for _, allowed := range AcceptanceReadOnlyTools {
		if tool == allowed {
			return nil
		}
	}
	return &ContextError{Code: CodeReadOnlyViolation, Msg: fmt.Sprintf("tool %q is not in the acceptance whitelist", tool)}
}

func SecurityViolationPayload(tool, stage, acceptanceID string) map[string]any {
	return map[string]any{
		"action":        "tool.call",
		"target":        tool,
		"stage":         stage,
		"acceptance_id": acceptanceID,
		"policy":        "acceptance.read_only",
		"decision":      "denied",
	}
}
func BuildRetryContext(prevAcceptanceID, prevAttemptID string, ev []Evidence, fc []FailedCriterion) map[string]any {
	return map[string]any{
		"previous_acceptance_id": prevAcceptanceID,
		"previous_attempt_id":    prevAttemptID,
		"evidence":               ev,
		"failed_criteria":        fc,
	}
}

type TempWorkspace struct {
	dir string
}

func NewTempWorkspace(harnessHome, acceptanceID string) (*TempWorkspace, error) {
	if harnessHome == "" || acceptanceID == "" {
		return nil, &ContextError{Code: CodeContextIncomplete, Msg: "harness home and acceptance id are required"}
	}
	dir := filepath.Join(harnessHome, "tmp", "acceptance", acceptanceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("acceptance: create temp workspace: %w", err)
	}
	return &TempWorkspace{dir: dir}, nil
}

func (w *TempWorkspace) Path() string { return w.dir }

func (w *TempWorkspace) Cleanup() error {
	if w == nil || w.dir == "" {
		return nil
	}
	if err := os.RemoveAll(w.dir); err != nil {
		return fmt.Errorf("acceptance: cleanup temp workspace: %w", err)
	}
	if _, err := os.Stat(w.dir); !os.IsNotExist(err) {
		return fmt.Errorf("acceptance: temp workspace %s still exists after cleanup", w.dir)
	}
	return nil
}
