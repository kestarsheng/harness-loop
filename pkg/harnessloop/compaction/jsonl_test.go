package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stubCompleter struct {
	output     string
	err        error
	lastSystem string
	lastUser   string
	calls      int
}

func (s *stubCompleter) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	s.calls++
	s.lastSystem = systemPrompt
	s.lastUser = userPrompt
	return s.output, s.err
}

func sampleHistory() History {
	return History{
		Goal:     "实现 harness loop 阶段验收",
		GoalHash: GoalHash("实现 harness loop 阶段验收"),
		Phase:    "c",
		Messages: []Message{{Role: "assistant", Content: "正在实现验收器"}},
		Done:     []string{"完成 FSM"}, Todo: []string{"补集成测试"},
		Constraints: []string{"禁止 mysql"}, Background: []string{"端口 50008"},
	}
}

func validSummary() Summary {
	return Summary{
		Goal: "实现 harness loop 阶段验收", Done: "完成 FSM",
		Todo: "补集成测试", Constraints: "禁止 mysql", Background: "端口 50008",
	}
}

func TestBuildLinesAndRoundTrip(t *testing.T) {
	lines, err := BuildLines(validSummary(), "c", time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build lines: %v", err)
	}
	if len(lines) != 5 {
		t.Fatalf("lines = %d", len(lines))
	}
	raw, err := EncodeJSONL(lines)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i, text := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(text), &obj); err != nil {
			t.Fatalf("line %d is not json: %v", i+1, err)
		}
		for _, key := range []string{"type", "phase", "summary", "source_refs", "timestamp"} {
			if _, ok := obj[key]; !ok {
				t.Fatalf("line %d missing %s", i+1, key)
			}
		}
	}
	decoded, err := DecodeJSONL(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := ValidateFiveElements(decoded); err != nil {
		t.Fatalf("five elements: %v", err)
	}
}

func TestValidateRejectsMissingElement(t *testing.T) {
	lines, err := BuildLines(validSummary(), "c", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var withoutTodo []Line
	for _, l := range lines {
		if l.Type != LineTodo {
			withoutTodo = append(withoutTodo, l)
		}
	}
	err = ValidateFiveElements(withoutTodo)
	if !errors.Is(err, ErrIncompleteSummary) {
		t.Fatalf("missing todo must fail: %v", err)
	}
}

func TestValidateRejectsEmptySourceRefs(t *testing.T) {
	line := Line{Type: LineGoal, Phase: "c", Summary: "goal", Timestamp: time.Now()}
	if err := ValidateLine(line); !errors.Is(err, ErrInvalidJSONL) {
		t.Fatalf("empty refs must fail: %v", err)
	}
}

func TestValidateRejectsSourceRef(t *testing.T) {
	line := Line{Type: LineDone, Phase: "c", Summary: "done", SourceRefs: []string{""}, Timestamp: time.Now()}
	if err := ValidateLine(line); err != nil {
		t.Fatalf("non-empty single ref must pass: %v", err)
	}
}

func TestEncodeRejectsSecret(t *testing.T) {
	summary := validSummary()
	summary.Background = "配置里写了 api key sk-abcdefgh12345678"
	lines, err := BuildLines(summary, "c", time.Now())
	if err == nil {
		_, err = EncodeJSONL(lines)
	}
	if !errors.Is(err, ErrSecretInSummary) {
		t.Fatalf("secret must be rejected: %v", err)
	}
}

func TestValidateFiveElementsRejectsEngineFragment(t *testing.T) {
	lines, _ := BuildLines(validSummary(), "c", time.Now())
	lines = append(lines, Line{Type: LineEngineFragment, Phase: "c", Summary: "fragment", SourceRefs: []string{"engine:1"}, Timestamp: time.Now()})
	if err := ValidateFiveElements(lines); err == nil {
		t.Fatal("engine_fragment must be rejected in phase one")
	}
}

func TestTemplateSummarizer(t *testing.T) {
	completer := &stubCompleter{output: `{"goal":"实现 harness loop 阶段验收","done":"完成 FSM","todo":"补集成测试","constraints":"禁止 mysql","background":"端口 50008"}`}
	summarizer := NewTemplateSummarizer(completer)
	summary, err := summarizer.Summarize(context.Background(), sampleHistory(), TemplateV1)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if summary.Goal == "" || summary.Done == "" || summary.Todo == "" || summary.Constraints == "" {
		t.Fatalf("summary incomplete: %+v", summary)
	}
	if !strings.Contains(completer.lastSystem, "五个字符串字段") {
		t.Fatalf("system prompt must state the five-element contract: %s", completer.lastSystem)
	}
	if !strings.Contains(completer.lastUser, "实现 harness loop 阶段验收") {
		t.Fatalf("user prompt must carry the original goal: %s", completer.lastUser)
	}
}

func TestTemplateSummarizerRejectsIncompleteOutput(t *testing.T) {
	cases := []string{
		`{"goal":"g","done":"d","todo":"t","constraints":"c"}`,
		`not json`,
		`{"goal":"g","done":"d","todo":"t","constraints":"c","background":123}`,
		`{"goal":"","done":"d","todo":"t","constraints":"c","background":"b"}`,
	}
	for _, output := range cases {
		summarizer := NewTemplateSummarizer(&stubCompleter{output: output})
		if _, err := summarizer.Summarize(context.Background(), sampleHistory(), TemplateV1); !errors.Is(err, ErrIncompleteSummary) {
			t.Fatalf("output %q must fail: %v", output, err)
		}
	}
}

func TestTemplateVersioning(t *testing.T) {
	summarizer := NewTemplateSummarizer(&stubCompleter{output: "{}"})
	if _, err := summarizer.Summarize(context.Background(), sampleHistory(), TemplateVersion("template.v9")); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown template must fail: %v", err)
	}
	if _, _, err := BuildPrompt(TemplateV1, sampleHistory()); err != nil {
		t.Fatalf("v1 prompt must build: %v", err)
	}
	if _, _, err := BuildPrompt("", sampleHistory()); err != nil {
		t.Fatalf("empty template must default to v1: %v", err)
	}
}

