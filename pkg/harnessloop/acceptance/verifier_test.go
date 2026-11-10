package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testInput(t *testing.T, stage string) *Input {
	t.Helper()
	spec := baseSpec(stage)
	spec.Checks = []CheckResult{
		{CriterionID: "c-1", Type: "tests_pass", Passed: true},
		{CriterionID: "c-2", Type: "build_pass", Passed: true},
	}
	if stage == "d" {
		spec.Aggregation = &AggregationReport{ReportID: "rep-1", AcceptedCount: 1, Artifacts: []Artifact{{Path: "a.go"}}}
	}
	in, err := Assemble(spec)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return in
}

func TestVerdictPassWithEvidence(t *testing.T) {
	in := testInput(t, "c")
	v := Verdict{
		AcceptanceID: in.AcceptanceID,
		Verdict:      VerdictPass,
		Reason:       "all criteria met",
		Evidence:     []Evidence{{Kind: "test", Ref: "go test ./..."}},
		VerifiedBy:   VerifiedByProgram,
	}
	res, err := Finalize(v, NormalizeOptions{Stage: "c", Input: in, Confidence: 0.9})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if res.Verdict.Verdict != VerdictPass || len(res.Issues) != 0 {
		t.Fatalf("unexpected: %+v", res)
	}
	if res.Verdict.InputDigest != in.Hash() || res.Verdict.PromptVersion != PromptVersion {
		t.Fatal("digest/prompt version must be filled from input")
	}
}

func TestVerdictPassWithoutEvidenceBecomesFail(t *testing.T) {
	in := testInput(t, "c")
	v := Verdict{AcceptanceID: in.AcceptanceID, Verdict: VerdictPass, Reason: "looks fine", VerifiedBy: VerifiedByModel}
	res, err := Finalize(v, NormalizeOptions{Stage: "c", Input: in, Confidence: 0.8})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if res.Verdict.Verdict != VerdictFail {
		t.Fatalf("verdict = %s", res.Verdict.Verdict)
	}
	if !hasIssue(res.Issues, "pass_without_evidence") {
		t.Fatalf("issues = %v", res.Issues)
	}
	if res.RollbackTarget != "b" {
		t.Fatalf("rollback = %s", res.RollbackTarget)
	}
}

func TestVerdictPassWithUnverifiedBecomesBlocked(t *testing.T) {
	spec := baseSpec("c")
	spec.Checks = nil
	in, err := Assemble(spec)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	v := Verdict{
		AcceptanceID: in.AcceptanceID,
		Verdict:      VerdictPass,
		Reason:       "verified by model only",
		Evidence:     []Evidence{{Kind: "snippet", Ref: "a.go"}},
		VerifiedBy:   VerifiedByModel,
	}
	res, err := Finalize(v, NormalizeOptions{Stage: "c", Input: in, Confidence: 0.7})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if res.Verdict.Verdict != VerdictBlocked {
		t.Fatalf("verdict = %s", res.Verdict.Verdict)
	}
	if !hasIssue(res.Issues, "pass_with_unverified_criteria") {
		t.Fatalf("issues = %v", res.Issues)
	}
}

func TestVerdictMissingIsFailClosed(t *testing.T) {
	in := testInput(t, "c")
	v := Verdict{AcceptanceID: in.AcceptanceID, Reason: "no verdict field", Evidence: []Evidence{{Kind: "file", Ref: "a"}}}
	res, err := Finalize(v, NormalizeOptions{Stage: "c", Input: in})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if res.Verdict.Verdict != VerdictFail || !hasIssue(res.Issues, "verdict_missing") {
		t.Fatalf("unexpected: %+v", res)
	}
}

func TestVerdictInvalidValueIsFailClosed(t *testing.T) {
	in := testInput(t, "c")
	v := Verdict{AcceptanceID: in.AcceptanceID, Verdict: "maybe", Reason: "x", Evidence: []Evidence{{Kind: "file", Ref: "a"}}}
	res, err := Finalize(v, NormalizeOptions{Stage: "c", Input: in})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if res.Verdict.Verdict != VerdictFail || !hasIssue(res.Issues, "verdict_invalid") {
		t.Fatalf("unexpected: %+v", res)
	}
}

