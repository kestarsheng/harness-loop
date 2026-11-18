package service

// T-P2-25 权限审计真实验证（e2e）
//
// 这条测试要证的是「一次工具调用 → 一条 policy 决策 → 一条 hlp_event.audit」
// 的一一对应关系，而不是「审计接口被调用过」。所以链路上除 agent 流之外全是
// 生产实现：
//
//	repository.TaskRepo.Create  → 真任务行（hlp_task.task_runs）
//	Runtime.Run                 → 真 runtime（阶段推进 / attempt / checkpoint）
//	StageRunner.Run             → 真 stage runner，逐个 tool_call 走 AuthorizeTool
//	PolicyBridge + Resolver     → 真四层解析（engine 载入仓库里真实的
//	                              stage_matrix.yaml / exec_allow.yaml）
//	repository.AuditRepo        → 真 INSERT INTO hlp_event.audit
//	orchestrator.PublisherEmitter → 真事件落 hlp_event.events（tool.denied /
//	                              approval.requested）
//
// 唯一被替换的是 agent 流：真实部署里它是远端 agent-service 的 LLM 流，这里换成
// 脚本化的 testdata（它「就是」tool_call 的发起方，替换它不影响被测的权限/审计
// 链）。脚本自己记录它实际发出了多少次 tool_call，审计条数必须与之相等——
// 这条断言是防止「audit=0 冒充通过」的关键。
//
// 需要 HARNESS_TEST_PG_DSN；未设置时 t.Skip，不假装通过。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentapproval "agent-platform/pkg/agent/approval"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/pkg/harnessloop/policy/sandbox"
	"agent-platform/pkg/harnessloop/retry"
	agentpb "agent-platform/pkg/pb/agent"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/repository"
)

const (
	policyAuditTenant  = "tenant-policy"
	policyAuditProject = "p2-audit"
)

// scriptedCall 是一次假的 agent 发出的 tool_call。
type scriptedCall struct {
	Tool string
	Args string
}

// policyScriptedAgent 按 stage 回放脚本化的 tool_call，并统计自己实际发出了多少次。
type policyScriptedAgent struct {
	mu      sync.Mutex
	scripts map[string][]scriptedCall
	emitted int
	perStage map[string]int
}

func (a *policyScriptedAgent) ExecuteStream(ctx context.Context, req *agentpb.ExecuteStreamRequest, onChunk func(*agentpb.ExecuteStreamChunk) error) error {
	stage := ""
	if req.ContextVars != nil {
		stage = req.ContextVars[VarStage]
	}

	a.mu.Lock()
	if a.perStage == nil {
		a.perStage = map[string]int{}
	}
	a.mu.Unlock()

	for _, c := range a.scripts[stage] {
		// 先计数再投递：被 gate 拒绝的调用同样是「发起过的工具调用」，
		// 它必须对应一条 deny 审计，否则会被统计漏掉。
		a.mu.Lock()
		a.emitted++
		a.perStage[stage]++
		a.mu.Unlock()
		if err := onChunk(&agentpb.ExecuteStreamChunk{
			Type:     "tool_call",
			ToolCall: &agentpb.ToolCall{Name: c.Tool, Arguments: c.Args},
		}); err != nil {
			return err
		}
	}

	finish, _ := json.Marshal(map[string]string{
		"attempt_id": req.SessionId,
		"stage":      stage,
		"summary":    "stage done",
	})
	return onChunk(&agentpb.ExecuteStreamChunk{
		Type:     "tool_call",
		ToolCall: &agentpb.ToolCall{Name: "stage_finish", Arguments: string(finish)},
	})
}

func (a *policyScriptedAgent) Emitted() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.emitted
}

