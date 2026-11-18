package orchestrator

import (
	"context"
	"testing"

	"agent-platform/pkg/harnessloop/subtask"
)

// edges 写的是 node_id，subtask 拿到的是新生成的 ULID。BuildSubtasks 必须把依赖
// 翻译成 SubtaskID，否则 Scheduler.Enqueue 按 SubtaskID 索引时判 E_DAG_INCOMPLETE
// ——单节点计划没有边，这条路径长期没人走过。
func TestBuildSubtasksTranslatesEdgeDependencies(t *testing.T) {
	plan := validPlan()
	subtasks, err := plan.BuildSubtasks("task-1", "tenant-1", "project-1")
	if err != nil {
		t.Fatalf("build subtasks: %v", err)
	}
	if len(subtasks) != 3 {
		t.Fatalf("expected 3 subtasks, got %d", len(subtasks))
	}

	byNode := make(map[string]string, len(subtasks))
	for _, st := range subtasks {
		byNode[st.DagNodeID] = st.SubtaskID
	}
	for _, st := range subtasks {
		for _, dep := range st.DependsOn {
			if dep == "n1" || dep == "n2" || dep == "n3" {
				t.Fatalf("subtask %s keeps the raw dag node id %q in DependsOn", st.SubtaskID, dep)
			}
			if dep != byNode["n1"] && dep != st.SubtaskID {
				t.Fatalf("subtask %s depends on unknown id %q", st.SubtaskID, dep)
			}
		}
	}
	for _, node := range []string{"n2", "n3"} {
		st := subtaskByNode(t, subtasks, node)
		if len(st.DependsOn) != 1 || st.DependsOn[0] != byNode["n1"] {
			t.Fatalf("node %s must depend on n1's subtask id, got %v", node, st.DependsOn)
		}
	}

	sched := NewSchedulerWithGate(DefaultQuotaConfig(), NewTenantGate(0), NewMemoryEmitter())
	if err := sched.Enqueue(subtasks); err != nil {
		t.Fatalf("scheduler must accept a 3-node plan with edges: %v", err)
	}
	running, err := sched.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("only the dependency-free node may start first, started %d", len(running))
	}
}

func subtaskByNode(t *testing.T, list []*subtask.Subtask, node string) *subtask.Subtask {
	t.Helper()
	for _, st := range list {
		if st.DagNodeID == node {
			return st
		}
	}
	t.Fatalf("no subtask for dag node %s", node)
	return nil
}
