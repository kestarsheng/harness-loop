package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"agent-platform/pkg/harnessloop/subtask"
)

func writeArtifact(t *testing.T, root, rel, content string) string {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func acceptedSubtask(id, dagNode string, index int, refs ...subtask.ArtifactRef) *subtask.Subtask {
	st := testSubtask("task-1", "dag-1", id, index, 1, "p1", "coder", "out/**")
	st.DagNodeID = dagNode
	st.Status = subtask.StatusAccepted
	st.AcceptanceID = "acc-" + id
	st.ArtifactRefs = refs
	return st
}

func TestAggregateTenSubtasksReport(t *testing.T) {
	root := t.TempDir()
	emitter := NewMemoryEmitter()
	aggregator := NewAggregator(FSVerifier{Root: root}, emitter)
	var batch []*subtask.Subtask
	for i := 0; i < 10; i++ {
		id := "st-" + string(rune('a'+i))
		rel := "out/f" + string(rune('a'+i)) + ".txt"
		hash := writeArtifact(t, root, rel, "content-"+id)
		batch = append(batch, acceptedSubtask(id, id, i, subtask.ArtifactRef{Path: rel, Hash: hash}))
	}
	report, err := aggregator.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: "task-1", DagID: "dag-1", TenantID: "tenant-1", ProjectID: "project-1",
		Subtasks: batch,
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if report.Summary.Total != 10 || report.Summary.Accepted != 10 || !report.Complete {
		t.Fatalf("unexpected summary: %+v complete=%v", report.Summary, report.Complete)
	}
	if len(report.Items) != 10 {
		t.Fatalf("expected 10 items, got %d", len(report.Items))
	}
	for _, item := range report.Items {
		if len(item.Artifacts) != 1 || !item.Artifacts[0].Exists || !item.Artifacts[0].HashMatch {
			t.Fatalf("artifact verification failed: %+v", item)
		}
	}
	if err := aggregator.RequireForAcceptance(report); err != nil {
		t.Fatalf("complete report should pass the d-stage gate: %v", err)
	}
	if AcceptanceInput(report)["aggregation_report"] == nil {
		t.Fatalf("acceptance injection must carry the report")
	}
	events := emitter.Find(EventAggregationCompleted)
	if len(events) != 1 || events[0].Payload["total"] != 10 || events[0].Payload["accepted"] != 10 {
		t.Fatalf("aggregation.completed payload is wrong: %+v", events)
	}
}

func TestAggregateMissingReport(t *testing.T) {
	aggregator := NewAggregator(nil, nil)
	assertCode(t, aggregator.RequireForAcceptance(nil), CodeAggregationMissing)
}

func TestAggregateRejectsNonAcceptedProduct(t *testing.T) {
	root := t.TempDir()
	hash := writeArtifact(t, root, "out/a.txt", "content")
	emitter := NewMemoryEmitter()
	aggregator := NewAggregator(FSVerifier{Root: root}, emitter)
	st := testSubtask("task-1", "dag-1", "st-a", 0, 1, "p1", "coder", "out/*.txt")
	st.Status = subtask.StatusRunning
	st.ArtifactRefs = []subtask.ArtifactRef{{Path: "out/a.txt", Hash: hash}}
	_, err := aggregator.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: "task-1", DagID: "dag-1", TenantID: "tenant-1", ProjectID: "project-1",
		Subtasks: []*subtask.Subtask{st},
	})
	assertCode(t, err, CodeSubtaskNotAccepted)
	if len(emitter.Find(EventSecurityViolation)) != 1 {
		t.Fatalf("expected security.violation for non-accepted product")
	}
}

func TestAggregateDetectsMissingArtifact(t *testing.T) {
	root := t.TempDir()
	aggregator := NewAggregator(FSVerifier{Root: root}, NewMemoryEmitter())
	st := acceptedSubtask("st-a", "st-a", 0, subtask.ArtifactRef{Path: "out/missing.txt", Hash: "sha256:abc"})
	report, err := aggregator.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: "task-1", DagID: "dag-1", TenantID: "tenant-1", ProjectID: "project-1",
		Subtasks: []*subtask.Subtask{st},
	})
	assertCode(t, err, CodeAggregationIncomplete)
	if report == nil || report.Complete || report.Items[0].Reason != "artifact_missing" {
		t.Fatalf("missing artifact should mark the report incomplete: %+v", report)
	}
}

func TestAggregateDetectsHashDrift(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "out/a.txt"), []byte("actual"), 0o644); err != nil {
		t.Fatal(err)
	}
	aggregator := NewAggregator(FSVerifier{Root: root}, NewMemoryEmitter())
	st := acceptedSubtask("st-a", "st-a", 0, subtask.ArtifactRef{Path: "out/a.txt", Hash: "sha256:deadbeef"})
	report, err := aggregator.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: "task-1", DagID: "dag-1", TenantID: "tenant-1", ProjectID: "project-1",
		Subtasks: []*subtask.Subtask{st},
	})
	assertCode(t, err, CodeAggregationIncomplete)
	if report.Items[0].Reason != "artifact_drift" {
		t.Fatalf("expected artifact_drift, got %q", report.Items[0].Reason)
	}
}

