package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-platform/pkg/harnessloop/acceptance/checks"
)

// StageVerifier is the independent acceptance channel added for REQ-011~020.
// It is deliberately separate from pkg/agent.Verifier: that interface is
// fail-open, while acceptance must never count an unverified result as a pass.
type StageVerifier interface {
	Verify(ctx context.Context, req StageVerifyRequest) (Verdict, error)
}

type StageVerifyRequest struct {
	Input    *Input
	Criteria []Criterion
	Channel  string
	WorkDir  string
	Timeout  time.Duration
}

const (
	CodeInvalidChannel = "E_INVALID_ACCEPTANCE_CHANNEL"

	ReasonVerifierNotConfigured = "stage_verifier_not_configured"
	ReasonContextIncomplete     = "acceptance_context_incomplete"
	ReasonModelUnavailable      = "model_verifier_unavailable"
	ReasonModelCallFailed       = "model_verifier_error"
	ReasonModelTimeout          = "model_verifier_timeout"
	ReasonModelUnparseable      = "model_verdict_unparseable"
	ReasonModelVerdictMissing   = "model_verdict_missing"
	ReasonModelVerdictInvalid   = "model_verdict_invalid"
	ReasonModelNoEvidence       = "model_pass_without_evidence"
	ReasonProgramFailed         = "program_check_failed"
	ReasonProgramUnverified     = "program_check_unverified"
)

// DeclaredChannel returns the channel explicitly declared by a criterion.
// The acceptance proto cannot carry a new field, so production callers may
// prefix the criterion type ("model:review", "mixed:artifact_exists", ...).
// Criterion.VerifiedBy is the typed form used by callers that build the
// acceptance context in-process.
func DeclaredChannel(c Criterion) string {
	if ch := normalizeChannel(c.VerifiedBy); ch != "" {
		return ch
	}
	lower := strings.ToLower(c.Type)
	for _, ch := range []string{VerifiedByMixed, VerifiedByModel, VerifiedByProgram} {
		if strings.HasPrefix(lower, ch+":") {
			return ch
		}
	}
	return ""
}

func normalizeChannel(ch string) string {
	switch strings.ToLower(strings.TrimSpace(ch)) {
	case VerifiedByProgram:
		return VerifiedByProgram
	case VerifiedByModel:
		return VerifiedByModel
	case VerifiedByMixed:
		return VerifiedByMixed
	default:
		return ""
	}
}

// ValidateCriterionChannels rejects malformed declarations instead of silently
// falling back to the program channel (which would make a typo fail-open on the
// legacy path).
func ValidateCriterionChannels(criteria []Criterion) error {
	for _, c := range criteria {
		if strings.TrimSpace(c.VerifiedBy) != "" && normalizeChannel(c.VerifiedBy) == "" {
			return &ContextError{
				Code: CodeInvalidChannel,
				Msg:  fmt.Sprintf("criterion %s declares invalid verified_by %q", c.ID, c.VerifiedBy),
			}
		}
	}
	return nil
}

// ResolveChannel aggregates criterion declarations into the channel the runtime
// must use. No model/mixed declaration keeps the historical program behaviour.
func ResolveChannel(criteria []Criterion) string {
	hasModel := false
	hasProgram := false
	for _, c := range criteria {
		switch DeclaredChannel(c) {
		case VerifiedByMixed:
			return VerifiedByMixed
		case VerifiedByModel:
			hasModel = true
		case VerifiedByProgram:
			hasProgram = true
		default:
			hasProgram = true
		}
	}
	if !hasModel {
		return VerifiedByProgram
	}
	if hasProgram {
		return VerifiedByMixed
	}
	return VerifiedByModel
}

// StripChannel removes the channel declaration from the criterion type before
// it is handed to a program checker or to the model prompt.
func StripChannel(c Criterion) Criterion {
	out := c
	out.VerifiedBy = ""
	lower := strings.ToLower(out.Type)
	for _, ch := range []string{VerifiedByMixed, VerifiedByModel, VerifiedByProgram} {
		if strings.HasPrefix(lower, ch+":") {
			out.Type = out.Type[len(ch)+1:]
			break
		}
	}
	return out
}

