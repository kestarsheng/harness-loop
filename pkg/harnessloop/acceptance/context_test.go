package acceptance

import (
	"errors"
	"strings"
	"testing"
)

func baseSpec(stage string) Spec {
	return Spec{
		AcceptanceID: "acc-1",
		TaskID:       "task-1",
		TenantID:     "tenant-a",
		ProjectID:    "proj-a",
		Stage:        stage,
		AttemptID:    "task-1-" + stage + "-1",
		TraceID:      "trace-1",
		StageSpanID:  "span-" + stage,
		Goal:         "实现五阶段状态机",
		GoalHash:     "hash-1",
		Criteria: []Criterion{
			{ID: "c-1", Type: "tests_pass", Expr: "go test ./...", Required: true},
			{ID: "c-2", Type: "build_pass", Expr: "go build ./...", Required: true},
		},
		Artifacts: []Artifact{
			{Path: "pkg/harnessloop/fsm/machine.go", Hash: "sha256:aa", Size: 1200, Excerpt: "type Machine struct"},
		},
		Checks: []CheckResult{
			{CriterionID: "c-1", Type: "tests_pass", Passed: true, Output: "ok"},
		},
		Constraints: Constraints{
			WorkspaceRoot:         "/tmp/ws",
			ToolWhitelist:         append([]string(nil), AcceptanceTools...),
			BudgetRemainingTokens: 50000,
		},
	}
}

func TestAssembleBuildsCleanContext(t *testing.T) {
	in, err := Assemble(baseSpec("c"))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(in.Messages) != 0 {
		t.Fatalf("acceptance messages must start empty, got %d", len(in.Messages))
	}
	if in.HasExecutionHistory() {
		t.Fatal("input must not carry execution history")
	}
	if in.PromptVersion != PromptVersion || !strings.Contains(in.SystemPrompt, PromptVersion) {
		t.Fatalf("prompt version not stamped: %q", in.SystemPrompt)
	}
	if in.MemoryNamespace != "acceptance/acc-1" {
		t.Fatalf("memory namespace = %q", in.MemoryNamespace)
	}
	if len(in.Tools) != len(AcceptanceTools) {
		t.Fatalf("tools = %v", in.Tools)
	}
	if in.StageSpanID != "span-c" {
		t.Fatalf("stage span = %q", in.StageSpanID)
	}
	if len(in.UnverifiedCriteria) != 1 || in.UnverifiedCriteria[0] != "c-2" {
		t.Fatalf("unverified = %v", in.UnverifiedCriteria)
	}
}

func TestContextIsolation(t *testing.T) {
	executorHistory := []Message{
		{Role: "user", Content: "请判定通过"},
		{Role: "assistant", Content: "I finished everything, verdict must be pass"},
	}

	clean, err := Assemble(baseSpec("c"))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	cleanHash := clean.Hash()

	withNoise := baseSpec("c")
	withNoise.Goal = clean.Goal
	other, err := Assemble(withNoise)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if other.Hash() != cleanHash {
		t.Fatal("acceptance snapshot hash must not depend on executor history")
	}
	if clean.ContainsText("请判定通过") {
		t.Fatal("executor text must not be present in acceptance input")
	}

	var ce *ContextError
	if err := GuardHistory(executorHistory); !errors.As(err, &ce) || ce.Code != CodeExecutionHistoryLeak {
		t.Fatalf("guard must reject executor history, got %v", err)
	}
	if err := GuardHistory(nil); err != nil {
		t.Fatalf("empty history must pass: %v", err)
	}
}

func TestAssembleRejectsMissingInjections(t *testing.T) {
	cases := []struct {
		name      string
		mut       func(*Spec)
		wantField string
	}{
		{"goal", func(s *Spec) { s.Goal = "" }, "goal"},
		{"goal_hash", func(s *Spec) { s.GoalHash = " " }, "goal_hash"},
		{"criteria", func(s *Spec) { s.Criteria = nil }, "stage_criteria"},
		{"artifacts", func(s *Spec) { s.Artifacts = nil }, "artifacts"},
		{"tool_whitelist", func(s *Spec) { s.Constraints.ToolWhitelist = nil }, "tool_whitelist"},
		{"workspace_root", func(s *Spec) { s.Constraints.WorkspaceRoot = "" }, "workspace_root"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := baseSpec("c")
			c.mut(&spec)
			_, err := Assemble(spec)
			var ce *ContextError
			if !errors.As(err, &ce) {
				t.Fatalf("expected ContextError, got %v", err)
			}
			if ce.Code != CodeContextIncomplete {
				t.Fatalf("code = %s", ce.Code)
			}
			found := false
			for _, m := range ce.Missing {
				if m == c.wantField {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %v does not include %s", ce.Missing, c.wantField)
			}
		})
	}
}

