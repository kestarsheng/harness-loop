package service

import (
	"path/filepath"
	"strings"
	"testing"

	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/orchestrator"
)

// 示例 DAG 必须是校验器认可的 3 节点计划。示例一旦不合法，模型照抄就永远被
// Planner 驳回，表现为「b accepted 但 subtasks=0」，比不给示例更糟。
func TestPlanStageExampleIsValidPlan(t *testing.T) {
	plan, err := orchestrator.ParsePlan([]byte(planStagePlanExample))
	if err != nil {
		t.Fatalf("parse plan stage example: %v", err)
	}
	if err := plan.Validate(orchestrator.DefaultPlanConfig()); err != nil {
		t.Fatalf("plan stage example must pass validation, got: %v", err)
	}
	if len(plan.Nodes) != 3 {
		t.Fatalf("example must show 3 nodes, got %d", len(plan.Nodes))
	}
	if len(plan.Edges) == 0 {
		t.Fatalf("example must declare dependency edges, otherwise edge_count=0")
	}
	if len(plan.ParallelSets) == 0 {
		t.Fatalf("example must declare a parallel set")
	}
}

// 提示词只给 3 节点示例：单节点示例会被模型当成合格答案照抄，产出与降级无法区分。
func TestPlanStageInstructionShowsThreeNodeExampleOnly(t *testing.T) {
	if !strings.Contains(planStageInstruction, planStagePlanExample) {
		t.Fatalf("instruction must embed the validated example verbatim")
	}
	if got := strings.Count(planStageInstruction, `"node_id"`); got != 3 {
		t.Fatalf("instruction must show exactly 3 nodes, found %d node_id entries", got)
	}
	if strings.Contains(planStageInstruction, `"edges": []`) || strings.Contains(planStageInstruction, `"edges":[]`) {
		t.Fatalf("instruction must not show an edge-free plan")
	}
	for _, want := range []string{"必须拆", "不许拆", "3 个"} {
		if !strings.Contains(planStageInstruction, want) {
			t.Fatalf("instruction must state the splitting rule, missing %q", want)
		}
	}
}

func TestStageMessageInjectsPlanPromptOnlyForPlanStage(t *testing.T) {
	planMsg := stageMessage(fsm.StagePlan, 1)
	if !strings.Contains(planMsg, planStagePlanExample) {
		t.Fatalf("plan stage message must carry the plan instruction")
	}
	planGoal := stageGoal(fsm.StagePlan, "goal-x")
	if !strings.Contains(planGoal, "goal-x") || !strings.Contains(planGoal, planStagePlanExample) {
		t.Fatalf("plan stage goal must keep the goal and append the plan instruction")
	}
	for _, other := range []fsm.Stage{fsm.StageRequirement, fsm.StageExecute, fsm.StageVerify} {
		if strings.Contains(stageMessage(other, 1), planStagePlanExample) {
			t.Fatalf("stage %s must not receive the plan instruction", other)
		}
		if stageGoal(other, "goal-x") != "goal-x" {
			t.Fatalf("stage %s goal must be untouched", other)
		}
	}
}

func TestStageModelPinPinsPlanStageOnly(t *testing.T) {
	model, window, ok := stageModelPin(fsm.StagePlan)
	if !ok || model != planStageModel || window <= 0 {
		t.Fatalf("plan stage must be pinned to %s with a positive window, got %q/%d/%v", planStageModel, model, window, ok)
	}
	if model != "qwen3.7-max" {
		t.Fatalf("plan stage model must be qwen3.7-max, got %q", model)
	}
	for _, other := range []fsm.Stage{fsm.StageRequirement, fsm.StageExecute, fsm.StageVerify} {
		if _, _, pinned := stageModelPin(other); pinned {
			t.Fatalf("stage %s must keep the configured stage_model", other)
		}
	}
}

// models.yaml 的 stage_overrides.b 与 stageModelPin 是同一件事的两处声明：
// 只改一处会让「模型策略门开着」和「关着」两条路径跑在不同模型上。
func TestModelsYAMLPlansWithPinnedModel(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "pkg", "config", "harness_loop", "models.yaml")
	reg, err := modelregistry.Load(path)
	if err != nil {
		t.Fatalf("load models.yaml: %v", err)
	}
	override, ok := reg.StageOverride(string(fsm.StagePlan))
	if !ok {
		t.Fatalf("models.yaml must declare stage_overrides.%s", fsm.StagePlan)
	}
	if override.Model != planStageModel {
		t.Fatalf("models.yaml pins %q but runtime pins %q", override.Model, planStageModel)
	}
	model, found := reg.Get(planStageModel)
	if !found || model == nil {
		t.Fatalf("models.yaml must declare %s", planStageModel)
	}
	if model.ContextWindow != planStageModelWindow {
		t.Fatalf("plan stage window drift: models.yaml=%d runtime=%d", model.ContextWindow, planStageModelWindow)
	}
	if !model.StructuredOutput || !model.ToolCall {
		t.Fatalf("%s must support tool_call and structured_output for the b stage", planStageModel)
	}
}

// b 阶段真实返回形如「DAG JSON + [stage_finish] {...}」，两段 JSON 相邻。
// 按首尾大括号整段截取会拼出非法 JSON，只能逐对象扫描。
func TestPlanArtifactPrefersParseablePlanOverProse(t *testing.T) {
	finish := `{"attempt_id":"1","stage":"b","summary":"拆成 3 个节点"}`

	mixed := &StageRunResult{
		Content:    planStagePlanExample + "\n[stage_finish] " + finish,
		FinishArgs: fsm.StageFinishArgs{Summary: "拆成 3 个节点"},
	}
	raw := planArtifact(mixed)
	plan, err := orchestrator.ParsePlan(raw)
	if err != nil {
		t.Fatalf("planArtifact must recover the DAG from mixed content: %v (raw=%q)", err, string(raw))
	}
	if len(plan.Nodes) != 3 {
		t.Fatalf("recovered plan must keep 3 nodes, got %d", len(plan.Nodes))
	}

	summaryPlan := &StageRunResult{FinishArgs: fsm.StageFinishArgs{Summary: planStagePlanExample}}
	if _, err := orchestrator.ParsePlan(planArtifact(summaryPlan)); err != nil {
		t.Fatalf("summary carrying the plan must be used as-is: %v", err)
	}

	proseOnly := &StageRunResult{
		Content:    "我完成了规划。",
		FinishArgs: fsm.StageFinishArgs{Summary: "我完成了规划。"},
	}
	if got := string(planArtifact(proseOnly)); got == "" {
		t.Fatalf("prose-only output must still be returned so the caller can report the parse error")
	}
}

func TestMatchClosingBraceIgnoresBracesInStrings(t *testing.T) {
	text := []byte(`{"a":"}{","b":{"c":1}}tail`)
	end := matchClosingBrace(text, 0)
	if end != len(`{"a":"}{","b":{"c":1}}`)-1 {
		t.Fatalf("brace matching must skip quoted braces, got end=%d text=%q", end, string(text))
	}
}