// policyEventStore 是 event.Publisher 的落库端，与 cmd/main.go 的 eventSink 同形。
//
// 这里多做一件事：按数据库 watermark 校正事件 seq。原因是 event.Publisher 用
// 内存里的 SeqAllocator（每个 task 从 1 开始），而 runtime 的 stage.transition /
// task.failed 走的是 repository 直接写库、seq 取 DB 的 max+1，两者各算各的，
// 于是同一条任务上会出现 seq 撞车，撞车的事件被 (task_id, seq) 唯一键丢掉。
// 本测试要验的是「决策 → 事件」的内容对应，不是 seq 分配器，所以在落库前把
// seq 对齐到 DB watermark；产品侧这个丢事件问题在报告里单列，不在这里掩盖。
type policyEventStore struct {
	repo *repository.EventRepo
	db   *pgindex.DB
	t    *testing.T
}

func (s policyEventStore) Append(ctx context.Context, events []*event.Event) error {
	if s.db != nil {
		for _, e := range events {
			if e == nil {
				continue
			}
			var maxSeq int64
			if err := s.db.Pool().QueryRow(ctx,
				`SELECT COALESCE(MAX(seq),0) FROM hlp_event.events WHERE task_id = $1`, e.TaskID,
			).Scan(&maxSeq); err != nil && s.t != nil {
				s.t.Logf("event sink seq watermark query failed: %v", err)
			}
			if e.Seq <= maxSeq {
				e.Seq = maxSeq + 1
			}
		}
	}
	_, err := s.repo.Append(ctx, "", "", events)
	if err != nil && s.t != nil {
		s.t.Logf("event sink append failed (%d events): %v", len(events), err)
	}
	return err
}

type policyE2EEnv struct {
	db        *pgindex.DB
	tasks     *repository.TaskRepo
	events    *repository.EventRepo
	bridge    *PolicyBridge
	manager   *agentapproval.ApprovalFlowManager
	gate      *policy.ApprovalGate
	rt        *Runtime
	agent     *policyScriptedAgent
	publisher *event.Publisher
}

// policyE2EOptions 让个别用例换掉系统层配置（例如把 kubectl 放进 exec 白名单，
// 才有办法走到审批分支）。默认一律用仓库里的真实配置。
type policyE2EOptions struct {
	execCommands []string
}

type policyE2EOption func(*policyE2EOptions)

func withExecCommands(cmds ...string) policyE2EOption {
	return func(o *policyE2EOptions) { o.execCommands = cmds }
}

// findRepoRoot 从当前测试目录向上找 go.mod，用来定位真实的 policy 配置文件。
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("go.mod not found above working directory")
	return ""
}

func newPolicyE2E(t *testing.T, scripts map[string][]scriptedCall, opts ...policyE2EOption) *policyE2EEnv {
	t.Helper()
	var options policyE2EOptions
	for _, opt := range opts {
		opt(&options)
	}
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	root := findRepoRoot(t)
	engine := policy.NewEngine()
	matrix, err := policy.LoadMatrix(filepath.Join(root, "pkg/config/harness_loop/stage_matrix.yaml"))
	if err != nil {
		t.Fatalf("load stage matrix: %v", err)
	}
	engine.Matrix = matrix
	execAllow, err := sandbox.LoadExecAllowlist(filepath.Join(root, "pkg/config/harness_loop/exec_allow.yaml"))
	if err != nil {
		t.Fatalf("load exec allowlist: %v", err)
	}
	if len(options.execCommands) > 0 {
		execAllow.Commands = options.execCommands
	}
	engine.ExecAllow = execAllow

	taskRepo := repository.NewTaskRepo(db)
	attemptRepo := repository.NewAttemptRepo(db)
	eventRepo := repository.NewEventRepo(db)
	checkpointRepo := repository.NewCheckpointRepo(db)
	auditRepo := repository.NewAuditRepo(db)

	publisher := event.NewPublisher(policyEventStore{repo: eventRepo, db: db, t: t}, event.NewMemoryIdempotency(1000), event.NewMemoryBuffer())
	emitter := orchestrator.NewPublisherEmitter(publisher, orchestrator.EmitContext{TenantID: "platform", ProjectID: "platform"})

	manager := agentapproval.NewApprovalFlowManager()
	gate := policy.NewApprovalGate(policy.DefaultApprovalConfig(), manager)
	gate.SetPollInterval(20 * time.Millisecond)

	bridge := NewPolicyBridge(true, engine, auditRepo, emitter, gate)

	agent := &policyScriptedAgent{scripts: scripts}
	stages := NewStageRunner(agent).WithEvents(publisher).WithToolGate(bridge)

	rt := NewRuntime(taskRepo, attemptRepo, stages)
	rt.Events = publisher
	rt.Checkpoints = checkpointRepo
	rt.Policy = noRetryPolicy()

	return &policyE2EEnv{
		db: db, tasks: taskRepo, events: eventRepo,
		bridge: bridge, manager: manager, gate: gate, rt: rt, agent: agent,
		publisher: publisher,
	}
}

