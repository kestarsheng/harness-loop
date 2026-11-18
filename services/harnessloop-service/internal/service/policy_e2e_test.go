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
	mu       sync.Mutex
	scripts  map[string][]scriptedCall
	emitted  int
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
// 这里刻意**不再**做任何 seq 校正。T-P2-32 之前 event.Publisher 用内存里的
// SeqAllocator（每个任务从 1 开始），runtime 的 stage.transition / task.failed
// 又直接写库取 DB 的 max+1，两条写者撞 (task_id, seq) 唯一键，撞车的事件被丢，
// 本测试曾经靠「落库前把 seq 对齐到 DB watermark」绕过。现在 seq 的唯一分配者
// 是 repository.EventRepo.Append，校正逻辑成了反向指标，必须消失。
type policyEventStore struct {
	repo *repository.EventRepo
	t    *testing.T
}

func (s policyEventStore) Append(ctx context.Context, events []*event.Event) error {
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

	// lastResult 是最近一次 runTaskWith 的 runtime 结果。阶段闸门拒绝时
	// runtime 会把任务收成 failed 并返回 nil error，失败原因只在 RunResult 里，
	// 断言错误码必须从这里读。
	lastResult *RunResult
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

	publisher := event.NewPublisher(policyEventStore{repo: eventRepo, t: t}, event.NewMemoryIdempotency(1000), event.NewMemoryBuffer())
	emitter := orchestrator.NewPublisherEmitter(publisher, orchestrator.EmitContext{TenantID: "platform", ProjectID: "platform"})

	manager := agentapproval.NewApprovalFlowManager()
	gate := policy.NewApprovalGate(policy.DefaultApprovalConfig(), manager)
	gate.SetPollInterval(20 * time.Millisecond)

	bridge := NewPolicyBridge(true, engine, auditRepo, emitter, gate)
	// 四层装配用的是仓库里真实的 policy_layers.yaml，不在测试里手写策略集：
	// 「生产代码从不注入」这个问题必须由配置来证明已经修好，否则测试只是重复
	// 了一遍手工注入。
	layerPath := filepath.Join(root, "pkg/config/harness_loop/policy_layers.yaml")
	layers, err := policy.LoadLayerStore(layerPath)
	if err != nil {
		t.Fatalf("load layer sets %s: %v", layerPath, err)
	}
	bridge.SetLayerStore(layers)

	agent := &policyScriptedAgent{scripts: scripts}
	stages := NewStageRunner(agent).WithEvents(publisher).WithToolGate(bridge)

	rt := NewRuntime(taskRepo, attemptRepo, stages)
	rt.Events = publisher
	rt.Checkpoints = checkpointRepo
	rt.Policy = noRetryPolicy()
	// 显式绑定，避免同进程里多个 bridge 抢默认注入点。
	rt.TaskPolicies = bridge

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
	status, _ := e.runTaskWith(t, runOptions{taskID: taskID}, workspace)
	return status
}

// runOptions 让个别用例换掉租户 / entry_agent / 任务快照（任务层策略集的来源）。
type runOptions struct {
	taskID     string
	tenantID   string
	entryAgent string
	snapshot   []byte
	timeout    time.Duration
}