func TestGoalHashStableAcrossCompactions(t *testing.T) {
	history := sampleHistory()
	before := GoalHash(history.Goal)
	summarizer := NewTemplateSummarizer(&stubCompleter{output: `{"goal":"实现 harness loop 阶段验收","done":"d","todo":"t","constraints":"c","background":"b"}`})
	for i := 0; i < 3; i++ {
		if _, err := summarizer.Summarize(context.Background(), history, TemplateV1); err != nil {
			t.Fatalf("summarize %d: %v", i, err)
		}
		if got := GoalHash(history.Goal); got != before {
			t.Fatalf("goal hash changed after compaction %d: %s != %s", i, got, before)
		}
	}
}

func TestWriteJSONL(t *testing.T) {
	lines, err := BuildLines(validSummary(), "c", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, err := WriteJSONL(dir, "task-1", "c", lines)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(path, filepath.Join("task-1", "compaction")) {
		t.Fatalf("path = %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	decoded, err := DecodeJSONL(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 5 {
		t.Fatalf("lines = %d", len(decoded))
	}

	bad := append([]Line{}, lines[:4]...)
	if _, err := WriteJSONL(dir, "task-2", "c", bad); !errors.Is(err, ErrIncompleteSummary) {
		t.Fatalf("incomplete jsonl must not be written: %v", err)
	}
}

func TestEngineSlotIsDisabledByDefault(t *testing.T) {
	slot := NewEngineSlot(false, nil)
	if slot.Enabled() {
		t.Fatal("engine slot must be disabled by default")
	}
	if _, err := slot.Compress(context.Background(), Fragments{}); !errors.Is(err, ErrEngineDisabled) {
		t.Fatalf("disabled engine must return ErrEngineDisabled: %v", err)
	}
	slot = NewEngineSlot(true, nil)
	if _, err := slot.Compress(context.Background(), Fragments{}); !errors.Is(err, ErrEngineNotConfigured) {
		t.Fatalf("enabled empty slot must return ErrEngineNotConfigured: %v", err)
	}
}