// noRetryPolicy 关掉重试：本测试要数「一次调用一条审计」，重试会把计数放大成噪声。
func noRetryPolicy() retry.Policy {
	p := retry.DefaultPolicy()
	p.MaxRetries = map[retry.Category]int{}
	for _, c := range []retry.Category{
		retry.System, retry.Runtime, retry.Quota, retry.Timeout, retry.NoFinish,
		retry.InvalidToolResult, retry.VerificationError, retry.ContentFilter, retry.Network,
	} {
		p.MaxRetries[c] = 0
	}
	return p
}

// workspace 建一个带文档的最小工作区，让 read_file 有真实路径可读。
func policyWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"README.md":              "# policy audit workspace\n",
		"docs/requirements.md":   "# requirements\n",
		"docs/design.md":         "# design\n",
		"internal/notes/todo.md": "- [ ] audit\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	return dir
}

// runTask 建一条真任务行，用真 runtime 跑完，返回终态。
func (e *policyE2EEnv) runTask(t *testing.T, taskID, workspace string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	task := &repository.TaskRun{
		TaskID:         taskID,
		TenantID:       policyAuditTenant,
		ProjectID:      policyAuditProject,
		WorkspaceRoot:  workspace,
		Goal:           "读取文档并执行只读命令，验证权限审计一一对应",
		GoalHash:       "goal-" + taskID,
		CurrentStage:   fsm.StageRequirement,
		Status:         repository.TaskRunning,
		BudgetTokens:   200000,
		BudgetCost:     10,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}
	if err := e.tasks.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	_, runErr := e.rt.Run(ctx, RunInput{
		TenantID:       policyAuditTenant,
		ProjectID:      policyAuditProject,
		TaskID:         taskID,
		SessionID:      "session-" + taskID,
		EntryAgent:     "main-agent",
		Goal:           task.Goal,
		GoalHash:       task.GoalHash,
		PolicySnapshot: task.PolicySnapshot,
		PolicyHash:     "policy-hash-" + taskID,
		StageModel:     "qwen-plus",
		ModelWindow:    32000,
		StageTimeout:   30 * time.Second,
	})
	view, getErr := e.tasks.Get(ctx, policyAuditTenant, taskID)
	if getErr != nil {
		t.Fatalf("get task: %v", getErr)
	}
	e.flushQuietly()
	if runErr != nil {
		t.Logf("task %s ended with runtime error (expected for deny cases): %v", taskID, runErr)
	}
	return string(view.Status)
}

// flushQuietly 触发一次 publisher 落库。事件是异步投递的，直接读表会拿到
// 「还没写进去」的假阴性，因此读事件前先 flush。Flush 可能重投已入库事件
// （events 表的 (task_id, seq) 唯一键会报 23505），这里不把它当成失败：
// 事件仍然已经落库，后续断言照样成立。
func (e *policyE2EEnv) flushQuietly() {
	if e.publisher == nil {
		return
	}
	_ = e.publisher.Flush(context.Background())
}

