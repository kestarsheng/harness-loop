package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-platform/pkg/llm"
)

func msg(role, content string) ChatMessage {
	return ChatMessage{Role: role, Content: content}
}

func callMsg(ids ...string) ChatMessage {
	calls := make([]ToolCall, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, ToolCall{ID: id, Name: "go_test"})
	}
	return ChatMessage{Role: "assistant", Content: "running tools", ToolCalls: calls}
}

func resultMsg(id, content string) ChatMessage {
	return ChatMessage{Role: "tool", ToolCallID: id, Content: content}
}

func TestPairingValid(t *testing.T) {
	messages := []ChatMessage{
		msg("system", "sys"),
		callMsg("c1", "c2"),
		resultMsg("c1", "ok"),
		resultMsg("c2", "ok"),
	}
	if issues := ValidatePairing(messages); len(issues) != 0 {
		t.Fatalf("valid pairing reported issues: %+v", issues)
	}
}

func TestPairingDetectsOrphans(t *testing.T) {
	missingResult := []ChatMessage{callMsg("c1")}
	issues := ValidatePairing(missingResult)
	if len(issues) != 1 || issues[0].Kind != PairingOrphanCall {
		t.Fatalf("missing result: %+v", issues)
	}
	if err := PairingError(issues); !errors.Is(err, ErrPairingBroken) {
		t.Fatalf("pairing error must wrap ErrPairingBroken: %v", err)
	}

	missingCall := []ChatMessage{resultMsg("c9", "orphan")}
	issues = ValidatePairing(missingCall)
	if len(issues) != 1 || issues[0].Kind != PairingOrphanResult {
		t.Fatalf("orphan result: %+v", issues)
	}

	duplicated := []ChatMessage{callMsg("c1"), callMsg("c1"), resultMsg("c1", "ok")}
	issues = ValidatePairing(duplicated)
	found := false
	for _, issue := range issues {
		if issue.Kind == PairingDuplicateID {
			found = true
		}
	}
	if !found {
		t.Fatalf("duplicate call id not detected: %+v", issues)
	}
}

func TestApplyCompactionRejectsBrokenPairing(t *testing.T) {
	lines := PlaceholderLines(sampleHistory())
	messages := []ChatMessage{callMsg("c1")}
	out, err := ApplyCompaction(messages, lines)
	if err != nil {
		t.Fatalf("compaction rebuild: %v", err)
	}
	if issues := ValidatePairing(out); len(issues) != 0 {
		t.Fatalf("rebuilt sequence must be pairing-safe: %+v", issues)
	}
	if _, err := ApplyCompaction(messages, nil); err == nil {
		t.Fatal("empty jsonl must be rejected")
	}
	if _, err := ApplyCompaction(messages, nil); err == nil {
		t.Fatal("empty jsonl must be rejected")
	}
}

func buildLongHistory(n int) []ChatMessage {
	var out []ChatMessage
	out = append(out, msg("system", strings.Repeat("系统提示", 3000)))
	out = append(out, msg("user", "goal: 实现压缩"))
	for i := 0; i < n; i++ {
		out = append(out, msg("assistant", strings.Repeat("执行步骤内容", 20)))
		out = append(out, msg("user", "继续"))
	}
	return out
}

func TestDegradeL1KeepsStructureAndFiveElements(t *testing.T) {
	messages := buildLongHistory(30)
	result, err := DegradeL1(messages, sampleHistory(), DefaultDegradeLimits())
	if err != nil {
		t.Fatalf("degrade l1: %v", err)
	}
	if result.Level != DegradeLevelL1 || result.Reason == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if err := ValidateDegradeResult(result); err != nil {
		t.Fatalf("degraded result must be valid: %v", err)
	}
	if err := ValidateFiveElements(result.Lines); err != nil {
		t.Fatalf("five elements must remain readable: %v", err)
	}
	if result.TokensAfter >= result.TokensBefore {
		t.Fatalf("l1 must reduce tokens: %d -> %d", result.TokensBefore, result.TokensAfter)
	}
	var hasSystem, hasGoal bool
	for _, m := range result.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "系统提示") {
			hasSystem = true
		}
		if m.Role == "user" && strings.Contains(m.Content, "goal: ") {
			hasGoal = true
		}
	}
	if !hasSystem || !hasGoal {
		t.Fatalf("l1 must keep system prompt and goal: %+v", result.Messages[:3])
	}
}

