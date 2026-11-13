package modelregistry_test

// T-P1-T5 · design 9.10 模型选择回归套件（只新增测试，不改实现）。
//
// 真实依赖：
//   - HARNESS_TEST_PG_DSN + HARNESS_REQUIRE_PG=1 缺一即 Fatal（禁止 Skip）；
//   - model_selections 写入与 008 DDL 约束用真实 PostgreSQL 验证；
//   - 关键事件经 T-P1-31 的 orchestrator.PublisherEmitter 转发到真实 event.Publisher，
//     并断言 seq 连续无缺号。
//
// 已知边界（报告中同步记录，不在此处用弱断言掩盖）：
//   1. pkg 测试不能 import services/.../internal/repository，因此 PG recorder 在测试侧
//      按 008 DDL 的列复刻写入；未直接调用生产 ModelSelectionRepo。
//   2. design 9.10 的 E_EMBEDDING_DIM_MISMATCH 在实现中不存在，现有拒绝路径是
//      注册表加载期 enforce_dim 校验（E_MODEL_META_INCOMPLETE）。
//   3. 模型“未经审批升级 + 审计”只有 ValidateSwitch 拒绝；modelregistry 没有 audit 端口，
//      无 hlp_event.audit 写入路径。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/pgindex"
)

const (
	msTenant  = "ms-suite-tenant"
	msProject = "ms-suite-project"
)

// ---------------------------------------------------------------------------
// 基础设施
// ---------------------------------------------------------------------------

var (
	msDBOnce sync.Once
	msDB     *pgindex.DB
	msDBErr  error
)

func msSuitePG(t *testing.T) *pgindex.DB {
	t.Helper()
	if os.Getenv("HARNESS_REQUIRE_PG") != "1" {
		t.Fatalf("HARNESS_REQUIRE_PG=1 is required for the T-P1-T5 model suite (no silent skip)")
	}
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Fatalf("HARNESS_TEST_PG_DSN is required for the T-P1-T5 model suite (no silent skip)")
	}
	msDBOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
		if err != nil {
			msDBErr = fmt.Errorf("open pg: %w", err)
			return
		}
		if err := db.Migrate(ctx); err != nil {
			db.Close()
			msDBErr = fmt.Errorf("migrate: %w", err)
			return
		}
		msDB = db
	})
	if msDBErr != nil {
		t.Fatalf("real PG is not usable: %v", msDBErr)
	}
	return msDB
}

type msSink struct {
	mu     sync.Mutex
	events []*event.Event
}

func (s *msSink) Append(_ context.Context, events []*event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (s *msSink) snapshot() []*event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*event.Event, len(s.events))
	copy(out, s.events)
	return out
}

func msEventEnv(t *testing.T, taskID string) (*msSink, *orchestrator.PublisherEmitter) {
	t.Helper()
	sink := &msSink{}
	pub := event.NewPublisher(sink, event.NewMemoryIdempotency(10000), nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{
		TenantID: msTenant, ProjectID: msProject, TaskID: taskID, ParentTaskID: taskID,
	})
	return sink, emitter
}

func msAssertSeq(t *testing.T, sink *msSink, taskID string) {
	t.Helper()
	var seqs []int64
	for _, e := range sink.snapshot() {
		if e.TaskID == taskID {
			seqs = append(seqs, e.Seq)
		}
	}
	if len(seqs) == 0 {
		t.Fatalf("no events for task %s", taskID)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("seq gap: position %d has %d (%v)", i, seq, seqs)
		}
	}
}

func msSeedTask(t *testing.T, db *pgindex.DB, taskID string) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		INSERT INTO hlp_task.task_runs
			(task_id, tenant_id, project_id, workspace_root, goal, goal_hash,
			 current_stage, status, policy_snapshot)
		VALUES ($1,$2,$3,'/workspace','model suite goal','sha256:model-suite',
		        'c','running','{"version":1}')
		ON CONFLICT (task_id) DO NOTHING`, taskID, msTenant, msProject)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := db.Exec(ctx, `DELETE FROM hlp_task.task_runs WHERE tenant_id LIKE 'ms-suite-%'`); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
}

const msBaseRegistry = `
version: 1
models:
  - id: strong
    provider: p
    kind: chat
    context_window: 131072
    max_output_tokens: 8192
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.02
    price_out_per_1k: 0.06
    status: available
    tags: [strong]
    api_key_ref: env:SUITE_KEY
  - id: cheap
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 4096
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.0005
    price_out_per_1k: 0.0015
    status: available
    tags: [cheap]
    api_key_ref: env:SUITE_KEY
  - id: notool
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 2048
    tool_call: false
    structured_output: false
    price_in_per_1k: 0.0001
    price_out_per_1k: 0.0002
    status: available
    tags: [cheap]
  - id: embed
    provider: p
    kind: embedding
    context_window: 8192
    max_output_tokens: 1
    embedding_dim: 1024
    price_in_per_1k: 0.0001
    price_out_per_1k: 0
    status: available
defaults:
  task_model: cheap
  strategy: balanced
  fallback: [strong, cheap]
stage_overrides:
  d: strong
embedding:
  memory_model: embed
  code_model: embed
  enforce_dim: 1024