func TestAggregateSamePathConflict(t *testing.T) {
	root := t.TempDir()
	hash := writeArtifact(t, root, "out/shared.txt", "content")
	emitter := NewMemoryEmitter()
	aggregator := NewAggregator(FSVerifier{Root: root}, emitter)
	a := acceptedSubtask("st-a", "st-a", 0, subtask.ArtifactRef{Path: "out/shared.txt", Hash: hash})
	b := acceptedSubtask("st-b", "st-b", 1, subtask.ArtifactRef{Path: "out/shared.txt", Hash: hash})
	report, err := aggregator.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: "task-1", DagID: "dag-1", TenantID: "tenant-1", ProjectID: "project-1",
		Subtasks: []*subtask.Subtask{a, b},
	})
	assertCode(t, err, CodeAggregationIncomplete)
	if len(report.Conflicts) != 1 || report.Conflicts[0].Kind != ConflictSamePathMultiWriter {
		t.Fatalf("expected same_path_multi_writer conflict: %+v", report.Conflicts)
	}
	if len(emitter.Find(EventAggregationConflict)) != 1 {
		t.Fatalf("expected aggregation.conflict event")
	}
}

func TestAggregateReportsFailedAndCancelled(t *testing.T) {
	root := t.TempDir()
	hash := writeArtifact(t, root, "out/a.txt", "content")
	aggregator := NewAggregator(FSVerifier{Root: root}, NewMemoryEmitter())
	ok := acceptedSubtask("st-ok", "st-ok", 0, subtask.ArtifactRef{Path: "out/a.txt", Hash: hash})
	failed := testSubtask("task-1", "dag-1", "st-failed", 1, 1, "p1", "coder", "out/*.txt")
	failed.Status = subtask.StatusFailed
	cancelled := testSubtask("task-1", "dag-1", "st-cancelled", 2, 1, "p1", "coder", "out/*.txt")
	cancelled.Status = subtask.StatusCancelled
	report, err := aggregator.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: "task-1", DagID: "dag-1", TenantID: "tenant-1", ProjectID: "project-1",
		Subtasks: []*subtask.Subtask{ok, failed, cancelled},
	})
	if err != nil {
		t.Fatalf("non-accepted terminal statuses must be reported, not rejected: %v", err)
	}
	if report.Summary.Accepted != 1 || report.Summary.Failed != 1 || report.Summary.Cancelled != 1 {
		t.Fatalf("unexpected summary: %+v", report.Summary)
	}
	if len(report.Items) != 3 {
		t.Fatalf("failed/cancelled items must not be filtered out: %+v", report.Items)
	}
}

func TestAggregateContractMismatch(t *testing.T) {
	root := t.TempDir()
	hash := writeArtifact(t, root, "out/a.txt", "content")
	aggregator := NewAggregator(FSVerifier{Root: root}, NewMemoryEmitter())
	upstream := acceptedSubtask("st-a", "n1", 0, subtask.ArtifactRef{Path: "out/a.txt", Hash: hash})
	upstream.OutputContract = map[string]any{"type": "artifacts", "paths": []string{"out/a.txt"}}
	downstream := acceptedSubtask("st-b", "n2", 1)
	downstream.InputContract = map[string]any{"type": "file_set", "paths": []string{"out/a.txt"}}
	downstream.DependsOn = []string{"n1"}
	report, err := aggregator.Aggregate(context.Background(), AggregationRequest{
		ParentTaskID: "task-1", DagID: "dag-1", TenantID: "tenant-1", ProjectID: "project-1",
		Subtasks: []*subtask.Subtask{upstream, downstream},
	})
	assertCode(t, err, CodeAggregationIncomplete)
	if len(report.Conflicts) != 1 || report.Conflicts[0].Kind != ConflictContractMismatch {
		t.Fatalf("expected contract_mismatch conflict: %+v", report.Conflicts)
	}
}

