package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/acceptance"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/services/harnessloop-service/internal/repository"
)

type legacyAcceptor struct {
	calls   int
	verdict acceptance.Verdict
}

func (c *legacyAcceptor) Evaluate(ctx context.Context, in AcceptanceInput) (acceptance.Verdict, error) {
	c.calls++
	if c.verdict.AcceptanceID != "" {
		return c.verdict, nil
	}
	return acceptance.Verdict{
		AcceptanceID:  "acc-legacy",
		Verdict:       acceptance.VerdictPass,
		Reason:        "legacy acceptor",
		Evidence:      []acceptance.Evidence{{Kind: "test", Ref: "legacy"}},
		VerifiedBy:    acceptance.VerifiedByProgram,
		PromptVersion: acceptance.PromptVersion,
		InputDigest:   "legacy",
	}, nil
}

type recordingStageVerifier struct {
	calls   int
	req     acceptance.StageVerifyRequest
	verdict acceptance.Verdict
	err     error
}

func (r *recordingStageVerifier) Verify(ctx context.Context, req acceptance.StageVerifyRequest) (acceptance.Verdict, error) {
	r.calls++
	r.req = req
	if r.err != nil {
		return acceptance.Verdict{}, r.err
	}
	return r.verdict, nil
}

func channelTask(t *testing.T, snapshot string, workspace string) *repository.TaskRun {
	t.Helper()
	if workspace == "" {
		workspace = t.TempDir()
	}
	return &repository.TaskRun{
		TaskID:         "task-1",
		TenantID:       "tenant-a",
		ProjectID:      "proj-a",
		WorkspaceRoot:  workspace,
		Goal:           "implement the harness loop",
		GoalHash:       "goal-hash",
		CurrentStage:   fsm.StageExecute,
		Status:         repository.TaskRunning,
		PolicySnapshot: []byte(snapshot),
		BudgetTokens:   1000,
		ConsumedTokens: 100,
	}
}

func criteriaSnapshot(stage, criteria string) string {
	return `{"criteria":[{"stage":"` + stage + `","criteria":[` + criteria + `]}]}`
}

func modelVerdict(channel string) acceptance.Verdict {
	return acceptance.Verdict{
		AcceptanceID:  "acc-verifier",
		Verdict:       acceptance.VerdictPass,
		Reason:        "verifier passed",
		Evidence:      []acceptance.Evidence{{Kind: "file", Ref: "a.go"}},
		VerifiedBy:    channel,
		PromptVersion: acceptance.PromptVersion,
		InputDigest:   "digest",
	}
}

func stageRunResult() *StageRunResult {
	return &StageRunResult{
		FinishReceived: true,
		FinishArgs: fsm.StageFinishArgs{
			Artifacts: []fsm.ArtifactRef{{Path: "a.go", Hash: "sha256:aa", Size: 10}},
		},
	}
}

