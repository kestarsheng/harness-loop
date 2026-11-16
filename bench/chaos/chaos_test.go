package chaos

// T-P2-T2 · design 9.8 混沌与故障注入（应用层用例）。
//
// 分工说明：
//   - 依赖级"真停机"（PG / Mongo / Redis）、kill -9、磁盘满由 bench/chaos/inject.sh
//     驱动 docker 完成，探针走真实客户端（bench/chaos/probe）；
//   - 本文件覆盖"停机后平台侧行为"的部分：子任务卡死、配额打满、模型全不可用、
//     审批不可达、工作区外部改动、kill -9 后的恢复幂等与事件续号、PG 不可达时
//     必须显式报错。
//
// 判定口径：每个用例都必须证明两件事——(1) 故障被识别并落到事件/错误上；
// (2) 没有静默放行（不出现"看起来成功"的返回值）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-platform/pkg/agent/approval"
	"agent-platform/pkg/harnessloop/checkpoint"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/subtask"
	"agent-platform/pkg/pgindex"
)

func chaosSubtask(id, role string, index int) *subtask.Subtask {
	return &subtask.Subtask{
		SubtaskID:       id,
		ParentTaskID:    "chaos-task",
		TenantID:        "chaos-tenant",
		ProjectID:       "chaos-project",
		DagID:           "chaos-dag",
		DagNodeID:       id,
		Depth:           1,
		IndexInParent:   index,
		ParallelSetID:   "p1",
		Role:            role,
		Status:          subtask.StatusPending,
		FailureStrategy: subtask.StrategyFailFast,
		AttemptNo:       1,
		OutputContract: map[string]any{
			"type":             "artifacts",
			"paths":            []string{"artifacts/" + id + "/**"},
			"estimated_tokens": 1024,
		},
		AcceptanceCriteria: []string{"go build ./..."},
	}
}

// TestChaosSubtaskHang 覆盖 design 9.8「子任务进程级卡死」：
// 超时必须触发 subtask.failed(category=timeout)，并由传播策略推进父任务，
// 不能"挂着当没事"。
func TestChaosSubtaskHang(t *testing.T) {
	cfg := orchestrator.DefaultQuotaConfig()
	cfg.MaxParallelPerParent = 2
	emitter := orchestrator.NewMemoryEmitter()
	scheduler := orchestrator.NewScheduler(cfg, emitter)
	fixed := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	scheduler.SetClock(func() time.Time { return fixed })
	scheduler.SetParentRemaining(2 * time.Second)

	batch := []*subtask.Subtask{
		chaosSubtask("hang-a", "coder", 0),
		chaosSubtask("hang-b", "verifier", 1),
	}
	if err := scheduler.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if started, err := scheduler.Dispatch(context.Background()); err != nil || len(started) != 2 {
		t.Fatalf("dispatch: started=%d err=%v", len(started), err)
	}

	timeouts, err := scheduler.CheckTimeouts(context.Background(), fixed.Add(3*time.Second))
	if err != nil {
		t.Fatalf("check timeouts: %v", err)
	}
	if len(timeouts) != 2 {
		t.Fatalf("hung subtasks must be timed out, got %d", len(timeouts))
	}
	failed := emitter.Find(orchestrator.EventSubtaskFailed)
	if len(failed) != 2 {
		t.Fatalf("expected 2 subtask.failed events, got %d", len(failed))
	}
	for _, ev := range failed {
		if ev.Payload["category"] != "timeout" {
			t.Fatalf("timeout must be classified as category=timeout: %+v", ev.Payload)
		}
	}
	// 卡死的子任务必须释放配额，否则后续所有子任务都会被它拖住。
	if parent, tenant := scheduler.ActiveSlots(); parent != 0 || tenant != 0 {
		t.Fatalf("timed-out subtasks must release their slots, got (%d,%d)", parent, tenant)
	}

	// 传播策略必须给出决策：不允许把超时静默吞掉。
	failedIDs := make([]orchestrator.FailedSubtask, 0, len(timeouts))
	for _, record := range timeouts {
		failedIDs = append(failedIDs, orchestrator.FailedSubtask{SubtaskID: record.SubtaskID, Category: "timeout"})
	}
	propagator := orchestrator.NewPropagator(cfg)
	decision, err := propagator.Propagate(orchestrator.PropagationRequest{
		ParentTaskID: "chaos-task",
		Sets:         []orchestrator.ParallelSet{{SetID: "p1", Nodes: []string{"hang-a", "hang-b"}, FailureStrategy: subtask.StrategyFailFast}},
		Subtasks:     batch,
		Failed:       failedIDs,
	})
	if err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if len(decision.Decisions) != 1 {
		t.Fatalf("expected one propagation decision, got %d", len(decision.Decisions))
	}
	got := decision.Decisions[0]
	if got.Action != orchestrator.ActionCancelSet || got.Rollback.From != "d" || got.Rollback.To != "c" {
		t.Fatalf("fail-fast must cancel the set and roll back d→c: %+v", got)
	}
}

