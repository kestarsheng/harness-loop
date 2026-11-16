package orchestrator

// T-P2-09 · design 4.13.8 公平调度（WFQ）与熔断调优回归。
//
// 覆盖验收条件：
//   - 同租户 3 父任务并发时各配额不超限、无单任务独占；
//   - 权重可配且按权重分配；
//   - 单任务 slot 硬上限优先于 WFQ，配额耗尽只排队不抢占；
//   - 熔断参数可调、恢复必须人工复位且留审计；
//   - hlp_scheduler_alloc 视图（内存行 + DDL）可用。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agent-platform/pkg/harnessloop/subtask"
)

func fairnessBatch(t *testing.T, parent string, count int) []*subtask.Subtask {
	t.Helper()
	batch := make([]*subtask.Subtask, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s-st-%02d", parent, i)
		batch = append(batch, testSubtask(parent, "dag-"+parent, id, i, 1, "p1", "coder", "src/"+parent+"/*.go"))
	}
	return batch
}

func fairnessParent(t *testing.T, cfg QuotaConfig, emitter Emitter, parent string, count int) *Scheduler {
	t.Helper()
	s := NewScheduler(cfg, emitter)
	if err := s.Enqueue(fairnessBatch(t, parent, count)); err != nil {
		t.Fatalf("enqueue %s: %v", parent, err)
	}
	return s
}

// TestFairnessThreeParentsShareTenantPool 是同租户 3 父任务的核心验收：
// 20 个租户 slot 必须分给三个父任务，任何一方都不得独占。
func TestFairnessThreeParentsShareTenantPool(t *testing.T) {
	cfg := DefaultQuotaConfig()
	emitter := NewMemoryEmitter()
	fair := NewTenantFairness(FairnessConfigFromQuota(cfg), nil)
	dispatcher := NewTenantDispatcher(fair)

	parents := []string{"parent-a", "parent-b", "parent-c"}
	for _, parent := range parents {
		s := fairnessParent(t, cfg, emitter, parent, 12)
		if err := dispatcher.Attach(s, 0); err != nil {
			t.Fatalf("attach %s: %v", parent, err)
		}
	}
	started, err := dispatcher.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(started) != 20 {
		t.Fatalf("tenant pool of 20 should be fully used, started %d", len(started))
	}

	used, max := fair.TenantActive()
	if used != 20 || max != 20 {
		t.Fatalf("tenant usage = %d/%d, want 20/20", used, max)
	}
	rows := fair.AllocRows()
	if len(rows) != 3 {
		t.Fatalf("alloc view must list every parent, got %d rows", len(rows))
	}
	for _, row := range rows {
		// ceil(20 * 1/3) = 7 是硬上限；16 个 slot 的"单任务独占"必须不可能。
		if row.SlotsUsed > row.SlotsCap {
			t.Fatalf("%s uses %d slots above its cap %d", row.ParentTaskID, row.SlotsUsed, row.SlotsCap)
		}
		if row.SlotsUsed > 7 {
			t.Fatalf("%s grabbed %d slots: a single parent must never dominate the tenant pool", row.ParentTaskID, row.SlotsUsed)
		}
		if row.SlotsUsed < 6 {
			t.Fatalf("%s only got %d slots: WFQ starved one parent", row.ParentTaskID, row.SlotsUsed)
		}
		if row.QueueDepth == 0 {
			t.Fatalf("%s should still have queued subtasks, got depth 0", row.ParentTaskID)
		}
	}
	if len(emitter.Find(EventSubtaskQueued)) == 0 {
		t.Fatalf("queued work must emit subtask.queued")
	}
}

// TestFairnessWeightedRoundRobin 验证权重生效：权重 2 的父任务应拿到约两倍 slot。
func TestFairnessWeightedRoundRobin(t *testing.T) {
	cfg := DefaultQuotaConfig()
	cfg.MaxAgentsPerTenant = 100
	fair := NewTenantFairness(FairnessConfigFromQuota(cfg), nil)
	for _, reg := range []struct {
		id     string
		weight float64
	}{{"parent-a", 1}, {"parent-b", 2}, {"parent-c", 1}} {
		if err := fair.Register(reg.id, "tenant-1", reg.weight); err != nil {
			t.Fatalf("register %s: %v", reg.id, err)
		}
	}
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		pick, ok := fair.Pick([]FairCandidate{
			{ParentTaskID: "parent-a", SubtaskID: "a", IndexInParent: i},
			{ParentTaskID: "parent-b", SubtaskID: "b", IndexInParent: i},
			{ParentTaskID: "parent-c", SubtaskID: "c", IndexInParent: i},
		})
		if !ok {
			t.Fatalf("pick %d returned no candidate", i)
		}
		// Pick 只做决策，virtual_time 由 Acquire 记账（设计与实现一致）。
		if ok, reason := fair.Acquire(pick.ParentTaskID, 0); !ok {
			t.Fatalf("acquire %s: %s", pick.ParentTaskID, reason)
		}
		counts[pick.ParentTaskID]++
	}
	if counts["parent-b"] != 4 || counts["parent-a"] != 2 || counts["parent-c"] != 2 {
		t.Fatalf("unexpected weighted allocation: %+v", counts)
	}
}