`

func msWriteRegistry(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func msLoadRegistry(t *testing.T, body string) *modelregistry.Registry {
	t.Helper()
	reg, err := modelregistry.Load(msWriteRegistry(t, body))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

func msReject(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %s", err, want)
	}
}

func msSelectReq(stage, strategy string) modelregistry.SelectRequest {
	return modelregistry.SelectRequest{
		TenantID:          msTenant,
		ProjectID:         msProject,
		TaskID:            "ms-select-task",
		Stage:             stage,
		Strategy:          strategy,
		RequireToolCall:   true,
		StageBudgetTokens: 8000,
		TaskBudgetTokens:  100000,
	}
}

// ---------------------------------------------------------------------------
// 9.10 注册表
// ---------------------------------------------------------------------------

func TestModelSuiteRegistryRejectsIncompleteMeta(t *testing.T) {
	msSuitePG(t)
	body := `
version: 1
models:
  - id: broken
    provider: p
    kind: chat
    max_output_tokens: 10
    price_in_per_1k: 0
    price_out_per_1k: 0
`
	_, err := modelregistry.Load(msWriteRegistry(t, body))
	msReject(t, err, modelregistry.CodeMetaIncomplete)
	msReject(t, err, "context_window")
}

func TestModelSuiteRegistryRejectsInvalidSnapshot(t *testing.T) {
	msSuitePG(t)
	// max_output > context_window：整份快照必须拒绝。
	body := strings.Replace(msBaseRegistry, "max_output_tokens: 4096", "max_output_tokens: 99999", 1)
	if _, err := modelregistry.Load(msWriteRegistry(t, body)); err == nil {
		t.Fatal("max_output_tokens > context_window must reject the whole snapshot")
	}
}

func TestModelSuiteRegistryPriceChangeKeepsHistory(t *testing.T) {
	db := msSuitePG(t)
	msSeedTask(t, db, "ms-select-task")
	old := msLoadRegistry(t, msBaseRegistry)
	oldSel := modelregistry.NewSelector(old).WithRecorder(&msPGRecorder{db: db, reg: old})
	firstReq := msSelectReq("c", modelregistry.StrategyCostFirst)
	firstReq.EstimatedTokensIn, firstReq.EstimatedTokensOut = 1000, 200
	first, err := oldSel.Select(firstReq)
	if err != nil {
		t.Fatalf("first select: %v", err)
	}

	// 新快照：cheap 单价变为 10 倍。
	newBody := strings.Replace(msBaseRegistry, "price_in_per_1k: 0.0005", "price_in_per_1k: 0.005", 1)
	newBody = strings.Replace(newBody, "price_out_per_1k: 0.0015", "price_out_per_1k: 0.015", 1)
	newReg := msLoadRegistry(t, newBody)
	newSel := modelregistry.NewSelector(newReg).WithRecorder(&msPGRecorder{db: db, reg: newReg})
	second, err := newSel.Select(firstReq)
	if err != nil {
		t.Fatalf("second select: %v", err)
	}
	if first.EstimatedCost >= second.EstimatedCost {
		t.Fatalf("new price must raise estimated cost: old=%f new=%f", first.EstimatedCost, second.EstimatedCost)
	}

	ctx := context.Background()
	var oldPrice, newPrice float64
	if err := db.QueryRow(ctx, `
		SELECT input_price FROM hlp_task.model_selections
		WHERE task_id='ms-select-task' AND model_id='cheap'
		ORDER BY created_at ASC LIMIT 1`).Scan(&oldPrice); err != nil {
		t.Fatalf("read old row: %v", err)
	}
	if err := db.QueryRow(ctx, `
		SELECT input_price FROM hlp_task.model_selections
		WHERE task_id='ms-select-task' AND model_id='cheap'
		ORDER BY created_at DESC LIMIT 1`).Scan(&newPrice); err != nil {
		t.Fatalf("read new row: %v", err)
	}
	if oldPrice != 0.0005 || newPrice != 0.005 {
		t.Fatalf("history changed: old=%f new=%f", oldPrice, newPrice)
	}
	var count int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM hlp_task.model_selections WHERE task_id='ms-select-task'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("selection rows = %d, want 2", count)
	}
}

func TestModelSuiteDisabledModelFallsBack(t *testing.T) {
	msSuitePG(t)
	disabled := strings.Replace(msBaseRegistry,
		"    status: available\n    tags: [strong]",
		"    status: disabled\n    tags: [strong]", 1)
	// defaults/stage_overrides 不能引用停用模型（validateConfig 会整份拒绝）。
	disabled = strings.Replace(disabled, "fallback: [strong, cheap]", "fallback: [cheap]", 1)
	disabled = strings.Replace(disabled, "  d: strong", "  d: cheap", 1)
	reg := msLoadRegistry(t, disabled)

	// 显式引用停用模型必须在调用前被拒。
	req := msSelectReq("c", modelregistry.StrategyBalanced)
	req.ModelID = "strong"
	_, err := modelregistry.NewSelector(reg).Select(req)
	msReject(t, err, modelregistry.CodeCapabilityMismatch)

	// 回退链不再把停用模型作为候选。
	chain := modelregistry.NewFallbackChain(reg, modelregistry.DefaultFallbackConfig())
	cands := chain.Candidates("cheap", "c", modelregistry.FallbackRequirement{RequireToolCall: true})
	for _, m := range cands {
		if m.ID == "strong" {
			t.Fatal("disabled model must not be a fallback candidate")
		}
	}

	// 引用它的任务在下一次调用前切换到候选并发出 model.switched。
	sink, emitter := msEventEnv(t, "ms-task-disabled")
	chain2 := modelregistry.NewFallbackChain(reg, modelregistry.DefaultFallbackConfig())
	res, err := chain2.Execute(context.Background(), modelregistry.FallbackRequest{
		TaskID: "ms-task-disabled", Stage: "c", Primary: "strong",
		Requirement: modelregistry.FallbackRequirement{RequireToolCall: true, Fallbacks: []string{"cheap"}},
	}, func(_ context.Context, modelID string) modelregistry.CallOutcome {
		if modelID == "strong" {
			return modelregistry.CallOutcome{Kind: modelregistry.FailureRateLimit, Err: errors.New("rate limited")}
		}
		return modelregistry.CallOutcome{TokensIn: 10, TokensOut: 5}
	})
	if err != nil {
		t.Fatalf("disabled-primary fallback: %v", err)
	}
	if !res.Switched || res.Model == nil || res.Model.ID != "cheap" {
		t.Fatalf("disabled model did not fall back: %+v", res)
	}
	for _, ev := range res.Events {
		if ev.Type == modelregistry.EventModelSwitched {
			if err := emitter.EmitModelEvent(context.Background(), orchestrator.EmitContext{Stage: "c"}, ev); err != nil {
				t.Fatalf("emit switched: %v", err)
			}
		}
	}
	var switched bool
	for _, e := range sink.snapshot() {
		if e.EventType == event.ModelSwitched {
			switched = true
			if !strings.Contains(string(e.Payload), `"from":"strong"`) {
				t.Fatalf("switched payload = %s", e.Payload)
			}
		}
	}
	if !switched {
		t.Fatalf("model.switched not published: %+v", res)
	}
	msAssertSeq(t, sink, "ms-task-disabled")
}

func TestModelSuiteHotReloadRejectsBadSnapshot(t *testing.T) {
	msSuitePG(t)
	path := msWriteRegistry(t, msBaseRegistry)
	reg, err := modelregistry.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reloader := modelregistry.NewHotReloader(reg, path)

	bad := strings.Replace(msBaseRegistry, "max_output_tokens: 4096", "max_output_tokens: 99999", 1)
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reloader.Reload("suite"); err == nil {
		t.Fatal("invalid snapshot must be rejected by hot reload")
	}
	if reloader.Registry().Version != reg.Version {
		t.Fatal("rejected snapshot must not replace the running registry")
	}
	if _, ok := reloader.Registry().Get("cheap"); !ok {
		t.Fatal("old snapshot must still be served")
	}
}

// ---------------------------------------------------------------------------
// 9.10 策略
// ---------------------------------------------------------------------------

func TestModelSuiteThreeStrategies(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	sel := modelregistry.NewSelector(reg)
	cases := []struct {
		strategy string
		want     string
	}{
		{modelregistry.StrategyCostFirst, "cheap"},
		{modelregistry.StrategyQualityFirst, "strong"},
		{modelregistry.StrategyBalanced, "cheap"},
	}
	for _, c := range cases {
		t.Run(c.strategy, func(t *testing.T) {
			req := msSelectReq("c", c.strategy)
			// 阶段预算接近 cheap 的 32k 窗口，使 quality_first 的质量权重
			// 能压过价差（否则廉价模型在三种策略下都赢，无法区分策略）。
			req.StageBudgetTokens = 31000
			first, err := sel.Select(req)
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			second, err := sel.Select(req)
			if err != nil {
				t.Fatalf("select again: %v", err)
			}
			if first.Model.ID != c.want || second.Model.ID != c.want {
				t.Fatalf("%s chose %s/%s, want %s", c.strategy, first.Model.ID, second.Model.ID, c.want)
			}
			if first.Strategy != c.strategy || second.Strategy != c.strategy {
				t.Fatalf("strategy recorded as %q/%q", first.Strategy, second.Strategy)
			}
			if len(first.Candidates) == 0 {
				t.Fatal("candidates[].score must be recorded")
			}
			if !sameScores(first.Candidates, second.Candidates) {
				t.Fatalf("strategy is not reproducible:\n%+v\n%+v", first.Candidates, second.Candidates)
			}
		})
	}
}

func sameScores(a, b []modelregistry.CandidateScore) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ModelID != b[i].ModelID || a[i].Score != b[i].Score || a[i].Chosen != b[i].Chosen {
			return false
		}
	}
	return true
}

const msTieRegistry = `
version: 1
models:
  - id: tie-a
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 4096
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.001
    price_out_per_1k: 0.001
  - id: tie-b
    provider: p
    kind: chat
    context_window: 65536
    max_output_tokens: 4096
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.001
    price_out_per_1k: 0.001
  - id: tie-c
    provider: p
    kind: chat
    context_window: 65536
    max_output_tokens: 4096
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.001
    price_out_per_1k: 0.001
