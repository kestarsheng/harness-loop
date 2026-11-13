package orchestrator

import (
	"context"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
)

func testSubtask(parent, dag, id string, index, depth int, setID, role string, paths ...string) *subtask.Subtask {
	return &subtask.Subtask{
		SubtaskID:       id,
		ParentTaskID:    parent,
		TenantID:        "tenant-1",
		ProjectID:       "project-1",
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
			"paths":            paths,
			"estimated_tokens": 1024,
		},
		AcceptanceCriteria: []string{"go build ./..."},
	}
}

func TestSchedulerCapsParallelismAtEight(t *testing.T) {
	emitter := NewMemoryEmitter()
	s := NewScheduler(DefaultQuotaConfig(), emitter)
	var batch []*subtask.Subtask
	for i := 0; i < 12; i++ {
		batch = append(batch, testSubtask("task-1", "dag-1", "st-"+string(rune('a'+i)), i, 1, "p1", "coder", "src/a/*.go"))
	}
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	started, err := s.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(started) != 8 {
		t.Fatalf("expected 8 running, got %d", len(started))
	}
	if got := len(s.Running()); got != 8 {
		t.Fatalf("running = %d, want 8", got)
	}
	if got := len(s.Pending()); got != 4 {
		t.Fatalf("pending = %d, want 4", got)
	}
	parent, tenant := s.ActiveSlots()
	if parent != 8 || tenant != 8 {
		t.Fatalf("slots = (%d,%d), want (8,8)", parent, tenant)
	}
	if len(emitter.Find(EventSubtaskQueued)) == 0 {
		t.Fatalf("expected subtask.queued when over the parent cap")
	}
	if len(emitter.Find(EventSubtaskStarted)) != 8 {
		t.Fatalf("expected 8 subtask.started events")
	}
}

func TestSchedulerTenantGateQueues(t *testing.T) {
	gate := NewTenantGate(20)
	for i := 0; i < 18; i++ {
		if !gate.TryAcquire() {
			t.Fatalf("pre-acquire %d failed", i)
		}
	}
	emitter := NewMemoryEmitter()
	s := NewSchedulerWithGate(DefaultQuotaConfig(), gate, emitter)
	var batch []*subtask.Subtask
	for i := 0; i < 4; i++ {
		batch = append(batch, testSubtask("task-1", "dag-1", "st-"+string(rune('a'+i)), i, 1, "p1", "coder", "src/a/*.go"))
	}
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	started, err := s.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(started) != 2 {
		t.Fatalf("expected 2 started under the tenant gate, got %d", len(started))
	}
	if gate.Active() != 20 {
		t.Fatalf("tenant active = %d, want 20", gate.Active())
	}
	queued := emitter.Find(EventSubtaskQueued)
	if len(queued) != 2 {
		t.Fatalf("expected 2 subtask.queued events, got %d", len(queued))
	}
	if queued[0].Payload["position"] != 1 || queued[1].Payload["position"] != 2 {
		t.Fatalf("unexpected queue positions: %+v", queued)
	}
}

func TestSchedulerCircuitOpensAfterFourSamples(t *testing.T) {
	cfg := DefaultQuotaConfig()
	cfg.MaxParallelPerParent = 4
	emitter := NewMemoryEmitter()
	s := NewScheduler(cfg, emitter)
	var batch []*subtask.Subtask
	for i := 0; i < 5; i++ {
		batch = append(batch, testSubtask("task-1", "dag-1", "st-"+string(rune('a'+i)), i, 1, "p1", "coder", "src/a/**"))
	}
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if started, err := s.Dispatch(context.Background()); err != nil || len(started) != 4 {
		t.Fatalf("dispatch: started=%d err=%v", len(started), err)
	}
	ctx := context.Background()
	if err := s.MarkFailed(ctx, "st-a", "timeout"); err != nil {
		t.Fatalf("fail a: %v", err)
	}
	if err := s.MarkFailed(ctx, "st-b", "timeout"); err != nil {
		t.Fatalf("fail b: %v", err)
	}
	if err := s.MarkFailed(ctx, "st-c", "timeout"); err != nil {
		t.Fatalf("fail c: %v", err)
	}
	if err := s.MarkAccepted(ctx, "st-d", "acc-1"); err != nil {
		t.Fatalf("accept d: %v", err)
	}
	if len(emitter.Find(EventSubtaskCircuitOpen)) != 1 {
		t.Fatalf("expected one subtask.circuit_open event, got %d", len(emitter.Find(EventSubtaskCircuitOpen)))
	}
	started, err := s.Dispatch(ctx)
	if err != nil {
		t.Fatalf("dispatch after open: %v", err)
	}
	if len(started) != 0 {
		t.Fatalf("circuit-open class must not be scheduled, started %d", len(started))
	}
	if pending := s.Pending(); len(pending) != 1 || pending[0].SubtaskID != "st-e" {
		t.Fatalf("expected st-e to stay pending, got %+v", pending)
	}

	s.ResetCircuit()
	started, err = s.Dispatch(ctx)
	if err != nil {
		t.Fatalf("dispatch after reset: %v", err)
	}
	if len(started) != 1 || started[0].SubtaskID != "st-e" {
		t.Fatalf("reset circuit should reschedule st-e, got %+v", started)
	}
}