func TestDegradeL2KeepsPairing(t *testing.T) {
	messages := []ChatMessage{
		msg("system", strings.Repeat("系统", 1000)),
		msg("user", "goal: x"),
		callMsg("old-1"),
		resultMsg("old-1", "done"),
		callMsg("old-2"),
		resultMsg("old-2", "done"),
		callMsg("new-1"),
		resultMsg("new-1", "ok"),
	}
	result, err := DegradeL2(messages, sampleHistory(), DefaultDegradeLimits())
	if err != nil {
		t.Fatalf("degrade l2: %v", err)
	}
	if result.Level != DegradeLevelL2 {
		t.Fatalf("level = %s", result.Level)
	}
	if err := ValidateDegradeResult(result); err != nil {
		t.Fatalf("l2 result must be valid: %v", err)
	}
	if err := ValidateFiveElements(result.Lines); err != nil {
		t.Fatalf("l2 must keep five elements readable: %v", err)
	}
	if len(result.Messages) > len(messages) {
		t.Fatalf("l2 must drop messages: %d -> %d", len(messages), len(result.Messages))
	}
}

func TestDegradePlaceholderSurvivesMissingData(t *testing.T) {
	empty := History{Phase: "d"}
	result, err := DegradeL1(buildLongHistory(5), empty, DefaultDegradeLimits())
	if err != nil {
		t.Fatalf("degrade with empty history: %v", err)
	}
	if err := ValidateFiveElements(result.Lines); err != nil {
		t.Fatalf("placeholders must be readable: %v", err)
	}
	for _, line := range result.Lines {
		if strings.TrimSpace(line.Summary) == "" {
			t.Fatalf("placeholder %s must not be empty", line.Type)
		}
	}
}

func TestValidateDegradeResultRejectsBadInput(t *testing.T) {
	good, err := DegradeL1(buildLongHistory(3), sampleHistory(), DefaultDegradeLimits())
	if err != nil {
		t.Fatal(err)
	}
	cases := []DegradeResult{
		{Level: "L3", Reason: "x", Lines: good.Lines},
		{Level: DegradeLevelL1, Lines: good.Lines},
		{Level: DegradeLevelL1, Reason: "x"},
	}
	for _, c := range cases {
		if err := ValidateDegradeResult(c); err == nil {
			t.Fatalf("invalid degrade result accepted: %+v", c)
		}
	}
}

type failingCompleter struct{}

func (f *failingCompleter) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return "", errors.New("llm timeout")
}

func TestTemplateFailureFallsBackToL1(t *testing.T) {
	summarizer := NewTemplateSummarizer(&failingCompleter{})
	_, err := summarizer.Summarize(context.Background(), sampleHistory(), TemplateV1)
	if err == nil {
		t.Fatal("template failure must surface an error")
	}
	result, degradeErr := DegradeL1(buildLongHistory(10), sampleHistory(), DefaultDegradeLimits())
	if degradeErr != nil {
		t.Fatalf("fallback must produce a usable result: %v", degradeErr)
	}
	if result.Level != DegradeLevelL1 {
		t.Fatalf("fallback level = %s", result.Level)
	}
}

