package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/retry"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/repository"
)

// T-P2-29 修正版：d 阶段收口失败必须走 d→c 回退，而不是直接 failed。
//
// 本用例把 Runtime 接在真实 TaskRepo + 真实 hlp_event.events 上，钉住三件事：
//  1. 每一次收口失败都留下一条 stage.transition(from=d,to=c) 事件行；
//  2. 回退次数等于验收预算 AcceptanceMaxFails，说明"耗尽"而不是"一碰就死"；
//  3. DB 的 task_runs 最终是 failed/failed，且 task.failed 只发了一次。
func TestRuntimeClosureFailureRollsBackToExecuteThenFails(t *testing.T) {
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		if os.Getenv("HARNESS_REQUIRE_PG") == "1" {
			t.Fatal("HARNESS_TEST_PG_DSN is required when HARNESS_REQUIRE_PG=1")
		}
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	stamp := time.Now().Format("150405.000000000")
	taskID := "task-rollback-" + stamp
	taskRepo := repository.NewTaskRepo(db)
	task := &repository.TaskRun{
		TaskID:         taskID,
		TenantID:       terminalTenant,
		ProjectID:      terminalProject,
		WorkspaceRoot:  t.TempDir(),
		Goal:           "closure failure must roll back to c before failing",
		GoalHash:       "hash-" + stamp,
		CurrentStage:   fsm.StageRequirement,
		Status:         repository.TaskRunning,
		BudgetTokens:   100000,
		BudgetCost:     10,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}
	if err := taskRepo.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	stages := &stubStages{fn: func(call int, _ context.Context) (*StageRunResult, error) {
		res := &StageRunResult{FinishReceived: true, Decision: retry.FinishDecision{Action: retry.ActionDone}}
		if call == 2 {
			res.FinishArgs = fsm.StageFinishArgs{Summary: testTriNodePlan("fail-fast")}
		}
		return res, nil
	}}
	rt := NewRuntime(taskRepo, newMemAttempts(), stages)
	events := &captureEvents{}
	rt.Events = events
	rt.Acceptor = &passAcceptor{}
	rt.Now = time.Now
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	rt.Orchestrator = NewOrchestratorBridge(true,
		orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{}),
		&scriptedExecutor{fail: map[string]bool{"n1": true, "n2": true, "n3": true}})

	in := terminalInput()
	in.TaskID = taskID
	res, err := rt.Run(ctx, in)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TerminalReason != ReasonFailed {
		t.Fatalf("terminal reason = %q, want %q after the acceptance budget is gone", res.TerminalReason, ReasonFailed)
	}

	rows, err := db.Pool().Query(ctx, `
		SELECT payload FROM hlp_event.events
		WHERE task_id = $1 AND event_type = 'stage.transition'
		ORDER BY seq`, taskID)
	if err != nil {
		t.Fatalf("query transitions: %v", err)
	}
	defer rows.Close()
	rollbacks := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var payload struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode payload %s: %v", raw, err)
		}
		if payload.From == string(fsm.StageVerify) && payload.To == string(fsm.StageExecute) {
			rollbacks++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if rollbacks != rt.Policy.AcceptanceMaxFails {
		t.Fatalf("persisted d→c transitions = %d, want %d (the budget decides when the parent dies)",
			rollbacks, rt.Policy.AcceptanceMaxFails)
	}

	reloaded, err := taskRepo.Get(ctx, terminalTenant, taskID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != repository.TaskFailed || reloaded.TerminalReason != ReasonFailed {
		t.Fatalf("db status/reason = %s/%s, want failed/failed", reloaded.Status, reloaded.TerminalReason)
	}
	if n := events.count(event.TaskFailed); n != 1 {
		t.Fatalf("task.failed events = %d, want exactly 1", n)
	}
	if events.count(event.TaskCompleted) != 0 {
		t.Fatalf("a parent that never closed its subtasks must not emit task.completed")
	}
}