func TestEvaluateUndeclaredCriteriaKeepsProgramPath(t *testing.T) {
	rt := &Runtime{}
	acceptor := &legacyAcceptor{}
	rt.Acceptor = acceptor
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByModel)}
	rt.StageVerifier = verifier

	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"tests_pass","required":true}`), "")
	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if acceptor.calls != 1 {
		t.Fatalf("legacy acceptor calls = %d", acceptor.calls)
	}
	if verifier.calls != 0 {
		t.Fatalf("stage verifier must not run for undeclared criteria, calls = %d", verifier.calls)
	}
	if v.Verdict != acceptance.VerdictPass || v.VerifiedBy != acceptance.VerifiedByProgram {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestEvaluateDeclaredProgramStaysLegacy(t *testing.T) {
	rt := &Runtime{}
	acceptor := &legacyAcceptor{}
	rt.Acceptor = acceptor
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByProgram)}
	rt.StageVerifier = verifier

	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"program:tests_pass","required":true}`), "")
	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if acceptor.calls != 1 || verifier.calls != 0 {
		t.Fatalf("acceptor=%d verifier=%d", acceptor.calls, verifier.calls)
	}
	if v.AcceptanceID != "acc-legacy" {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestEvaluateSelectsModelChannel(t *testing.T) {
	rt := &Runtime{}
	acceptor := &legacyAcceptor{}
	rt.Acceptor = acceptor
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByModel)}
	rt.StageVerifier = verifier

	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"model:review","required":true}`), "")
	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if verifier.calls != 1 || acceptor.calls != 0 {
		t.Fatalf("verifier=%d acceptor=%d", verifier.calls, acceptor.calls)
	}
	if verifier.req.Channel != acceptance.VerifiedByModel {
		t.Fatalf("channel = %s", verifier.req.Channel)
	}
	if verifier.req.Input == nil || verifier.req.Input.GoalHash != "goal-hash" {
		t.Fatalf("acceptance input not injected: %+v", verifier.req.Input)
	}
	if len(verifier.req.Input.Artifacts) != 1 || verifier.req.Input.Artifacts[0].Path != "a.go" {
		t.Fatalf("artifact injection = %+v", verifier.req.Input.Artifacts)
	}
	if v.Verdict != acceptance.VerdictPass || v.VerifiedBy != acceptance.VerifiedByModel {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestEvaluateSelectsMixedChannel(t *testing.T) {
	rt := &Runtime{}
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByMixed)}
	rt.StageVerifier = verifier

	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"program:tests_pass","required":true},{"criterion_id":"c-2","type":"model:review","required":true}`), "")
	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if verifier.calls != 1 || verifier.req.Channel != acceptance.VerifiedByMixed {
		t.Fatalf("calls=%d channel=%s", verifier.calls, verifier.req.Channel)
	}
	if v.VerifiedBy != acceptance.VerifiedByMixed {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestEvaluateModelChannelWithoutVerifierFailsClosed(t *testing.T) {
	rt := &Runtime{}
	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"model:review","required":true}`), "")

	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if v.Verdict == acceptance.VerdictPass {
		t.Fatalf("missing verifier must not pass: %+v", v)
	}
	if !strings.Contains(v.Reason, acceptance.ReasonVerifierNotConfigured) {
		t.Fatalf("reason = %q", v.Reason)
	}
	if v.VerifiedBy != acceptance.VerifiedByModel {
		t.Fatalf("verified_by = %s", v.VerifiedBy)
	}
}

func TestEvaluateInvalidChannelFailsClosed(t *testing.T) {
	rt := &Runtime{}
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByModel)}
	rt.StageVerifier = verifier
	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"review","verified_by":"human","required":true}`), "")

	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if v.Verdict == acceptance.VerdictPass || verifier.calls != 0 {
		t.Fatalf("invalid channel must fail closed: %+v calls=%d", v, verifier.calls)
	}
	if !strings.Contains(v.Reason, "invalid_channel") {
		t.Fatalf("reason = %q", v.Reason)
	}
}

func TestEvaluateIncompleteContextFailsClosed(t *testing.T) {
	rt := &Runtime{}
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByModel)}
	rt.StageVerifier = verifier
	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"model:review","required":true}`), "")
	task.WorkspaceRoot = ""

	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if v.Verdict == acceptance.VerdictPass || verifier.calls != 0 {
		t.Fatalf("missing workspace must fail closed: %+v calls=%d", v, verifier.calls)
	}
	if !strings.Contains(v.Reason, acceptance.ReasonContextIncomplete) {
		t.Fatalf("reason = %q", v.Reason)
	}
}

func TestEvaluateVerifierErrorFailsClosed(t *testing.T) {
	rt := &Runtime{}
	rt.StageVerifier = &recordingStageVerifier{err: errors.New("judge exploded")}
	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"model:review","required":true}`), "")

	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if v.Verdict == acceptance.VerdictPass {
		t.Fatalf("verifier error must not pass: %+v", v)
	}
	if !strings.Contains(v.Reason, "judge exploded") {
		t.Fatalf("reason = %q", v.Reason)
	}
}