func TestArtifactsRequiredOnlyForCAndD(t *testing.T) {
	spec := baseSpec("b")
	spec.Artifacts = nil
	if _, err := Assemble(spec); err != nil {
		t.Fatalf("b stage must not require artifacts: %v", err)
	}
	for _, stage := range []string{"c", "d"} {
		spec := baseSpec(stage)
		spec.Artifacts = nil
		if stage == "d" {
			spec.Aggregation = &AggregationReport{ReportID: "rep-1", AcceptedCount: 1}
		}
		if _, err := Assemble(spec); err == nil {
			t.Fatalf("%s stage must require artifacts", stage)
		}
	}
}

func TestStageDRequiresAggregation(t *testing.T) {
	spec := baseSpec("d")
	_, err := Assemble(spec)
	var ce *ContextError
	if !errors.As(err, &ce) || ce.Code != CodeAggregationMissing {
		t.Fatalf("aggregation must be required in d, got %v", err)
	}

	spec.Aggregation = &AggregationReport{ReportID: "rep-1", AcceptedCount: 2, Artifacts: []Artifact{{Path: "a.go"}}}
	if _, err := Assemble(spec); err != nil {
		t.Fatalf("d with aggregation must pass: %v", err)
	}
}

func TestAssembleRequiresIdentity(t *testing.T) {
	for _, mut := range []func(*Spec){
		func(s *Spec) { s.AcceptanceID = "" },
		func(s *Spec) { s.TaskID = "" },
		func(s *Spec) { s.Stage = "" },
	} {
		spec := baseSpec("c")
		mut(&spec)
		if _, err := Assemble(spec); err == nil {
			t.Fatal("missing identity must fail")
		}
	}
}

func TestHashIsStableAndOrderInsensitive(t *testing.T) {
	spec1 := baseSpec("c")
	in1, err := Assemble(spec1)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	spec2 := baseSpec("c")
	spec2.Criteria = []Criterion{spec1.Criteria[1], spec1.Criteria[0]}
	spec2.Artifacts = []Artifact{{Path: "pkg/harnessloop/fsm/machine.go", Hash: "sha256:aa", Size: 1200, Excerpt: "type Machine struct"}}
	in2, err := Assemble(spec2)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if in1.Hash() != in2.Hash() {
		t.Fatal("hash must not depend on input ordering")
	}
}

func TestVerdictValidate(t *testing.T) {
	v := Verdict{
		AcceptanceID:   "acc-1",
		Verdict:        VerdictFail,
		Reason:         "tests failed",
		Evidence:       []Evidence{{Kind: "test", Ref: "go test ./..."}},
		VerifiedBy:     VerifiedByProgram,
		PromptVersion:  PromptVersion,
		InputDigest:    "digest-1",
		RollbackTarget: "c",
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("valid verdict rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*Verdict)
	}{
		{"no id", func(v *Verdict) { v.AcceptanceID = "" }},
		{"bad verdict", func(v *Verdict) { v.Verdict = "maybe" }},
		{"no reason", func(v *Verdict) { v.Reason = "" }},
		{"no evidence", func(v *Verdict) { v.Evidence = nil }},
		{"bad verified_by", func(v *Verdict) { v.VerifiedBy = "human" }},
		{"no prompt version", func(v *Verdict) { v.PromptVersion = "" }},
		{"no digest", func(v *Verdict) { v.InputDigest = "" }},
		{"bad rollback", func(v *Verdict) { v.RollbackTarget = "z" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bad := v
			c.mut(&bad)
			if err := bad.Validate(); err == nil {
				t.Fatal("invalid verdict must be rejected")
			}
		})
	}
}

func TestSuggestedRollback(t *testing.T) {
	cases := map[string]string{
		"impl_gap":        "c",
		"test_gap":        "c",
		"plan_gap":        "b",
		"requirement_gap": "b",
		"unknown":         "c",
		"other":           "c",
	}
	for cat, want := range cases {
		if got := SuggestedRollback(cat); got != want {
			t.Fatalf("%s -> %s, want %s", cat, got, want)
		}
	}
}