// TestFairnessHardCapBeatsWFQ 验证 parent_share 硬上限优先于 WFQ 选择。
func TestFairnessHardCapBeatsWFQ(t *testing.T) {
	cfg := DefaultQuotaConfig()
	cfg.Fairness.ParentShare = 0.25
	emitter := NewMemoryEmitter()
	fair := NewTenantFairness(FairnessConfigFromQuota(cfg), nil)
	s := fairnessParent(t, cfg, emitter, "solo", 12)
	if err := s.AttachFairness(fair, 0); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := fair.SlotsCap("solo"); got != 5 {
		t.Fatalf("slots cap = %d, want ceil(20*0.25)=5", got)
	}
	started, err := s.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(started) != 5 {
		t.Fatalf("hard cap must stop at 5 slots, started %d", len(started))
	}
	if pending := s.PendingCount(); pending != 7 {
		t.Fatalf("pending = %d, want 7", pending)
	}
	for _, ev := range emitter.Find(EventSubtaskQueued) {
		if ev.Payload["reason"] != AllocDeniedParentSlots {
			t.Fatalf("queued event should carry reason=%s, got %+v", AllocDeniedParentSlots, ev.Payload)
		}
	}
	used, _ := fair.TenantActive()
	if used != 5 {
		t.Fatalf("tenant usage = %d, want 5 (the other 15 slots must stay free for other parents)", used)
	}
}

// TestFairnessQueuesInsteadOfPreempting 验证"配额打满只排队、不抢占"（REQ-128）。
func TestFairnessQueuesInsteadOfPreempting(t *testing.T) {
	cfg := DefaultQuotaConfig()
	cfg.MaxAgentsPerTenant = 4
	emitter := NewMemoryEmitter()
	fair := NewTenantFairness(FairnessConfigFromQuota(cfg), nil)
	dispatcher := NewTenantDispatcher(fair)

	first := fairnessParent(t, cfg, emitter, "parent-a", 6)
	second := fairnessParent(t, cfg, emitter, "parent-b", 6)
	for _, s := range []*Scheduler{first, second} {
		if err := dispatcher.Attach(s, 0); err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	running := len(first.Running()) + len(second.Running())
	if running != 4 {
		t.Fatalf("tenant cap 4 must be respected, running=%d", running)
	}
	if len(emitter.Find(EventSubtaskCancelled)) != 0 {
		t.Fatalf("queueing must never cancel a running subtask (no preemption)")
	}
	queued := emitter.Find(EventSubtaskQueued)
	if len(queued) == 0 {
		t.Fatalf("expected queued events for the blocked work")
	}
	for _, ev := range queued {
		reason, _ := ev.Payload["reason"].(string)
		if reason == "" {
			t.Fatalf("queued event must carry a reason: %+v", ev.Payload)
		}
	}
	// 释放一个 slot 后，排队任务必须能进入 running。
	if err := first.MarkAccepted(context.Background(), first.Running()[0].SubtaskID, "acc-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if total := len(first.Running()) + len(second.Running()); total != 4 {
		t.Fatalf("released slot should be refilled, running=%d", total)
	}
}

// TestFairnessAllocView 验证 hlp_scheduler_alloc 视图的列与 DDL 可用。
func TestFairnessAllocView(t *testing.T) {
	cfg := DefaultQuotaConfig()
	fair := NewTenantFairness(FairnessConfigFromQuota(cfg), nil)
	s := fairnessParent(t, cfg, NewMemoryEmitter(), "parent-a", 3)
	if err := s.AttachFairness(fair, 1.5); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := s.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	rows := fair.AllocRows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 alloc row, got %d", len(rows))
	}
	row := rows[0]
	if row.TenantID != "tenant-1" || row.ParentTaskID != "parent-a" {
		t.Fatalf("unexpected row identity: %+v", row)
	}
	if row.SlotsUsed != 3 || row.Weight != 1.5 {
		t.Fatalf("unexpected slot/weight: %+v", row)
	}
	if row.TokensUsed <= 0 || row.SlotsCap <= 0 {
		t.Fatalf("tokens/alloc caps must be reported: %+v", row)
	}
	if row.VirtualTime <= 0 || row.AllocCount != 3 {
		t.Fatalf("WFQ accounting must be visible: %+v", row)
	}
	if SchedulerAllocViewName != "hlp_scheduler_alloc" {
		t.Fatalf("view name drifted: %s", SchedulerAllocViewName)
	}
	ddl := AllocViewSQL()
	for _, needle := range []string{"hlp_scheduler_alloc", "hlp_task.subtasks", "slots_used", "tokens_alloc", "queue_depth"} {
		if !strings.Contains(ddl, needle) {
			t.Fatalf("alloc view DDL is missing %q:\n%s", needle, ddl)
		}
	}
}