func TestVerdictFailWithoutCriteriaGetsUnknown(t *testing.T) {
	in := testInput(t, "c")
	v := Verdict{
		AcceptanceID: in.AcceptanceID,
		Verdict:      VerdictFail,
		Reason:       "something broke",
		Evidence:     []Evidence{{Kind: "command", Ref: "go test ./..."}},
	}
	res, err := Finalize(v, NormalizeOptions{Stage: "c", Input: in})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if len(res.Verdict.FailedCriteria) != 1 || res.Verdict.FailedCriteria[0].Category != "unknown" {
		t.Fatalf("failed criteria = %+v", res.Verdict.FailedCriteria)
	}
	if !hasIssue(res.Issues, "fail_without_failed_criteria") {
		t.Fatalf("issues = %v", res.Issues)
	}
}

func TestRollbackMappingPerStage(t *testing.T) {
	cases := []struct {
		stage    string
		category string
		want     string
	}{
		{"a", "unknown", "a"},
		{"b", "plan_gap", "a"},
		{"c", "impl_gap", "b"},
		{"d", "impl_gap", "c"},
		{"d", "test_gap", "c"},
		{"d", "plan_gap", "b"},
		{"d", "requirement_gap", "b"},
	}
	for _, c := range cases {
		got, overridden := RollbackFor(c.stage, []FailedCriterion{{CriterionID: "c-1", Category: c.category}}, 0.9, "")
		if got != c.want || overridden {
			t.Fatalf("%s/%s -> %s (overridden=%v), want %s", c.stage, c.category, got, overridden, c.want)
		}
	}
}

func TestRollbackModelAdviceIsOverridden(t *testing.T) {
	got, overridden := RollbackFor("d", []FailedCriterion{{CriterionID: "c-1", Category: "impl_gap"}}, 0.9, "b")
	if got != "c" || !overridden {
		t.Fatalf("rule table must win: %s overridden=%v", got, overridden)
	}

	got, overridden = RollbackFor("d", []FailedCriterion{{CriterionID: "c-1", Category: "impl_gap"}}, 0.9, "a")
	if got != "c" || !overridden {
		t.Fatalf("illegal advice must be dropped: %s overridden=%v", got, overridden)
	}

	got, overridden = RollbackFor("d", []FailedCriterion{{CriterionID: "c-1", Category: "impl_gap"}}, 0.9, "c")
	if got != "c" || overridden {
		t.Fatalf("matching advice must not be overridden: %s overridden=%v", got, overridden)
	}
}

func TestRollbackUnknownLowConfidence(t *testing.T) {
	got, _ := RollbackFor("d", []FailedCriterion{{CriterionID: "c-1", Category: "unknown"}}, 0.3, "")
	if got != "c" {
		t.Fatalf("low confidence unknown -> %s", got)
	}
	got, _ = RollbackFor("d", []FailedCriterion{{CriterionID: "c-1", Category: "unknown"}}, 0.8, "")
	if got != "c" {
		t.Fatalf("unknown must stay conservative -> %s", got)
	}
}

func TestAllowedRollback(t *testing.T) {
	cases := map[string][]string{
		"a": {"a"},
		"b": {"a"},
		"c": {"b"},
		"d": {"b", "c"},
	}
	for stage, targets := range cases {
		for _, target := range []string{"a", "b", "c", "d", "e"} {
			want := false
			for _, tgt := range targets {
				if tgt == target {
					want = true
				}
			}
			if got := AllowedRollback(stage, target); got != want {
				t.Fatalf("%s->%s = %v, want %v", stage, target, got, want)
			}
		}
	}
}

func TestFinalizeRejectsInvalidVerdictShape(t *testing.T) {
	in := testInput(t, "c")
	if _, err := Finalize(Verdict{Verdict: VerdictPass, Reason: ""}, NormalizeOptions{Stage: "c", Input: in}); err == nil {
		t.Fatal("empty reason must fail validation")
	}
}

func TestAuditEventType(t *testing.T) {
	cases := map[string]string{
		VerdictPass:    "acceptance.passed",
		VerdictFail:    "acceptance.failed",
		VerdictBlocked: "acceptance.invalid",
	}
	for verdict, want := range cases {
		if got := (AuditRecord{Verdict: verdict}).EventType(); got != want {
			t.Fatalf("%s -> %s", verdict, got)
		}
	}
}