// waitForEvent 在截止时间内轮询事件表，等到指定类型的事件出现。
func (e *policyE2EEnv) waitForEvent(t *testing.T, taskID string, typ event.EventType) *event.Event {
	t.Helper()
	return e.waitForEventIn(t, policyAuditTenant, taskID, typ)
}

func (e *policyE2EEnv) waitForEventIn(t *testing.T, tenantID, taskID string, typ event.EventType) *event.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.flushQuietly()
		events, err := e.events.List(context.Background(), repository.EventFilter{TenantID: tenantID, TaskID: taskID})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		for _, ev := range events {
			if ev.EventType == typ {
				return ev
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

type policyAuditRow struct {
	AuditID  string
	Tool     string
	Target   string
	Decision string
	RuleID   string
	Stage    string
	Code     string
	Layer    string
}

func (e *policyE2EEnv) auditRows(t *testing.T, taskID string) []policyAuditRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := e.db.Pool().Query(ctx, `
		SELECT audit_id,
		       action,
		       COALESCE(target,''),
		       COALESCE(decision,''),
		       COALESCE(rule_id,''),
		       COALESCE(payload->>'stage',''),
		       COALESCE(payload->>'code',''),
		       COALESCE(payload->>'layer','')
		FROM hlp_event.audit
		WHERE tenant_id = $1 AND task_id = $2 AND kind = 'tool_decision'
		ORDER BY created_at, audit_id`, policyAuditTenant, taskID)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()

	var out []policyAuditRow
	for rows.Next() {
		var r policyAuditRow
		if err := rows.Scan(&r.AuditID, &r.Tool, &r.Target, &r.Decision, &r.RuleID, &r.Stage, &r.Code, &r.Layer); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	return out
}

func (e *policyE2EEnv) taskEvents(t *testing.T, taskID string) []*event.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := e.events.List(ctx, repository.EventFilter{TenantID: policyAuditTenant, TaskID: taskID})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return rows
}

// TestPolicyAuditPositiveOneToOne 正向用例：允许的工具调用 → 审计条数 == 调用次数。
func TestPolicyAuditPositiveOneToOne(t *testing.T) {
	e := newPolicyE2E(t, map[string][]scriptedCall{
		"a": {{Tool: "read_file", Args: `{"path":"docs/requirements.md"}`}},
		"b": {
			{Tool: "read_file", Args: `{"path":"docs/design.md"}`},
			{Tool: "list_dir", Args: `{"path":"docs"}`},
		},
		"c": {
			{Tool: "read_file", Args: `{"path":"README.md"}`},
			{Tool: "run_command", Args: `{"command":["go","version"]}`},
		},
		"d": {{Tool: "run_command", Args: `{"command":["go","env","GOPATH"]}`}},
	})

	taskID := fmt.Sprintf("task-audit-pos-%d", time.Now().UnixNano())
	status := e.runTask(t, taskID, policyWorkspace(t))

	if status != string(repository.TaskCompleted) {
		t.Fatalf("positive task status = %s, want completed", status)
	}

	rows := e.auditRows(t, taskID)
	emitted := e.agent.Emitted()
	if emitted == 0 {
		t.Fatalf("agent emitted 0 tool calls: the audit assertion below would be vacuous")
	}
	if len(rows) != emitted {
		t.Fatalf("audit rows = %d, tool calls = %d (must be one-to-one)", len(rows), emitted)
	}

	// 逐条对应：按 (stage, tool, target) 做多重集比对，抵消同一工具在不同阶段的重复调用。
	expected := map[string]int{
		"a|read_file|docs/requirements.md": 1,
		"b|read_file|docs/design.md":       1,
		"b|list_dir|docs":                  1,
		"c|read_file|README.md":            1,
		"c|run_command|":                   1,
		"d|run_command|":                   1,
	}
	got := map[string]int{}
	for _, r := range rows {
		if r.Decision != string(policy.EffectAllow) {
			t.Errorf("audit %s: decision = %s (%s %s), want allow", r.AuditID, r.Decision, r.Code, r.RuleID)
		}
		if r.AuditID == "" {
			t.Errorf("audit row without audit_id: %+v", r)
		}
		if r.Layer == "" {
			t.Errorf("audit %s: empty layer breaks the payload contract", r.AuditID)
		}
		got[r.Stage+"|"+r.Tool+"|"+filepath.ToSlash(r.Target)]++
		t.Logf("audit=%s stage=%s tool=%s target=%q decision=%s rule=%s code=%s layer=%s",
			r.AuditID, r.Stage, r.Tool, r.Target, r.Decision, r.RuleID, r.Code, r.Layer)
	}
	for k, n := range expected {
		if got[k] != n {
			t.Errorf("audit multiset %q = %d, want %d (all rows: %v)", k, got[k], n, got)
		}
	}
	for k := range got {
		if _, ok := expected[k]; !ok {
			t.Errorf("unexpected audit row %q", k)
		}
	}
}

// TestPolicyAuditNegativeDeniedAndAudited 负向用例：越权/高危被拒且留痕。
func TestPolicyAuditNegativeDeniedAndAudited(t *testing.T) {
	cases := []struct {
		name       string
		stage      string
		call       scriptedCall
		wantCode   string
		wantRule   string
		wantEvents []string
	}{
		{
			name: "path escape", stage: "b",
			call:     scriptedCall{Tool: "read_file", Args: `{"path":"../../../etc/passwd"}`},
			wantCode: policy.CodePathEscape,
			wantRule: "sandbox.",
		},
		{
			name: "high risk command", stage: "c",
			call:     scriptedCall{Tool: "run_command", Args: `{"command":["rm","-rf","/"]}`},
			wantCode: policy.CodeExecDenied,
			wantRule: "sandbox.exec",
		},
		{
			name: "write tool outside stage matrix", stage: "a",
			call:     scriptedCall{Tool: "write_file", Args: `{"path":"notes.txt"}`},
			wantCode: policy.CodeToolDenied,
			wantRule: "stage_matrix.a.write",
		},
		{
			name: "unregistered tool", stage: "a",
			call:     scriptedCall{Tool: "shell_exec", Args: `{"command":["whoami"]}`},
			wantCode: policy.CodeUnknownTool,
			wantRule: "registry.unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPolicyE2E(t, map[string][]scriptedCall{tc.stage: {tc.call}})
			taskID := fmt.Sprintf("task-audit-neg-%d", time.Now().UnixNano())
			status := e.runTask(t, taskID, policyWorkspace(t))

			if status == string(repository.TaskCompleted) {
				t.Fatalf("task with denied tool call completed; the gate did not block %s", tc.call.Tool)
			}

			rows := e.auditRows(t, taskID)
			if len(rows) != 1 {
				t.Fatalf("audit rows = %d, want exactly 1 deny row for one denied call: %+v", len(rows), rows)
			}
			row := rows[0]
			if row.Decision != string(policy.EffectDeny) {
				t.Fatalf("audit decision = %s, want deny", row.Decision)
			}
			if row.Tool != tc.call.Tool {
				t.Fatalf("audit tool = %s, want %s", row.Tool, tc.call.Tool)
			}
			if row.Code != tc.wantCode {
				t.Fatalf("audit code = %s, want %s (rule=%s)", row.Code, tc.wantCode, row.RuleID)
			}
			if !strings.HasPrefix(row.RuleID, tc.wantRule) {
				t.Fatalf("audit rule_id = %s, want prefix %s", row.RuleID, tc.wantRule)
			}
			t.Logf("deny audit=%s tool=%s stage=%s decision=%s rule=%s code=%s target=%q",
				row.AuditID, row.Tool, row.Stage, row.Decision, row.RuleID, row.Code, row.Target)

			// 留痕还要落到事件流：tool.denied 必须与审计同 rule_id，这是「一一对应」的
			// 另一半（人能看见的告警面 vs 可追溯的审计面）。
			denied := e.waitForEvent(t, taskID, event.ToolDenied)
			if denied == nil {
				t.Fatalf("no tool.denied event recorded for task %s", taskID)
			}
			var payload map[string]any
			if err := json.Unmarshal(denied.Payload, &payload); err != nil {
				t.Fatalf("decode tool.denied payload: %v", err)
			}
			if got, _ := payload["rule_id"].(string); got != row.RuleID {
				t.Fatalf("tool.denied rule_id = %v, audit rule_id = %s (must match)", payload["rule_id"], row.RuleID)
			}
			if got, _ := payload["tool"].(string); got != row.Tool {
				t.Fatalf("tool.denied tool = %v, audit tool = %s", payload["tool"], row.Tool)
			}
		})
	}
}