defaults:
  strategy: balanced
`

func TestModelSuiteTieBreakReliabilityWindowID(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msTieRegistry)
	req := msSelectReq("c", modelregistry.StrategyBalanced)
	req.StageBudgetTokens = 0 // headroom 全部为 1，保证 score 真正相同

	// reliability 优先：tie-a 无错误（rel=1），tie-b/c 有 50% 错误（rel=0.5）。
	req.RecentErrorRates = map[string]float64{"tie-a": 0, "tie-b": 0.5, "tie-c": 0.5}
	sel, err := modelregistry.NewSelector(reg).Select(req)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.Model.ID != "tie-a" {
		t.Fatalf("reliability tie-break chose %s, want tie-a", sel.Model.ID)
	}

	// 同 reliability：window 大者优先（tie-b/c > tie-a）。
	req.RecentErrorRates = map[string]float64{"tie-a": 0.5, "tie-b": 0, "tie-c": 0}
	sel, err = modelregistry.NewSelector(reg).Select(req)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.Model.ID != "tie-b" {
		t.Fatalf("window tie-break chose %s, want tie-b", sel.Model.ID)
	}

	// 同 reliability 同 window：id 字典序最小者优先。
	req.RecentErrorRates = map[string]float64{}
	sel, err = modelregistry.NewSelector(reg).Select(req)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.Model.ID != "tie-b" {
		t.Fatalf("id tie-break chose %s, want tie-b", sel.Model.ID)
	}
	if got := sel.Candidates[0].ModelID; got != "tie-b" {
		t.Fatalf("candidate order = %s, want tie-b", got)
	}
}

func TestModelSuiteStageOverride(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	sink, emitter := msEventEnv(t, "ms-task-stage")
	sel := modelregistry.NewSelector(reg)
	for _, stage := range []string{"a", "b", "c", "d", "e"} {
		got, err := sel.Select(msSelectReq(stage, ""))
		if err != nil {
			t.Fatalf("stage %s: %v", stage, err)
		}
		want := "cheap"
		if stage == "d" {
			want = "strong"
		}
		if got.Model.ID != want {
			t.Fatalf("stage %s model = %s, want %s", stage, got.Model.ID, want)
		}
		if err := emitter.EmitModelEvent(context.Background(), orchestrator.EmitContext{Stage: stage}, got.Event); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	for _, raw := range sink.snapshot() {
		if raw.EventType != event.ModelSelected {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(raw.Payload, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if payload["stage"] == "" || payload["model_id"] == "" {
			t.Fatalf("model.selected lost stage/model: %s", raw.Payload)
		}
	}
	msAssertSeq(t, sink, "ms-task-stage")
}

// ---------------------------------------------------------------------------
// 9.10 能力匹配 / 切换
// ---------------------------------------------------------------------------

func TestModelSuiteCapabilityMismatch(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	req := msSelectReq("c", modelregistry.StrategyBalanced)
	req.ModelID = "notool"
	req.RequireToolCall = true
	_, err := modelregistry.NewSelector(reg).Select(req)
	msReject(t, err, modelregistry.CodeCapabilityMismatch)
	var capErr *modelregistry.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("expected CapabilityError, got %T", err)
	}
	if len(capErr.Suggested) == 0 {
		t.Fatal("suggested models must not be empty")
	}
	for _, id := range capErr.Suggested {
		m, ok := reg.Get(id)
		if !ok || !m.ToolCall {
			t.Fatalf("suggested model %s does not satisfy tool_call", id)
		}
	}
}

func TestModelSuiteCapabilityWindowTooSmall(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	req := msSelectReq("c", modelregistry.StrategyBalanced)
	req.ModelID = "cheap"
	req.RequiredContextWindow = 1 << 20
	_, err := modelregistry.NewSelector(reg).Select(req)
	msReject(t, err, modelregistry.CodeCapabilityMismatch)
	msReject(t, err, "context_window_too_small")
}

func TestModelSuiteSwitchForbiddenMidStage(t *testing.T) {
	msSuitePG(t)
	err := modelregistry.ValidateSwitch(modelregistry.SwitchCheck{From: "cheap", To: "strong"})
	if err == nil {
		t.Fatal("mid-stage switch without an allowed reason must be forbidden")
	}
	var fe *modelregistry.FallbackError
	if !errors.As(err, &fe) || fe.Code != modelregistry.CodeSwitchForbidden {
		t.Fatalf("code = %v, want %s", err, modelregistry.CodeSwitchForbidden)
	}
	// 未审批的 manual upgrade 同样拒绝；审批齐全才放行。
	if err := modelregistry.ValidateSwitch(modelregistry.SwitchCheck{
		From: "cheap", To: "strong", Reason: modelregistry.SwitchReasonManualUpgrade,
	}); err == nil {
		t.Fatal("manual upgrade without approval must be forbidden")
	}
	if err := modelregistry.ValidateSwitch(modelregistry.SwitchCheck{
		From: "cheap", To: "strong", Reason: modelregistry.SwitchReasonManualUpgrade,
		Approved: true, ApprovalID: "appr-1",
	}); err != nil {
		t.Fatalf("approved upgrade must pass: %v", err)
	}
}

func TestModelSuiteRateLimitSwitch(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	sink, emitter := msEventEnv(t, "ms-task-switch")
	chain := modelregistry.NewFallbackChain(reg, modelregistry.DefaultFallbackConfig())
	res, err := chain.Execute(context.Background(), modelregistry.FallbackRequest{
		TaskID: "ms-task-switch", Stage: "c", Primary: "cheap",
		Requirement: modelregistry.FallbackRequirement{RequireToolCall: true, Fallbacks: []string{"strong"}},
	}, func(_ context.Context, modelID string) modelregistry.CallOutcome {
		if modelID == "cheap" {
			return modelregistry.CallOutcome{Kind: modelregistry.FailureRateLimit, Err: errors.New("429")}
		}
		return modelregistry.CallOutcome{TokensIn: 100, TokensOut: 50, Latency: 12 * time.Millisecond}
	})
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if !res.Switched || res.Model.ID != "strong" || res.Trigger != modelregistry.TriggerRateLimit {
		t.Fatalf("result = %+v", res)
	}
	var switched, fallback bool
	for _, ev := range res.Events {
		switch ev.Type {
		case modelregistry.EventModelSwitched:
			switched = true
			payload := ev.Payload
			if payload["from"] != "cheap" || payload["to"] != "strong" || payload["reason"] == "" {
				t.Fatalf("model.switched payload = %+v", payload)
			}
			if err := emitter.EmitModelEvent(context.Background(), orchestrator.EmitContext{Stage: "c"}, ev); err != nil {
				t.Fatalf("emit: %v", err)
			}
		case modelregistry.EventModelFallback:
			fallback = true
			if err := emitter.EmitModelEvent(context.Background(), orchestrator.EmitContext{Stage: "c"}, ev); err != nil {
				t.Fatalf("emit: %v", err)
			}
		}
	}
	if !switched || !fallback {
		t.Fatalf("missing events: switched=%v fallback=%v (%+v)", switched, fallback, res.Events)
	}
	msAssertSeq(t, sink, "ms-task-switch")
}

// ---------------------------------------------------------------------------
// 9.10 回退
// ---------------------------------------------------------------------------

func TestModelSuiteFourthFallbackNotAttempted(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	cfg := modelregistry.DefaultFallbackConfig()
	cfg.CandidatesPerStage = 1
	cfg.MaxPerStage = 3
	chain := modelregistry.NewFallbackChain(reg, cfg)
	failAll := func(_ context.Context, _ string) modelregistry.CallOutcome {
		return modelregistry.CallOutcome{Kind: modelregistry.FailureRateLimit, Err: errors.New("429")}
	}
	req := modelregistry.FallbackRequest{
		Stage: "c", Primary: "cheap",
		Requirement: modelregistry.FallbackRequirement{RequireToolCall: true, Fallbacks: []string{"strong"}},
	}
	for i := 1; i <= 3; i++ {
		if _, err := chain.Execute(context.Background(), req, failAll); err == nil {
			t.Fatalf("attempt %d must fail", i)
		}
		if got := chain.StageFallbacks("c"); got != i {
			t.Fatalf("stage fallbacks after %d = %d", i, got)
		}
	}
	res, err := chain.Execute(context.Background(), req, failAll)
	if err == nil {
		t.Fatal("4th execution must be refused")
	}
	var fe *modelregistry.FallbackError
	if !errors.As(err, &fe) || fe.Code != modelregistry.CodeFallbackExhausted {
		t.Fatalf("code = %v, want %s", err, modelregistry.CodeFallbackExhausted)
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("4th execution attempts = %d, want only primary (no fallback call)", len(res.Attempts))
	}
	if res.Attempts[0].ModelID != "cheap" {
		t.Fatalf("4th execution called %s", res.Attempts[0].ModelID)
	}
}

func TestModelSuiteFallbackBudget(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	chain := modelregistry.NewFallbackChain(reg, modelregistry.DefaultFallbackConfig())
	budget := &modelregistry.Budget{LimitTokens: 1000, LimitCost: 10}
	res, err := chain.Execute(context.Background(), modelregistry.FallbackRequest{
		TaskID: "ms-task-budget", Stage: "c", Primary: "cheap",
		Requirement:       modelregistry.FallbackRequirement{RequireToolCall: true, Fallbacks: []string{"strong"}},
		Budget:            budget,
		TaskBudgetTokens:  100000,
		EstimatedTokensIn: 100, EstimatedTokensOut: 50,
	}, func(_ context.Context, modelID string) modelregistry.CallOutcome {
		if modelID == "cheap" {
			return modelregistry.CallOutcome{Kind: modelregistry.FailureRateLimit, Err: errors.New("429"), TokensIn: 300, TokensOut: 100}
		}
		return modelregistry.CallOutcome{TokensIn: 300, TokensOut: 100}
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if budget.UsedTokens != 800 {
		t.Fatalf("used tokens = %d, want 800 (both attempts charged to the same pool)", budget.UsedTokens)
	}
	if res.CompactionBudget <= 0 {
		t.Fatalf("compaction budget not recomputed: %+v", res)
	}

	// 超预算必须立即失败，不得继续回退。
	over := &modelregistry.Budget{LimitTokens: 500, LimitCost: 10}
	res, err = chain.Execute(context.Background(), modelregistry.FallbackRequest{
		Stage: "c", Primary: "cheap",
		Requirement: modelregistry.FallbackRequirement{RequireToolCall: true, Fallbacks: []string{"strong"}},
		Budget:      over,
	}, func(_ context.Context, _ string) modelregistry.CallOutcome {
		return modelregistry.CallOutcome{Kind: modelregistry.FailureRateLimit, Err: errors.New("429"), TokensIn: 600}
	})
	if err == nil || !res.BudgetExceeded {
		t.Fatalf("over-budget fallback must stop: res=%+v err=%v", res, err)
	}
	var fe *modelregistry.FallbackError
	if !errors.As(err, &fe) || fe.Code != modelregistry.CodeBudgetExceeded {
		t.Fatalf("code = %v, want %s", err, modelregistry.CodeBudgetExceeded)
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("over-budget execution made %d attempts, want 1", len(res.Attempts))
	}
}

const msSoloRegistry = `
version: 1
models:
  - id: solo
    provider: p
    kind: chat
    context_window: 32768
    max_output_tokens: 4096
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.001
    price_out_per_1k: 0.002