func TestAggregatePropagationFailFast(t *testing.T) {
	cfg := DefaultQuotaConfig()
	propagator := NewPropagator(cfg)
	var batch []*subtask.Subtask
	for i := 0; i < 5; i++ {
		st := testSubtask("task-1", "dag-1", "st-"+string(rune('a'+i)), i, 1, "p1", "coder", "out/*.txt")
		st.Status = subtask.StatusRunning
		batch = append(batch, st)
	}
	batch[0].Status = subtask.StatusFailed
	sets := []ParallelSet{{SetID: "p1", Nodes: []string{"st-a", "st-b", "st-c", "st-d", "st-e"}, FailureStrategy: subtask.StrategyFailFast}}
	result, err := propagator.Propagate(PropagationRequest{ParentTaskID: "task-1", Sets: sets, Subtasks: batch, Failed: []FailedSubtask{{SubtaskID: "st-a", Category: "timeout"}}})
	if err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if len(result.Decisions) != 1 {
		t.Fatalf("expected one decision, got %d", len(result.Decisions))
	}
	decision := result.Decisions[0]
	if decision.Action != ActionCancelSet || len(decision.Cancelled) != 4 {
		t.Fatalf("fail-fast must cancel the other four: %+v", decision)
	}
	if decision.Rollback != (ParentRollback{From: "d", To: "c"}) {
		t.Fatalf("expected d→c rollback, got %+v", decision.Rollback)
	}

	scheduler := NewScheduler(DefaultQuotaConfig(), nil)
	if err := scheduler.Enqueue(batch); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := scheduler.Dispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := propagator.Apply(context.Background(), scheduler, result); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, id := range decision.Cancelled {
		item, ok := scheduler.Item(id)
		if !ok || item.Status != subtask.StatusCancelled {
			t.Fatalf("%s should be cancelled, got %+v", id, item)
		}
	}
}

func TestAggregatePropagationTolerate(t *testing.T) {
	propagator := NewPropagator(DefaultQuotaConfig())
	sets := []ParallelSet{{SetID: "p1", FailureStrategy: subtask.StrategyTolerate, TolerateRatio: 0.2}}
	var batch []*subtask.Subtask
	for i := 0; i < 5; i++ {
		st := testSubtask("task-1", "dag-1", "st-"+string(rune('a'+i)), i, 1, "p1", "coder", "out/*.txt")
		if i == 0 {
			st.Status = subtask.StatusFailed
		} else {
			st.Status = subtask.StatusAccepted
		}
		batch = append(batch, st)
	}
	result, err := propagator.Propagate(PropagationRequest{Sets: sets, Subtasks: batch, Failed: []FailedSubtask{{SubtaskID: "st-a", Category: "timeout"}}})
	if err != nil {
		t.Fatalf("propagate: %v", err)
	}
	decision := result.Decisions[0]
	if decision.Action != ActionContinue || !decision.Tolerated || decision.FailureRatio != 0.2 {
		t.Fatalf("1/5 failure within tolerate ratio should continue: %+v", decision)
	}

	batch[1].Status = subtask.StatusFailed
	result, err = propagator.Propagate(PropagationRequest{Sets: sets, Subtasks: batch, Failed: []FailedSubtask{{SubtaskID: "st-a"}, {SubtaskID: "st-b"}}})
	if err != nil {
		t.Fatalf("propagate 2: %v", err)
	}
	if result.Decisions[0].Action != ActionCancelSet {
		t.Fatalf("2/5 failure exceeds tolerate ratio and must fail fast: %+v", result.Decisions[0])
	}
}

func TestAggregatePropagationRetry(t *testing.T) {
	propagator := NewPropagator(DefaultQuotaConfig())
	sets := []ParallelSet{{SetID: "p1", FailureStrategy: subtask.StrategyRetry}}
	st := testSubtask("task-1", "dag-1", "st-a", 0, 1, "p1", "coder", "out/*.txt")
	st.Status = subtask.StatusFailed
	st.AttemptNo = 1
	result, err := propagator.Propagate(PropagationRequest{Sets: sets, Subtasks: []*subtask.Subtask{st}, Failed: []FailedSubtask{{SubtaskID: "st-a"}}})
	if err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if result.Decisions[0].Action != ActionRetry {
		t.Fatalf("attempt 1 < max 2 should retry: %+v", result.Decisions[0])
	}

	st.AttemptNo = 2
	result, err = propagator.Propagate(PropagationRequest{Sets: sets, Subtasks: []*subtask.Subtask{st}, Failed: []FailedSubtask{{SubtaskID: "st-a"}}})
	if err != nil {
		t.Fatalf("propagate 2: %v", err)
	}
	if result.Decisions[0].Action != ActionCancelSet || result.Decisions[0].Rollback.To != "c" {
		t.Fatalf("exhausted retry must degrade to fail-fast: %+v", result.Decisions[0])
	}
}

func TestAggregatePropagationUndeclared(t *testing.T) {
	propagator := NewPropagator(DefaultQuotaConfig())
	sets := []ParallelSet{{SetID: "p1"}}
	st := testSubtask("task-1", "dag-1", "st-a", 0, 1, "p1", "coder", "out/*.txt")
	st.Status = subtask.StatusFailed
	_, err := propagator.Propagate(PropagationRequest{Sets: sets, Subtasks: []*subtask.Subtask{st}, Failed: []FailedSubtask{{SubtaskID: "st-a"}}})
	assertCode(t, err, CodePropagationUndeclared)
}