// TestChaosQuotaSaturated 覆盖 design 9.8「并发额度被打满」：
// 只排队（subtask.queued），不失败、不抢占其他父任务配额。
func TestChaosQuotaSaturated(t *testing.T) {
	cfg := orchestrator.DefaultQuotaConfig()
	emitter := orchestrator.NewMemoryEmitter()
	fair := orchestrator.NewTenantFairness(orchestrator.FairnessConfigFromQuota(cfg), nil)
	dispatcher := orchestrator.NewTenantDispatcher(fair)

	var schedulers []*orchestrator.Scheduler
	for _, parent := range []string{"chaos-a", "chaos-b"} {
		scheduler := orchestrator.NewScheduler(cfg, emitter)
		batch := make([]*subtask.Subtask, 0, 14)
		for i := 0; i < 14; i++ {
			st := chaosSubtask(fmt.Sprintf("%s-%02d", parent, i), "coder", i)
			st.ParentTaskID = parent
			batch = append(batch, st)
		}
		if err := scheduler.Enqueue(batch); err != nil {
			t.Fatalf("enqueue %s: %v", parent, err)
		}
		if err := dispatcher.Attach(scheduler, 0); err != nil {
			t.Fatalf("attach %s: %v", parent, err)
		}
		schedulers = append(schedulers, scheduler)
	}
	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	running := 0
	for _, scheduler := range schedulers {
		running += len(scheduler.Running())
	}
	used, max := fair.TenantActive()
	if running != max || used != max || max != cfg.MaxAgentsPerTenant {
		t.Fatalf("tenant pool must be saturated without exceeding it: running=%d used=%d max=%d", running, used, max)
	}
	if len(emitter.Find(orchestrator.EventSubtaskFailed)) != 0 {
		t.Fatalf("quota exhaustion must queue, never fail subtasks")
	}
	if len(emitter.Find(orchestrator.EventSubtaskCancelled)) != 0 {
		t.Fatalf("quota exhaustion must not preempt running work")
	}
	queued := emitter.Find(orchestrator.EventSubtaskQueued)
	if len(queued) == 0 {
		t.Fatalf("queued work must be visible as subtask.queued")
	}
	for _, ev := range queued {
		if ev.Payload["reason"] == nil || ev.Payload["position"] == nil {
			t.Fatalf("queued event must carry position and reason: %+v", ev.Payload)
		}
	}
	// 每个父任务都不得独占整池（T-P2-09 与混沌用例的共同底线）。
	for _, row := range fair.AllocRows() {
		if row.SlotsUsed == cfg.MaxAgentsPerTenant {
			t.Fatalf("%s took the whole tenant pool: %+v", row.ParentTaskID, row)
		}
	}
}