defaults:
  task_model: solo
`

func TestModelSuiteEmptyFallbackChain(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msSoloRegistry)
	chain := modelregistry.NewFallbackChain(reg, modelregistry.DefaultFallbackConfig())
	res, err := chain.Execute(context.Background(), modelregistry.FallbackRequest{
		Stage: "c", Primary: "solo",
	}, func(_ context.Context, _ string) modelregistry.CallOutcome {
		return modelregistry.CallOutcome{Kind: modelregistry.FailureRateLimit, Err: errors.New("429")}
	})
	if err == nil {
		t.Fatal("empty fallback list must fail fast")
	}
	var fe *modelregistry.FallbackError
	if !errors.As(err, &fe) || fe.Code != modelregistry.CodeFallbackUnavailable {
		t.Fatalf("code = %v, want %s", err, modelregistry.CodeFallbackUnavailable)
	}
	if !res.Exhausted || res.Switched {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("empty chain must not hang or retry: %+v", res.Attempts)
	}
}

// ---------------------------------------------------------------------------
// 9.10 可用性 / 成本联动 / embedding / 密钥 / 热更新
// ---------------------------------------------------------------------------

func TestModelSuiteFiveFailuresUnavailable(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	chain := modelregistry.NewFallbackChain(reg, modelregistry.DefaultFallbackConfig())
	var event modelregistry.ModelEvent
	for i := 0; i < 5; i++ {
		events := chain.ObserveFailure("strong", modelregistry.FailureError)
		for _, ev := range events {
			if ev.Type == modelregistry.EventModelUnavailable {
				event = ev
			}
		}
	}
	if !chain.IsUnavailable("strong") {
		t.Fatal("5 consecutive failures must mark the model unavailable")
	}
	if event.Type != modelregistry.EventModelUnavailable {
		t.Fatalf("model.unavailable not emitted: %+v", event)
	}
	unavailable := chain.UnavailableModels()
	for _, strategy := range []string{
		modelregistry.StrategyCostFirst, modelregistry.StrategyQualityFirst, modelregistry.StrategyBalanced,
	} {
		req := msSelectReq("c", strategy)
		req.Unavailable = unavailable
		sel, err := modelregistry.NewSelector(reg).WithUnavailable(chain.UnavailableModels).Select(req)
		if err != nil {
			t.Fatalf("%s: %v", strategy, err)
		}
		if sel.Model.ID == "strong" {
			t.Fatalf("%s still selected unavailable model", strategy)
		}
	}
	// 恢复后 model.recovered 且重新可选（用显式选择证明可用，而不是假定
        // 某种策略必然选它）。
	rec := chain.ObserveSuccess("strong")
	if len(rec) == 0 || rec[0].Type != modelregistry.EventModelRecovered {
		t.Fatalf("recovery events = %+v", rec)
	}
	if chain.IsUnavailable("strong") {
		t.Fatal("model must be selectable again after recovery")
	}
	req := msSelectReq("c", modelregistry.StrategyQualityFirst)
	req.ModelID = "strong"
	sel, err := modelregistry.NewSelector(reg).WithUnavailable(chain.UnavailableModels).Select(req)
	if err != nil || sel.Model.ID != "strong" {
		t.Fatalf("recovered strong must be selectable: %+v err=%v", sel, err)
	}
}

func TestModelSuiteCostLinkedToWindow(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	cheap, _ := reg.Get("cheap")
	strong, _ := reg.Get("strong")

	small := modelregistry.Recompute(modelregistry.RecomputeRequest{
		Model: cheap, TaskBudgetTokens: 100000, EstimatedTokensIn: 1000, EstimatedTokensOut: 200,
	})
	large := modelregistry.Recompute(modelregistry.RecomputeRequest{
		Model: strong, TaskBudgetTokens: 100000, EstimatedTokensIn: 1000, EstimatedTokensOut: 200,
	})
	if small.Window != 32768 || large.Window != 131072 {
		t.Fatalf("windows = %d/%d", small.Window, large.Window)
	}
	if large.ThresholdTokens <= small.ThresholdTokens {
		t.Fatalf("128k threshold %d must be greater than 32k threshold %d", large.ThresholdTokens, small.ThresholdTokens)
	}
	if large.EstimatedCost <= small.EstimatedCost {
		t.Fatalf("strong cost %f must exceed cheap cost %f", large.EstimatedCost, small.EstimatedCost)
	}
	if large.Event.Type != modelregistry.EventModelConfigApplied || large.Event.Payload["window"] != 131072 {
		t.Fatalf("config event = %+v", large.Event)
	}
}

func TestModelSuiteEmbeddingDimGuard(t *testing.T) {
	msSuitePG(t)
	// 加载期 enforce_dim 校验保留（整份快照拒绝）。
	badLoad := strings.Replace(msBaseRegistry, "enforce_dim: 1024", "enforce_dim: 1536", 1)
	_, err := modelregistry.Load(msWriteRegistry(t, badLoad))
	msReject(t, err, modelregistry.CodeMetaIncomplete)
	msReject(t, err, "embedding_dim != enforce_dim")
}

// 9.10 embedding：768 维模型写入 1536 维索引必须返回 E_EMBEDDING_DIM_MISMATCH，
// 且校验发生在写入前（向量长度/索引维度均可拒绝）。
func TestModelSuiteEmbeddingDimMismatch(t *testing.T) {
	msSuitePG(t)
	body768 := strings.Replace(msBaseRegistry, "embedding_dim: 1024", "embedding_dim: 768", 1)
	body768 = strings.Replace(body768, "enforce_dim: 1024", "enforce_dim: 768", 1)
	reg := msLoadRegistry(t, body768)

	if err := reg.ValidateEmbeddingWrite("embed", 768); err != nil {
		t.Fatalf("768-dim write must pass: %v", err)
	}
	err := reg.ValidateEmbeddingWrite("embed", 1536)
	if err == nil {
		t.Fatal("768-dim model writing a 1536-dim index must be rejected")
	}
	var dimErr *modelregistry.EmbeddingDimError
	if !errors.As(err, &dimErr) {
		t.Fatalf("expected EmbeddingDimError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), modelregistry.CodeEmbeddingDimMismatch) {
		t.Fatalf("error = %v, want %s", err, modelregistry.CodeEmbeddingDimMismatch)
	}
	if dimErr.ModelDim != 768 || dimErr.IndexDim != 1536 {
		t.Fatalf("dimension error = %+v", dimErr)
	}

	// 向量长度路径同样在写入前拒绝。
	if err := reg.ValidateEmbeddingVector("embed", make([]float32, 768)); err != nil {
		t.Fatalf("768-length vector must pass: %v", err)
	}
	if err := reg.ValidateEmbeddingVector("embed", make([]float32, 1536)); !errors.As(err, &dimErr) {
		t.Fatalf("1536-length vector must be rejected with %s: %v", modelregistry.CodeEmbeddingDimMismatch, err)
	}

	// 加载期 enforce_dim 与写入维度同时校验：模型 768 / enforce 768 时写 1024 也拒绝。
	if err := reg.ValidateEmbeddingWrite("embed", 1024); !errors.As(err, &dimErr) {
		t.Fatalf("enforce_dim path must reject 1024: %v", err)
	}
}

func TestModelSuiteSecretNotExposed(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	raw, err := json.Marshal(reg.Views())
	if err != nil {
		t.Fatalf("marshal views: %v", err)
	}
	body := string(raw)
	for _, forbidden := range []string{"api_key_ref", "SUITE_KEY", "sk-"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("views leaked %q: %s", forbidden, body)
		}
	}
	if ref, ok := reg.ResolveCredential("strong"); !ok || ref != "env:SUITE_KEY" {
		t.Fatalf("credential ref = %q ok=%v", ref, ok)
	}
	sel, err := modelregistry.NewSelector(reg).Select(msSelectReq("c", modelregistry.StrategyBalanced))
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	payload, _ := json.Marshal(sel.Event.Payload)
	for _, forbidden := range []string{"api_key_ref", "SUITE_KEY"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("model.selected leaked %q: %s", forbidden, payload)
		}
	}
}

func TestModelSuiteHotReloadNextCall(t *testing.T) {
	msSuitePG(t)
	body := strings.Replace(msBaseRegistry, "  d: strong", "  d:\n    model: strong\n    strategy: quality_first\n", 1)
	path := msWriteRegistry(t, body)
	reg, err := modelregistry.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reloader := modelregistry.NewHotReloader(reg, path)
	oldSel := modelregistry.NewSelector(reloader.Registry())

	current, err := oldSel.Select(msSelectReq("d", ""))
	if err != nil {
		t.Fatalf("current select: %v", err)
	}
	if current.Model.ID != "strong" {
		t.Fatalf("current call model = %s", current.Model.ID)
	}

	nextBody := strings.Replace(body, "  d:\n    model: strong\n    strategy: quality_first\n",
		"  d:\n    model: cheap\n    strategy: balanced\n", 1)
	if err := os.WriteFile(path, []byte(nextBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// 确保 mtime 变化（部分文件系统精度为秒）。
	time.Sleep(10 * time.Millisecond)
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err := reloader.Reload("operator")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !res.Changed {
		t.Fatalf("reload must report a change: %+v", res)
	}
	found := false
	for _, key := range res.ChangedKeys {
		if key == "stage_overrides.d" {
			found = true
		}
	}
	if !found {
		t.Fatalf("changed keys = %v", res.ChangedKeys)
	}
	// 已经完成的调用仍用旧配置（旧 selector 不变）。
	again, err := oldSel.Select(msSelectReq("d", ""))
	if err != nil {
		t.Fatalf("old selector: %v", err)
	}
	if again.Model.ID != "strong" {
		t.Fatalf("in-flight selection changed to %s", again.Model.ID)
	}
	// 下一次调用使用新配置。
	next, err := modelregistry.NewSelector(reloader.Registry()).Select(msSelectReq("d", ""))
	if err != nil {
		t.Fatalf("next select: %v", err)
	}
	if next.Model.ID != "cheap" {
		t.Fatalf("next call model = %s, want cheap", next.Model.ID)
	}
}

// ---------------------------------------------------------------------------
// model_selections 真实 PG 写入 + 约束
// ---------------------------------------------------------------------------

// msPGRecorder 直接向 hlp_task.model_selections 写入与生产 repository 相同的列。
// 说明：services/harnessloop-service/internal 受 Go internal 规则限制，pkg 测试
// 无法 import 生产 ModelSelectionRepo，因此这里按 008 DDL 复刻写入。
type msPGRecorder struct {
	db      *pgindex.DB
	reg     *modelregistry.Registry
	attempt int
}

func (r *msPGRecorder) RecordSelection(rec modelregistry.SelectionRecord) error {
	m, ok := r.reg.Get(rec.ModelID)
	if !ok {
		return fmt.Errorf("recorder: unknown model %s", rec.ModelID)
	}
	candidates, err := json.Marshal(rec.Candidates)
	if err != nil {
		return err
	}
	if r.attempt == 0 {
		r.attempt = 1
	}
	_, err = r.db.Exec(context.Background(), `
		INSERT INTO hlp_task.model_selections
			(selection_id, tenant_id, project_id, task_id, stage, attempt_no, model_id,
			 strategy, resolved_from, reason, candidates, context_window, threshold_tokens,
			 input_price, output_price, tokens_in, tokens_out, cost, is_fallback)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULL,$10,$11,$12,$13,$14,$15,$16,$17,false)`,
		rec.SelectionID, rec.TenantID, rec.ProjectID, rec.TaskID, rec.Stage, r.attempt,
		rec.ModelID, rec.Strategy, rec.ResolvedFrom, candidates,
		rec.Window, rec.ThresholdTokens, m.View().PriceInPer1K, m.View().PriceOutPer1K,
		rec.TokensIn, rec.TokensOut, rec.Cost)
	return err
}

func TestModelSuiteSelectionPersistedToPG(t *testing.T) {
	db := msSuitePG(t)
	msSeedTask(t, db, "ms-select-task")
	reg := msLoadRegistry(t, msBaseRegistry)
	rec := &msPGRecorder{db: db, reg: reg}
	sel := modelregistry.NewSelector(reg).WithRecorder(rec)
	req := msSelectReq("c", modelregistry.StrategyQualityFirst)
	req.StageBudgetTokens = 31000
	req.EstimatedTokensIn, req.EstimatedTokensOut = 1000, 200
	got, err := sel.Select(req)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if got.Model.ID != "strong" {
		t.Fatalf("model = %s", got.Model.ID)
	}

	ctx := context.Background()
	var (
		modelID, strategy, resolvedFrom, stage string
		window, threshold                      int
		candidates                             []byte
		cost                                   float64
	)
	err = db.QueryRow(ctx, `
		SELECT model_id, strategy, resolved_from, stage, context_window, threshold_tokens, candidates, cost
		FROM hlp_task.model_selections WHERE task_id='ms-select-task' ORDER BY created_at DESC LIMIT 1`).
		Scan(&modelID, &strategy, &resolvedFrom, &stage, &window, &threshold, &candidates, &cost)
	if err != nil {
		t.Fatalf("read persisted selection: %v", err)
	}
	if modelID != "strong" || strategy != modelregistry.StrategyQualityFirst || resolvedFrom != modelregistry.ResolvedFromStrategy {
		t.Fatalf("persisted = %s/%s/%s", modelID, strategy, resolvedFrom)
	}
	windowRatio := float64(131072) * modelregistry.DefaultCompactionRatio
	if stage != "c" || window != 131072 || threshold != int(windowRatio) {
		t.Fatalf("persisted window/threshold = %d/%d stage=%s", window, threshold, stage)
	}
	if cost <= 0 || len(candidates) == 0 || !json.Valid(candidates) {
		t.Fatalf("cost=%f candidates=%s", cost, candidates)
	}
}

func TestModelSuiteSelectionConstraintsEnforced(t *testing.T) {
	db := msSuitePG(t)
	msSeedTask(t, db, "ms-task-constraints")
	ctx := context.Background()
	insert := `
		INSERT INTO hlp_task.model_selections
			(selection_id, tenant_id, project_id, task_id, stage, attempt_no, model_id,
			 strategy, resolved_from, candidates, context_window, threshold_tokens,
			 input_price, output_price)
		VALUES ($1,$2,$3,$4,$5,1,'strong','balanced','strategy','[]',32768,26214,0.02,0.06)`

	// 非法 stage：CHECK 必须拒绝。
	_, err := db.Exec(ctx, insert, "ms-bad-stage", msTenant, msProject, "ms-task-constraints", "z")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("invalid stage must violate CHECK: %v", err)
	}
	// 非法 strategy：CHECK 必须拒绝。
	_, err = db.Exec(ctx, `
		INSERT INTO hlp_task.model_selections
			(selection_id, tenant_id, project_id, task_id, stage, attempt_no, model_id,
			 strategy, resolved_from, candidates, context_window, threshold_tokens,
			 input_price, output_price)
		VALUES ('ms-bad-strategy',$1,$2,'ms-task-constraints','c',1,'strong','made_up','strategy','[]',32768,26214,0.02,0.06)`,
		msTenant, msProject)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("invalid strategy must violate CHECK: %v", err)
	}
	// 不存在的 task_id：外键必须拒绝。
	_, err = db.Exec(ctx, insert, "ms-bad-fk", msTenant, msProject, "ms-task-does-not-exist", "c")
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("unknown task must violate FK: %v", err)
	}
}
// 9.10 回退性能口径：一次 rate_limit 回退（primary 失败 + 1 个 fallback 成功）
// 的 Execute 耗时；无 design 阈值，留档用于报告区间。
func TestModelSuiteFallbackLatency(t *testing.T) {
	msSuitePG(t)
	reg := msLoadRegistry(t, msBaseRegistry)
	cfg := modelregistry.DefaultFallbackConfig()
	req := modelregistry.FallbackRequest{
		TaskID: "ms-task-fallback-latency", Stage: "c", Primary: "cheap",
		Requirement: modelregistry.FallbackRequirement{RequireToolCall: true, Fallbacks: []string{"strong"}},
	}
	call := func(_ context.Context, modelID string) modelregistry.CallOutcome {
		if modelID == "cheap" {
			return modelregistry.CallOutcome{Kind: modelregistry.FailureRateLimit, Err: errors.New("429")}
		}
		return modelregistry.CallOutcome{TokensIn: 100, TokensOut: 50}
	}
	var samples []time.Duration
	for i := 0; i < 100; i++ {
		chain := modelregistry.NewFallbackChain(reg, cfg)
		start := time.Now()
		res, err := chain.Execute(context.Background(), req, call)
		samples = append(samples, time.Since(start))
		if err != nil || !res.Switched {
			t.Fatalf("round %d: res=%+v err=%v", i, res, err)
		}
	}
	p95 := msP95(samples)
	var min, max time.Duration = samples[0], samples[0]
	for _, d := range samples {
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	t.Logf("model fallback latency: n=%d P95=%v min=%v max=%v", len(samples), p95, min, max)
	if p95 > 100*time.Millisecond {
		t.Fatalf("fallback P95 = %v, must be <= 100ms", p95)
	}
}

func msP95(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	idx := int(float64(len(samples)-1)*0.95 + 0.5)
	if idx >= len(samples) {
		idx = len(samples) - 1
	}
	return samples[idx]
}