func TestSchedulerTimeout(t *testing.T) {
	emitter := NewMemoryEmitter()
	s := NewScheduler(DefaultQuotaConfig(), emitter)
	fixed := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return fixed })
	s.SetParentRemaining(500 * time.Millisecond)
	batch := []*subtask.Subtask{
		testSubtask("task-1", "dag-1", "st-a", 0, 1, "p1", "coder", "src/a/*.go"),
		testSubtask("task-1", "dag-1", "st-b", 1, 1, "p1", "coder", "src/b/*.go"),
	}
	if err := s.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	timeouts, err := s.CheckTimeouts(context.Background(), fixed.Add(time.Second))
	if err != nil {
		t.Fatalf("check timeouts: %v", err)
	}
	if len(timeouts) != 2 {
		t.Fatalf("expected 2 timeouts, got %d", len(timeouts))
	}
	for _, id := range []string{"st-a", "st-b"} {
		item, ok := s.Item(id)
		if !ok || item.Status != subtask.StatusFailed {
			t.Fatalf("%s should be failed, got %+v", id, item)
		}
	}
	failed := emitter.Find(EventSubtaskFailed)
	if len(failed) != 2 {
		t.Fatalf("expected 2 subtask.failed events, got %d", len(failed))
	}
	for _, ev := range failed {
		if ev.Payload["category"] != "timeout" {
			t.Fatalf("timeout event must carry category=timeout: %+v", ev.Payload)
		}
	}
	if parent, tenant := s.ActiveSlots(); parent != 0 || tenant != 0 {
		t.Fatalf("timeout must release slots, got (%d,%d)", parent, tenant)
	}
}

func TestSchedulerDependencyGate(t *testing.T) {
	s := NewScheduler(DefaultQuotaConfig(), nil)
	n1 := testSubtask("task-1", "dag-1", "st-1", 0, 1, "p1", "coder", "src/a/*.go")
	n2 := testSubtask("task-1", "dag-1", "st-2", 1, 1, "p2", "verifier", "src/b/*.go")
	n2.DependsOn = []string{"st-1"}
	if err := s.Enqueue([]*subtask.Subtask{n1, n2}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	started, err := s.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(started) != 1 || started[0].SubtaskID != "st-1" {
		t.Fatalf("only st-1 should start, got %+v", started)
	}
	if err := s.MarkAccepted(context.Background(), "st-1", "acc-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	started, err = s.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	if len(started) != 1 || started[0].SubtaskID != "st-2" {
		t.Fatalf("st-2 should start after st-1 accepted, got %+v", started)
	}
}

func TestSchedulerRejectsOverLimit(t *testing.T) {
	s := NewScheduler(DefaultQuotaConfig(), nil)
	var batch []*subtask.Subtask
	for i := 0; i < 21; i++ {
		batch = append(batch, testSubtask("task-1", "dag-1", "st-"+string(rune('a'+i)), i, 1, "p1", "coder", "src/a/*.go"))
	}
	assertCode(t, s.Enqueue(batch), CodeSubtaskLimitExceeded)

	s2 := NewScheduler(DefaultQuotaConfig(), nil)
	deep := testSubtask("task-1", "dag-1", "st-deep", 0, 3, "p1", "coder", "src/a/*.go")
	assertCode(t, s2.Enqueue([]*subtask.Subtask{deep}), CodeSubtaskDepthExceeded)
}

func TestSchedulerRejectsCrossTenant(t *testing.T) {
	s := NewScheduler(DefaultQuotaConfig(), nil)
	a := testSubtask("task-1", "dag-1", "st-a", 0, 1, "p1", "coder", "src/a/*.go")
	b := testSubtask("task-1", "dag-1", "st-b", 1, 1, "p1", "coder", "src/b/*.go")
	b.TenantID = "tenant-2"
	assertCode(t, s.Enqueue([]*subtask.Subtask{a, b}), CodeCrossTenantDenied)
}

func TestSchedulerSplitBudget(t *testing.T) {
	alloc := SplitBudget(100, []string{"n1", "n2", "n3"})
	if alloc["n1"] != 34 || alloc["n2"] != 33 || alloc["n3"] != 33 {
		t.Fatalf("unexpected split: %v", alloc)
	}
	if got := SplitBudget(0, []string{"n1"}); len(got) != 0 {
		t.Fatalf("zero budget should allocate nothing, got %v", got)
	}
}

func TestSchedulerRejectsUnknownDependency(t *testing.T) {
	s := NewScheduler(DefaultQuotaConfig(), nil)
	st := testSubtask("task-1", "dag-1", "st-a", 0, 1, "p1", "coder", "src/a/*.go")
	st.DependsOn = []string{"missing"}
	assertCode(t, s.Enqueue([]*subtask.Subtask{st}), CodeDAGIncomplete)
}