func TestEvaluateRejectsInvalidVerifierVerdict(t *testing.T) {
	rt := &Runtime{}
	rt.StageVerifier = &recordingStageVerifier{verdict: acceptance.Verdict{
		AcceptanceID: "acc-1",
		Verdict:      acceptance.VerdictPass,
		Reason:       "no audit fields",
	}}
	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"model:review","required":true}`), "")

	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if v.Verdict == acceptance.VerdictPass {
		t.Fatalf("invalid verifier verdict must fail closed: %+v", v)
	}
	if !strings.Contains(v.Reason, acceptance.ReasonContextIncomplete) {
		t.Fatalf("reason = %q", v.Reason)
	}
}

func TestEvaluateUndeclaredMalformedSnapshotKeepsLegacy(t *testing.T) {
	rt := &Runtime{}
	acceptor := &legacyAcceptor{}
	rt.Acceptor = acceptor
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByModel)}
	rt.StageVerifier = verifier
	task := channelTask(t, `{not json`, "")
	task.Goal = "g"
	task.GoalHash = "h"

	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if acceptor.calls != 1 || verifier.calls != 0 {
		t.Fatalf("acceptor=%d verifier=%d", acceptor.calls, verifier.calls)
	}
	if v.AcceptanceID != "acc-legacy" {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestEvaluateForwardsTimeoutAndBudgetSnapshot(t *testing.T) {
	rt := &Runtime{}
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByModel)}
	rt.StageVerifier = verifier
	task := channelTask(t, criteriaSnapshot("c", `{"criterion_id":"c-1","type":"model:review","required":true}`), "")
	in := runtimeInput()
	in.StageTimeout = 5 * time.Second

	_ = rt.evaluate(context.Background(), in, task, nil, fsm.StageExecute, fsm.Attempt{AttemptNo: 1, AttemptID: "a1"}, stageRunResult())

	if verifier.req.Input.Constraints.BudgetRemainingTokens != 900 {
		t.Fatalf("budget = %d", verifier.req.Input.Constraints.BudgetRemainingTokens)
	}
	if verifier.req.Input.Constraints.WorkspaceRoot != task.WorkspaceRoot {
		t.Fatalf("workspace = %q", verifier.req.Input.Constraints.WorkspaceRoot)
	}
	if verifier.req.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v", verifier.req.Timeout)
	}
}

func TestEvaluateInjectsDAggregation(t *testing.T) {
	rt := &Runtime{}
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByMixed)}
	rt.StageVerifier = verifier
	task := channelTask(t, criteriaSnapshot("d", `{"criterion_id":"c-1","type":"model:review","required":true}`), "")
	report := &orchestrator.AggregationReport{
		TaskID:  "task-1",
		DagID:   "dag-1",
		Summary: orchestrator.AggregationSummary{Total: 2, Accepted: 2},
		Items: []orchestrator.AggregationItem{{
			SubtaskID: "s1",
			Artifacts: []orchestrator.AggregatedArtifact{{Path: "out.go", Hash: "sha256:bb"}},
		}},
	}

	v := rt.evaluate(context.Background(), runtimeInput(), task, report, fsm.StageVerify, fsm.Attempt{AttemptNo: 1, AttemptID: "d1"}, stageRunResult())

	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d", verifier.calls)
	}
	agg := verifier.req.Input.Aggregation
	if agg == nil || agg.ReportID != "dag-1" || agg.AcceptedCount != 2 || len(agg.Artifacts) != 1 {
		t.Fatalf("aggregation injection = %+v", agg)
	}
	if v.VerifiedBy != acceptance.VerifiedByMixed {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestEvaluateDWithoutAggregationFailsClosed(t *testing.T) {
	rt := &Runtime{}
	verifier := &recordingStageVerifier{verdict: modelVerdict(acceptance.VerifiedByMixed)}
	rt.StageVerifier = verifier
	task := channelTask(t, criteriaSnapshot("d", `{"criterion_id":"c-1","type":"model:review","required":true}`), "")

	v := rt.evaluate(context.Background(), runtimeInput(), task, nil, fsm.StageVerify, fsm.Attempt{AttemptNo: 1, AttemptID: "d1"}, stageRunResult())

	if v.Verdict == acceptance.VerdictPass || verifier.calls != 0 {
		t.Fatalf("missing aggregation must fail closed: %+v calls=%d", v, verifier.calls)
	}
	if !strings.Contains(v.Reason, acceptance.CodeAggregationMissing) {
		t.Fatalf("reason = %q", v.Reason)
	}
}
