package orchestrator

// T-P1-T5 · design 9.9 多 Agent 协作回归套件。
//
// 本文件只新增测试，不修改任何实现代码。所有用例都要求真实 PostgreSQL：
//   - HARNESS_TEST_PG_DSN 缺失    → t.Fatalf（禁止静默 Skip）；
//   - HARNESS_REQUIRE_PG != "1"   → t.Fatalf（显式硬开关）。
//
// 事件一律走真实 event.Publisher + orchestrator.PublisherEmitter（T-P1-31 接线），
// 并在收尾统一断言 seq 连续无缺号。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/subtask"
	"agent-platform/pkg/pgindex"
)

const (
	maSuiteTenant  = "ma-suite-tenant"
	maSuiteProject = "ma-suite-project"
)

// ---------------------------------------------------------------------------
// 基础设施：真实 PG + 真实 Publisher
// ---------------------------------------------------------------------------

var (
	maDBOnce sync.Once
	maDB     *pgindex.DB
	maDBErr  error
)

// maSuitePG 返回真实 PG 连接。任何环境缺项都直接 Fatal，绝不 Skip。
func maSuitePG(t *testing.T) *pgindex.DB {
	t.Helper()
	if os.Getenv("HARNESS_REQUIRE_PG") != "1" {
		t.Fatalf("HARNESS_REQUIRE_PG=1 is required for the T-P1-T5 multi-agent suite (no silent skip)")
	}
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Fatalf("HARNESS_TEST_PG_DSN is required for the T-P1-T5 multi-agent suite (no silent skip)")
	}
	maDBOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
		if err != nil {
			maDBErr = fmt.Errorf("open pg: %w", err)
			return
		}
		if err := db.Migrate(ctx); err != nil {
			db.Close()
			maDBErr = fmt.Errorf("migrate: %w", err)
			return
		}
		maDB = db
	})
	if maDBErr != nil {
		t.Fatalf("real PG is not usable: %v", maDBErr)
	}
	return maDB
}

// maSuiteSink 记录真实 Publisher 分配过 seq 的事件。
type maSuiteSink struct {
	mu     sync.Mutex
	events []*event.Event
}

func (s *maSuiteSink) Append(_ context.Context, events []*event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (s *maSuiteSink) snapshot() []*event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*event.Event, len(s.events))
	copy(out, s.events)
	return out
}

type maSuiteEnv struct {
	db      *pgindex.DB
	sink    *maSuiteSink
	pub     *event.Publisher
	emitter *PublisherEmitter
	tenant  string
	project string
	taskID  string
}

func maSuiteNewEnv(t *testing.T, taskID string) *maSuiteEnv {
	t.Helper()
	db := maSuitePG(t)
	sink := &maSuiteSink{}
	pub := event.NewPublisher(sink, event.NewMemoryIdempotency(10000), nil)
	env := &maSuiteEnv{
		db: db, sink: sink, pub: pub, tenant: maSuiteTenant, project: maSuiteProject, taskID: taskID,
	}
	env.emitter = NewPublisherEmitter(pub, EmitContext{
		TenantID: env.tenant, ProjectID: env.project, TaskID: taskID, ParentTaskID: taskID, Stage: "b", Attempt: 1,
	})
	t.Cleanup(func() { maSuiteCleanup(t, db, taskID) })
	return env
}