func TestAuditPayloadAndTemplateVersion(t *testing.T) {
	rec := AuditRecord{
		AcceptanceID:   "acc-1",
		TaskID:         "task-1",
		Stage:          "c",
		AttemptID:      "task-1-c-1",
		PromptVersion:  "acceptance.v2",
		Model:          "qwen-max",
		InputDigest:    "digest-1",
		DurationMs:     120,
		Verdict:        VerdictFail,
		VerifiedBy:     VerifiedByProgram,
		Evidence:       []Evidence{{Kind: "test", Ref: "go test ./..."}},
		FailedCriteria: []FailedCriterion{{CriterionID: "c-1", Category: "impl_gap"}},
	}
	p := rec.Payload()
	if p["prompt_version"] != "acceptance.v2" {
		t.Fatalf("audit must carry prompt version, got %v", p["prompt_version"])
	}
	if p["verdict"] != VerdictFail || p["input_digest"] != "digest-1" {
		t.Fatalf("payload = %v", p)
	}
	if _, ok := p["evidence"]; !ok {
		t.Fatal("evidence missing from audit payload")
	}
}

func TestGuardToolReadOnly(t *testing.T) {
	for _, tool := range AcceptanceReadOnlyTools {
		if err := GuardTool(tool); err != nil {
			t.Fatalf("read-only tool %s rejected: %v", tool, err)
		}
	}
	for _, tool := range []string{"write_file", "edit_file", "delete", "move", "apply_patch"} {
		var ce *ContextError
		err := GuardTool(tool)
		if err == nil || !asContextError(err, &ce) || ce.Code != CodeReadOnlyViolation {
			t.Fatalf("write tool %s must be rejected, got %v", tool, err)
		}
	}
	if err := GuardTool("shell_exec"); err == nil {
		t.Fatal("unknown tool must be rejected")
	}
}

func TestSecurityViolationPayload(t *testing.T) {
	p := SecurityViolationPayload("write_file", "c", "acc-1")
	if p["decision"] != "denied" || p["policy"] != "acceptance.read_only" || p["acceptance_id"] != "acc-1" {
		t.Fatalf("payload = %v", p)
	}
}

func hasIssue(issues []string, want string) bool {
	for _, i := range issues {
		if strings.Contains(i, want) {
			return true
		}
	}
	return false
}

func asContextError(err error, target **ContextError) bool {
	if ce, ok := err.(*ContextError); ok {
		*target = ce
		return true
	}
	return false
}

func TestBuildRetryContext(t *testing.T) {
	ev := []Evidence{{Kind: "test", Ref: "go test ./...", Excerpt: "FAIL"}}
	fc := []FailedCriterion{{CriterionID: "c-1", Category: "impl_gap"}}
	ctx := BuildRetryContext("acc-1", "task-1-c-1", ev, fc)
	if ctx["previous_acceptance_id"] != "acc-1" || ctx["previous_attempt_id"] != "task-1-c-1" {
		t.Fatalf("retry context identity = %v", ctx)
	}
	gotEv, ok := ctx["evidence"].([]Evidence)
	if !ok || len(gotEv) != 1 || gotEv[0].Ref != "go test ./..." {
		t.Fatalf("evidence must stay structured: %v", ctx["evidence"])
	}
	gotFc, ok := ctx["failed_criteria"].([]FailedCriterion)
	if !ok || len(gotFc) != 1 || gotFc[0].Category != "impl_gap" {
		t.Fatalf("failed criteria must stay structured: %v", ctx["failed_criteria"])
	}
}

func TestTempWorkspaceCleanup(t *testing.T) {
	home := t.TempDir()
	ws, err := NewTempWorkspace(home, "acc-1")
	if err != nil {
		t.Fatalf("new temp workspace: %v", err)
	}
	if _, err := os.Stat(ws.Path()); err != nil {
		t.Fatalf("temp workspace missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path(), "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(ws.Path()); !os.IsNotExist(err) {
		t.Fatal("temp workspace must be removed and verified empty")
	}
	if _, err := NewTempWorkspace("", "acc-1"); err == nil {
		t.Fatal("empty harness home must fail")
	}
}