// TestFairnessCircuitManualResetAudited 验证熔断恢复只有人工复位一条路，
// 且复位必须带操作人并留下审计（T-P2-09 验收条件）。
func TestFairnessCircuitManualResetAudited(t *testing.T) {
	cfg := DefaultQuotaConfig()
	cfg.MaxParallelPerParent = 4
	emitter := NewMemoryEmitter()
	audit := NewMemorySchedulerAudit()
	s := NewScheduler(cfg, emitter).WithAuditSink(audit)
	batch := fairnessBatch(t, "task-1", 5)
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ctx := context.Background()
	if _, err := s.Dispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, id := range []string{"task-1-st-00", "task-1-st-01", "task-1-st-02"} {
		if err := s.MarkFailed(ctx, id, "timeout"); err != nil {
			t.Fatalf("fail %s: %v", id, err)
		}
	}
	if err := s.MarkAccepted(ctx, "task-1-st-03", "acc-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	open := s.CircuitStates()
	if len(open) != 1 || !open[0].Open {
		t.Fatalf("expected exactly one open class, got %+v", open)
	}
	classKey := open[0].ClassKey
	if started, err := s.Dispatch(ctx); err != nil || len(started) != 0 {
		t.Fatalf("open class must not be scheduled: started=%d err=%v", len(started), err)
	}

	// 没有操作人的复位必须被拒绝：不允许"匿名偷偷放行"。
	if _, err := s.ResetCircuitClass(ctx, CircuitResetRequest{ClassKey: classKey, Reason: "looks fine now"}); err == nil {
		t.Fatalf("reset without an actor must be rejected")
	} else {
		assertCode(t, err, CodeCircuitResetDenied)
	}
	// 没有原因的复位同样被拒绝。
	if _, err := s.ResetCircuitClass(ctx, CircuitResetRequest{ClassKey: classKey, Actor: "oncall"}); err == nil {
		t.Fatalf("reset without a reason must be rejected")
	}
	if len(audit.Entries()) != 0 {
		t.Fatalf("rejected resets must not write audit records: %+v", audit.Entries())
	}

	closed, err := s.ResetCircuitClass(ctx, CircuitResetRequest{
		ClassKey: classKey,
		Actor:    "oncall-zhang",
		Reason:   "根因已修复：verifier 误配 build 命令",
	})
	if err != nil {
		t.Fatalf("manual reset: %v", err)
	}
	if !closed {
		t.Fatalf("manual reset should report the class as closed")
	}
	entries := audit.FindByAction(AuditActionCircuitReset)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit record, got %d", len(entries))
	}
	if entries[0].Actor != "oncall-zhang" || entries[0].Source != CircuitResetSourceOperator {
		t.Fatalf("audit record must keep actor and source: %+v", entries[0])
	}
	if entries[0].Detail["failures"] == nil {
		t.Fatalf("audit record must keep the sampled failures: %+v", entries[0].Detail)
	}

	states := s.CircuitStates()
	if states[0].ResetCount != 1 || states[0].LastResetBy != "oncall-zhang" || states[0].Open {
		t.Fatalf("breaker state after reset: %+v", states[0])
	}
	started, err := s.Dispatch(ctx)
	if err != nil {
		t.Fatalf("dispatch after reset: %v", err)
	}
	if len(started) != 1 || started[0].SubtaskID != "task-1-st-04" {
		t.Fatalf("reset class must be schedulable again, got %+v", started)
	}
}

