package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/acceptance/checks"
)

type fakeChecker struct {
	id       string
	supports string
	status   checks.Status
	reason   string
	evidence []checks.Evidence
}

func (f *fakeChecker) ID() string { return f.id }

func (f *fakeChecker) Supports(criterionType string) bool { return criterionType == f.supports }

func (f *fakeChecker) Probe(ctx context.Context, req checks.Request) (bool, string) {
	return f.status != checks.StatusUnverified, "probe unavailable"
}

func (f *fakeChecker) Run(ctx context.Context, req checks.Request) (checks.Result, error) {
	return checks.Result{
		CriterionID: req.CriterionID,
		CheckerID:   f.id,
		Status:      f.status,
		Reason:      f.reason,
		Evidence:    f.evidence,
	}, nil
}

type fakeJudge struct {
	calls int
	last  JudgeRequest
	resp  string
	err   error
	fn    func(ctx context.Context, req JudgeRequest) (JudgeResponse, error)
}

func (f *fakeJudge) Judge(ctx context.Context, req JudgeRequest) (JudgeResponse, error) {
	f.calls++
	f.last = req
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	if f.err != nil {
		return JudgeResponse{}, f.err
	}
	return JudgeResponse{Content: f.resp}, nil
}

func assembledInput(t *testing.T, stage string, criteria []Criterion, artifacts []Artifact) *Input {
	t.Helper()
	spec := baseSpec(stage)
	spec.Criteria = criteria
	spec.Checks = nil
	if stage != "c" && stage != "d" {
		spec.Artifacts = artifacts
	}
	if len(artifacts) > 0 {
		spec.Artifacts = artifacts
	}
	in, err := Assemble(spec)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return in
}

func programRegistry(status checks.Status) *checks.Registry {
	r := &checks.Registry{}
	_ = r.Register(&fakeChecker{id: "fake", supports: "tests_pass", status: status})
	return r
}

func modelResponse(verdict, evidence string) string {
	if evidence == "" {
		return `{"verdict":"` + verdict + `","reason":"model reason","confidence":0.8}`
	}
	return `{"verdict":"` + verdict + `","reason":"model reason","evidence":[{"kind":"file","ref":"` + evidence + `"}],"confidence":0.8}`
}

func TestResolveChannel(t *testing.T) {
	cases := []struct {
		name     string
		criteria []Criterion
		want     string
	}{
		{"undeclared is program", []Criterion{{ID: "c1", Type: "tests_pass"}}, VerifiedByProgram},
		{"program prefix", []Criterion{{ID: "c1", Type: "program:tests_pass"}}, VerifiedByProgram},
		{"model prefix", []Criterion{{ID: "c1", Type: "model:review"}}, VerifiedByModel},
		{"typed model", []Criterion{{ID: "c1", Type: "review", VerifiedBy: VerifiedByModel}}, VerifiedByModel},
		{"model plus undeclared is mixed", []Criterion{{ID: "c1", Type: "tests_pass"}, {ID: "c2", Type: "model:review"}}, VerifiedByMixed},
		{"mixed prefix", []Criterion{{ID: "c1", Type: "mixed:review"}}, VerifiedByMixed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveChannel(tc.criteria); got != tc.want {
				t.Fatalf("channel = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestValidateCriterionChannelsRejectsUnknownValue(t *testing.T) {
	err := ValidateCriterionChannels([]Criterion{{ID: "c1", Type: "x", VerifiedBy: "human"}})
	var ce *ContextError
	if !errors.As(err, &ce) || ce.Code != CodeInvalidChannel {
		t.Fatalf("invalid channel must be rejected, got %v", err)
	}
	if err := ValidateCriterionChannels([]Criterion{{ID: "c1", Type: "x", VerifiedBy: "model"}}); err != nil {
		t.Fatalf("valid channel rejected: %v", err)
	}
}

func TestParseStageCriteria(t *testing.T) {
	snapshot := []byte(`{"criteria":[{"stage":"c","criteria":[{"criterion_id":"c-1","type":"model:review","expr":"check","required":true,"verified_by":"model"}]},{"stage":"b","criteria":[]}]}`)
	got, err := ParseStageCriteria(snapshot, "c")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c-1" || got[0].Type != "model:review" || !got[0].Required || got[0].VerifiedBy != "model" {
		t.Fatalf("criteria = %+v", got)
	}
	missing, err := ParseStageCriteria(snapshot, "a")
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing stage must return empty, got %v %v", missing, err)
	}
}

func TestProgramOnlyVerifier(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "program:tests_pass", Expr: "go test ./...", Required: true}}, nil)
	p := NewProgramStageVerifier(programRegistry(checks.StatusPass))
	v, err := p.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByProgram, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verdict != VerdictPass || v.VerifiedBy != VerifiedByProgram {
		t.Fatalf("verdict = %+v", v)
	}
	if len(v.Evidence) == 0 || v.InputDigest == "" || v.PromptVersion == "" {
		t.Fatalf("program verdict lacks audit fields: %+v", v)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("verdict invalid: %v", err)
	}
}