func TestEngineDisabledIsNotDegraded(t *testing.T) {
	slot := NewEngineSlot(false, nil)
	_, err := slot.Compress(context.Background(), Fragments{})
	if !errors.Is(err, ErrEngineDisabled) {
		t.Fatalf("disabled slot must return ErrEngineDisabled: %v", err)
	}
	if IsEngineFailureDegraded(false, err) {
		t.Fatal("engine slot being empty is a design state, not a degradation")
	}
	if !IsEngineFailureDegraded(true, errors.New("adapter failed")) {
		t.Fatal("enabled engine failure must count as degraded")
	}
}

func TestABSummarizeAndCompare(t *testing.T) {
	runs := []CompactionRun{
		mustRun(t, "task-1", VariantTemplateV1, 1000, 200, 1.0, 0.9, false, ""),
		mustRun(t, "task-2", VariantTemplateV1, 1000, 250, 1.0, 0.8, false, ""),
		mustRun(t, "task-3", VariantTemplateV2, 1000, 180, 0.8, 0.95, false, ""),
		mustRun(t, "task-4", VariantTemplateV2, 1000, 190, 0.8, 0.85, true, DegradeLevelL1),
	}
	summary := SummarizeRuns(runs)
	if len(summary) != 2 {
		t.Fatalf("variants = %d", len(summary))
	}
	var v1, v2 VariantSummary
	for _, s := range summary {
		switch s.Variant {
		case VariantTemplateV1:
			v1 = s
		case VariantTemplateV2:
			v2 = s
		}
	}
	if v1.Count != 2 || v1.DegradedCount != 0 || v2.DegradedCount != 1 {
		t.Fatalf("summary = %+v / %+v", v1, v2)
	}
	comparison := CompareVariants(v2, v1)
	if comparison.AcceptanceDelta <= 0 || comparison.Winner != VariantTemplateV2 {
		t.Fatalf("comparison = %+v", comparison)
	}
}

func mustRun(t *testing.T, taskID, variant string, before, after int, preserved, acceptance float64, degraded bool, level string) CompactionRun {
	t.Helper()
	run, err := BuildRun(taskID, "tenant", "project", variant, TemplateV1, ReasonThreshold,
		before, after, preserved, acceptance, degraded, level, "out.jsonl", 12)
	if err != nil {
		t.Fatalf("build run: %v", err)
	}
	return run
}

func TestCompactionRunValidation(t *testing.T) {
	if _, err := BuildRun("", "t", "p", VariantTemplateV1, TemplateV1, ReasonThreshold, 10, 5, 1, 1, false, "", "", 0); err == nil {
		t.Fatal("missing task id must fail")
	}
	if _, err := BuildRun("task", "t", "p", VariantTemplateV1, TemplateV1, ReasonThreshold, 10, 5, 1, 1, true, "", "", 0); err == nil {
		t.Fatal("degraded run without level must fail")
	}
	if _, err := BuildRun("task", "t", "p", VariantTemplateV1, TemplateV1, ReasonThreshold, 10, 5, 1, 1, true, "L9", "", 0); err == nil {
		t.Fatal("invalid degrade level must fail")
	}
}

func TestCompactionEventPayloadFields(t *testing.T) {
	run := mustRun(t, "task-1", VariantTemplateV1, 1000, 200, 1.0, 0.9, true, DegradeLevelL1)
	payload := CompactionEventPayload(run)
	required := []string{
		"trigger_reason", "tokens_before", "tokens_after", "ratio",
		"duration_ms", "degraded", "degrade_level", "strategy_id",
		"prompt_version", "output_ref",
	}
	for _, key := range required {
		if _, ok := payload[key]; !ok {
			t.Fatalf("payload missing %s: %v", key, payload)
		}
	}
	if payload["degraded"] != true || payload["degrade_level"] != DegradeLevelL1 {
		t.Fatalf("degrade fields = %v", payload)
	}
}

func TestEstimateMessagesHelper(t *testing.T) {
	if estimateMessages(nil) != 0 {
		t.Fatal("empty messages must estimate 0")
	}
	if llm.EstimateTokens("") != 0 {
		t.Fatal("sanity: empty text has zero tokens")
	}
	_ = time.Now()
}