// TestFairnessCircuitTunableRatio 验证熔断阈值可调（ratio）。
func TestFairnessCircuitTunableRatio(t *testing.T) {
	cfg := DefaultQuotaConfig()
	cfg.CircuitFailureRatio = 0.9
	emitter := NewMemoryEmitter()
	breaker := NewCircuitBreaker(cfg, emitter)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if opened, err := breaker.Record(ctx, "class-x", true); err != nil {
			t.Fatalf("record: %v", err)
		} else if opened {
			t.Fatalf("ratio 0.9 must not open after 3/3 samples below min_samples")
		}
	}
	if opened, err := breaker.Record(ctx, "class-x", true); err != nil || !opened {
		t.Fatalf("4/4 failures must open at ratio 0.9: opened=%v err=%v", opened, err)
	}
	if len(emitter.Find(EventSubtaskCircuitOpen)) != 1 {
		t.Fatalf("expected exactly one circuit_open event")
	}
}

// TestFairnessCircuitWindowSlides 验证滚动窗口：窗口外的旧失败不再计入。
func TestFairnessCircuitWindowSlides(t *testing.T) {
	cfg := DefaultQuotaConfig()
	cfg.CircuitMinSamples = 4
	cfg.CircuitSampleWindow = 4
	cfg.CircuitFailureRatio = 0.55
	breaker := NewCircuitBreaker(cfg, nil)
	ctx := context.Background()
	// 窗口内 2/4 = 0.5 < 0.55 → 不开；若按全量累计 3/5 = 0.6 就会误开。
	seq := []bool{true, true, false, false, true}
	for _, failed := range seq {
		if opened, err := breaker.Record(ctx, "class-y", failed); err != nil {
			t.Fatalf("record: %v", err)
		} else if opened {
			t.Fatalf("sliding window 2/4=0.5 must stay below ratio 0.55: %+v", breaker.State("class-y"))
		}
	}
	// 再补两次失败：最后 4 个样本变成 T,F,F,F → 3/4 = 0.75 ≥ 0.55 → 开。
	if opened, err := breaker.Record(ctx, "class-y", true); err != nil || opened {
		t.Fatalf("window 2/4 after the 6th sample must still stay closed: opened=%v err=%v", opened, err)
	}
	if opened, err := breaker.Record(ctx, "class-y", true); err != nil || !opened {
		t.Fatalf("window 3/4 must open: opened=%v err=%v", opened, err)
	}
}

// TestFairnessReplanResetIsAudited 验证重规划复位同样留审计（来源可区分）。
func TestFairnessReplanResetIsAudited(t *testing.T) {
	cfg := DefaultQuotaConfig()
	cfg.MaxParallelPerParent = 4
	audit := NewMemorySchedulerAudit()
	s := NewScheduler(cfg, NewMemoryEmitter()).WithAuditSink(audit)
	if err := s.Enqueue(fairnessBatch(t, "task-2", 5)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ctx := context.Background()
	if _, err := s.Dispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, id := range []string{"task-2-st-00", "task-2-st-01", "task-2-st-02"} {
		if err := s.MarkFailed(ctx, id, "timeout"); err != nil {
			t.Fatalf("fail %s: %v", id, err)
		}
	}
	if err := s.MarkAccepted(ctx, "task-2-st-03", "acc-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	reset, err := s.ResetCircuit(ctx, CircuitResetRequest{
		Reason: "replan produced dag-2",
		DagID:  "dag-2",
	})
	if err != nil {
		t.Fatalf("replan reset: %v", err)
	}
	if len(reset) != 1 {
		t.Fatalf("expected one open class reset, got %v", reset)
	}
	entries := audit.FindByAction(AuditActionCircuitReset)
	if len(entries) != 1 || entries[0].Source != CircuitResetSourceReplan {
		t.Fatalf("replan reset must be audited with source=dag_replan: %+v", entries)
	}
	if entries[0].Actor != "orchestrator" || entries[0].DagID != "dag-2" {
		t.Fatalf("replan audit must carry the orchestrator actor and the new dag: %+v", entries[0])
	}
}

// TestFairnessRejectsCrossTenantQueue 验证同一 WFQ 队列不混租户（REQ-137）。
func TestFairnessRejectsCrossTenantQueue(t *testing.T) {
	fair := NewTenantFairness(FairnessConfigFromQuota(DefaultQuotaConfig()), nil)
	if err := fair.Register("parent-a", "tenant-1", 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	assertCode(t, fair.Register("parent-b", "tenant-2", 0), CodeCrossTenantDenied)
}