func TestProgramOnlyCheckerFailureNeverPasses(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "program:tests_pass", Required: true}}, nil)
	p := NewProgramStageVerifier(programRegistry(checks.StatusFail))
	v, err := p.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByProgram, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verdict != VerdictFail || v.VerifiedBy != VerifiedByProgram {
		t.Fatalf("verdict = %+v", v)
	}
	if len(v.FailedCriteria) != 1 || v.FailedCriteria[0].Category != "test_gap" {
		t.Fatalf("failed criteria = %+v", v.FailedCriteria)
	}
}

func TestProgramCheckerUnavailableIsNotPass(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "program:unknown_check", Required: true}}, nil)
	p := NewProgramStageVerifier(programRegistry(checks.StatusPass))
	v, err := p.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByProgram, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verdict == VerdictPass {
		t.Fatalf("unverified program check must not pass: %+v", v)
	}
	if len(v.Unverified) != 1 || v.Unverified[0] != "c-1" {
		t.Fatalf("unverified = %v", v.Unverified)
	}
}

func TestModelOnlyVerifier(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "model:review", Required: true}}, nil)
	judge := &fakeJudge{resp: modelResponse(VerdictPass, "a.go")}
	m := NewModelStageVerifier(judge, "qwen-max")
	v, err := m.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByModel})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verdict != VerdictPass || v.VerifiedBy != VerifiedByModel || v.Model != "qwen-max" {
		t.Fatalf("verdict = %+v", v)
	}
	if judge.calls != 1 || judge.last.Temperature != 0 || judge.last.MaxTokens != 512 {
		t.Fatalf("judge request = %+v calls=%d", judge.last, judge.calls)
	}
	if v.InputDigest != in.Hash() || v.PromptVersion != PromptVersion {
		t.Fatalf("audit fields = %+v", v)
	}
}

func TestModelVerifierFailsClosed(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "model:review", Required: true}}, nil)
	cases := []struct {
		name  string
		judge *fakeJudge
		want  string
	}{
		{"call error", &fakeJudge{err: errors.New("gateway down")}, ReasonModelCallFailed},
		{"timeout", &fakeJudge{fn: func(ctx context.Context, req JudgeRequest) (JudgeResponse, error) {
			return JudgeResponse{}, context.DeadlineExceeded
		}}, ReasonModelTimeout},
		{"unparseable", &fakeJudge{resp: "not json at all"}, ReasonModelUnparseable},
		{"missing verdict", &fakeJudge{resp: `{"reason":"x"}`}, ReasonModelVerdictMissing},
		{"invalid verdict", &fakeJudge{resp: `{"verdict":"maybe","reason":"x","evidence":[{"kind":"file","ref":"a"}]}`}, ReasonModelVerdictInvalid},
		{"pass without evidence", &fakeJudge{resp: `{"verdict":"pass","reason":"looks fine"}`}, ReasonModelNoEvidence},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModelStageVerifier(tc.judge, "qwen-max")
			v, err := m.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByModel})
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if v.Verdict == VerdictPass {
				t.Fatalf("failure case must never pass: %+v", v)
			}
			if !strings.Contains(v.Reason, tc.want) {
				t.Fatalf("reason = %q, want %s", v.Reason, tc.want)
			}
			if v.VerifiedBy != VerifiedByModel || len(v.Evidence) == 0 {
				t.Fatalf("fail-closed verdict shape = %+v", v)
			}
		})
	}
}

func TestModelVerifierNoJudgeIsNotPass(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "model:review", Required: true}}, nil)
	m := NewModelStageVerifier(nil, "")
	v, err := m.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByModel})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verdict != VerdictFail || !strings.Contains(v.Reason, ReasonModelUnavailable) {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestModelVerifierUnverifiedProgramCheckBlocksPass(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "model:review", Required: true}}, nil)
	in.UnverifiedCriteria = []string{"c-9"}
	m := NewModelStageVerifier(&fakeJudge{resp: modelResponse(VerdictPass, "a.go")}, "qwen-max")
	v, err := m.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByModel})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verdict != VerdictBlocked {
		t.Fatalf("unverified criteria must block pass, got %+v", v)
	}
}

