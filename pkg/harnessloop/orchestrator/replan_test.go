package orchestrator

// T-P2-10 · design 4.13.2/4.13.4/4.13.5 多 Agent 高级场景回归。
//
// 覆盖验收条件：
//   - 重规划生成新 dag_id，旧 DAG 标 superseded 且仍可查；
//   - 嵌套展开到 depth=2，第 3 层被拒（E_SUBTASK_DEPTH_EXCEEDED）；
//   - 跨阶段 handoff（c→d）五要素齐备才放行，缺要素/越阶被拒；
//   - 汇聚兼容多 DAG 版本，未知版本被拒；
//   - 重规划事件携带新旧 dag_id，并复位该类熔断（审计 source=dag_replan）。

import (
	"context"
	"testing"

	"agent-platform/pkg/harnessloop/subtask"
)

func replanNode(id, role string) PlanNode {
	node := validNode(id, role)
	node.OutputContract.Paths = []string{"src/" + id + "/**"}
	return node
}

func replanPlan(dagID string, nodes ...PlanNode) *Plan {
	edges := make([]PlanEdge, 0, len(nodes))
	for i := 1; i < len(nodes); i++ {
		edges = append(edges, PlanEdge{From: nodes[0].NodeID, To: nodes[i].NodeID})
	}
	return &Plan{DagID: dagID, Nodes: nodes, Edges: edges}
}

func replanFixture(t *testing.T) (*Replanner, *MemoryDAGStore, *MemoryEmitter, *DAGRecord) {
	t.Helper()
	emitter := NewMemoryEmitter()
	planner := NewPlanner(DefaultPlanConfig(), emitter)
	store := NewMemoryDAGStore()
	replanner := NewReplanner(planner, store, emitter)
	ctx := context.Background()

	initial, err := replanner.RecordInitial(ctx, PlanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
	}, mustPlan(t, planner, "dag-1", replanNode("n1", "coder"), replanNode("n2", "coder"), replanNode("n3", "verifier")))
	if err != nil {
		t.Fatalf("record initial: %v", err)
	}
	return replanner, store, emitter, initial
}

func mustPlan(t *testing.T, planner *Planner, dagID string, nodes ...PlanNode) *PlanResult {
	t.Helper()
	result, err := planner.Plan(context.Background(), PlanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		DagID:        dagID,
		Plan:         replanPlan(dagID, nodes...),
	})
	if err != nil {
		t.Fatalf("plan %s: %v", dagID, err)
	}
	return result
}