// ParseStageCriteria extracts the criteria for one stage from a task policy
// snapshot. Both the proto JSON spelling (criterion_id) and camelCase are
// accepted so the runtime does not depend on a single protobuf generator.
func ParseStageCriteria(snapshot []byte, stage string) ([]Criterion, error) {
	if len(snapshot) == 0 {
		return nil, nil
	}
	var root struct {
		Criteria []struct {
			Stage    string           `json:"stage"`
			Criteria []map[string]any `json:"criteria"`
		} `json:"criteria"`
	}
	if err := json.Unmarshal(snapshot, &root); err != nil {
		return nil, err
	}
	for _, sc := range root.Criteria {
		if sc.Stage != stage {
			continue
		}
		out := make([]Criterion, 0, len(sc.Criteria))
		for _, raw := range sc.Criteria {
			out = append(out, Criterion{
				ID:         firstString(raw, "criterion_id", "criterionId"),
				Type:       firstString(raw, "type"),
				Expr:       firstString(raw, "expr"),
				Required:   firstBool(raw, "required"),
				VerifiedBy: firstString(raw, "verified_by", "verifiedBy"),
			})
		}
		return out, nil
	}
	return nil, nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func firstBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return false
}

// Judge is the minimal LLM surface the model channel needs. Keeping it local to
// acceptance avoids coupling the verification rules to a concrete LLM client.
type Judge interface {
	Judge(ctx context.Context, req JudgeRequest) (JudgeResponse, error)
}

type JudgeRequest struct {
	Model        string
	SystemPrompt string
	Prompt       string
	Temperature  float64
	MaxTokens    int
}

type JudgeResponse struct {
	Content   string
	TokensIn  int
	TokensOut int
}

type ProgramStageVerifier struct {
	Registry *checks.Registry
	Timeout  time.Duration
}

func NewProgramStageVerifier(registry *checks.Registry) *ProgramStageVerifier {
	if registry == nil {
		registry = checks.NewRegistry()
	}
	return &ProgramStageVerifier{Registry: registry, Timeout: 120 * time.Second}
}

type programOutcome struct {
	Checks     []CheckResult
	Evidence   []Evidence
	Failed     []FailedCriterion
	Unverified []string
}

func (p *ProgramStageVerifier) run(ctx context.Context, req StageVerifyRequest) programOutcome {
	var out programOutcome
	for _, raw := range req.Criteria {
		c := StripChannel(raw)
		timeout := p.Timeout
		if req.Timeout > 0 && req.Timeout < timeout {
			timeout = req.Timeout
		}
		if p.Registry == nil {
			out.Unverified = append(out.Unverified, c.ID)
			out.Checks = append(out.Checks, CheckResult{CriterionID: c.ID, Type: c.Type, Output: "program registry is not configured"})
			continue
		}
		if _, ok := p.Registry.Resolve(c.Type); !ok {
			out.Unverified = append(out.Unverified, c.ID)
			out.Checks = append(out.Checks, CheckResult{CriterionID: c.ID, Type: c.Type, Output: "no program checker supports this criterion"})
			out.Evidence = append(out.Evidence, Evidence{Kind: "command", Ref: "program:" + c.ID, Excerpt: "no program checker available"})
			continue
		}
		res := p.Registry.Run(ctx, checks.Request{
			CriterionID: c.ID,
			Type:        c.Type,
			Expr:        c.Expr,
			WorkDir:     req.WorkDir,
			Timeout:     timeout,
		})
		out.Checks = append(out.Checks, CheckResult{
			CriterionID: c.ID,
			Type:        c.Type,
			Passed:      res.Status == checks.StatusPass,
			Output:      firstNonEmpty(res.Reason, res.Err, string(res.Status)),
			DurationMs:  res.DurationMs,
		})
		switch res.Status {
		case checks.StatusPass:
			out.Evidence = append(out.Evidence, checkEvidence(res, c.ID))
		case checks.StatusFail:
			out.Failed = append(out.Failed, FailedCriterion{CriterionID: c.ID, Category: categoryForCheck(c.Type)})
			out.Evidence = append(out.Evidence, checkEvidence(res, c.ID))
		default:
			out.Unverified = append(out.Unverified, c.ID)
			out.Evidence = append(out.Evidence, Evidence{
				Kind:    "command",
				Ref:     firstNonEmpty(res.CheckerID, "program") + ":" + c.ID,
				Excerpt: firstNonEmpty(res.Reason, res.Err, string(res.Status)),
			})
		}
	}
	if len(out.Evidence) == 0 {
		out.Evidence = append(out.Evidence, Evidence{Kind: "command", Ref: "program", Excerpt: "no program criteria"})
	}
	return out
}

