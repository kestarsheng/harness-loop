package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func fidelityHistory(i int) History {
	goal := fmt.Sprintf("目标-%02d：完成模块 %d 的验收", i, i)
	return History{
		Goal:        goal,
		GoalHash:    GoalHash(goal),
		Phase:       "c",
		Messages:    []Message{{Role: "assistant", Content: fmt.Sprintf("正在处理模块 %d", i)}},
		Done:        []string{fmt.Sprintf("完成子任务-%02d", i)},
		Todo:        []string{fmt.Sprintf("补齐测试-%02d", i)},
		Constraints: []string{fmt.Sprintf("约束-%02d 禁止外部依赖", i)},
		Background:  []string{fmt.Sprintf("背景-%02d 端口 50%03d", i, i)},
	}
}

func fidelitySummary(h History) Summary {
	return Summary{
		Goal:        h.Goal,
		Done:        strings.Join(h.Done, "; "),
		Todo:        strings.Join(h.Todo, "; "),
		Constraints: strings.Join(h.Constraints, "; "),
		Background:  strings.Join(h.Background, "; "),
	}
}

func fidelityJSON(h History) string {
	raw, err := json.Marshal(map[string]string{
		"goal":        h.Goal,
		"done":        strings.Join(h.Done, "; "),
		"todo":        strings.Join(h.Todo, "; "),
		"constraints": strings.Join(h.Constraints, "; "),
		"background":  strings.Join(h.Background, "; "),
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func fidelityMessages(i int) []ChatMessage {
	out := []ChatMessage{
		{Role: "system", Content: "系统提示 " + strings.Repeat("规则", 120)},
		{Role: "user", Content: "goal: " + fidelityHistory(i).Goal},
	}
	for j := 0; j < 3+i%3; j++ {
		id := fmt.Sprintf("c-%02d-%d", i, j)
		out = append(out, ChatMessage{Role: "assistant", Content: "调用工具", ToolCalls: []ToolCall{{ID: id, Name: "go_test"}}})
		out = append(out, ChatMessage{Role: "tool", ToolCallID: id, Content: "ok"})
		out = append(out, ChatMessage{Role: "assistant", Content: "继续下一步"})
	}
	return out
}

func fidelitySummarize(t *testing.T, h History) Summary {
	t.Helper()
	summarizer := NewTemplateSummarizer(&stubCompleter{output: fidelityJSON(h)})
	summary, err := summarizer.Summarize(context.Background(), h, TemplateV1)
	if err != nil {
		t.Fatalf("summarize sample: %v", err)
	}
	return summary
}

func TestFidelityFiveElementCompletenessRate100(t *testing.T) {
	hits := 0
	const samples = 20
	for i := 1; i <= samples; i++ {
		h := fidelityHistory(i)
		lines, err := BuildLines(fidelitySummarize(t, h), h.Phase, time.Now())
		if err != nil {
			t.Fatalf("sample %d build lines: %v", i, err)
		}
		if err := ValidateFiveElements(lines); err != nil {
			t.Fatalf("sample %d five elements: %v", i, err)
		}
		seen := map[string]int{}
		for _, line := range lines {
			seen[line.Type]++
		}
		for _, typ := range []string{LineGoal, LineDone, LineTodo, LineConstraints, LineBackground} {
			if seen[typ] != 1 {
				t.Fatalf("sample %d has %d lines of type %s", i, seen[typ], typ)
			}
		}
		hits++
	}
	rate := float64(hits) / samples
	if rate != 1 {
		t.Fatalf("five-element completeness = %.3f", rate)
	}
}

func TestFidelityPairingLegalityRate100(t *testing.T) {
	hits := 0
	const samples = 20
	for i := 1; i <= samples; i++ {
		h := fidelityHistory(i)
		messages := fidelityMessages(i)
		lines, err := BuildLines(fidelitySummarize(t, h), h.Phase, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		compacted, err := ApplyCompaction(messages, lines)
		if err != nil {
			t.Fatalf("sample %d apply: %v", i, err)
		}
		l1, err := DegradeL1(messages, h, DefaultDegradeLimits())
		if err != nil {
			t.Fatalf("sample %d L1: %v", i, err)
		}
		l2, err := DegradeL2(messages, h, DefaultDegradeLimits())
		if err != nil {
			t.Fatalf("sample %d L2: %v", i, err)
		}
		for name, seq := range map[string][]ChatMessage{
			"compacted": compacted,
			"l1":        l1.Messages,
			"l2":        l2.Messages,
		} {
			if issues := ValidatePairing(seq); len(issues) != 0 {
				t.Fatalf("sample %d %s pairing: %+v", i, name, issues)
			}
		}
		hits++
	}
	if rate := float64(hits) / samples; rate != 1 {
		t.Fatalf("pairing legality rate = %.3f", rate)
	}
}

func TestFidelityJSONLParseabilityRate100(t *testing.T) {
	hits := 0
	const samples = 20
	for i := 1; i <= samples; i++ {
		h := fidelityHistory(i)
		lines, err := BuildLines(fidelitySummarize(t, h), h.Phase, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := EncodeJSONL(lines)
		if err != nil {
			t.Fatalf("sample %d encode: %v", i, err)
		}
		decoded, err := DecodeJSONL(raw)
		if err != nil {
			t.Fatalf("sample %d decode: %v", i, err)
		}
		if len(decoded) != 5 {
			t.Fatalf("sample %d decoded lines = %d", i, len(decoded))
		}
		for n, text := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(text), &obj); err != nil {
				t.Fatalf("sample %d line %d is not json: %v", i, n+1, err)
			}
			for _, key := range []string{"type", "phase", "summary", "source_refs", "timestamp"} {
				if _, ok := obj[key]; !ok {
					t.Fatalf("sample %d line %d missing %s", i, n+1, key)
				}
			}
		}
		hits++
	}
	if rate := float64(hits) / samples; rate != 1 {
		t.Fatalf("jsonl parseability rate = %.3f", rate)
	}
}

func sampleDistorted(lines []Line, h History) bool {
	byType := map[string]Line{}
	for _, line := range lines {
		byType[line.Type] = line
	}
	if byType[LineGoal].Summary != h.Goal {
		return true
	}
	for _, item := range h.Done {
		if !strings.Contains(byType[LineDone].Summary, item) {
			return true
		}
	}
	for _, item := range h.Todo {
		if !strings.Contains(byType[LineTodo].Summary, item) {
			return true
		}
	}
	for _, item := range h.Constraints {
		if !strings.Contains(byType[LineConstraints].Summary, item) {
			return true
		}
	}
	for _, item := range h.Background {
		if !strings.Contains(byType[LineBackground].Summary, item) {
			return true
		}
	}
	return false
}

func TestFidelityTwentySampleDistortionRateAtMost10Percent(t *testing.T) {
	distorted := 0
	const samples = 20
	for i := 1; i <= samples; i++ {
		h := fidelityHistory(i)
		lines, err := BuildLines(fidelitySummarize(t, h), h.Phase, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := EncodeJSONL(lines)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeJSONL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if sampleDistorted(decoded, h) {
			distorted++
			t.Logf("sample %d lost a five-element item", i)
		}
	}
	rate := float64(distorted) / samples
	t.Logf("20-sample distortion rate = %d/%d = %.3f", distorted, samples, rate)
	if rate > 0.10 {
		t.Fatalf("distortion rate = %.3f, must be <= 0.10", rate)
	}
}

func TestFidelityDistortionDetectorCatchesRewrite(t *testing.T) {
	h := fidelityHistory(1)
	lines, err := BuildLines(fidelitySummarize(t, h), h.Phase, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := range lines {
		if lines[i].Type == LineGoal {
			lines[i].Summary = "被改写后的目标"
		}
	}
	if !sampleDistorted(lines, h) {
		t.Fatal("detector must flag a rewritten goal")
	}
	h.Done = append(h.Done, "被遗漏的子任务")
	if !sampleDistorted(lines, h) {
		t.Fatal("detector must flag a dropped done item")
	}
}

func TestFidelityDefaultConfigProducesNoDegradedAndNoEngineFragment(t *testing.T) {
	slot := NewEngineSlot(false, nil)
	if slot.Enabled() {
		t.Fatal("default engine slot must be disabled")
	}
	_, engineErr := slot.Compress(context.Background(), Fragments{})
	if !errors.Is(engineErr, ErrEngineDisabled) {
		t.Fatalf("default slot must return ErrEngineDisabled: %v", engineErr)
	}
	if IsEngineFailureDegraded(slot.Enabled(), engineErr) {
		t.Fatal("empty engine slot is a design state, not a degradation")
	}

	fragments := 0
	for i := 1; i <= 20; i++ {
		h := fidelityHistory(i)
		lines, err := BuildLines(fidelitySummarize(t, h), h.Phase, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			if line.Type == LineEngineFragment {
				fragments++
			}
		}
		if err := ValidateFiveElements(lines); err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
	}
	if fragments != 0 {
		t.Fatalf("engine_fragment count = %d, want 0", fragments)
	}

	run, err := BuildRun("task-default", "tenant", "project", VariantTemplateV1, TemplateV1, ReasonThreshold,
		1000, 300, 1.0, 0.9, false, "", "out.jsonl", 5)
	if err != nil {
		t.Fatalf("default run: %v", err)
	}
	if run.Degraded || run.DegradeLevel != "" {
		t.Fatalf("default run must not degrade: %+v", run)
	}
}

func TestFidelityTemplateFailureFallsBackToL1ThenL2(t *testing.T) {
	summarizer := NewTemplateSummarizer(&failingCompleter{})
	if _, err := summarizer.Summarize(context.Background(), fidelityHistory(1), TemplateV1); err == nil {
		t.Fatal("template failure must surface an error")
	}

	messages := buildLongHistory(30)
	h := fidelityHistory(1)
	l1, err := DegradeL1(messages, h, DefaultDegradeLimits())
	if err != nil {
		t.Fatalf("L1: %v", err)
	}
	if l1.Level != DegradeLevelL1 || l1.Reason != DegradeReasonTemplateFailed {
		t.Fatalf("L1 result = %+v", l1)
	}
	if err := ValidateDegradeResult(l1); err != nil {
		t.Fatalf("L1 validation: %v", err)
	}
	if err := ValidateFiveElements(l1.Lines); err != nil {
		t.Fatalf("L1 five elements: %v", err)
	}
	if l1.TokensAfter >= l1.TokensBefore {
		t.Fatalf("L1 must reduce tokens: %d -> %d", l1.TokensBefore, l1.TokensAfter)
	}

	const budget = 1
	if l1.TokensAfter <= budget {
		t.Fatal("fixture must exceed the tiny budget so L2 is reachable")
	}
	l2, err := DegradeL2(l1.Messages, h, DefaultDegradeLimits())
	if err != nil {
		t.Fatalf("L2: %v", err)
	}
	if l2.Level != DegradeLevelL2 || l2.Reason != DegradeReasonEmergency {
		t.Fatalf("L2 result = %+v", l2)
	}
	if err := ValidateDegradeResult(l2); err != nil {
		t.Fatalf("L2 validation: %v", err)
	}
	if err := ValidateFiveElements(l2.Lines); err != nil {
		t.Fatalf("L2 five elements: %v", err)
	}
	if l2.TokensAfter > l1.TokensAfter {
		t.Fatalf("L2 must not grow tokens: %d -> %d", l1.TokensAfter, l2.TokensAfter)
	}
}

type mockContextEngine struct {
	calls int
	out   []byte
	err   error
}

func (m *mockContextEngine) Compress(ctx context.Context, fragments Fragments) (Compressed, error) {
	m.calls++
	if m.err != nil {
		return Compressed{}, m.err
	}
	return Compressed{JSONL: m.out}, nil
}

func TestFidelityMockContextEngineToggle(t *testing.T) {
	h := fidelityHistory(1)
	lines, err := BuildLines(fidelitySummarize(t, h), h.Phase, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	jsonl, err := EncodeJSONL(lines)
	if err != nil {
		t.Fatal(err)
	}

	engine := &mockContextEngine{out: jsonl}
	off := NewEngineSlot(false, engine)
	if off.Enabled() {
		t.Fatal("disabled slot must report disabled even with an engine attached")
	}
	if _, err := off.Compress(context.Background(), Fragments{}); !errors.Is(err, ErrEngineDisabled) {
		t.Fatalf("disabled slot: %v", err)
	}
	if engine.calls != 0 {
		t.Fatalf("disabled slot must not call the engine, calls = %d", engine.calls)
	}
	if IsEngineFailureDegraded(off.Enabled(), ErrEngineDisabled) {
		t.Fatal("disabled slot must not degrade")
	}

	on := NewEngineSlot(true, engine)
	if !on.Enabled() {
		t.Fatal("enabled slot with engine must be usable")
	}
	compressed, err := on.Compress(context.Background(), Fragments{})
	if err != nil {
		t.Fatalf("enabled slot: %v", err)
	}
	if engine.calls != 1 {
		t.Fatalf("enabled slot calls = %d", engine.calls)
	}
	decoded, err := DecodeJSONL(compressed.JSONL)
	if err != nil {
		t.Fatalf("engine output must stay parseable: %v", err)
	}
	if err := ValidateFiveElements(decoded); err != nil {
		t.Fatalf("engine output five elements: %v", err)
	}

	failing := &mockContextEngine{err: errors.New("engine adapter failed")}
	failOn := NewEngineSlot(true, failing)
	if _, err := failOn.Compress(context.Background(), Fragments{}); err == nil {
		t.Fatal("failing adapter must surface an error")
	}
	if !IsEngineFailureDegraded(failOn.Enabled(), errors.New("engine adapter failed")) {
		t.Fatal("enabled engine failure must count as degraded")
	}
	if IsEngineFailureDegraded(false, errors.New("engine adapter failed")) {
		t.Fatal("closed switch must not defer degradation to the engine")
	}
}

func TestFidelityNoExternalEngineImports(t *testing.T) {
	allowed := map[string]bool{
		"bytes": true, "context": true, "crypto/sha256": true, "encoding/hex": true,
		"encoding/json": true, "errors": true, "fmt": true, "os": true,
		"path/filepath": true, "regexp": true, "sort": true, "strings": true,
		"time": true, "gopkg.in/yaml.v3": true,
		"agent-platform/pkg/harnessloop/event": true,
		"agent-platform/pkg/llm":               true,
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package dir: %v", err)
	}
	checked := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			checked++
			for _, spec := range file.Imports {
				path := strings.Trim(spec.Path.Value, `"`)
				if !allowed[path] {
					t.Fatalf("%s imports %q, which is not in the one-phase whitelist", filepath.Base(name), path)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no source files scanned")
	}

	cmd := exec.Command("go", "list", "-deps", ".")

	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	var thirdParty []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "agent-platform") || strings.HasPrefix(line, "vendor/") || !strings.Contains(line, ".") {
			continue
		}
		thirdParty = append(thirdParty, line)
	}
	sort.Strings(thirdParty)
	for _, dep := range thirdParty {
		lower := strings.ToLower(dep)
		for _, token := range []string{"compress", "engine", "llmlingua", "recomp", "headroom"} {
			if strings.Contains(lower, token) {
				t.Fatalf("external compression engine dependency found: %s", dep)
			}
		}
	}
	if len(thirdParty) != 1 || thirdParty[0] != "gopkg.in/yaml.v3" {
		t.Fatalf("unexpected third-party deps: %v", thirdParty)
	}
}

func TestFidelityABReportMetrics(t *testing.T) {
	const samples = 10
	var runs []CompactionRun
	for i := 1; i <= samples; i++ {
		v1, err := BuildRun(fmt.Sprintf("ab-v1-%02d", i), "tenant", "project", VariantTemplateV1, TemplateV1,
			ReasonThreshold, 10000, 2500+i*50, 1.0, 0.88, false, "", "v1.jsonl", 10)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := BuildRun(fmt.Sprintf("ab-v2-%02d", i), "tenant", "project", VariantTemplateV2, TemplateV1,
			ReasonThreshold, 10000, 2200+i*20, 1.0, 0.94, false, "", "v2.jsonl", 10)
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, v1, v2)
	}
	summaries := SummarizeRuns(runs)
	if len(summaries) != 2 {
		t.Fatalf("variant summaries = %d", len(summaries))
	}
	byVariant := map[string]VariantSummary{}
	for _, summary := range summaries {
		byVariant[summary.Variant] = summary
	}
	v1, v2 := byVariant[VariantTemplateV1], byVariant[VariantTemplateV2]
	if v1.Count != samples || v2.Count != samples {
		t.Fatalf("counts = %d/%d", v1.Count, v2.Count)
	}
	if v1.AvgRatio <= 0 || v2.AvgRatio <= 0 || v1.AvgAcceptance <= 0 || v2.AvgAcceptance <= 0 {
		t.Fatalf("A/B metrics must be populated: %+v / %+v", v1, v2)
	}
	if v1.DegradedCount != 0 || v2.DegradedCount != 0 {
		t.Fatalf("A/B samples must not degrade: %d/%d", v1.DegradedCount, v2.DegradedCount)
	}
	comparison := CompareVariants(v2, v1)
	if comparison.Winner != VariantTemplateV2 || comparison.AcceptanceDelta <= 0 {
		t.Fatalf("comparison = %+v", comparison)
	}
	for _, run := range runs {
		payload := CompactionEventPayload(run)
		for _, key := range []string{"tokens_after", "ratio"} {
			if _, ok := payload[key]; !ok {
				t.Fatalf("payload missing %s", key)
			}
		}
	}

	var v1After, v2After int
	for _, run := range runs {
		switch run.Variant {
		case VariantTemplateV1:
			v1After += run.TokensAfter
		case VariantTemplateV2:
			v2After += run.TokensAfter
		}
	}
	t.Logf("A/B v1: tokens_after=%d ratio=%.3f acceptance=%.3f", v1After/samples, v1.AvgRatio, v1.AvgAcceptance)
	t.Logf("A/B v2: tokens_after=%d ratio=%.3f acceptance=%.3f", v2After/samples, v2.AvgRatio, v2.AvgAcceptance)
}