// TestReplanCreatesNewDagAndKeepsOldQueryable 是 T-P2-10 的主验收用例。
func TestReplanCreatesNewDagAndKeepsOldQueryable(t *testing.T) {
	replanner, store, emitter, initial := replanFixture(t)
	ctx := context.Background()

	result, err := replanner.Replan(ctx, ReplanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		OldDagID:     initial.DagID,
		Reason:       "d 阶段验收判定 plan_gap：缺客户端生成节点",
		Actor:        "orchestrator",
		Bucket:       "plan_gap",
		Plan:         replanPlan("", replanNode("n1", "coder"), replanNode("n4", "coder")),
	})
	if err != nil {
		t.Fatalf("replan: %v", err)
	}
	if result.NewDagID == "" || result.NewDagID == initial.DagID {
		t.Fatalf("replan must allocate a fresh dag_id, got %q", result.NewDagID)
	}
	if result.New.Revision != initial.Revision+1 {
		t.Fatalf("revision = %d, want %d", result.New.Revision, initial.Revision+1)
	}
	for _, st := range result.Subtasks {
		if st.DagID != result.NewDagID {
			t.Fatalf("subtask %s belongs to %s, want %s", st.SubtaskID, st.DagID, result.NewDagID)
		}
		if st.Status != subtask.StatusPending {
			t.Fatalf("new subtask %s must start pending, got %s", st.SubtaskID, st.Status)
		}
	}

	// 旧 DAG 可查，且保留完整快照与 superseded 标记。
	old, err := store.Get(ctx, initial.DagID, "tenant-1", "project-1")
	if err != nil {
		t.Fatalf("old dag must stay queryable: %v", err)
	}
	if old.Status != DAGStatusSuperseded || old.SupersededBy != result.NewDagID {
		t.Fatalf("old dag status = %s superseded_by=%s", old.Status, old.SupersededBy)
	}
	if old.Plan == nil || len(old.Plan.Nodes) != 3 {
		t.Fatalf("old dag snapshot must keep the plan, got %+v", old.Plan)
	}
	if len(old.Subtasks) != 3 {
		t.Fatalf("old dag snapshot must keep its subtasks, got %d", len(old.Subtasks))
	}
	for _, st := range old.Subtasks {
		if st.Status != subtask.StatusCancelled {
			t.Fatalf("old subtask %s must be cancelled, got %s", st.SubtaskID, st.Status)
		}
	}
	if len(result.Cancelled) != 3 {
		t.Fatalf("cancelled list = %v, want 3 ids", result.Cancelled)
	}

	versions, err := store.ListByParent(ctx, "task-1", "tenant-1", "project-1")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 || versions[0].DagID != initial.DagID || versions[1].DagID != result.NewDagID {
		t.Fatalf("unexpected DAG versions: %+v", versions)
	}

	// 事件带新旧 dag_id。
	plans := emitter.Find(EventSubtaskPlanCreated)
	var replanned *Event
	for i := range plans {
		if plans[i].Payload["replanned"] == true {
			replanned = &plans[i]
		}
	}
	if replanned == nil {
		t.Fatalf("replan must emit subtask.plan_created with replanned=true")
	}
	if replanned.Payload["old_dag_id"] != initial.DagID || replanned.Payload["new_dag_id"] != result.NewDagID {
		t.Fatalf("replan event must carry both dag ids: %+v", replanned.Payload)
	}
	if replanned.OldDagID != initial.DagID || replanned.NewDagID != result.NewDagID {
		t.Fatalf("replan event struct must carry both dag ids: %+v", replanned)
	}

	// 旧 DAG 不能被重规划第二次，也不能被别的租户查到。
	assertCode(t, secondReplan(replanner, initial.DagID), CodeDAGNotActive)
	if _, err := store.Get(ctx, initial.DagID, "tenant-2", "project-1"); CodeDAGNotFound != ErrorCode(err) {
		t.Fatalf("cross-tenant dag read must look like not-found, got %v", err)
	}
}

func secondReplan(replanner *Replanner, oldDagID string) error {
	_, err := replanner.Replan(context.Background(), ReplanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		OldDagID:     oldDagID,
		Reason:       "second attempt",
		Plan:         replanPlan("", replanNode("n1", "coder")),
	})
	return err
}

// TestReplanRejectsSameDagID 验证"必须换 dag_id"这条硬规则。
func TestReplanRejectsSameDagID(t *testing.T) {
	replanner, _, _, initial := replanFixture(t)
	_, err := replanner.Replan(context.Background(), ReplanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		OldDagID:     initial.DagID,
		Reason:       "忘改 dag_id 的计划",
		Plan:         replanPlan(initial.DagID, replanNode("n1", "coder")),
	})
	assertCode(t, err, CodeReplanSameDag)
}

// TestReplanNestedDepthTwo 验证 depth=2 正确展开、第 3 层被拒。
func TestReplanNestedDepthTwo(t *testing.T) {
	replanner, store, _, initial := replanFixture(t)
	ctx := context.Background()

	parent := replanNode("n1", "coder")
	childA := replanNode("n1-a", "coder")
	childB := replanNode("n1-b", "coder")
	parent.Children = []PlanNode{childA, childB}
	nested := replanPlan("", parent, replanNode("n2", "verifier"))

	result, err := replanner.Replan(ctx, ReplanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		OldDagID:     initial.DagID,
		Reason:       "拆细 n1",
		Plan:         nested,
	})
	if err != nil {
		t.Fatalf("nested replan: %v", err)
	}
	depths := map[string]int{}
	for _, st := range result.Subtasks {
		depths[st.DagNodeID] = st.Depth
	}
	if depths["n1"] != 1 || depths["n2"] != 1 {
		t.Fatalf("top-level nodes must be depth 1: %+v", depths)
	}
	if depths["n1-a"] != 2 || depths["n1-b"] != 2 {
		t.Fatalf("declared children must expand to depth 2: %+v", depths)
	}
	expanded := result.New.Plan.Expand()
	if len(expanded) != 4 {
		t.Fatalf("expand must list 4 nodes, got %d", len(expanded))
	}
	if expanded[1].ParentNodeID != "n1" || expanded[1].Depth != 2 {
		t.Fatalf("expanded child must carry its parent and depth: %+v", expanded[1])
	}
	if result.New.Plan.MaxDepthUsed() != 2 {
		t.Fatalf("MaxDepthUsed = %d, want 2", result.New.Plan.MaxDepthUsed())
	}

	// 第 3 层必须被拒，且旧 DAG 不会被改成 superseded（失败不产生半状态）。
	third := replanNode("n1", "coder")
	deepChild := replanNode("n1-a", "coder")
	deepChild.Children = []PlanNode{replanNode("n1-a-i", "coder")}
	third.Children = []PlanNode{deepChild}
	_, err = replanner.Replan(ctx, ReplanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		OldDagID:     result.NewDagID,
		Reason:       "三层拆分",
		Plan:         replanPlan("", third),
	})
	assertCode(t, err, CodeSubtaskDepthExceeded)

	active, err := store.Get(ctx, result.NewDagID, "tenant-1", "project-1")
	if err != nil || active.Status != DAGStatusActive {
		t.Fatalf("a rejected replan must leave the current DAG active: %+v err=%v", active, err)
	}
}