func (p *ProgramStageVerifier) verdict(in *Input, out programOutcome) Verdict {
	v := Verdict{
		AcceptanceID: in.AcceptanceID,
		VerifiedBy:   VerifiedByProgram,
		Evidence:     out.Evidence,
	}
	switch {
	case len(out.Failed) > 0:
		v.Verdict = VerdictFail
		v.FailedCriteria = out.Failed
		v.Reason = ReasonProgramFailed + ": " + failedIDs(out.Failed)
	case len(out.Unverified) > 0:
		v.Verdict = VerdictBlocked
		v.Unverified = out.Unverified
		v.Reason = ReasonProgramUnverified + ": " + strings.Join(out.Unverified, ", ")
	default:
		v.Verdict = VerdictPass
		v.Reason = "program checks passed"
	}
	res, err := Finalize(v, NormalizeOptions{Stage: in.Stage, Input: in, PromptVersion: in.PromptVersion})
	if err != nil {
		return v
	}
	return res.Verdict
}

func (p *ProgramStageVerifier) Verify(ctx context.Context, req StageVerifyRequest) (Verdict, error) {
	if req.Input == nil {
		return invalidArgumentVerdict(VerifiedByProgram, "acceptance input is required"), nil
	}
	out := p.run(ctx, req)
	in := cloneInput(req.Input)
	in.Checks = append(in.Checks, out.Checks...)
	in.UnverifiedCriteria = append([]string(nil), out.Unverified...)
	return p.verdict(in, out), nil
}

type ModelStageVerifier struct {
	Judge     Judge
	Model     string
	Timeout   time.Duration
	MaxTokens int
}

func NewModelStageVerifier(judge Judge, model string) *ModelStageVerifier {
	return &ModelStageVerifier{
		Judge:     judge,
		Model:     model,
		Timeout:   30 * time.Second,
		MaxTokens: 512,
	}
}

