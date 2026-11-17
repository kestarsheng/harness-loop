package orchestrator

// T-P2-16 · 租户池准入层流控回归（design 4.13.3 / 4.13.8）。
//
// 覆盖验收条件：
//   - 同租户存在 15 分钟级慢任务时，新父任务在准入层被拦下，返回
//     E_SCHEDULER_LIMIT_EXCEEDED + details.reason=tenant_slots_full；
//   - 慢任务完成后新任务自动放行（Admit 重投与 Drain 两条路径）；
//   - max_subtask_minutes 作为衰减窗口上界生效，窗口不再无限；
//   - 衰减记账放行之后还有一次 gate 二次确认：慢任务仍真实占满闸门时必须拒绝
//     （能估算时带 retry_after_ms；估不出就缺省，缺省 ≠ 立刻可重试），
//     不许"放行后换个地方排队"；
//   - 准入拒绝/排队留下结构化记录；
//   - 调度层"超额在跑、靠自然完成衰减"的兜底保留，且依然不抢占。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testClock 是可推进的假时钟：衰减窗口按 slot hold 的年龄算，所以占用必须
// 在时钟注入之后产生，否则"窗口内/窗口外"根本测不准。
type testClock struct{ at time.Time }

func (c *testClock) Now() time.Time          { return c.at }
func (c *testClock) Advance(d time.Duration) { c.at = c.at.Add(d) }

// admissionFixture 造一个「老父任务吃满租户池」的场景，返回已占满的池子。
func admissionFixture(t *testing.T, tenantSlots, perParent int, clock *testClock) (QuotaConfig, *TenantFairness, *Scheduler, *MemoryEmitter) {
	t.Helper()
	cfg := DefaultQuotaConfig()
	cfg.MaxAgentsPerTenant = tenantSlots
	cfg.MaxParallelPerParent = perParent
	emitter := NewMemoryEmitter()
	fair := NewTenantFairness(FairnessConfigFromQuota(cfg), nil)
	if clock != nil {
		fair.SetClock(clock.Now)
	}
	old := fairnessParent(t, cfg, emitter, "parent-old", perParent)
	if clock != nil {
		old.SetClock(clock.Now)
	}
	if err := old.AttachFairness(fair, 0); err != nil {
		t.Fatalf("attach fairness: %v", err)
	}
	started, err := old.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch old parent: %v", err)
	}
	if len(started) != perParent {
		t.Fatalf("slow parent should hold %d slots, started %d", perParent, len(started))
	}
	return cfg, fair, old, emitter
}