func (e *policyE2EEnv) runTaskWith(t *testing.T, opts runOptions, workspace string) (string, error) {
	t.Helper()
	tenantID := opts.tenantID
	if tenantID == "" {
		tenantID = policyAuditTenant
	}
	entryAgent := opts.entryAgent
	if entryAgent == "" {
		entryAgent = "main-agent"
	}
	snapshot := opts.snapshot
	if len(snapshot) == 0 {
		snapshot = []byte(`{"mode":"enforce"}`)
	}
	timeout := opts.timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	task := &repository.TaskRun{
		TaskID:         opts.taskID,
		TenantID:       tenantID,
		ProjectID:      policyAuditProject,
		WorkspaceRoot:  workspace,
		Goal:           "读取文档并执行只读命令，验证权限审计一一对应",
		GoalHash:       "goal-" + opts.taskID,
		CurrentStage:   fsm.StageRequirement,
		Status:         repository.TaskRunning,
		BudgetTokens:   200000,
		BudgetCost:     10,
		PolicySnapshot: snapshot,
	}
	if err := e.tasks.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	res, runErr := e.rt.Run(ctx, RunInput{
		TenantID:       tenantID,
		ProjectID:      policyAuditProject,
		TaskID:         opts.taskID,
		SessionID:      "session-" + opts.taskID,
		EntryAgent:     entryAgent,
		Goal:           task.Goal,
		GoalHash:       task.GoalHash,
		PolicySnapshot: task.PolicySnapshot,
		PolicyHash:     "policy-hash-" + opts.taskID,
		StageModel:     "qwen-plus",
		ModelWindow:    32000,
		StageTimeout:   30 * time.Second,
	})
	view, getErr := e.tasks.Get(ctx, tenantID, opts.taskID)
	if getErr != nil {
		t.Fatalf("get task: %v", getErr)
	}
	e.lastResult = res
	e.flushQuietly()
	if runErr != nil {
		t.Logf("task %s ended with runtime error (expected for deny cases): %v", opts.taskID, runErr)
	}
	return string(view.Status), runErr
}

// flushQuietly 触发一次 publisher 落库。事件是异步投递的，直接读表会拿到
// 「还没写进去」的假阴性，因此读事件前先 flush。Flush 重投已入库事件时由
// event_id 主键幂等吃掉（ON CONFLICT DO NOTHING），不会撞 (task_id, seq)。
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
	AuditID      string
	Tool         string
	Target       string
	Decision     string
	RuleID       string
	Stage        string
	Code         string
	Layer        string
	PolicySource string
}

func (e *policyE2EEnv) auditRows(t *testing.T, taskID string) []policyAuditRow {
	t.Helper()
	return e.auditRowsIn(t, policyAuditTenant, taskID)
}

func (e *policyE2EEnv) auditRowsIn(t *testing.T, tenantID, taskID string) []policyAuditRow {
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
		       COALESCE(payload->>'layer',''),
		       COALESCE(payload->'args_summary'->>'policy_source','')
		FROM hlp_event.audit
		WHERE tenant_id = $1 AND task_id = $2 AND kind = 'tool_decision'
		ORDER BY created_at, audit_id`, tenantID, taskID)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()

	var out []policyAuditRow
	for rows.Next() {
		var r policyAuditRow
		if err := rows.Scan(&r.AuditID, &r.Tool, &r.Target, &r.Decision, &r.RuleID, &r.Stage, &r.Code, &r.Layer, &r.PolicySource); err != nil {
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

// assertContiguousSeq 断言某任务事件的 seq 从 1 开始连续、不重号。
// seq 由事件库单一分配时这是恒等式；一旦有人重新自己编号，就会出现重号
// （后写的那条被唯一键丢掉 → 事件缺失）或空洞。
func assertContiguousSeq(t *testing.T, events []*event.Event) {
	t.Helper()
	seen := make(map[int64]bool, len(events))
	for _, ev := range events {
		if ev.Seq <= 0 {
			t.Fatalf("event %s has no assigned seq", ev.EventID)
		}
		if seen[ev.Seq] {
			t.Fatalf("duplicate seq %d on task %s (event %s)", ev.Seq, ev.TaskID, ev.EventID)
		}
		seen[ev.Seq] = true
	}
	for i := 1; i <= len(events); i++ {
		if !seen[int64(i)] {
			t.Fatalf("seq gap at %d (events=%d): %v", i, len(events), seen)
		}
	}
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

	// 终态事件必须真的落库：T-P2-32 之前 task.completed 会被 seq 撞键吞掉。
	if completed := e.waitForEvent(t, taskID, event.TaskCompleted); completed == nil {
		t.Fatalf("no task.completed event recorded for task %s", taskID)
	}
	assertContiguousSeq(t, e.taskEvents(t, taskID))

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

