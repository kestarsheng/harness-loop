package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

var AcceptanceTools = []string{"read_file", "list_dir", "grep", "run_command"}

type Spec struct {
	AcceptanceID string
	TaskID       string
	TenantID     string
	ProjectID    string
	Stage        string
	AttemptID    string
	TraceID      string
	StageSpanID  string
	Goal         string
	GoalHash     string
	Criteria     []Criterion
	Artifacts    []Artifact
	Checks       []CheckResult
	Constraints  Constraints
	Previous     *PreviousVerdict
	Aggregation  *AggregationReport
}

func SystemPrompt(stage string) string {
	return fmt.Sprintf(`You are an independent acceptance verifier for stage %s of a harness loop task.
You have no access to the executor conversation. Judge only the injected goal, criteria, artifacts,
deterministic check results, constraints and the previous verdict.
Return a structured verdict (pass|fail|blocked) with evidence. Deterministic program checks take precedence
over your own judgement; never mark a criterion as passed when a program check failed.
Prompt version: %s.`, stage, PromptVersion)
}

func Assemble(spec Spec) (*Input, error) {
	var missing []string
	if strings.TrimSpace(spec.AcceptanceID) == "" {
		missing = append(missing, "acceptance_id")
	}
	if strings.TrimSpace(spec.TaskID) == "" {
		missing = append(missing, "task_id")
	}
	if strings.TrimSpace(spec.Stage) == "" {
		missing = append(missing, "stage")
	}
	if len(missing) > 0 {
		return nil, &ContextError{Code: CodeContextIncomplete, Missing: missing}
	}

	in := &Input{
		AcceptanceID:    spec.AcceptanceID,
		TaskID:          spec.TaskID,
		TenantID:        spec.TenantID,
		ProjectID:       spec.ProjectID,
		Stage:           spec.Stage,
		AttemptID:       spec.AttemptID,
		TraceID:         spec.TraceID,
		StageSpanID:     spec.StageSpanID,
		Goal:            spec.Goal,
		GoalHash:        spec.GoalHash,
		Criteria:        cloneCriteria(spec.Criteria),
		Artifacts:       cloneArtifacts(spec.Artifacts),
		Checks:          cloneChecks(spec.Checks),
		Constraints:     spec.Constraints,
		Previous:        spec.Previous,
		Aggregation:     spec.Aggregation,
		SystemPrompt:    SystemPrompt(spec.Stage),
		PromptVersion:   PromptVersion,
		Messages:        []Message{},
		Tools:           append([]string(nil), AcceptanceTools...),
		MemoryNamespace: "acceptance/" + spec.AcceptanceID,
	}
	in.UnverifiedCriteria = unverifiedCriteria(in.Criteria, in.Checks)

	if err := in.Validate(); err != nil {
		return nil, err
	}
	return in, nil
}

func cloneCriteria(src []Criterion) []Criterion {
	if len(src) == 0 {
		return nil
	}
	out := make([]Criterion, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func cloneArtifacts(src []Artifact) []Artifact {
	if len(src) == 0 {
		return nil
	}
	out := make([]Artifact, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func cloneChecks(src []CheckResult) []CheckResult {
	if len(src) == 0 {
		return nil
	}
	out := make([]CheckResult, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool { return out[i].CriterionID < out[j].CriterionID })
	return out
}

func unverifiedCriteria(criteria []Criterion, checks []CheckResult) []string {
	checked := make(map[string]bool, len(checks))
	for _, c := range checks {
		checked[c.CriterionID] = true
	}
	var out []string
	for _, c := range criteria {
		if c.Required && !checked[c.ID] {
			out = append(out, c.ID)
		}
	}
	return out
}

func (in *Input) Hash() string {
	payload := struct {
		SystemPrompt    string             `json:"system_prompt"`
		PromptVersion   string             `json:"prompt_version"`
		Goal            string             `json:"goal"`
		GoalHash        string             `json:"goal_hash"`
		Criteria        []Criterion        `json:"criteria"`
		Artifacts       []Artifact         `json:"artifacts"`
		Checks          []CheckResult      `json:"checks"`
		Constraints     Constraints        `json:"constraints"`
		Previous        *PreviousVerdict   `json:"previous,omitempty"`
		Aggregation     *AggregationReport `json:"aggregation,omitempty"`
		Tools           []string           `json:"tools"`
		MemoryNamespace string             `json:"memory_namespace"`
		Messages        []Message          `json:"messages"`
	}{
		SystemPrompt:    in.SystemPrompt,
		PromptVersion:   in.PromptVersion,
		Goal:            in.Goal,
		GoalHash:        in.GoalHash,
		Criteria:        in.Criteria,
		Artifacts:       in.Artifacts,
		Checks:          in.Checks,
		Constraints:     in.Constraints,
		Previous:        in.Previous,
		Aggregation:     in.Aggregation,
		Tools:           in.Tools,
		MemoryNamespace: in.MemoryNamespace,
		Messages:        in.Messages,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func GuardHistory(history []Message) error {
	for _, m := range history {
		if strings.TrimSpace(m.Content) != "" {
			return &ContextError{Code: CodeExecutionHistoryLeak, Msg: "executor history must not be injected into acceptance context"}
		}
	}
	return nil
}

func (in *Input) HasExecutionHistory() bool {
	return len(in.Messages) > 0
}

func (in *Input) ContainsText(s string) bool {
	if strings.Contains(in.SystemPrompt, s) || strings.Contains(in.Goal, s) {
		return true
	}
	for _, m := range in.Messages {
		if strings.Contains(m.Content, s) {
			return true
		}
	}
	return false
}