// TestAdmissionBlocksNewParentWhenTenantPoolFull 是核心验收：池子被慢任务占满时，
// 新父任务在准入层被拦，错误码复用、拒因结构化，且在跑的子任务一根汗毛都不动。
func TestAdmissionBlocksNewParentWhenTenantPoolFull(t *testing.T) {
	ctx := context.Background()
	cfg, fair, old, _ := admissionFixture(t, 20, 20, nil)
	sink := NewMemoryAdmissionSink()
	admission := NewAdmission(fair, AdmissionConfigFromQuota(cfg)).WithSink(sink)

	decision, err := admission.Admit(ctx, "parent-new")
	if err == nil {
		t.Fatalf("a full tenant pool must not admit a new parent")
	}
	if got := ErrorCode(err); got != CodeSchedulerLimitExceeded {
		t.Fatalf("error code = %q, want %q (no new code is allowed)", got, CodeSchedulerLimitExceeded)
	}
	if got := AdmissionReason(err); got != AllocDeniedTenantFull {
		t.Fatalf("reason = %q, want %q", got, AllocDeniedTenantFull)
	}
	details := AdmissionDetails(err)
	if details["reason"] != AllocDeniedTenantFull || details["queued"] != true {
		t.Fatalf("details must carry the structured reason, got %v", details)
	}
	if details["tenant_slots"] != 20 || details["slots_used"] != 20 {
		t.Fatalf("details pool numbers = %v/%v, want 20/20", details["slots_used"], details["tenant_slots"])
	}
	if retry, ok := details["retry_after_ms"].(int64); !ok || retry <= 0 {
		t.Fatalf("a queued parent needs a predictable retry_after_ms, got %v", details["retry_after_ms"])
	}
	if !decision.Queued || decision.Admitted || decision.Position != 1 {
		t.Fatalf("decision = %+v, want queued at position 1", decision)
	}
	if queued := admission.Queued(); len(queued) != 1 || queued[0] != "parent-new" {
		t.Fatalf("queue = %v, want [parent-new]", queued)
	}

	records := sink.Find(AdmissionQueued)
	if len(records) != 1 {
		t.Fatalf("admission queue must leave one observable record, got %d", len(records))
	}
	if records[0].Reason != AllocDeniedTenantFull || records[0].SlotsMax != 20 || records[0].RetryAfterMs <= 0 {
		t.Fatalf("record = %+v, want a structured tenant_slots_full record", records[0])
	}
	// REQ-128：准入拒绝不允许影响已在跑的子任务。
	if running := len(old.Running()); running != 20 {
		t.Fatalf("running subtasks = %d, want 20 untouched", running)
	}
	if used, max := fair.TenantActive(); used != 20 || max != 20 {
		t.Fatalf("tenant usage = %d/%d, want 20/20", used, max)
	}
}

// TestAdmissionPassesNewParentAfterSlowTaskCompletes 验证「慢任务完成后自动放行」：
// 既包括调用方重投 Admit，也包括 Drain 主动放行排队中的父任务。
func TestAdmissionPassesNewParentAfterSlowTaskCompletes(t *testing.T) {
	ctx := context.Background()
	cfg, fair, old, _ := admissionFixture(t, 20, 20, nil)
	sink := NewMemoryAdmissionSink()
	admission := NewAdmission(fair, AdmissionConfigFromQuota(cfg)).WithSink(sink)

	if _, err := admission.Admit(ctx, "parent-new"); err == nil {
		t.Fatalf("precondition: new parent must be blocked while the pool is full")
	}
	if err := old.MarkAccepted(ctx, "parent-old-st-00", "acc-1"); err != nil {
		t.Fatalf("accept slow subtask: %v", err)
	}

	released := admission.Drain(ctx)
	if len(released) != 1 || released[0] != "parent-new" {
		t.Fatalf("drain = %v, want [parent-new] once a slot frees up", released)
	}
	decision, err := admission.Admit(ctx, "parent-new")
	if err != nil {
		t.Fatalf("an admitted parent must stay admitted: %v", err)
	}
	if !decision.Admitted || decision.SlotsUsed != 19 {
		t.Fatalf("decision = %+v, want admitted with 19/20 used", decision)
	}
	if stats := admission.Stats(); stats.Queued != 1 || stats.Admitted != 1 {
		t.Fatalf("stats = %+v, want 1 queued + 1 admitted", stats)
	}
	if len(sink.Find(AdmissionAdmitted)) != 1 {
		t.Fatalf("admission must record the release, got %v", sink.Records())
	}
	if queued := admission.Queued(); len(queued) != 0 {
		t.Fatalf("queue must be empty after drain, got %v", queued)
	}
}