// TestPolicyAuditCrossTenantSecurityViolation 跨租户调用：被拒 + 审计 + security.violation。
func TestPolicyAuditCrossTenantSecurityViolation(t *testing.T) {
	e := newPolicyE2E(t, map[string][]scriptedCall{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	owner := fmt.Sprintf("task-audit-tenant-%d", time.Now().UnixNano())
	if err := e.tasks.Create(ctx, &repository.TaskRun{
		TaskID: owner, TenantID: policyAuditTenant, ProjectID: policyAuditProject,
		WorkspaceRoot: t.TempDir(), Goal: "own the task", GoalHash: "h",
		CurrentStage: fsm.StageRequirement, Status: repository.TaskRunning,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}); err != nil {
		t.Fatalf("create owner task: %v", err)
	}
	e.bridge.SetTaskLookup(func(ctx context.Context, taskID string) (string, string, error) {
		task, err := e.tasks.Get(ctx, policyAuditTenant, taskID)
		if err != nil {
			return "", "", err
		}
		return task.TenantID, task.ProjectID, nil
	})

	// 以 tenant-intruder 的身份对 tenant-policy 的任务发起 read_file。
	res := e.bridge.Preflight(ctx, ToolCallInput{
		TenantID: "tenant-intruder", ProjectID: policyAuditProject, TaskID: owner,
		Stage: "b", Tool: "read_file", Paths: []string{"README.md"},
	})
	if res.Allowed {
		t.Fatalf("cross-tenant call allowed: %+v", res)
	}
	if res.Code != "E_CROSS_TENANT_DENIED" {
		t.Fatalf("code = %s, want E_CROSS_TENANT_DENIED", res.Code)
	}

	rows, err := func() ([]policyAuditRow, error) {
		q, qerr := e.db.Pool().Query(ctx, `
			SELECT audit_id, action, COALESCE(target,''), COALESCE(decision,''),
			       COALESCE(rule_id,''), COALESCE(payload->>'stage',''),
			       COALESCE(payload->>'code',''), COALESCE(payload->>'layer','')
			FROM hlp_event.audit
			WHERE tenant_id = 'tenant-intruder' AND task_id = $1 AND kind = 'tool_decision'
			ORDER BY created_at, audit_id`, owner)
		if qerr != nil {
			return nil, qerr
		}
		defer q.Close()
		var out []policyAuditRow
		for q.Next() {
			var r policyAuditRow
			if serr := q.Scan(&r.AuditID, &r.Tool, &r.Target, &r.Decision, &r.RuleID, &r.Stage, &r.Code, &r.Layer); serr != nil {
				return nil, serr
			}
			out = append(out, r)
		}
		return out, q.Err()
	}()
	if err != nil {
		t.Fatalf("query cross-tenant audit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("cross-tenant audit rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].RuleID != "security.cross_tenant" || rows[0].Decision != string(policy.EffectDeny) {
		t.Fatalf("cross-tenant audit = %+v, want deny/security.cross_tenant", rows[0])
	}

	violation := e.waitForEventIn(t, "tenant-intruder", owner, event.SecurityViolation)
	if violation == nil {
		t.Fatalf("no security.violation event for cross-tenant attempt")
	}
	t.Logf("cross-tenant: audit=%s rule=%s event=%s", rows[0].AuditID, rows[0].RuleID, violation.EventID)
}

// TestPolicyAuditApprovalDecisionLinkage 审批链路：require_approval → 工单 → 批准/驳回。
//
// 两条重要口径（都是本期实测出来的约束，写进报告）：
//  1. 线上 stage 路径不会注入 tenant/role/task 策略集，所以 require_approval 在
//     真实任务里目前不可达；这里验证的是 bridge 契约（传策略集时决策-工单-审计
//     三者对齐）。
//  2. 审批闸门只为「高危类」建工单（delete/network_write/deploy/cost），而默认
//     exec 白名单只有 go/gofmt/...，高危命令先被 sandbox.exec 拒掉。要把审批分支
//     真正跑通，必须显式放开一个高危命令（本用例把 kubectl 放进白名单）。
func TestPolicyAuditApprovalDecisionLinkage(t *testing.T) {
	e := newPolicyE2E(t, map[string][]scriptedCall{}, withExecCommands("go", "kubectl"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	approver := &policy.PolicySet{Rules: []policy.Rule{{
		ID:               "task.approval.exec",
		Tool:             "run_command",
		Stages:           []string{"c"},
		Effect:           policy.EffectRequireApproval,
		RequiresApproval: true,
	}}}

	taskID := fmt.Sprintf("task-audit-approval-%d", time.Now().UnixNano())
	if err := e.tasks.Create(ctx, &repository.TaskRun{
		TaskID: taskID, TenantID: policyAuditTenant, ProjectID: policyAuditProject,
		WorkspaceRoot: t.TempDir(), Goal: "approval linkage", GoalHash: "h",
		CurrentStage: fsm.StageRequirement, Status: repository.TaskRunning,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	in := ToolCallInput{
		TenantID: policyAuditTenant, ProjectID: policyAuditProject, TaskID: taskID,
		Stage: "c", AgentID: "main-agent", Tool: "run_command",
		Command: []string{"kubectl", "apply", "-f", "deploy.yaml"}, TaskPolicy: approver,
	}

	pre := e.bridge.Preflight(ctx, in)
	if !pre.RequiresApproval || pre.Allowed {
		t.Fatalf("preflight = %+v, want requires_approval and not allowed", pre)
	}
	if pre.ApprovalID == "" {
		t.Fatalf("preflight did not create an approval ticket: %+v", pre)
	}
	if pre.RuleID != "task.approval.exec" {
		t.Fatalf("preflight rule = %s, want task.approval.exec", pre.RuleID)
	}

	// 人工批准后，阻塞式的 AuthorizeTool 应当放行。
	//
	// 注意：Preflight 和 AuthorizeTool 各自 Begin 一次工单（两处决策各留一条审计），
	// 所以这里要把两处 pending 工单都批掉——只批 preflight 那张，AuthorizeTool
	// 会一直等到超时，测试就变成在验证「超时不放行」而不是「批准后放行」。
	done := make(chan error, 1)
	go func() {
		done <- e.bridge.AuthorizeTool(ctx, in)
	}()
	waitPending := func(want int) []*agentapproval.ApprovalRequest {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if got := e.manager.GetPendingRequests(""); len(got) >= want {
				return got
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("approval tickets did not reach %d pending requests", want)
		return nil
	}
	pending := waitPending(2)
	ticket := pre.ApprovalID
	for _, req := range pending {
		if err := e.manager.SubmitDecision(ctx, &agentapproval.ApprovalDecision{
			RequestID: req.ID, Decision: agentapproval.StatusApproved, UserID: "auditor",
		}); err != nil {
			t.Fatalf("approve %s: %v", req.ID, err)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("approved call was not allowed: %v", err)
	}

	rows := e.auditRows(t, taskID)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2 (preflight + authorize decisions, both require_approval): %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Decision != string(policy.EffectRequireApproval) {
			t.Fatalf("audit %s decision = %s, want require_approval", r.AuditID, r.Decision)
		}
		if r.RuleID != "task.approval.exec" {
			t.Fatalf("audit %s rule = %s, want task.approval.exec", r.AuditID, r.RuleID)
		}
	}

	// 驳回路径：同一 (tenant, task, stage, tool, command) 的重复调用会被闸门拒绝。
	rejectTask := taskID + "-reject"
	if err := e.tasks.Create(ctx, &repository.TaskRun{
		TaskID: rejectTask, TenantID: policyAuditTenant, ProjectID: policyAuditProject,
		WorkspaceRoot: t.TempDir(), Goal: "approval reject", GoalHash: "h",
		CurrentStage: fsm.StageRequirement, Status: repository.TaskRunning,
		PolicySnapshot: []byte(`{"mode":"enforce"}`),
	}); err != nil {
		t.Fatalf("create reject task: %v", err)
	}
	rejectIn := in
	rejectIn.TaskID = rejectTask
	rejectIn.Command = []string{"kubectl", "apply", "-f", "app.yaml"}

	rejectDone := make(chan error, 1)
	go func() { rejectDone <- e.bridge.AuthorizeTool(ctx, rejectIn) }()

	// AuthorizeTool 内部 Begin 出一张新工单；等到它出现再驳回。
	var rejectTicket string
	rejectDeadline := time.Now().Add(5 * time.Second)
	for rejectTicket == "" && time.Now().Before(rejectDeadline) {
		for _, req := range e.manager.GetPendingRequests("") {
			if req.SessionID == rejectTask {
				rejectTicket = req.ID
			}
		}
		if rejectTicket == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if rejectTicket == "" {
		t.Fatalf("no pending approval request for the reject case")
	}
	if err := e.manager.SubmitDecision(ctx, &agentapproval.ApprovalDecision{
		RequestID: rejectTicket, Decision: agentapproval.StatusRejected,
		UserID: "auditor", Reason: "not this time",
	}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := <-rejectDone; err == nil {
		t.Fatalf("rejected call was allowed")
	} else if !strings.Contains(err.Error(), policy.CodeApprovalRejected) {
		t.Fatalf("reject error = %v, want code %s", err, policy.CodeApprovalRejected)
	}

	rejectRows := e.auditRows(t, rejectTask)
	if len(rejectRows) != 1 || rejectRows[0].Decision != string(policy.EffectRequireApproval) {
		t.Fatalf("reject-case audit = %+v, want 1 require_approval row", rejectRows)
	}

	// approval.requested 事件必须与审计同 rule_id。
	e.flushQuietly()
	var requested int
	deadline := time.Now().Add(5 * time.Second)
	for {
		requested = 0
		events, err := e.events.List(context.Background(), repository.EventFilter{TenantID: policyAuditTenant, TaskID: taskID})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		for _, ev := range events {
			if ev.EventType == event.ApprovalRequested {
				requested++
			}
		}
		if requested >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	events, err := e.events.List(context.Background(), repository.EventFilter{TenantID: policyAuditTenant, TaskID: taskID})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range events {
		if ev.EventType != event.ApprovalRequested {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decode approval.requested: %v", err)
		}
		if got, _ := payload["rule_id"].(string); got != "task.approval.exec" {
			t.Fatalf("approval.requested rule_id = %v, want task.approval.exec", payload["rule_id"])
		}
	}
	if requested != 2 {
		t.Fatalf("approval.requested events = %d, want 2 (preflight + authorize)", requested)
	}
	t.Logf("approval: ticket=%s approved=true audits=%v requested_events=%d", ticket, len(rows), requested)
}