func (m *ModelStageVerifier) Verify(ctx context.Context, req StageVerifyRequest) (Verdict, error) {
	start := time.Now()
	if req.Input == nil {
		return invalidArgumentVerdict(VerifiedByModel, "acceptance input is required"), nil
	}
	in := cloneInput(req.Input)
	if m.Judge == nil {
		return m.failure(in, ReasonModelUnavailable, "no LLM judge is configured", start), nil
	}
	prompt, err := BuildJudgePrompt(in, req.Criteria)
	if err != nil {
		return m.failure(in, ReasonContextIncomplete, err.Error(), start), nil
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if req.Timeout > 0 && req.Timeout < timeout {
		timeout = req.Timeout
	}
	judgeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	maxTokens := m.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 512
	}
	resp, err := m.Judge.Judge(judgeCtx, JudgeRequest{
		Model:        m.Model,
		SystemPrompt: in.SystemPrompt,
		Prompt:       prompt,
		Temperature:  0,
		MaxTokens:    maxTokens,
	})
	if err != nil {
		code := ReasonModelCallFailed
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(judgeCtx.Err(), context.DeadlineExceeded) {
			code = ReasonModelTimeout
		}
		return m.failure(in, code, err.Error(), start), nil
	}

	parsed, err := parseModelVerdict(resp.Content)
	if err != nil {
		return m.failure(in, ReasonModelUnparseable, err.Error(), start), nil
	}
	if parsed.Verdict == "" {
		return m.failure(in, ReasonModelVerdictMissing, "model response has no verdict field", start), nil
	}
	if !isVerdictValue(parsed.Verdict) {
		return m.failure(in, ReasonModelVerdictInvalid, fmt.Sprintf("verdict %q is not pass|fail|blocked", parsed.Verdict), start), nil
	}
	if parsed.Verdict == VerdictPass && len(parsed.Evidence) == 0 {
		return m.failure(in, ReasonModelNoEvidence, "model returned pass without evidence", start), nil
	}

	reason := strings.TrimSpace(parsed.Reason)
	if reason == "" {
		reason = "model verdict: " + parsed.Verdict
	}
	evidence := parsed.Evidence
	if len(evidence) == 0 {
		evidence = []Evidence{{Kind: "verdict", Ref: "model", Excerpt: reason}}
	}
	unverified := append([]string(nil), in.UnverifiedCriteria...)
	unverified = append(unverified, parsed.Unverified...)

	v := Verdict{
		AcceptanceID:   in.AcceptanceID,
		Verdict:        parsed.Verdict,
		Reason:         reason,
		Evidence:       evidence,
		FailedCriteria: parsed.FailedCriteria,
		Unverified:     unverified,
		VerifiedBy:     VerifiedByModel,
		Confidence:     parsed.Confidence,
		Model:          m.Model,
		InputDigest:    in.Hash(),
		PromptVersion:  in.PromptVersion,
		DurationMs:     time.Since(start).Milliseconds(),
	}
	res, err := Finalize(v, NormalizeOptions{
		Stage:         in.Stage,
		Input:         in,
		Confidence:    parsed.Confidence,
		DurationMs:    v.DurationMs,
		PromptVersion: in.PromptVersion,
		Model:         m.Model,
	})
	if err != nil {
		return m.failure(in, ReasonModelVerdictInvalid, err.Error(), start), nil
	}
	return res.Verdict, nil
}

func (m *ModelStageVerifier) failure(in *Input, code, detail string, start time.Time) Verdict {
	reason := strings.TrimSpace(code + ": " + detail)
	v := Verdict{
		AcceptanceID:   in.AcceptanceID,
		Verdict:        VerdictFail,
		Reason:         reason,
		Evidence:       []Evidence{{Kind: "verdict", Ref: "model", Excerpt: reason}},
		FailedCriteria: []FailedCriterion{{CriterionID: "acceptance", Category: "unknown"}},
		Unverified:     append([]string(nil), in.UnverifiedCriteria...),
		VerifiedBy:     VerifiedByModel,
		Model:          m.Model,
		InputDigest:    in.Hash(),
		PromptVersion:  in.PromptVersion,
		DurationMs:     time.Since(start).Milliseconds(),
	}
	res, err := Finalize(v, NormalizeOptions{
		Stage:         in.Stage,
		Input:         in,
		DurationMs:    v.DurationMs,
		PromptVersion: in.PromptVersion,
		Model:         m.Model,
	})
	if err != nil {
		return v
	}
	return res.Verdict
}

type modelVerdict struct {
	Verdict        string            `json:"verdict"`
	Reason         string            `json:"reason"`
	Evidence       []Evidence        `json:"evidence"`
	FailedCriteria []FailedCriterion `json:"failed_criteria"`
	Unverified     []string          `json:"unverified_criteria"`
	Confidence     float64           `json:"confidence"`
}

func parseModelVerdict(content string) (modelVerdict, error) {
	raw := extractJSONObject(content)
	if raw == "" {
		return modelVerdict{}, errors.New("model response is not a JSON object")
	}
	var v modelVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return modelVerdict{}, err
	}
	v.Verdict = strings.ToLower(strings.TrimSpace(v.Verdict))
	if v.Confidence < 0 {
		v.Confidence = 0
	}
	if v.Confidence > 1 {
		v.Confidence = 1
	}
	return v, nil
}