// TestAdmissionDecayWindowStillNeedsGateRoom 验证 max_subtask_minutes 的两层语义：
// 窗口内继续拦；越过窗口后占用在"记账"上衰减，但慢任务只要还真实占着 gate，
// 准入就必须继续拒绝——二次确认把"衰减"限定为记账口径，而不是凭空造出 slot。
func TestAdmissionDecayWindowStillNeedsGateRoom(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{at: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	cfg, fair, old, _ := admissionFixture(t, 20, 20, clock)
	admission := NewAdmission(fair, AdmissionConfigFromQuota(cfg))
	admission.SetClock(clock.Now)

	if got := admission.Config().MaxSubtaskMinutes; got != DefaultMaxSubtaskMinutes {
		t.Fatalf("max_subtask_minutes default = %d, want %d", got, DefaultMaxSubtaskMinutes)
	}

	clock.Advance(13 * time.Minute)
	if _, err := admission.Admit(ctx, "parent-new"); AdmissionReason(err) != AllocDeniedTenantFull {
		t.Fatalf("inside the decay window the parent must stay blocked, got %v", err)
	}
	if used, decayed, max := fair.Occupancy(admission.Config().window()); used != 20 || decayed != 0 || max != 20 {
		t.Fatalf("occupancy = %d used / %d decayed / %d max, want 20/0/20", used, decayed, max)
	}
	if retry := fair.RetryAfter(admission.Config().window()); retry != 2*time.Minute {
		t.Fatalf("retry after = %v, want 2m (window 15m - oldest hold 13m)", retry)
	}

	clock.Advance(3 * time.Minute) // 累计 16 分钟 > 窗口 15 分钟
	if used, decayed, _ := fair.Occupancy(admission.Config().window()); used != 20 || decayed != 20 {
		t.Fatalf("occupancy after the window = %d/%d, want all 20 decayed", used, decayed)
	}

	// 衰减记账已放行，但 20 个慢任务仍真实占着 gate（used 依然是 20）：
	// 二次确认必须拦住，"衰减"不能变成超额接单。
	decision, err := admission.Admit(ctx, "parent-new")
	if AdmissionReason(err) != AllocDeniedTenantFull {
		t.Fatalf("the decayed fast path must be re-confirmed against the gate, got %v", err)
	}
	if decision.Admitted {
		t.Fatalf("decision = %+v, want a rejection while the gate is still full", decision)
	}
	details := AdmissionDetails(err)
	if details["gate_available"] != 0 {
		t.Fatalf("details must expose the gate headroom, got %v", details["gate_available"])
	}
	if details["slots_decayed"] != 20 || details["slots_used"] != 20 {
		t.Fatalf("details = %v, want the decayed accounting reported as-is", details)
	}

	// 慢任务真的完成一个，gate 归还 slot，准入才放行——这才是"完成即放行"。
	if err := old.MarkAccepted(ctx, "parent-old-st-00", "acc-1"); err != nil {
		t.Fatalf("accept slow subtask: %v", err)
	}
	decision, err = admission.Admit(ctx, "parent-new")
	if err != nil {
		t.Fatalf("a freed tenant slot must admit the waiting parent: %v", err)
	}
	if !decision.Admitted || decision.SlotsUsed != 19 || decision.SlotsDecayed != 19 {
		t.Fatalf("decision = %+v, want admitted with 19 decayed slots out of 19 used", decision)
	}
	if stats := admission.Stats(); stats.AdmittedByDecay != 1 || stats.DeniedByGate != 1 {
		t.Fatalf("stats = %+v, want 1 gate denial + 1 decayed admission", stats)
	}
}

// TestAdmissionGateConfirmationRejectsDecayedAdmit 是准入层二次确认的核心验收：
// 慢任务占满租户池并越过衰减窗口后，衰减记账会说"有空位"，但同一个 gate 的真实
// 可用量仍是 0——此时必须拒绝（同一错误码 + reason=tenant_slots_full +
// retry_after_ms），而不是把父任务放进来排队。慢任务真的完成一个，gate 归还 slot，
// 准入才放行。
func TestAdmissionGateConfirmationRejectsDecayedAdmit(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{at: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	cfg, fair, old, _ := admissionFixture(t, 4, 4, clock)
	sink := NewMemoryAdmissionSink()
	policy := AdmissionConfigFromQuota(cfg)
	policy.QueueWhenFull = false // 生产桥语义：池满就拒，不排队
	admission := NewAdmission(fair, policy).WithSink(sink)
	admission.SetClock(clock.Now)

	clock.Advance(16 * time.Minute)
	if used, decayed, max := fair.Occupancy(admission.Config().window()); used != 4 || decayed != 4 || max != 4 {
		t.Fatalf("occupancy = %d used / %d decayed / %d max, want 4/4/4", used, decayed, max)
	}

	decision, err := admission.Admit(ctx, "parent-new")
	if err == nil {
		t.Fatalf("a decayed accounting must not admit on top of a full gate")
	}
	if got := ErrorCode(err); got != CodeSchedulerLimitExceeded {
		t.Fatalf("error code = %q, want %q", got, CodeSchedulerLimitExceeded)
	}
	if got := AdmissionReason(err); got != AllocDeniedTenantFull {
		t.Fatalf("reason = %q, want %q", got, AllocDeniedTenantFull)
	}
	details := AdmissionDetails(err)
	if details["reason"] != AllocDeniedTenantFull || details["queued"] != false {
		t.Fatalf("details must carry the structured reason, got %v", details)
	}
	if details["gate_available"] != 0 {
		t.Fatalf("detail gate_available = %v, want 0 (the second confirmation failed)", details["gate_available"])
	}
	if details["slots_used"] != 4 || details["tenant_slots"] != 4 || details["slots_decayed"] != 4 {
		t.Fatalf("detail pool numbers = %v/%v/%v, want 4/4/4",
			details["slots_used"], details["tenant_slots"], details["slots_decayed"])
	}
	if _, ok := details["retry_after_ms"].(int64); ok {
		// 这条路径上所有 hold 都已越过衰减窗口，准入层其实估不出时间，于是字段
		// 整个缺省——缺省 ≠ 立刻可重试，调用方走指数退避（见 AdmissionError 契约）。
		t.Fatalf("no in-window hold means no estimate: retry_after_ms must be omitted, got %v", details["retry_after_ms"])
	}
	if decision.Admitted || decision.Queued || decision.Position != 0 {
		t.Fatalf("decision = %+v, want an outright rejection", decision)
	}
	if stats := admission.Stats(); stats.DeniedByGate != 1 || stats.AdmittedByDecay != 0 {
		t.Fatalf("stats = %+v, want 1 gate denial and 0 decayed admissions", stats)
	}
	if records := sink.Find(AdmissionRejected); len(records) != 1 || records[0].Reason != AllocDeniedTenantFull {
		t.Fatalf("the gate rejection must be observable, got %v", sink.Records())
	}
	// REQ-128：二次确认只改准入记账，绝不动已在跑的子任务。
	if running := len(old.Running()); running != 4 {
		t.Fatalf("running subtasks = %d, want 4 untouched", running)
	}

	// 慢任务真的完成一个：gate 归还 1 个真实 slot，准入这时才该放行。
	if err := old.MarkAccepted(ctx, "parent-old-st-00", "acc-1"); err != nil {
		t.Fatalf("accept slow subtask: %v", err)
	}
	decision, err = admission.Admit(ctx, "parent-new")
	if err != nil {
		t.Fatalf("a freed tenant slot must admit the waiting parent: %v", err)
	}
	if !decision.Admitted || decision.SlotsUsed != 3 || decision.SlotsDecayed != 3 {
		t.Fatalf("decision = %+v, want admitted with 3/4 slots used (all decayed)", decision)
	}
	if stats := admission.Stats(); stats.AdmittedByDecay != 1 || stats.DeniedByGate != 1 {
		t.Fatalf("stats = %+v, want 1 gate denial then 1 decayed admission", stats)
	}
}

// TestAdmissionRejectsWhenQueueDisabled 覆盖配置了"拒绝"而不是"排队"的租户策略：
// 同一条错误码，不同的可预期行为。
func TestAdmissionRejectsWhenQueueDisabled(t *testing.T) {
	ctx := context.Background()
	cfg, fair, _, _ := admissionFixture(t, 4, 4, nil)
	sink := NewMemoryAdmissionSink()
	policy := AdmissionConfigFromQuota(cfg)
	policy.QueueWhenFull = false
	admission := NewAdmission(fair, policy).WithSink(sink)

	decision, err := admission.Admit(ctx, "parent-new")
	if AdmissionReason(err) != AllocDeniedTenantFull {
		t.Fatalf("rejection reason = %v, want %q", err, AllocDeniedTenantFull)
	}
	if decision.Queued || decision.Admitted || decision.Position != 0 {
		t.Fatalf("decision = %+v, want an outright rejection", decision)
	}
	if queued := admission.Queued(); len(queued) != 0 {
		t.Fatalf("a rejected parent must not sit in the queue, got %v", queued)
	}
	if stats := admission.Stats(); stats.Rejected != 1 || stats.Queued != 0 {
		t.Fatalf("stats = %+v, want 1 rejected", stats)
	}
	if records := sink.Find(AdmissionRejected); len(records) != 1 || records[0].Reason != AllocDeniedTenantFull {
		t.Fatalf("rejection must be observable, got %v", sink.Records())
	}
}

// TestAdmissionConfigFromPolicyFile 确认 scheduler.yaml 真的被解析（键名错一个
// 就该报错，而不是悄悄回落默认值），并且 max_subtask_minutes 与父任务超时解耦。
func TestAdmissionConfigFromPolicyFile(t *testing.T) {
	cfg, err := LoadQuotaConfig(filepath.Join("..", "..", "config", "harness_loop", "scheduler.yaml"))
	if err != nil {
		t.Fatalf("load scheduler.yaml: %v", err)
	}
	if cfg.MaxSubtaskMinutes != DefaultMaxSubtaskMinutes {
		t.Fatalf("max_subtask_minutes = %d, want %d", cfg.MaxSubtaskMinutes, DefaultMaxSubtaskMinutes)
	}
	if cfg.MaxAgentsPerTenant != 20 || cfg.MaxParallelPerParent != 8 || cfg.TimeoutMinutes != 15 {
		t.Fatalf("policy snapshot drifts from the code defaults: %+v", cfg)
	}

	dir := t.TempDir()
	override := filepath.Join(dir, "quota.yaml")
	body := "version: 1\nsubtask:\n  timeout_minutes: 30\n  max_subtask_minutes: 3\n"
	if err := os.WriteFile(override, []byte(body), 0o600); err != nil {
		t.Fatalf("write override: %v", err)
	}
	got, err := LoadQuotaConfig(override)
	if err != nil {
		t.Fatalf("load override: %v", err)
	}
	if got.MaxSubtaskMinutes != 3 {
		t.Fatalf("explicit max_subtask_minutes = %d, want 3 (decoupled from timeout 30)", got.MaxSubtaskMinutes)
	}
	if got.MaxAgentsPerTenant != 20 {
		t.Fatalf("unspecified keys must fall back to defaults, got %d slots", got.MaxAgentsPerTenant)
	}

	typo := filepath.Join(dir, "typo.yaml")
	if err := os.WriteFile(typo, []byte("version: 1\nsubtask:\n  max_subtask_minuts: 3\n"), 0o600); err != nil {
		t.Fatalf("write typo: %v", err)
	}
	if _, err := LoadQuotaConfig(typo); err == nil {
		t.Fatalf("a misspelled policy key must fail loudly, not fall back to defaults")
	}

	// 默认值语义：min(阶段超时, 15min)。
	short := DefaultQuotaConfig()
	short.TimeoutMinutes = 10
	if got := AdmissionConfigFromQuota(short).MaxSubtaskMinutes; got != 10 {
		t.Fatalf("max_subtask_minutes with a 10m stage timeout = %d, want 10", got)
	}
	long := DefaultQuotaConfig()
	long.TimeoutMinutes = 30
	if got := AdmissionConfigFromQuota(long).MaxSubtaskMinutes; got != DefaultMaxSubtaskMinutes {
		t.Fatalf("max_subtask_minutes with a 30m stage timeout = %d, want %d", got, DefaultMaxSubtaskMinutes)
	}
}

// TestAdmissionKeepsSchedulerOverflowDecay 是 T-P2-16 point 5 的兜底回归：
// 准入放行之后，调度层仍然按租户池排队（超额在跑靠自然完成衰减），不抢占、不失败。
// 二次确认把"准入真的放了"与"闸门真的有空位"绑在一起，所以这里用"慢任务完成
// 一个"制造唯一一个空位：准入放行，调度层只肯给这一个，剩下的继续排队。
func TestAdmissionKeepsSchedulerOverflowDecay(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultQuotaConfig()
	cfg.MaxAgentsPerTenant = 4
	cfg.MaxParallelPerParent = 4
	clock := &testClock{at: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	fair := NewTenantFairness(FairnessConfigFromQuota(cfg), nil)
	fair.SetClock(clock.Now)
	admission := NewAdmission(fair, AdmissionConfigFromQuota(cfg))
	admission.SetClock(clock.Now)
	dispatcher := NewTenantDispatcher(fair).WithAdmission(admission)
	emitter := NewMemoryEmitter()

	old := fairnessParent(t, cfg, emitter, "parent-a", 6)
	old.SetClock(clock.Now)
	if err := dispatcher.Attach(old, 0); err != nil {
		t.Fatalf("attach parent-a: %v", err)
	}
	if started, err := dispatcher.Tick(ctx); err != nil || len(started) != 4 {
		t.Fatalf("first tick started %d (%v), want the 4-slot tenant pool full", len(started), err)
	}

	late := fairnessParent(t, cfg, emitter, "parent-b", 4)
	err := dispatcher.Attach(late, 0)
	if AdmissionReason(err) != AllocDeniedTenantFull {
		t.Fatalf("attach of parent-b = %v, want E_SCHEDULER_LIMIT_EXCEEDED/tenant_slots_full", err)
	}
	if got := dispatcher.Parents(); len(got) != 1 || got[0] != "parent-a" {
		t.Fatalf("a rejected parent must not join the dispatch queue, got %v", got)
	}

	// 一个慢任务完成 → 闸门空出 1 个：准入放行，调度层只肯给这 1 个，其余排队。
	if err := old.MarkAccepted(ctx, "parent-a-st-00", "acc-1"); err != nil {
		t.Fatalf("accept a slow subtask: %v", err)
	}
	if err := dispatcher.Attach(late, 0); err != nil {
		t.Fatalf("attach parent-b once a real slot frees up: %v", err)
	}
	started, err := dispatcher.Tick(ctx)
	if err != nil {
		t.Fatalf("tick after attach: %v", err)
	}
	if len(started) != 1 {
		t.Fatalf("tick started %d subtasks on a pool with one free slot, want 1", len(started))
	}
	if cancelled := len(emitter.Find(EventSubtaskCancelled)); cancelled != 0 {
		t.Fatalf("no subtask may be cancelled to make room (REQ-128), got %d", cancelled)
	}
	if len(late.Running()) != 1 || late.PendingCount() != 3 {
		t.Fatalf("parent-b should be queued at the scheduler layer, running=%d pending=%d",
			len(late.Running()), late.PendingCount())
	}
	queued := emitter.Find(EventSubtaskQueued)
	if len(queued) == 0 {
		t.Fatalf("scheduler-layer queueing must stay observable")
	}
	last := queued[len(queued)-1]
	if last.Payload["reason"] != AllocDeniedTenantFull {
		t.Fatalf("scheduler queued reason = %v, want %q", last.Payload["reason"], AllocDeniedTenantFull)
	}
	if used, max := fair.TenantActive(); used != 4 || max != 4 {
		t.Fatalf("tenant usage = %d/%d, want 4/4", used, max)
	}
}

// TestAdmissionRetryAfterContract 守住 retry_after_ms 的两种状态：
//   - 能估算（还有 hold 在衰减窗口内）→ 给正数，且与 decision.RetryAfter 一致；
//   - 估不出来（没有可观测的 hold，或衰减窗口为 0）→ 字段整个缺省。
//
// 第二种场合以前发 int64(0)，读起来就是"立刻可以重试"，调用方一顿重试就把拒绝变成
// 热循环。缺省 ≠ 立刻可重试：调用方必须回落到指数退避。
func TestAdmissionRetryAfterContract(t *testing.T) {
	t.Run("estimated", func(t *testing.T) {
		ctx := context.Background()
		clock := &testClock{at: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
		cfg, fair, _, _ := admissionFixture(t, 4, 4, clock)
		policy := AdmissionConfigFromQuota(cfg)
		policy.QueueWhenFull = false
		admission := NewAdmission(fair, policy).WithSink(NewMemoryAdmissionSink())
		admission.SetClock(clock.Now)

		clock.Advance(40 * time.Second) // hold 还在窗口内 → 估得出剩余时间
		decision, err := admission.Admit(ctx, "parent-new")
		if AdmissionReason(err) != AllocDeniedTenantFull {
			t.Fatalf("precondition: pool must be full, got %v", err)
		}
		ms, ok := AdmissionDetails(err)["retry_after_ms"].(int64)
		if !ok || ms <= 0 {
			t.Fatalf("an in-window hold yields a positive estimate, got %v", AdmissionDetails(err)["retry_after_ms"])
		}
		if decision.RetryAfter <= 0 || ms != decision.RetryAfter.Milliseconds() {
			t.Fatalf("details and decision must agree: ms=%d decision=%s", ms, decision.RetryAfter)
		}
		window := time.Duration(admission.Config().MaxSubtaskMinutes) * time.Minute
		if decision.RetryAfter > window {
			t.Fatalf("estimate %s must not exceed the decay window %s", decision.RetryAfter, window)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		ctx := context.Background()
		clock := &testClock{at: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
		cfg, fair, _, _ := admissionFixture(t, 4, 4, clock)
		sink := NewMemoryAdmissionSink()
		policy := AdmissionConfigFromQuota(cfg)
		policy.QueueWhenFull = false
		admission := NewAdmission(fair, policy).WithSink(sink)
		admission.SetClock(clock.Now)

		clock.Advance(16 * time.Minute) // 全部越过窗口：没有任何可估算的 hold
		decision, err := admission.Admit(ctx, "parent-new")
		if AdmissionReason(err) != AllocDeniedTenantFull {
			t.Fatalf("precondition: pool must be full, got %v", err)
		}
		details := AdmissionDetails(err)
		if value, ok := details["retry_after_ms"]; ok {
			t.Fatalf("an unestimable retry must omit the field, got %v", value)
		}
		if _, ok := details["reason"]; !ok {
			t.Fatalf("omitting retry_after_ms must not strip the reason: %v", details)
		}
		if decision.RetryAfter != 0 {
			t.Fatalf("decision.RetryAfter = %s, want 0 (unknown)", decision.RetryAfter)
		}

		records := sink.Find(AdmissionRejected)
		if len(records) != 1 || records[0].RetryAfterMs != 0 {
			t.Fatalf("record = %+v, want RetryAfterMs=0 (unknown)", records)
		}
		raw, marshalErr := json.Marshal(records[0])
		if marshalErr != nil {
			t.Fatalf("marshal record: %v", marshalErr)
		}
		if strings.Contains(string(raw), "retry_after_ms") {
			t.Fatalf("an unknown retry must not appear in the record JSON: %s", raw)
		}
	})
}