func maSuiteCleanup(t *testing.T, db *pgindex.DB, taskID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.Exec(ctx, `DELETE FROM hlp_task.task_runs WHERE tenant_id LIKE 'ma-suite-%'`); err != nil {
		t.Logf("cleanup task_runs: %v", err)
	}
	if taskID != "" {
		var left int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM hlp_task.subtasks WHERE parent_task_id = $1`, taskID).Scan(&left)
		if left != 0 {
			t.Errorf("cleanup left %d subtasks for %s", left, taskID)
		}
	}
}

// maSuiteSeedTask 插入一条父任务（b 阶段）。tenant/project 固定使用套件前缀，
// 收尾统一清理，避免污染其它测试线。
func maSuiteSeedTask(t *testing.T, db *pgindex.DB, taskID, stage string) {
	t.Helper()
	ctx := context.Background()
	_, err := db.Exec(ctx, `
		INSERT INTO hlp_task.task_runs
			(task_id, tenant_id, project_id, workspace_root, goal, goal_hash,
			 current_stage, status, policy_snapshot)
		VALUES ($1,$2,$3,'/workspace','multi-agent suite goal','sha256:suite',
		        $4,'running','{"version":1}')
		ON CONFLICT (task_id) DO NOTHING`,
		taskID, maSuiteTenant, maSuiteProject, stage)
	if err != nil {
		t.Fatalf("seed task %s: %v", taskID, err)
	}
}

func maSuiteTaskStage(t *testing.T, db *pgindex.DB, taskID string) string {
	t.Helper()
	var stage string
	if err := db.QueryRow(context.Background(),
		`SELECT current_stage FROM hlp_task.task_runs WHERE task_id = $1`, taskID).Scan(&stage); err != nil {
		t.Fatalf("read task stage: %v", err)
	}
	return stage
}

func maSuiteBool(v bool) *bool { return &v }

// maSuitePlanNode 返回一个可通过 Plan.Validate 的节点。
func maSuitePlanNode(id, role string) PlanNode {
	return PlanNode{
		NodeID: id,
		Role:   role,
		InputContract: Contract{
			Type: "goal",
		},
		OutputContract: Contract{
			Type:  "artifacts",
			Paths: []string{"artifacts/" + id + "/*.json"},
		},
		Acceptance: []AcceptanceSpec{
			{Type: "tests_pass", Expr: "go test ./...",},
			{Type: "build_pass", Expr: "go build ./...",},
		},
		Splittable: maSuiteBool(true),
	}
}

func maSuitePlan(nodes ...PlanNode) *Plan {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.NodeID)
	}
	return &Plan{
		DagID: "dag-suite",
		Nodes: nodes,
		ParallelSets: []ParallelSet{
			{SetID: "ps-1", Nodes: ids, FailureStrategy: subtask.StrategyFailFast},
		},
	}
}

func maSuiteSubtask(parent, dag, id string, index, depth int, setID, role string) *subtask.Subtask {
	return &subtask.Subtask{
		SubtaskID:       id,
		ParentTaskID:    parent,
		TenantID:        maSuiteTenant,
		ProjectID:       maSuiteProject,
		DagID:           dag,
		DagNodeID:       id,
		Depth:           depth,
		IndexInParent:   index,
		ParallelSetID:   setID,
		Role:            role,
		Status:          subtask.StatusPending,
		FailureStrategy: subtask.StrategyFailFast,
		AttemptNo:       1,
		OutputContract: map[string]any{
			"type":             "artifacts",
			"paths":            []string{"artifacts/" + id + "/*.json"},
			"estimated_tokens": 1024,
		},
		AcceptanceCriteria: []string{"go test ./..."},
	}
}

func maSuiteCode(t *testing.T, err error, want string) {
	t.Helper()
	if got := ErrorCode(err); got != want {
		t.Fatalf("error code = %q, want %q (err=%v)", got, want, err)
	}
}

// assertSeqContinuous 断言同一 task_id 的事件 seq 恰好是 1..N（无缺号、无重复）。
func assertSeqContinuous(t *testing.T, sink *maSuiteSink, taskID string) int {
	t.Helper()
	var seqs []int64
	for _, e := range sink.snapshot() {
		if e.TaskID == taskID {
			seqs = append(seqs, e.Seq)
		}
	}
	if len(seqs) == 0 {
		t.Fatalf("no events recorded for task %s", taskID)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("seq gap for %s: position %d has seq %d (got %v)", taskID, i, seq, seqs)
		}
	}
	return len(seqs)
}

// ---------------------------------------------------------------------------
// 9.9 父子层级
// ---------------------------------------------------------------------------

// 9.9 父子层级：父 b 阶段产出 3 子任务 → 父 current_stage 仍为 b；3 子任务各有
// 独立 subtask_id、attempt_no 从 1 起、各自可恢复（持久化后可重复读取）。
func TestMultiAgentParentChildHierarchy(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-hierarchy")
	maSuiteSeedTask(t, env.db, env.taskID, "b")

	planner := NewPlanner(DefaultPlanConfig(), env.emitter)
	plan := maSuitePlan(
		maSuitePlanNode("n1", "coder"),
		maSuitePlanNode("n2", "coder"),
		maSuitePlanNode("n3", "verifier"),
	)
	res, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "b", Plan: plan,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Subtasks) != 3 {
		t.Fatalf("subtasks = %d, want 3", len(res.Subtasks))
	}
	repo := subtask.NewRepo(env.db)
	seen := map[string]bool{}
	for _, st := range res.Subtasks {
		if seen[st.SubtaskID] {
			t.Fatalf("duplicate subtask id %s", st.SubtaskID)
		}
		seen[st.SubtaskID] = true
		if st.AttemptNo != 1 {
			t.Fatalf("attempt_no = %d, want 1", st.AttemptNo)
		}
		if err := repo.Insert(context.Background(), st); err != nil {
			t.Fatalf("insert %s: %v", st.SubtaskID, err)
		}
	}
	if got := maSuiteTaskStage(t, env.db, env.taskID); got != "b" {
		t.Fatalf("parent current_stage = %q, want b", got)
	}
	rows, err := repo.ListByParent(context.Background(), env.taskID, "dag-suite")
	if err != nil || len(rows) != 3 {
		t.Fatalf("persisted subtasks = %d err=%v, want 3", len(rows), err)
	}
	for _, row := range rows {
		if row.AttemptNo != 1 || row.Depth != 1 {
			t.Fatalf("unexpected persisted row: %+v", row)
		}
	}
	if n := assertSeqContinuous(t, env.sink, env.taskID); n < 1 {
		t.Fatalf("expected subtask.plan_created event, got %d", n)
	}
	if len(env.sink.snapshot()) == 0 || env.sink.snapshot()[0].EventType != event.SubtaskPlanCreated {
		t.Fatalf("first event must be subtask.plan_created, got %+v", env.sink.snapshot())
	}
}

// 9.9 层级：任一子任务失败 → 父 current_stage 不变，仅汇聚视图回写。
func TestMultiAgentChildFailureKeepsParentStage(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-child-fail")
	maSuiteSeedTask(t, env.db, env.taskID, "b")
	repo := subtask.NewRepo(env.db)

	st := maSuiteSubtask(env.taskID, "dag-suite", "ma-st-fail", 0, 1, "ps-1", "coder")
	if err := repo.Insert(context.Background(), st); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := env.db.Exec(context.Background(),
		`UPDATE hlp_task.subtasks SET status='failed' WHERE subtask_id=$1`, st.SubtaskID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if got := maSuiteTaskStage(t, env.db, env.taskID); got != "b" {
		t.Fatalf("child failure changed parent stage to %q", got)
	}

	agg := NewAggregator(maSuiteFakeVerifier{}, env.emitter)
	failed := maSuiteSubtask(env.taskID, "dag-suite", "ma-st-fail", 0, 1, "ps-1", "coder")
	failed.Status = subtask.StatusFailed
	report, err := agg.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: env.taskID, DagID: "dag-suite", TenantID: env.tenant, ProjectID: env.project,
		Subtasks: []*subtask.Subtask{failed},
	})
	// 9.9 只要求「汇聚视图回写失败项」，不要求 failed 终态导致整份报告不可用；
	// 因此这里断言视图内容，而不是 err 非空。
	if err != nil {
		t.Fatalf("aggregate terminal failure rows: %v", err)
	}
	if report == nil || report.Summary.Failed != 1 || report.Items[0].Verdict != "fail" {
		t.Fatalf("aggregation view did not record the failure: %+v", report)
	}
	if got := maSuiteTaskStage(t, env.db, env.taskID); got != "b" {
		t.Fatalf("aggregation changed parent stage to %q", got)
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

// ---------------------------------------------------------------------------
// 9.9 子任务状态机
// ---------------------------------------------------------------------------

// 9.9 子任务状态机：强制把未验收子任务置 accepted → Go 状态机拒绝
// (E_SUBTASK_ACCEPTANCE_REQUIRED)，DB CHECK 亦拒绝（SQLSTATE 23514）。
func TestMultiAgentSubtaskAcceptanceRequired(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-accept-required")
	maSuiteSeedTask(t, env.db, env.taskID, "c")
	repo := subtask.NewRepo(env.db)

	st := maSuiteSubtask(env.taskID, "dag-suite", "ma-st-acc", 0, 1, "ps-1", "coder")
	st.Status = subtask.StatusRunning
	if err := repo.Insert(context.Background(), st); err != nil {
		t.Fatalf("insert: %v", err)
	}
	err := repo.Transition(context.Background(), st.SubtaskID, subtask.StatusAccepted, "", "")
	if err == nil {
		t.Fatal("running -> accepted without acceptance_id must be rejected")
	}
	var acceptance *subtask.AcceptanceRequiredError
	if !errors.As(err, &acceptance) && !strings.Contains(err.Error(), CodeSubtaskNotAccepted) {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := subtask.Transition(subtask.StatusRunning, subtask.StatusAccepted, "", ""); err == nil {
		t.Fatal("subtask state machine must reject accepted without acceptance_id")
	}

	// 绕过 Go 状态机直接写库，CHECK 必须兜底。
	_, dbErr := env.db.Exec(context.Background(),
		`UPDATE hlp_task.subtasks SET status='accepted' WHERE subtask_id=$1`, st.SubtaskID)
	if dbErr == nil {
		t.Fatal("DB accepted without acceptance_id must violate the CHECK constraint")
	}
	var pgErr *pgconn.PgError
	if !errors.As(dbErr, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("expected SQLSTATE 23514, got %v", dbErr)
	}
	var status string
	if err := env.db.QueryRow(context.Background(),
		`SELECT status FROM hlp_task.subtasks WHERE subtask_id=$1`, st.SubtaskID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %q, want running after rejected update", status)
	}
}

// ---------------------------------------------------------------------------
// 9.9 DAG 完整性
// ---------------------------------------------------------------------------

func TestMultiAgentDAGIncomplete(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-dag-incomplete")
	plan := &Plan{
		DagID: "dag-incomplete",
		Nodes: []PlanNode{
			maSuitePlanNode("n1", "coder"),
			maSuitePlanNode("n2", "coder"),
		},
	}
	planner := NewPlanner(DefaultPlanConfig(), env.emitter)
	_, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "b", Plan: plan,
	})
	maSuiteCode(t, err, CodeDAGIncomplete)
}

func TestMultiAgentDAGCycle(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-dag-cycle")
	plan := &Plan{
		DagID: "dag-cycle",
		Nodes: []PlanNode{
			maSuitePlanNode("a", "coder"),
			maSuitePlanNode("b", "coder"),
		},
		Edges: []PlanEdge{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}
	planner := NewPlanner(DefaultPlanConfig(), env.emitter)
	_, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "b", Plan: plan,
	})
	maSuiteCode(t, err, CodeDAGCycle)
}

func TestMultiAgentDAGParallelConflict(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-dag-parallel")
	plan := &Plan{
		DagID: "dag-parallel",
		Nodes: []PlanNode{
			maSuitePlanNode("a", "coder"),
			maSuitePlanNode("b", "coder"),
		},
		Edges: []PlanEdge{{From: "a", To: "b"}},
		ParallelSets: []ParallelSet{
			{SetID: "ps-bad", Nodes: []string{"a", "b"}, FailureStrategy: subtask.StrategyFailFast},
		},
	}
	planner := NewPlanner(DefaultPlanConfig(), env.emitter)
	_, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "b", Plan: plan,
	})
	maSuiteCode(t, err, CodeDAGParallelConflict)
}

// ---------------------------------------------------------------------------
// 9.9 上限：21 直接子任务 / 第 3 层 / 不可拆
// ---------------------------------------------------------------------------

func TestMultiAgentDirectChildLimit(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-limit")
	nodes := make([]PlanNode, 0, 21)
	ids := make([]string, 0, 21)
	for i := 0; i < 21; i++ {
		id := fmt.Sprintf("n%02d", i)
		nodes = append(nodes, maSuitePlanNode(id, "coder"))
		ids = append(ids, id)
	}
	plan := &Plan{
		DagID: "dag-limit",
		Nodes: nodes,
		ParallelSets: []ParallelSet{
			{SetID: "ps-1", Nodes: ids, FailureStrategy: subtask.StrategyFailFast},
		},
	}
	planner := NewPlanner(DefaultPlanConfig(), env.emitter)
	_, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "b", Plan: plan,
	})
	maSuiteCode(t, err, CodeSubtaskLimitExceeded)
}

func TestMultiAgentDepthLimit(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-depth")
	root := maSuitePlanNode("root", "coder")
	child := maSuitePlanNode("child", "coder")
	grand := maSuitePlanNode("grand", "coder")
	child.Children = []PlanNode{grand}
	root.Children = []PlanNode{child}
	plan := &Plan{DagID: "dag-depth", Nodes: []PlanNode{root}}
	planner := NewPlanner(DefaultPlanConfig(), env.emitter)
	_, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "b", Plan: plan,
	})
	maSuiteCode(t, err, CodeSubtaskDepthExceeded)
}

func TestMultiAgentNotSplittable(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-not-splittable")
	node := maSuitePlanNode("approval-unit", "coder")
	node.RequiresApproval = true
	plan := &Plan{DagID: "dag-ns", Nodes: []PlanNode{node}}
	planner := NewPlanner(DefaultPlanConfig(), env.emitter)
	_, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "b", Plan: plan,
	})
	maSuiteCode(t, err, CodeSubtaskNotSplittable)
}

// ---------------------------------------------------------------------------
// 9.9 并发 / 租户并发
// ---------------------------------------------------------------------------

func TestMultiAgentParallelCapEight(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-parallel-cap")
	s := NewScheduler(DefaultQuotaConfig(), env.emitter)
	var batch []*subtask.Subtask
	for i := 0; i < 12; i++ {
		batch = append(batch, maSuiteSubtask(env.taskID, "dag-suite",
			fmt.Sprintf("ma-st-%02d", i), i, 1, "ps-1", "coder"))
	}
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	started, err := s.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(started) != 8 {
		t.Fatalf("started = %d, want 8", len(started))
	}
	if got := len(s.Running()); got != 8 {
		t.Fatalf("running = %d, want 8", got)
	}
	if got := len(s.Pending()); got != 4 {
		t.Fatalf("pending = %d, want 4", got)
	}
	for _, st := range s.Pending() {
		if st.Status != subtask.StatusPending {
			t.Fatalf("queued subtask %s status = %s, must stay pending", st.SubtaskID, st.Status)
		}
	}
	if n := len(env.sink.snapshot()); n == 0 {
		t.Fatal("expected real publisher events")
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

func TestMultiAgentTenantConcurrencyQueues(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-tenant-gate")
	gate := NewTenantGate(20)
	for i := 0; i < 18; i++ {
		if !gate.TryAcquire() {
			t.Fatalf("pre-acquire %d failed", i)
		}
	}
	s := NewSchedulerWithGate(DefaultQuotaConfig(), gate, env.emitter)
	var batch []*subtask.Subtask
	for i := 0; i < 4; i++ {
		batch = append(batch, maSuiteSubtask(env.taskID, "dag-suite",
			fmt.Sprintf("ma-st-t%02d", i), i, 1, "ps-1", "coder"))
	}
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	started, err := s.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(started) != 2 {
		t.Fatalf("started = %d, want 2 under the tenant gate", len(started))
	}
	if gate.Active() != 20 {
		t.Fatalf("tenant active = %d, want 20", gate.Active())
	}
	if got := len(s.Pending()); got != 2 {
		t.Fatalf("pending = %d, want 2 (never failed)", got)
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

// ---------------------------------------------------------------------------
// 9.9 超时 / 熔断 / 三种传播
// ---------------------------------------------------------------------------

func TestMultiAgentTimeoutPropagation(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-timeout")
	s := NewScheduler(DefaultQuotaConfig(), env.emitter)
	fixed := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return fixed })
	s.SetParentRemaining(500 * time.Millisecond)
	st := maSuiteSubtask(env.taskID, "dag-suite", "ma-st-timeout", 0, 1, "ps-1", "coder")
	if err := s.Enqueue([]*subtask.Subtask{st}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	timeouts, err := s.CheckTimeouts(context.Background(), fixed.Add(2*time.Second))
	if err != nil {
		t.Fatalf("check timeouts: %v", err)
	}
	if len(timeouts) != 1 || timeouts[0].SubtaskID != st.SubtaskID {
		t.Fatalf("timeouts = %+v", timeouts)
	}
	item, _ := s.Item(st.SubtaskID)
	if item.Status != subtask.StatusFailed {
		t.Fatalf("status = %s, want failed", item.Status)
	}
	var timeoutEvent bool
	for _, e := range env.sink.snapshot() {
		if e.EventType == event.SubtaskFailed {
			if strings.Contains(string(e.Payload), `"category":"timeout"`) {
				timeoutEvent = true
			}
		}
	}
	if !timeoutEvent {
		t.Fatal("subtask.failed(category=timeout) event was not published")
	}

	prop := NewPropagator(DefaultQuotaConfig())
	st.Status = subtask.StatusFailed
	res, err := prop.Propagate(PropagationRequest{
		ParentTaskID: env.taskID,
		Sets:         []ParallelSet{{SetID: "ps-1", Nodes: []string{st.SubtaskID}, FailureStrategy: subtask.StrategyFailFast}},
		Subtasks:     []*subtask.Subtask{st},
		Failed:       []FailedSubtask{{SubtaskID: st.SubtaskID, Category: "timeout"}},
	})
	if err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if len(res.Decisions) != 1 || res.Decisions[0].Rollback.From != "d" || res.Decisions[0].Rollback.To != "c" {
		t.Fatalf("fail-fast rollback = %+v", res.Decisions)
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

func TestMultiAgentCircuitOpens(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-circuit")
	cfg := DefaultQuotaConfig()
	cfg.MaxParallelPerParent = 4
	s := NewScheduler(cfg, env.emitter)
	var batch []*subtask.Subtask
	for i := 0; i < 5; i++ {
		st := maSuiteSubtask(env.taskID, "dag-suite",
			fmt.Sprintf("ma-st-c%d", i), i, 1, "ps-1", "coder")
		// 熔断按 role|output paths|primary acceptance 归类，必须构造同类节点。
		st.OutputContract = map[string]any{
			"type": "artifacts", "paths": []string{"artifacts/shared/*.json"}, "estimated_tokens": 1024,
		}
		st.AcceptanceCriteria = []string{"go test ./..."}
		batch = append(batch, st)
	}
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, id := range []string{"ma-st-c0", "ma-st-c1", "ma-st-c2"} {
		if err := s.MarkFailed(context.Background(), id, "timeout"); err != nil {
			t.Fatalf("fail %s: %v", id, err)
		}
	}
	if err := s.MarkAccepted(context.Background(), "ma-st-c3", "acc-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	open := false
	for _, e := range env.sink.snapshot() {
		if e.EventType == event.SubtaskCircuitOpen {
			open = true
		}
	}
	if !open {
		t.Fatal("subtask.circuit_open was not published after 4 samples / 3 failures")
	}
	started, err := s.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch after open: %v", err)
	}
	if len(started) != 0 {
		t.Fatalf("open class was scheduled: %+v", started)
	}
	pending := s.Pending()
	if len(pending) != 1 || pending[0].SubtaskID != "ma-st-c4" {
		t.Fatalf("remaining same-class node must stay pending, got %+v", pending)
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

func TestMultiAgentFailFastCancelsSet(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-fail-fast")
	s := NewScheduler(DefaultQuotaConfig(), env.emitter)
	var batch []*subtask.Subtask
	for i := 0; i < 5; i++ {
		batch = append(batch, maSuiteSubtask(env.taskID, "dag-suite",
			fmt.Sprintf("ma-st-ff%d", i), i, 1, "ps-ff", "coder"))
	}
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := s.MarkFailed(context.Background(), "ma-st-ff0", "timeout"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	prop := NewPropagator(DefaultQuotaConfig())
	var all []*subtask.Subtask
	for _, st := range batch {
		item, _ := s.Item(st.SubtaskID)
		all = append(all, item)
	}
	res, err := prop.Propagate(PropagationRequest{
		ParentTaskID: env.taskID,
		Sets:         []ParallelSet{{SetID: "ps-ff", Nodes: []string{"ma-st-ff0", "ma-st-ff1", "ma-st-ff2", "ma-st-ff3", "ma-st-ff4"}, FailureStrategy: subtask.StrategyFailFast}},
		Subtasks:     all,
		Failed:       []FailedSubtask{{SubtaskID: "ma-st-ff0", Category: "timeout"}},
	})
	if err != nil {
		t.Fatalf("propagate: %v", err)
	}
	d := res.Decisions[0]
	if d.Action != ActionCancelSet || d.Rollback.From != "d" || d.Rollback.To != "c" {
		t.Fatalf("fail-fast decision = %+v", d)
	}
	if len(d.Cancelled) != 4 {
		t.Fatalf("cancelled = %v, want the other 4", d.Cancelled)
	}
	if err := prop.Apply(context.Background(), s, res); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, id := range d.Cancelled {
		item, _ := s.Item(id)
		if item.Status != subtask.StatusCancelled {
			t.Fatalf("%s status = %s, want cancelled", id, item.Status)
		}
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

func TestMultiAgentTolerateOneOfFive(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-tolerate")
	prop := NewPropagator(DefaultQuotaConfig())
	var all []*subtask.Subtask
	for i := 0; i < 5; i++ {
		st := maSuiteSubtask(env.taskID, "dag-suite", fmt.Sprintf("ma-st-tol%d", i), i, 1, "ps-tol", "coder")
		st.Status = subtask.StatusAccepted
		all = append(all, st)
	}
	all[0].Status = subtask.StatusFailed
	res, err := prop.Propagate(PropagationRequest{
		ParentTaskID: env.taskID,
		Sets: []ParallelSet{{
			SetID: "ps-tol", Nodes: []string{"ma-st-tol0", "ma-st-tol1", "ma-st-tol2", "ma-st-tol3", "ma-st-tol4"},
			FailureStrategy: subtask.StrategyTolerate, TolerateRatio: 0.2,
		}},
		Subtasks: all,
		Failed:   []FailedSubtask{{SubtaskID: "ma-st-tol0", Category: "timeout"}},
	})
	if err != nil {
		t.Fatalf("propagate: %v", err)
	}
	d := res.Decisions[0]
	if d.Action != ActionContinue || !d.Tolerated {
		t.Fatalf("tolerate(0.2) with 1/5 failures must continue: %+v", d)
	}
	if len(d.Failures) != 1 || d.Failures[0].SubtaskID != "ma-st-tol0" {
		t.Fatalf("aggregation must still flag the failed item: %+v", d.Failures)
	}
}

func TestMultiAgentRetryExhaustedDegradesToFailFast(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-retry")
	prop := NewPropagator(DefaultQuotaConfig())
	var all []*subtask.Subtask
	for i := 0; i < 3; i++ {
		st := maSuiteSubtask(env.taskID, "dag-suite", fmt.Sprintf("ma-st-r%d", i), i, 1, "ps-r", "coder")
		st.AttemptNo = 2 // RetryMaxAttempts = 2 -> 预算耗尽
		st.Status = subtask.StatusFailed
		all = append(all, st)
	}
	all[1].Status = subtask.StatusRunning
	res, err := prop.Propagate(PropagationRequest{
		ParentTaskID: env.taskID,
		Sets:         []ParallelSet{{SetID: "ps-r", Nodes: []string{"ma-st-r0", "ma-st-r1", "ma-st-r2"}, FailureStrategy: subtask.StrategyRetry}},
		Subtasks:     all,
		Failed:       []FailedSubtask{{SubtaskID: "ma-st-r0", Category: "error"}},
	})
	if err != nil {
		t.Fatalf("propagate: %v", err)
	}
	d := res.Decisions[0]
	if d.Action != ActionCancelSet || d.Rollback.From != "d" || d.Rollback.To != "c" {
		t.Fatalf("retry exhaustion must degrade to fail-fast: %+v", d)
	}
}

// ---------------------------------------------------------------------------
// 9.9 handoff
// ---------------------------------------------------------------------------

func maSuiteHandoff() *HandoffContract {
	h := validHandoff()
	h.HandoffID = "ma-handoff-1"
	h.ParentTaskID = "ma-task-handoff"
	h.SubtaskID = "ma-st-h1"
	h.TenantID = maSuiteTenant
	h.ProjectID = maSuiteProject
	h.From = HandoffParty{AgentID: "agent-a", Role: "coder", SubtaskID: "ma-st-h1"}
	return h
}

func TestMultiAgentHandoffIncomplete(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-handoff")
	h := maSuiteHandoff()
	h.Unfinished = nil
	service := NewHandoffService(nil, nil, NewMemoryHandoffStore(), env.emitter)
	_, _, err := service.Create(context.Background(), h, HandoffRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "c",
	}, nil)
	maSuiteCode(t, err, CodeHandoffIncomplete)
}

func TestMultiAgentHandoffNoSenderHistory(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-handoff")
	h := maSuiteHandoff()
	store := NewMemoryHandoffStore()
	transport := &maSuiteTransport{}
	service := NewHandoffService(nil, transport, store, env.emitter)
	record, receiverCtx, err := service.Create(context.Background(), h, HandoffRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "c",
	}, []InjectedItem{{Source: "runtime.injection:workspace", Content: "workspace=/workspace"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if record.Status != HandoffCreated || record.InputDigest == "" {
		t.Fatalf("unexpected record: %+v", record)
	}
	for _, msg := range receiverCtx.Messages {
		switch {
		case strings.HasPrefix(msg.Source, "contract."), strings.HasPrefix(msg.Source, "runtime.injection:"):
		default:
			t.Fatalf("receiver snapshot leaked a non-whitelisted source %q", msg.Source)
		}
	}
	for _, src := range receiverCtx.InjectedFrom {
		if strings.Contains(src, "executor_history") || strings.Contains(src, "history") {
			t.Fatalf("injected_from contains sender history: %q", src)
		}
	}
	if len(transport.envelopes) != 1 || transport.envelopes[0].HandoffID != h.HandoffID {
		t.Fatalf("handoff was not transported: %+v", transport.envelopes)
	}
	// handoff.* 事件必须带 handoff_id。
	var created bool
	for _, e := range env.sink.snapshot() {
		if e.EventType == event.HandoffCreated {
			created = true
			if e.HandoffID != h.HandoffID || !strings.Contains(string(e.Payload), h.HandoffID) {
				t.Fatalf("handoff.created missing handoff_id: %+v", e)
			}
		}
	}
	if !created {
		t.Fatal("handoff.created was not published")
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

func TestMultiAgentHandoffRejectsSharedHistory(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-handoff")
	h := maSuiteHandoff()
	service := NewHandoffService(nil, nil, NewMemoryHandoffStore(), env.emitter)
	for _, source := range []string{"executor_history", "runtime.history", "conversation", "workspace_snapshot"} {
		_, _, err := service.Create(context.Background(), h, HandoffRequest{
			ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "c",
		}, []InjectedItem{{Source: source, Content: "leak"}})
		maSuiteCode(t, err, CodeHandoffHistoryForbid)
	}
	var violation int
	for _, e := range env.sink.snapshot() {
		if e.EventType == event.SecurityViolation {
			violation++
		}
	}
	if violation < 4 {
		t.Fatalf("security.violation count = %d, want >= 4", violation)
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

func TestMultiAgentHandoffCrossTenantDenied(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-handoff")
	h := maSuiteHandoff()
	h.TenantID = "ma-suite-other"
	service := NewHandoffService(nil, nil, NewMemoryHandoffStore(), env.emitter)
	_, _, err := service.Create(context.Background(), h, HandoffRequest{
		ParentTaskID: env.taskID, TenantID: env.tenant, ProjectID: env.project, Stage: "c",
	}, nil)
	maSuiteCode(t, err, CodeCrossTenantDenied)
	var found bool
	for _, e := range env.sink.snapshot() {
		if e.EventType == event.SecurityViolation && strings.Contains(string(e.Payload), CodeCrossTenantDenied) {
			found = true
		}
	}
	if !found {
		t.Fatal("cross-tenant handoff must emit security.violation with E_CROSS_TENANT_DENIED")
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

// ValidateMemoryAccess 必须拒绝读取其它 handoff 的隔离命名空间。
func TestMultiAgentHandoffMemoryNamespaceIsolated(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-handoff")
	h := maSuiteHandoff()
	own := HandoffNamespace(h.HandoffID)
	if err := ValidateMemoryAccess(context.Background(), h, own+"notes", env.emitter); err != nil {
		t.Fatalf("own namespace must be readable: %v", err)
	}
	err := ValidateMemoryAccess(context.Background(), h, "handoff/other/notes", env.emitter)
	maSuiteCode(t, err, CodeHandoffHistoryForbid)
	var violation bool
	for _, e := range env.sink.snapshot() {
		if e.EventType == event.SecurityViolation {
			violation = true
		}
	}
	if !violation {
		t.Fatal("cross-namespace read must emit security.violation")
	}
}

// ---------------------------------------------------------------------------
// 9.9 汇聚
// ---------------------------------------------------------------------------

type maSuiteFakeVerifier struct{ missing string }

func (v maSuiteFakeVerifier) Verify(_ context.Context, path, _ string) (ArtifactCheck, error) {
	if path == v.missing {
		return ArtifactCheck{Exists: false}, nil
	}
	return ArtifactCheck{Exists: true, ActualHash: "sha256:ok", HashMatch: true}, nil
}

func maSuiteAcceptedSt(id, node string, index int) *subtask.Subtask {
	st := maSuiteSubtask("ma-task-aggregate", "dag-suite", id, index, 1, "ps-agg", "coder")
	st.DagNodeID = node
	st.Status = subtask.StatusAccepted
	st.AcceptanceID = "acc-" + id
	st.ArtifactRefs = []subtask.ArtifactRef{{Path: "artifacts/" + node + "/out.json", Hash: "sha256:ok"}}
	return st
}

func TestMultiAgentAggregationTenItems(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-aggregate")
	agg := NewAggregator(maSuiteFakeVerifier{}, env.emitter)
	var items []*subtask.Subtask
	for i := 0; i < 10; i++ {
		items = append(items, maSuiteAcceptedSt(fmt.Sprintf("ma-st-agg%02d", i), fmt.Sprintf("node-%02d", i), i))
	}
	report, err := agg.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: env.taskID, DagID: "dag-suite", TenantID: env.tenant, ProjectID: env.project, Subtasks: items,
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(report.Items) != 10 || report.Summary.Accepted != 10 || !report.Complete {
		t.Fatalf("report = %+v", report)
	}
	for _, item := range report.Items {
		if item.Verdict != "pass" || len(item.Artifacts) != 1 || item.Artifacts[0].Hash == "" {
			t.Fatalf("artifact reference missing: %+v", item)
		}
	}
	if err := agg.RequireForAcceptance(report); err != nil {
		t.Fatalf("d-stage acceptance must accept the report: %v", err)
	}
	injected := AcceptanceInput(report)
	if injected["aggregation_report"] == nil || injected["items"] == nil {
		t.Fatalf("acceptance input is incomplete: %+v", injected)
	}
	assertSeqContinuous(t, env.sink, env.taskID)
}

func TestMultiAgentAggregationMissingReport(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-aggregate")
	agg := NewAggregator(maSuiteFakeVerifier{}, env.emitter)
	maSuiteCode(t, agg.RequireForAcceptance(nil), CodeAggregationMissing)
	maSuiteCode(t, agg.RequireForAcceptance(&AggregationReport{}), CodeAggregationMissing)
}

func TestMultiAgentAggregationRejectsNonAccepted(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-aggregate")
	agg := NewAggregator(maSuiteFakeVerifier{}, env.emitter)
	pending := maSuiteSubtask(env.taskID, "dag-suite", "ma-st-pending", 0, 1, "ps-agg", "coder")
	report, err := agg.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: env.taskID, DagID: "dag-suite", TenantID: env.tenant, ProjectID: env.project,
		Subtasks: []*subtask.Subtask{pending},
	})
	maSuiteCode(t, err, CodeSubtaskNotAccepted)
	if report == nil || report.Complete {
		t.Fatalf("non-accepted subtask must make the report incomplete: %+v", report)
	}
	var violation bool
	for _, e := range env.sink.snapshot() {
		if e.EventType == event.SecurityViolation {
			violation = true
		}
	}
	if !violation {
		t.Fatal("non-accepted artifact must emit security.violation")
	}
}

// 同路径多写者与契约不匹配必须判为冲突，且不能算 pass。
func TestMultiAgentAggregationConflict(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-aggregate")
	agg := NewAggregator(maSuiteFakeVerifier{}, env.emitter)
	a := maSuiteAcceptedSt("ma-st-x1", "node-x1", 0)
	b := maSuiteAcceptedSt("ma-st-x2", "node-x2", 1)
	b.ArtifactRefs = []subtask.ArtifactRef{{Path: a.ArtifactRefs[0].Path, Hash: "sha256:ok"}}
	report, err := agg.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: env.taskID, DagID: "dag-suite", TenantID: env.tenant, ProjectID: env.project,
		Subtasks: []*subtask.Subtask{a, b},
	})
	maSuiteCode(t, err, CodeAggregationIncomplete)
	if report == nil || len(report.Conflicts) == 0 || report.Conflicts[0].Kind != ConflictSamePathMultiWriter {
		t.Fatalf("same-path conflict not detected: %+v", report)
	}
	maSuiteCode(t, agg.RequireForAcceptance(report), CodeAggregationIncomplete)
}

// ---------------------------------------------------------------------------
// 9.9 隔离 / 预算 / 事件关联
// ---------------------------------------------------------------------------

func TestMultiAgentTenantIsolation(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-isolation")
	maSuiteSeedTask(t, env.db, env.taskID, "b")
	repo := subtask.NewRepo(env.db)
	st := maSuiteSubtask(env.taskID, "dag-suite", "ma-st-iso", 0, 1, "ps-1", "coder")
	if err := repo.Insert(context.Background(), st); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 用另一个租户的父任务 id 查询：不得泄露任何行。
	other, err := repo.ListByParent(context.Background(), "ma-task-not-ours", "")
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("cross-tenant query leaked %d rows", len(other))
	}
	// scheduler 必须拒绝跨租户拼接的批次。
	a := maSuiteSubtask(env.taskID, "dag-suite", "ma-st-iso-a", 0, 1, "ps-1", "coder")
	b := maSuiteSubtask(env.taskID, "dag-suite", "ma-st-iso-b", 1, 1, "ps-1", "coder")
	b.TenantID = "ma-suite-other"
	s := NewScheduler(DefaultQuotaConfig(), nil)
	maSuiteCode(t, s.Enqueue([]*subtask.Subtask{a, b}), CodeCrossTenantDenied)
}

func TestMultiAgentBudgetRollsUpToParent(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-budget")
	maSuiteSeedTask(t, env.db, env.taskID, "c")
	repo := subtask.NewRepo(env.db)
	st := maSuiteSubtask(env.taskID, "dag-suite", "ma-st-budget", 0, 1, "ps-1", "coder")
	if err := repo.Insert(context.Background(), st); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.AddConsumed(context.Background(), st.SubtaskID, 1200, 0.75); err != nil {
		t.Fatalf("add consumed: %v", err)
	}
	var parentTokens int64
	var parentCost float64
	if err := env.db.QueryRow(context.Background(),
		`SELECT consumed_tokens, consumed_cost FROM hlp_task.task_runs WHERE task_id=$1`, env.taskID).
		Scan(&parentTokens, &parentCost); err != nil {
		t.Fatalf("read parent budget: %v", err)
	}
	if parentTokens != 1200 || parentCost < 0.749 || parentCost > 0.751 {
		t.Fatalf("parent budget = (%d,%f), want (1200,0.75)", parentTokens, parentCost)
	}
	// 子任务表不存在独立预算账户。
	rows, err := env.db.Query(context.Background(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema='hlp_task' AND table_name='subtasks'
		  AND column_name LIKE 'budget\_%'`)
	if err != nil {
		t.Fatalf("inspect columns: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("subtasks must not own an independent budget_* account")
	}
	// 父预算池唯一：租约释放后 token 归还父级。
	q := NewParentQuota(DefaultQuotaConfig())
	if !q.Acquire(1000) {
		t.Fatal("quota acquire failed")
	}
	if q.RemainingTokens() != DefaultQuotaConfig().ParentBudgetTokens-1000 {
		t.Fatalf("remaining = %d", q.RemainingTokens())
	}
	q.Reclaim(1000)
	if q.RemainingTokens() != DefaultQuotaConfig().ParentBudgetTokens {
		t.Fatalf("reclaim did not return tokens: %d", q.RemainingTokens())
	}
}

func TestMultiAgentEventCorrelation(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-events")
	s := NewScheduler(DefaultQuotaConfig(), env.emitter)
	var batch []*subtask.Subtask
	for i := 0; i < 2; i++ {
		st := maSuiteSubtask(env.taskID, "dag-suite", fmt.Sprintf("ma-st-ev%d", i), i, 1, "ps-ev", "coder")
		batch = append(batch, st)
	}
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := s.MarkAccepted(context.Background(), "ma-st-ev0", "acc-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := s.MarkFailed(context.Background(), "ma-st-ev1", "error"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	// 通过 parent_task_id 查询事件，可还原 DAG 节点、并行区间与执行顺序。
	var started []*event.Event
	for _, e := range env.sink.snapshot() {
		if e.TaskID != env.taskID && e.ParentTaskID != env.taskID {
			t.Fatalf("event escaped the parent task: %+v", e)
		}
		if e.EventType == event.SubtaskStarted {
			started = append(started, e)
		}
	}
	if len(started) != 2 {
		t.Fatalf("started events = %d, want 2", len(started))
	}
	for _, e := range started {
		if e.DagID != "dag-suite" || e.DagNodeID == "" || e.ParentTaskID != env.taskID {
			t.Fatalf("started event lost DAG correlation: %+v", e)
		}
	}
	if n := assertSeqContinuous(t, env.sink, env.taskID); n < 4 {
		t.Fatalf("expected at least 4 correlated events, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type maSuiteTransport struct {
	envelopes []HandoffEnvelope
}

func (f *maSuiteTransport) SendHandoff(_ context.Context, envelope HandoffEnvelope) error {
	f.envelopes = append(f.envelopes, envelope)
	return nil
}
// 9.9 并发性能口径：12 节点并行集在 8 并发上限下完成一次 Dispatch 的耗时。
// design 9.1 的调度门槛为 P95 ≤ 300ms；本用例留档实测区间。
func TestMultiAgentDispatchLatency(t *testing.T) {
	env := maSuiteNewEnv(t, "ma-task-dispatch-latency")
	var samples []time.Duration
	for round := 0; round < 50; round++ {
		s := NewScheduler(DefaultQuotaConfig(), env.emitter)
		batch := make([]*subtask.Subtask, 0, 12)
		for i := 0; i < 12; i++ {
			batch = append(batch, maSuiteSubtask(env.taskID, "dag-suite",
				fmt.Sprintf("ma-lat-%02d-%02d", round, i), i, 1, "ps-lat", "coder"))
		}
		if err := s.Enqueue(batch); err != nil {
			t.Fatalf("enqueue round %d: %v", round, err)
		}
		start := time.Now()
		started, err := s.Dispatch(context.Background())
		samples = append(samples, time.Since(start))
		if err != nil {
			t.Fatalf("dispatch round %d: %v", round, err)
		}
		if len(started) != 8 {
			t.Fatalf("round %d started = %d, want 8", round, len(started))
		}
	}
	p95 := maSuiteP95(samples)
	var min, max time.Duration = samples[0], samples[0]
	for _, d := range samples {
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	t.Logf("DAG dispatch latency: n=%d P95=%v min=%v max=%v", len(samples), p95, min, max)
	if p95 > 300*time.Millisecond {
		t.Fatalf("dispatch P95 = %v, must be <= 300ms", p95)
	}
}

func maSuiteP95(samples []time.Duration) time.Duration {
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