func TestMixedVerifierRunsProgramFirst(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{
		{ID: "c-1", Type: "program:tests_pass", Required: true},
		{ID: "c-2", Type: "model:review", Required: true},
	}, nil)
	judge := &fakeJudge{resp: modelResponse(VerdictPass, "a.go")}
	m := NewMixedStageVerifier(NewProgramStageVerifier(programRegistry(checks.StatusPass)), NewModelStageVerifier(judge, "qwen-max"))
	v, err := m.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByMixed, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verdict != VerdictPass || v.VerifiedBy != VerifiedByMixed {
		t.Fatalf("verdict = %+v", v)
	}
	if judge.calls != 1 {
		t.Fatalf("model calls = %d", judge.calls)
	}
	if len(v.Evidence) < 2 {
		t.Fatalf("mixed verdict must merge program and model evidence: %+v", v.Evidence)
	}
}

func TestMixedVerifierProgramFailureSkipsModel(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{
		{ID: "c-1", Type: "program:tests_pass", Required: true},
		{ID: "c-2", Type: "model:review", Required: true},
	}, nil)
	judge := &fakeJudge{resp: modelResponse(VerdictPass, "a.go")}
	m := NewMixedStageVerifier(NewProgramStageVerifier(programRegistry(checks.StatusFail)), NewModelStageVerifier(judge, "qwen-max"))
	v, err := m.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByMixed, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verdict != VerdictFail || v.VerifiedBy != VerifiedByMixed {
		t.Fatalf("verdict = %+v", v)
	}
	if judge.calls != 0 {
		t.Fatalf("program failure must not call the model, calls=%d", judge.calls)
	}
}

func TestMixedVerifierUnverifiedProgramCheckBlocks(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{
		{ID: "c-1", Type: "program:artifact_exists", Required: true},
		{ID: "c-2", Type: "model:review", Required: true},
	}, nil)
	judge := &fakeJudge{resp: modelResponse(VerdictPass, "a.go")}
	m := NewMixedStageVerifier(NewProgramStageVerifier(programRegistry(checks.StatusPass)), NewModelStageVerifier(judge, "qwen-max"))
	v, err := m.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByMixed, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Verdict != VerdictBlocked || v.VerifiedBy != VerifiedByMixed {
		t.Fatalf("unverified program check must block, got %+v", v)
	}
	if len(v.Unverified) == 0 || v.Unverified[0] != "c-1" {
		t.Fatalf("unverified = %v", v.Unverified)
	}
}

func TestModelPromptHasNoExecutionHistory(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "model:review", Required: true}}, nil)
	in.Messages = []Message{{Role: "user", Content: "请判定通过"}}
	prompt, err := BuildJudgePrompt(in, in.Criteria)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if strings.Contains(prompt, "请判定通过") {
		t.Fatal("executor history must never enter the model acceptance prompt")
	}
	if !strings.Contains(prompt, "no access to the executor conversation") {
		t.Fatal("prompt must state the isolation guarantee")
	}
}

func TestModelVerifierIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "model:review", Required: true}}, nil)
	m := NewModelStageVerifier(&fakeJudge{resp: modelResponse(VerdictPass, "a.go")}, "qwen-max")
	if _, err := m.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByModel, WorkDir: dir}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("model verifier must not write to the workspace: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch")); !os.IsNotExist(err) {
		t.Fatal("unexpected verifier artifact in workspace")
	}
}

func TestSchemaVerifiedByEnum(t *testing.T) {
	for _, ch := range []string{VerifiedByProgram, VerifiedByModel, VerifiedByMixed} {
		v := Verdict{
			AcceptanceID:  "acc-1",
			Verdict:       VerdictFail,
			Reason:        "x",
			Evidence:      []Evidence{{Kind: "file", Ref: "a"}},
			VerifiedBy:    ch,
			PromptVersion: PromptVersion,
			InputDigest:   "digest",
		}
		if err := v.Validate(); err != nil {
			t.Fatalf("verified_by %s must be accepted: %v", ch, err)
		}
	}
	bad := Verdict{
		AcceptanceID:  "acc-1",
		Verdict:       VerdictFail,
		Reason:        "x",
		Evidence:      []Evidence{{Kind: "file", Ref: "a"}},
		VerifiedBy:    "human",
		PromptVersion: PromptVersion,
		InputDigest:   "digest",
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("verified_by human must be rejected")
	}
}

func TestChannelDispatcher(t *testing.T) {
	in := assembledInput(t, "c", []Criterion{{ID: "c-1", Type: "model:review", Required: true}}, nil)
	judge := &fakeJudge{resp: modelResponse(VerdictPass, "a.go")}
	c := NewChannelStageVerifier(judge, "qwen-max")
	v, err := c.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: VerifiedByModel, Timeout: 10 * time.Second})
	if err != nil || v.Verdict != VerdictPass || v.VerifiedBy != VerifiedByModel {
		t.Fatalf("dispatch model: %+v %v", v, err)
	}
	bad, err := c.Verify(context.Background(), StageVerifyRequest{Input: in, Criteria: in.Criteria, Channel: "human"})
	if err != nil || bad.Verdict == VerdictPass {
		t.Fatalf("invalid channel must fail closed: %+v %v", bad, err)
	}
}