func extractJSONObject(content string) string {
	s := strings.TrimSpace(content)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

func isVerdictValue(v string) bool {
	return v == VerdictPass || v == VerdictFail || v == VerdictBlocked
}

// BuildJudgePrompt injects exactly the acceptance context and nothing from the
// executor conversation. Artifact excerpts are read-only snapshots supplied by
// the caller; the verifier itself performs no file or tool side effects.
func BuildJudgePrompt(in *Input, criteria []Criterion) (string, error) {
	stripped := make([]Criterion, 0, len(criteria))
	for _, c := range criteria {
		stripped = append(stripped, StripChannel(c))
	}
	payload := struct {
		Goal         string             `json:"goal"`
		GoalHash     string             `json:"goal_hash"`
		Stage        string             `json:"stage"`
		AttemptID    string             `json:"attempt_id"`
		Criteria     []Criterion        `json:"criteria"`
		Artifacts    []Artifact         `json:"artifacts"`
		ProgramCheck []CheckResult      `json:"program_checks"`
		Constraints  Constraints        `json:"constraints"`
		Previous     *PreviousVerdict   `json:"previous,omitempty"`
		Aggregation  *AggregationReport `json:"aggregation,omitempty"`
		Unverified   []string           `json:"unverified_criteria,omitempty"`
	}{
		Goal:         in.Goal,
		GoalHash:     in.GoalHash,
		Stage:        in.Stage,
		AttemptID:    in.AttemptID,
		Criteria:     stripped,
		Artifacts:    in.Artifacts,
		ProgramCheck: in.Checks,
		Constraints:  in.Constraints,
		Previous:     in.Previous,
		Aggregation:  in.Aggregation,
		Unverified:   in.UnverifiedCriteria,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`Judge whether the acceptance criteria for this stage are met.
You are an independent verifier. You have no access to the executor conversation or tools.
Judge only the injected goal, criteria, artifact snapshots, deterministic check results,
constraints, previous verdict and aggregation report below.
Rules:
- Deterministic program checks take precedence; never mark a criterion pass when a program check failed.
- Never claim a criterion is verified without concrete evidence from the injected snapshot.
- If evidence is missing or an injected program check is unverified, return blocked or fail, never pass.
Reply with JSON only, no prose:
{"verdict":"pass|fail|blocked","reason":"...","evidence":[{"kind":"file|line|command|test|snippet","ref":"...","excerpt":"..."}],"failed_criteria":[{"criterion_id":"...","category":"impl_gap|test_gap|plan_gap|requirement_gap|unknown"}],"unverified_criteria":["..."],"confidence":0.0}
Injected acceptance context:
%s`, string(raw)), nil
}

type MixedStageVerifier struct {
	Program *ProgramStageVerifier
	Model   *ModelStageVerifier
}

func NewMixedStageVerifier(program *ProgramStageVerifier, model *ModelStageVerifier) *MixedStageVerifier {
	return &MixedStageVerifier{Program: program, Model: model}
}

func (m *MixedStageVerifier) Verify(ctx context.Context, req StageVerifyRequest) (Verdict, error) {
	start := time.Now()
	if req.Input == nil {
		return invalidArgumentVerdict(VerifiedByMixed, "acceptance input is required"), nil
	}
	if m.Program == nil {
		m.Program = NewProgramStageVerifier(nil)
	}

	var programCriteria, modelCriteria []Criterion
	for _, raw := range req.Criteria {
		c := StripChannel(raw)
		declared := DeclaredChannel(raw)
		if declared == VerifiedByModel {
			modelCriteria = append(modelCriteria, c)
			continue
		}
		if _, ok := m.Program.Registry.Resolve(c.Type); ok {
			programCriteria = append(programCriteria, c)
			continue
		}
		if declared == VerifiedByProgram {
			programCriteria = append(programCriteria, c)
			continue
		}
		modelCriteria = append(modelCriteria, c)
	}

	in := cloneInput(req.Input)
	var programVerdict Verdict
	if len(programCriteria) > 0 {
		out := m.Program.run(ctx, StageVerifyRequest{
			Input: in, Criteria: programCriteria, Channel: VerifiedByProgram, WorkDir: req.WorkDir, Timeout: req.Timeout,
		})
		in.Checks = append(in.Checks, out.Checks...)
		in.UnverifiedCriteria = append([]string(nil), out.Unverified...)
		programVerdict = m.Program.verdict(in, out)
		if programVerdict.Verdict == VerdictFail {
			return markMixed(programVerdict), nil
		}
	}

	if len(modelCriteria) == 0 {
		if programVerdict.Verdict == "" {
			return invalidArgumentVerdict(VerifiedByMixed, "no judgeable acceptance criteria"), nil
		}
		return markMixed(programVerdict), nil
	}
	if m.Model == nil {
		return m.mixedFailure(in, ReasonModelUnavailable, "no LLM judge is configured", start), nil
	}
	modelVerdict, err := m.Model.Verify(ctx, StageVerifyRequest{
		Input: in, Criteria: modelCriteria, Channel: VerifiedByModel, WorkDir: req.WorkDir, Timeout: req.Timeout,
	})
	if err != nil {
		return m.mixedFailure(in, ReasonModelCallFailed, err.Error(), start), nil
	}
	if modelVerdict.Verdict == VerdictFail {
		modelVerdict.Evidence = mergeSlices(programVerdict.Evidence, modelVerdict.Evidence)
		modelVerdict.FailedCriteria = mergeSlices(programVerdict.FailedCriteria, modelVerdict.FailedCriteria)
		return markMixed(modelVerdict), nil
	}
	if programVerdict.Verdict == VerdictBlocked {
		merged := modelVerdict
		merged.Verdict = VerdictBlocked
		merged.Reason = "mixed: " + programVerdict.Reason + "; " + modelVerdict.Reason
		merged.Unverified = mergeSlices(programVerdict.Unverified, modelVerdict.Unverified)
		merged.Evidence = mergeSlices(programVerdict.Evidence, modelVerdict.Evidence)
		return markMixed(merged), nil
	}
	merged := modelVerdict
	merged.Verdict = VerdictPass
	merged.Reason = "mixed: program checks passed; model judgement passed"
	merged.Evidence = mergeSlices(programVerdict.Evidence, modelVerdict.Evidence)
	merged.FailedCriteria = nil
	merged.Unverified = nil
	return markMixed(merged), nil
}

func (m *MixedStageVerifier) mixedFailure(in *Input, code, detail string, start time.Time) Verdict {
	v := Verdict{
		AcceptanceID:   in.AcceptanceID,
		Verdict:        VerdictFail,
		Reason:         "mixed: " + code + ": " + detail,
		Evidence:       []Evidence{{Kind: "verdict", Ref: "model", Excerpt: code + ": " + detail}},
		FailedCriteria: []FailedCriterion{{CriterionID: "acceptance", Category: "unknown"}},
		VerifiedBy:     VerifiedByMixed,
		InputDigest:    in.Hash(),
		PromptVersion:  in.PromptVersion,
		DurationMs:     time.Since(start).Milliseconds(),
	}
	res, err := Finalize(v, NormalizeOptions{Stage: in.Stage, Input: in, DurationMs: time.Since(start).Milliseconds(), PromptVersion: in.PromptVersion})
	if err != nil {
		return v
	}
	return res.Verdict
}

func markMixed(v Verdict) Verdict {
	v.VerifiedBy = VerifiedByMixed
	if !strings.HasPrefix(v.Reason, "mixed:") {
		v.Reason = "mixed: " + strings.TrimSpace(v.Reason)
	}
	return v
}

type ChannelStageVerifier struct {
	Program *ProgramStageVerifier
	Model   *ModelStageVerifier
	Mixed   *MixedStageVerifier
}

// NewChannelStageVerifier builds the three channel implementations. A nil judge
// is allowed: the model channel then fails closed instead of passing.
func NewChannelStageVerifier(judge Judge, model string) *ChannelStageVerifier {
	p := NewProgramStageVerifier(nil)
	m := NewModelStageVerifier(judge, model)
	return &ChannelStageVerifier{
		Program: p,
		Model:   m,
		Mixed:   NewMixedStageVerifier(p, m),
	}
}

func (c *ChannelStageVerifier) Verify(ctx context.Context, req StageVerifyRequest) (Verdict, error) {
	switch normalizeChannel(req.Channel) {
	case VerifiedByModel:
		if c.Model == nil {
			return invalidArgumentVerdict(VerifiedByModel, ReasonVerifierNotConfigured), nil
		}
		return c.Model.Verify(ctx, req)
	case VerifiedByMixed:
		if c.Mixed == nil {
			return invalidArgumentVerdict(VerifiedByMixed, ReasonVerifierNotConfigured), nil
		}
		return c.Mixed.Verify(ctx, req)
	case VerifiedByProgram:
		if c.Program == nil {
			return invalidArgumentVerdict(VerifiedByProgram, ReasonVerifierNotConfigured), nil
		}
		return c.Program.Verify(ctx, req)
	default:
		return invalidArgumentVerdict(VerifiedByProgram, CodeInvalidChannel+": "+req.Channel), nil
	}
}

func checkEvidence(res checks.Result, criterionID string) Evidence {
	if len(res.Evidence) > 0 {
		e := res.Evidence[0]
		return Evidence{Kind: e.Kind, Ref: e.Ref, Excerpt: e.Excerpt}
	}
	return Evidence{
		Kind:    "command",
		Ref:     firstNonEmpty(res.CheckerID, "program") + ":" + criterionID,
		Excerpt: firstNonEmpty(res.Reason, res.Err, string(res.Status)),
	}
}

func categoryForCheck(criterionType string) string {
	switch criterionType {
	case "tests_pass":
		return "test_gap"
	case "build_pass", "static_check", "lint_pass", "artifact_exists", "artifact_unchanged", "code_symbol":
		return "impl_gap"
	default:
		return "unknown"
	}
}

func failedIDs(failed []FailedCriterion) string {
	ids := make([]string, 0, len(failed))
	for _, f := range failed {
		ids = append(ids, f.CriterionID)
	}
	return strings.Join(ids, ", ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func cloneInput(in *Input) *Input {
	if in == nil {
		return &Input{}
	}
	cp := *in
	cp.Messages = cloneSlice(in.Messages)
	cp.Criteria = cloneSlice(in.Criteria)
	cp.Artifacts = cloneSlice(in.Artifacts)
	cp.Checks = cloneSlice(in.Checks)
	cp.UnverifiedCriteria = cloneSlice(in.UnverifiedCriteria)
	return &cp
}

func mergeSlices[T any](first, second []T) []T {
	out := make([]T, 0, len(first)+len(second))
	out = append(out, first...)
	out = append(out, second...)
	return out
}

func cloneSlice[T any](src []T) []T {
	if src == nil {
		return nil
	}
	out := make([]T, len(src))
	copy(out, src)
	return out
}

func invalidArgumentVerdict(channel, detail string) Verdict {
	if normalizeChannel(channel) == "" {
		channel = VerifiedByProgram
	}
	reason := ReasonContextIncomplete + ": " + detail
	return Verdict{
		AcceptanceID:   "acc-invalid",
		Verdict:        VerdictBlocked,
		Reason:         reason,
		Evidence:       []Evidence{{Kind: "verdict", Ref: "acceptance", Excerpt: reason}},
		FailedCriteria: []FailedCriterion{{CriterionID: "acceptance", Category: "unknown"}},
		VerifiedBy:     channel,
		PromptVersion:  PromptVersion,
		InputDigest:    "invalid",
	}
}