// TestReplanCrossStageHandoff c→d 交接：五要素齐备放行，缺要素/越阶拒绝。
func TestReplanCrossStageHandoff(t *testing.T) {
	emitter := NewMemoryEmitter()
	transport := &fakeTransport{}
	service := NewHandoffService(nil, transport, NewMemoryHandoffStore(), emitter)
	ctx := context.Background()

	contract := validHandoff()
	req := handoffReq()
	req.Stage = "c"
	req.ToStage = "d"
	record, receiverCtx, err := service.Create(ctx, contract, req, nil)
	if err != nil {
		t.Fatalf("c→d handoff must be accepted: %v", err)
	}
	if record.FromStage != "c" || record.ToStage != "d" {
		t.Fatalf("record must keep the stage pair: %+v", record)
	}
	if len(transport.envelopes) != 1 {
		t.Fatalf("expected one envelope, got %d", len(transport.envelopes))
	}
	meta := transport.envelopes[0].Metadata
	if meta["from_stage"] != "c" || meta["to_stage"] != "d" {
		t.Fatalf("a2a metadata must carry the stage pair: %+v", meta)
	}
	if receiverCtx.InputDigest == "" {
		t.Fatalf("receiver context must be frozen with a digest")
	}
	created := emitter.Find(EventHandoffCreated)
	if len(created) != 1 || created[0].Payload["cross_stage"] != true {
		t.Fatalf("handoff.created must mark cross_stage: %+v", created)
	}

	// 五要素缺一项（背景为空）→ E_HANDOFF_INCOMPLETE，交接被拒。
	incomplete := validHandoff()
	incomplete.HandoffID = "handoff-2"
	incomplete.ContextSummary.Background = ""
	_, _, err = service.Create(ctx, incomplete, req, nil)
	assertCode(t, err, CodeHandoffIncomplete)

	// 未完成项为空 → 拒绝（没有未完成项就不该 handoff）。
	noUnfinished := validHandoff()
	noUnfinished.HandoffID = "handoff-3"
	noUnfinished.Unfinished = nil
	_, _, err = service.Create(ctx, noUnfinished, req, nil)
	assertCode(t, err, CodeHandoffIncomplete)

	// c→b 不是合法跨阶段转移。
	backwards := handoffReq()
	backwards.Stage = "c"
	backwards.ToStage = "b"
	assertCode(t, ValidateCrossStageHandoff(backwards), CodeCrossStageDenied)
	// 未声明发送阶段却要跨阶段 → 拒绝（不允许隐式"下一阶段"）。
	implicit := handoffReq()
	implicit.Stage = ""
	implicit.ToStage = "d"
	assertCode(t, ValidateCrossStageHandoff(implicit), CodeCrossStageDenied)
	// 同阶段 handoff 不受跨阶段规则限制。
	sameStage := handoffReq()
	sameStage.Stage = "c"
	if err := ValidateCrossStageHandoff(sameStage); err != nil {
		t.Fatalf("same-stage handoff must pass: %v", err)
	}
}