// TestChaosAllModelsUnavailable 覆盖 design 9.8「模型提供方整体不可用」：
// 回退链逐级尝试后必须给出分类错误，父任务不能被吊死。
func TestChaosAllModelsUnavailable(t *testing.T) {
	registry, err := modelregistry.Load(filepath.Join("..", "..", "pkg", "harnessloop", "modelregistry", "seed_models.yaml"))
	if err != nil {
		t.Fatalf("load model registry: %v", err)
	}
	chain := modelregistry.NewFallbackChain(registry, modelregistry.DefaultFallbackConfig())
	for _, id := range []string{"qwen-max", "qwen-plus", "qwen-lite"} {
		for i := 0; i < 5; i++ {
			chain.ObserveFailure(id, modelregistry.FailureError)
		}
	}
	if len(chain.UnavailableModels()) == 0 {
		t.Fatalf("seed models should be marked unavailable after 5 consecutive failures")
	}

	start := time.Now()
	result, err := chain.Execute(context.Background(), modelregistry.FallbackRequest{
		Primary: "qwen-max",
		Stage:   "c",
	}, func(_ context.Context, modelID string) modelregistry.CallOutcome {
		return modelregistry.CallOutcome{Err: fmt.Errorf("dial %s: connection refused", modelID)}
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("all-unavailable must return an error instead of a silent success")
	}
	var fallbackErr *modelregistry.FallbackError
	if !errors.As(err, &fallbackErr) {
		t.Fatalf("error must be classified as FallbackError, got %T: %v", err, err)
	}
	if !result.Exhausted {
		t.Fatalf("result must be marked exhausted: %+v", result)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("all-unavailable must fail fast, took %s", elapsed)
	}
	if len(result.Attempts) > modelregistry.DefaultFallbackConfig().MaxPerStage+1 {
		t.Fatalf("attempt count must stay bounded, got %d", len(result.Attempts))
	}
}

// TestChaosApprovalUnreachable 覆盖 design 9.8「审批服务不可达」：
// 高危调用必须保持挂起，绝不能被放行。
func TestChaosApprovalUnreachable(t *testing.T) {
	manager := approval.NewApprovalFlowManager()
	manager.SetTimeout(1)
	manager.SetAutoApprove(false)

	ctx := context.Background()
	request, err := manager.CreateRequest(ctx, &approval.ApprovalRequest{
		Type:           approval.ApprovalTypeToolCall,
		Priority:       approval.PriorityHigh,
		AgentID:        "agent-chaos",
		UserID:         "oncall",
		Description:    "delete production workspace",
		RiskLevel:      "high",
		Details:        map[string]interface{}{"tool": "rm_rf"},
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}

	// 审批服务不可达 = 永远没有人来裁决；等待方必须超时返回，而不是拿到"通过"。
	waitCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	decision, err := manager.WaitForApproval(waitCtx, request.ID)
	if err == nil && (decision == nil || decision.Decision == approval.StatusApproved) {
		t.Fatalf("unreachable approval must never resolve to approved: decision=%+v err=%v", decision, err)
	}
	if err == nil && decision.Decision == approval.StatusTimeout {
		// 超时也是"没放行"，可以接受，但必须能从待办里看出来。
		t.Logf("approval expired instead of hanging: %+v", decision)
	}
	pending := manager.GetPendingRequests("oncall")
	if len(pending) != 1 || pending[0].ID != request.ID {
		t.Fatalf("the high-risk request must stay pending (suspended), got %+v", pending)
	}
	if pending[0].Status != approval.StatusPending {
		t.Fatalf("pending request status drifted to %s", pending[0].Status)
	}
}

// TestChaosWorkspaceDrift 覆盖 design 9.8「工作区被外部改动」：
// 汇聚必须报 artifact_drift 并把报告判为不完整（不静默）。
func TestChaosWorkspaceDrift(t *testing.T) {
	root := t.TempDir()
	emitter := orchestrator.NewMemoryEmitter()
	aggregator := orchestrator.NewAggregator(orchestrator.FSVerifier{Root: root}, emitter)

	path := filepath.Join(root, "artifacts", "drift.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	hash := sha256Of(t, path)

	st := chaosSubtask("drift-st", "coder", 0)
	st.Status = subtask.StatusAccepted
	st.AcceptanceID = "acc-drift"
	st.ArtifactRefs = []subtask.ArtifactRef{{Path: "artifacts/drift.txt", Hash: hash}}

	// 外部进程在验收之后改了工作区文件。
	if err := os.WriteFile(path, []byte("v2-external-change"), 0o644); err != nil {
		t.Fatalf("external modify: %v", err)
	}

	report, err := aggregator.Aggregate(context.Background(), orchestrator.AggregationRequest{
		ParentTaskID: "chaos-task",
		DagID:        "chaos-dag",
		TenantID:     "chaos-tenant",
		ProjectID:    "chaos-project",
		Subtasks:     []*subtask.Subtask{st},
	})
	if err == nil {
		t.Fatalf("drift must be reported as an error, got a clean report: %+v", report)
	}
	if orchestrator.ErrorCode(err) != orchestrator.CodeAggregationIncomplete {
		t.Fatalf("drift must surface E_AGGREGATION_INCOMPLETE, got %s (%v)", orchestrator.ErrorCode(err), err)
	}
	if report == nil || report.Complete {
		t.Fatalf("report must be marked incomplete")
	}
	if len(report.Items) != 1 || report.Items[0].Reason != "artifact_drift" {
		t.Fatalf("item must carry artifact_drift, got %+v", report.Items)
	}
	if len(emitter.Find(orchestrator.EventAggregationCompleted)) != 1 {
		t.Fatalf("a drift report must still emit aggregation.completed with warn severity")
	}
}

// TestChaosKill9RecoveryNoDoubleCharge 覆盖 design 9.8「kill -9」：
// 重启后事件续号、恢复幂等、已消耗 token 不被重复计入。
func TestChaosKill9RecoveryNoDoubleCharge(t *testing.T) {
	store := checkpoint.NewMemoryStore()
	ctx := context.Background()

	cp := &checkpoint.Checkpoint{
		CheckpointID:      "cp-chaos-1",
		TaskID:            "chaos-task",
		TenantID:          "chaos-tenant",
		Stage:             "c",
		AttemptNo:         1,
		Step:              7,
		Kind:              checkpoint.KindInterval,
		FSM:               checkpoint.FSMSnapshot{CurrentStage: "c", Status: "running"},
		Consumed:          checkpoint.Consumed{Tokens: 4096, Cost: 0.12},
		EventSeqWatermark: 3,
		CreatedAt:         time.Now().UTC(),
	}
	cp.Seal()
	if err := store.Save(ctx, cp); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	// kill -9 之后是新进程：新的 Restorer、新的 Publisher。
	restored, err := checkpoint.NewRestorer(store).RestoreLatest(ctx, "chaos-task", checkpoint.TaskSnapshot{Stage: "c", AttemptNo: 1}, checkpoint.RestoreOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Checkpoint.Consumed.Tokens != 4096 || restored.Checkpoint.Consumed.Cost != 0.12 {
		t.Fatalf("consumed accounting must be restored verbatim: %+v", restored.Checkpoint.Consumed)
	}

	// 二次恢复必须幂等：不能把同一段消耗再加一遍。
	again, err := checkpoint.NewRestorer(store).Restore(ctx, "chaos-task", "cp-chaos-1", checkpoint.TaskSnapshot{Stage: "c", AttemptNo: 1}, checkpoint.RestoreOptions{})
	if err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if again.Checkpoint.Consumed.Tokens != 4096 {
		t.Fatalf("recovery must not double-charge, tokens=%d", again.Checkpoint.Consumed.Tokens)
	}

	// 事件续号：新 Publisher 先按水位恢复，再发的事件序号必须接上且不重复。
	sink := &seqSink{}
	pub := eventPublisher(sink)
	for i := 0; i < 3; i++ {
		emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{
			TenantID: "chaos-tenant", ProjectID: "chaos-project", TaskID: "chaos-task", ParentTaskID: "chaos-task",
		})
		if err := emitter.Emit(ctx, orchestrator.Event{Type: orchestrator.EventSubtaskStarted, ParentTaskID: "chaos-task", Payload: map[string]any{"n": i}}); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	restarted := eventPublisher(sink)
	restarted.RestoreSeq("chaos-task", restored.Checkpoint.EventSeqWatermark)
	emitter := orchestrator.NewPublisherEmitter(restarted, orchestrator.EmitContext{
		TenantID: "chaos-tenant", ProjectID: "chaos-project", TaskID: "chaos-task", ParentTaskID: "chaos-task",
	})
	if err := emitter.Emit(ctx, orchestrator.Event{Type: orchestrator.EventSubtaskAccepted, ParentTaskID: "chaos-task", Payload: map[string]any{"n": 99}}); err != nil {
		t.Fatalf("emit after restart: %v", err)
	}
	seqs := sink.seqFor("chaos-task")
	if len(seqs) != 4 {
		t.Fatalf("expected 4 events, got %d", len(seqs))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("event seq must stay continuous after kill -9: %v", seqs)
		}
	}
	if seqs[3] != 4 {
		t.Fatalf("restored watermark must continue at 4, got %d", seqs[3])
	}
}

// TestChaosPGDownNoSilentPass 覆盖 design 9.8「PG 停机」的底线：
// 客户端必须报错，任何"返回 nil 假装成功"的实现都会在这里被抓住。
func TestChaosPGDownNoSilentPass(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig("postgres://agent:agent_dev_password@127.0.0.1:1/agent_platform?sslmode=disable&connect_timeout=1"))
	if err == nil {
		defer db.Close()
		err = db.Ping(ctx)
	}
	if err == nil {
		t.Fatalf("an unreachable PostgreSQL must surface an error, not a silent success")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refused") &&
		!strings.Contains(strings.ToLower(err.Error()), "connect") &&
		!strings.Contains(strings.ToLower(err.Error()), "timeout") {
		t.Logf("pg error class: %v", err)
	}
}

func sha256Of(t *testing.T, path string) string {
	t.Helper()
	hash, err := checkpoint.DefaultHashFile(path)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hash
}
