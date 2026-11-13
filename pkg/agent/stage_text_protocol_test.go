package agent

import (
	"context"
	"testing"
)

func TestParseStageFinishProtocol(t *testing.T) {
	valid := []string{
		`[stage_finish] {"attempt_id":"task-1-a-1","stage":"a","summary":"done"}`,
		"分析已完成。\n[stage_finish] {\"attempt_id\":\"task-1-b-2\",\"stage\":\"B\",\"summary\":\"计划完成\"}\n",
	}
	for _, content := range valid {
		args, ok := ParseStageFinishProtocol(content)
		if !ok || len(args) == 0 {
			t.Fatalf("valid protocol rejected: %q", content)
		}
	}

	invalid := []string{
		"我完成了",
		"stage_finish done",
		`[stage_finish] {"stage":"a","summary":"x"}`,
		`[stage_finish] {"attempt_id":"t","stage":"z","summary":"x"}`,
		`[stage_finish] {"attempt_id":"t","stage":"a","summary":""}`,
		`[stage_finish] not-json`,
	}
	for _, content := range invalid {
		if _, ok := ParseStageFinishProtocol(content); ok {
			t.Fatalf("invalid protocol accepted: %q", content)
		}
	}
}

func TestStageModeAcceptsTextProtocol(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&Agent{ID: "exec", Name: "exec", Instructions: "run"}); err != nil {
		t.Fatal(err)
	}
	content := `阶段目标已确认：验证链路。` + "\n" + `[stage_finish] {"attempt_id":"task-1-a-1","stage":"a","summary":"已确认目标"}`
	engine := NewEngine(reg, &stubLLM{resp: &LLMResponse{Content: content}}, &stubTools{}, nil, DefaultEngineConfig())

	res, err := engine.Run(context.Background(), &ExecutionRequest{
		SessionID: "s-1", Message: "do work", EntryAgent: "exec", StageMode: true,
	})
	if err != nil {
		t.Fatalf("strict text protocol must finish the stage: %v", err)
	}
	if res == nil || res.Status == AgentStatusError {
		t.Fatalf("result = %+v", res)
	}
}

func TestStageModeStillRejectsFreeFormText(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&Agent{ID: "exec", Name: "exec", Instructions: "run"}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(reg, &stubLLM{resp: &LLMResponse{Content: "我完成了，请验收"}}, &stubTools{}, nil, DefaultEngineConfig())

	if _, err := engine.Run(context.Background(), &ExecutionRequest{
		SessionID: "s-2", Message: "do work", EntryAgent: "exec", StageMode: true,
	}); err != ErrStageNotFinished {
		t.Fatalf("free-form text must stay no_finish, got %v", err)
	}
}