// TestReplanAggregationAcrossDagVersions 验证汇聚兼容多 DAG 版本。
func TestReplanAggregationAcrossDagVersions(t *testing.T) {
	root := t.TempDir()
	emitter := NewMemoryEmitter()
	aggregator := NewAggregator(FSVerifier{Root: root}, emitter)
	ctx := context.Background()

	oldHash := writeArtifact(t, root, "out/old.txt", "old")
	newHash := writeArtifact(t, root, "out/new.txt", "new")
	oldAccepted := acceptedSubtask("st-old", "n1", 0, subtask.ArtifactRef{Path: "out/old.txt", Hash: oldHash})
	newAccepted := acceptedSubtask("st-new", "n1", 1, subtask.ArtifactRef{Path: "out/new.txt", Hash: newHash})
	newAccepted.DagID = "dag-2"

	report, err := aggregator.Aggregate(ctx, AggregationRequest{
		ParentTaskID: "task-1",
		DagID:        "dag-2",
		DagIDs:       []string{"dag-1", "dag-2"},
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		Subtasks:     []*subtask.Subtask{oldAccepted, newAccepted},
	})
	if err != nil {
		t.Fatalf("multi-dag aggregate: %v", err)
	}
	if len(report.DagIDs) != 2 || report.DagVersions != 2 {
		t.Fatalf("report must cover both dag versions: %+v", report)
	}
	if report.Summary.Accepted != 2 || len(report.Items) != 2 {
		t.Fatalf("both versions must be aggregated: %+v", report.Summary)
	}
	seen := map[string]bool{}
	for _, item := range report.Items {
		seen[item.DagID] = true
	}
	if !seen["dag-1"] || !seen["dag-2"] {
		t.Fatalf("items must keep their own dag_id: %+v", report.Items)
	}

	// 未知 DAG 版本的产物不得计入汇聚。
	stray := acceptedSubtask("st-stray", "n9", 2, subtask.ArtifactRef{Path: "out/new.txt", Hash: newHash})
	stray.DagID = "dag-3"
	_, err = aggregator.Aggregate(ctx, AggregationRequest{
		ParentTaskID: "task-1",
		DagID:        "dag-2",
		DagIDs:       []string{"dag-1", "dag-2"},
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		Subtasks:     []*subtask.Subtask{newAccepted, stray},
	})
	assertCode(t, err, CodeAggregationDagMismatch)
}

// TestReplanResetsCircuitWithAudit 验证重规划是合法复位路径且留审计。
func TestReplanResetsCircuitWithAudit(t *testing.T) {
	cfg := DefaultQuotaConfig()
	cfg.MaxParallelPerParent = 4
	audit := NewMemorySchedulerAudit()
	scheduler := NewScheduler(cfg, NewMemoryEmitter()).WithAuditSink(audit)
	if err := scheduler.Enqueue(fairnessBatch(t, "task-1", 5)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ctx := context.Background()
	if _, err := scheduler.Dispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, id := range []string{"task-1-st-00", "task-1-st-01", "task-1-st-02"} {
		if err := scheduler.MarkFailed(ctx, id, "timeout"); err != nil {
			t.Fatalf("fail %s: %v", id, err)
		}
	}
	if err := scheduler.MarkAccepted(ctx, "task-1-st-03", "acc-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if len(scheduler.CircuitStates()) != 1 || !scheduler.CircuitStates()[0].Open {
		t.Fatalf("expected the class to be open before the replan")
	}

	emitter := NewMemoryEmitter()
	planner := NewPlanner(DefaultPlanConfig(), emitter)
	store := NewMemoryDAGStore()
	replanner := NewReplanner(planner, store, emitter).WithCircuitResetter(scheduler)
	initial, err := replanner.RecordInitial(ctx, PlanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
	}, mustPlan(t, planner, "dag-1", replanNode("n1", "coder")))
	if err != nil {
		t.Fatalf("record initial: %v", err)
	}

	result, err := replanner.Replan(ctx, ReplanRequest{
		ParentTaskID: "task-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		OldDagID:     initial.DagID,
		Reason:       "换实现思路",
		Actor:        "oncall-li",
		Plan:         replanPlan("", replanNode("n9", "coder")),
	})
	if err != nil {
		t.Fatalf("replan: %v", err)
	}
	if len(result.ResetClasses) != 1 {
		t.Fatalf("replan must reset the open class, got %v", result.ResetClasses)
	}
	if len(scheduler.CircuitStates()) == 1 && scheduler.CircuitStates()[0].Open {
		t.Fatalf("class must be closed after the replan")
	}
	entries := audit.FindByAction(AuditActionCircuitReset)
	if len(entries) != 1 {
		t.Fatalf("expected one reset audit entry, got %d", len(entries))
	}
	if entries[0].Source != CircuitResetSourceReplan || entries[0].DagID != result.NewDagID {
		t.Fatalf("replan reset audit must carry source=dag_replan and the new dag: %+v", entries[0])
	}
}
