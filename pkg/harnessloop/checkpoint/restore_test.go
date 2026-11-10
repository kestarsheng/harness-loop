package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newCheckpoint(id, taskID, stage, kind string, attempt int) *Checkpoint {
	cp := &Checkpoint{
		CheckpointID: id,
		TaskID:       taskID,
		TenantID:     "tenant-a",
		SessionID:    "sess-1",
		TraceID:      "trace-1",
		Stage:        stage,
		AttemptNo:    attempt,
		AttemptID:    "att-1",
		Step:         3,
		Kind:         kind,
		FSM: FSMSnapshot{
			CurrentStage:          stage,
			Status:                "running",
			AttemptNoByStage:      map[string]int{stage: attempt},
			RetryCountByStage:     map[string]int{},
			AcceptanceFailByStage: map[string]int{},
		},
		MessagesRef:       MessagesRef{Store: "mongo", Collection: "messages", DocID: "doc-1", Count: 12, Digest: "sha256:x"},
		MemoryRefs:        []MemoryRef{{File: "project.md", TopicKey: "constraint", ContentHash: "h1", OrderRank: 1}},
		Consumed:          Consumed{Tokens: 100, Cost: 0.02, CompactionTokens: 10},
		EventSeqWatermark: 42,
		CreatedAt:         time.Now().UTC(),
	}
	cp.Seal()
	return cp
}

func TestCheckpointRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	cp := newCheckpoint("cp-1", "task-1", "c", KindStageEntry, 2)

	if err := store.Save(ctx, cp); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.Load(ctx, "cp-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Checksum != cp.Checksum || got.SchemaVersion != SchemaVersion {
		t.Fatalf("checkpoint not sealed: %+v", got)
	}
	if got.FSM.CurrentStage != "c" || got.AttemptNo != 2 || got.EventSeqWatermark != 42 {
		t.Fatalf("fields lost: %+v", got)
	}
	if len(got.MemoryRefs) != 1 || got.MessagesRef.Count != 12 {
		t.Fatalf("refs lost: %+v", got)
	}
}

func TestValidateRejectsCorruptedCheckpoint(t *testing.T) {
	cp := newCheckpoint("cp-1", "task-1", "c", KindStageEntry, 1)
	cp.Checksum = "deadbeef"
	if err := cp.Validate(); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("checksum mismatch must be corrupted: %v", err)
	}

	cases := []func(*Checkpoint){
		func(c *Checkpoint) { c.CheckpointID = "" },
		func(c *Checkpoint) { c.TaskID = "" },
		func(c *Checkpoint) { c.SchemaVersion = "9.9.9" },
		func(c *Checkpoint) { c.Kind = "nope" },
		func(c *Checkpoint) { c.Stage = "" },
		func(c *Checkpoint) { c.FSM.CurrentStage = "" },
		func(c *Checkpoint) { c.CreatedAt = time.Time{} },
	}
	for _, mut := range cases {
		bad := newCheckpoint("cp-1", "task-1", "c", KindStageEntry, 1)
		mut(bad)
		if err := bad.Validate(); !errors.Is(err, ErrCorrupted) {
			t.Fatalf("invalid checkpoint must be corrupted: %+v -> %v", bad, err)
		}
	}
}

func TestRestoreWithoutDrift(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	cp := newCheckpoint("cp-1", "task-1", "c", KindStageExit, 2)
	if err := store.Save(ctx, cp); err != nil {
		t.Fatal(err)
	}
	task := TaskSnapshot{Stage: "c", AttemptNo: 2, Consumed: cp.Consumed, EventMaxSeq: 50}

	r := NewRestorer(store)
	res, err := r.Restore(ctx, "task-1", "cp-1", task, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Duplicate || res.Rebuild || res.Drift.Drifted {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Checkpoint.CheckpointID != "cp-1" {
		t.Fatalf("wrong checkpoint: %s", res.Checkpoint.CheckpointID)
	}
}

func TestRestoreFsmDriftBlocks(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	cp := newCheckpoint("cp-1", "task-1", "c", KindStageExit, 2)
	if err := store.Save(ctx, cp); err != nil {
		t.Fatal(err)
	}
	task := TaskSnapshot{Stage: "d", AttemptNo: 1, Consumed: cp.Consumed, EventMaxSeq: 50}

	r := NewRestorer(store)
	res, err := r.Restore(ctx, "task-1", "cp-1", task, RestoreOptions{})
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("drift must block: %v", err)
	}
	if res == nil || !res.Drift.Drifted || len(res.Drift.Fields) < 2 {
		t.Fatalf("drift report = %+v", res)
	}
	if res.Drift.Action != DriftBlock {
		t.Fatalf("action = %s", res.Drift.Action)
	}
}

func TestRestoreArtifactDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := DefaultHashFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cp := newCheckpoint("cp-1", "task-1", "c", KindToolBatch, 1)
	cp.Artifacts = []Artifact{{Path: "a.go", Hash: hash, Size: 10}}
	cp.Seal()

	store := NewMemoryStore()
	if err := store.Save(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	task := TaskSnapshot{Stage: "c", AttemptNo: 1, Consumed: cp.Consumed, EventMaxSeq: 50}

	r := NewRestorer(store)
	if _, err := r.Restore(context.Background(), "task-1", "cp-1", task, RestoreOptions{Workspace: dir}); err != nil {
		t.Fatalf("clean workspace must restore: %v", err)
	}

	if err := os.WriteFile(path, []byte("package a\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Restore(context.Background(), "task-1", "cp-1", task, RestoreOptions{Workspace: dir})
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("artifact drift must block: %v", err)
	}
	if res.Drift.Fields[0].Field != "artifact.a.go" {
		t.Fatalf("drift fields = %+v", res.Drift.Fields)
	}
}

func TestRestoreWatermarkAheadTriggersRebuild(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	cp := newCheckpoint("cp-1", "task-1", "c", KindPreCompaction, 1)
	if err := store.Save(ctx, cp); err != nil {
		t.Fatal(err)
	}
	task := TaskSnapshot{Stage: "c", AttemptNo: 1, Consumed: cp.Consumed, EventMaxSeq: 10}

	r := NewRestorer(store)
	res, err := r.Restore(ctx, "task-1", "cp-1", task, RestoreOptions{})
	if err != nil {
		t.Fatalf("watermark ahead must not error: %v", err)
	}
	if !res.Rebuild || !res.Drift.Drifted {
		t.Fatalf("rebuild must be requested: %+v", res)
	}
	if res.Drift.Action != DriftRebuild {
		t.Fatalf("action = %s", res.Drift.Action)
	}
}

func TestRestoreIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	cp := newCheckpoint("cp-1", "task-1", "c", KindPreTransition, 1)
	if err := store.Save(ctx, cp); err != nil {
		t.Fatal(err)
	}
	task := TaskSnapshot{Stage: "c", AttemptNo: 1, Consumed: cp.Consumed, EventMaxSeq: 50}
	r := NewRestorer(store)

	first, err := r.Restore(ctx, "task-1", "cp-1", task, RestoreOptions{})
	if err != nil || first.Duplicate {
		t.Fatalf("first restore: %v %+v", err, first)
	}
	second, err := r.Restore(ctx, "task-1", "cp-1", task, RestoreOptions{})
	if err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("repeated restore must be flagged as duplicate")
	}
	if second.Checkpoint.CheckpointID != first.Checkpoint.CheckpointID {
		t.Fatal("idempotent restore must return the same checkpoint")
	}
}

func TestRestoreFallsBackOnCorruptedLatest(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	good := newCheckpoint("cp-old", "task-1", "c", KindStageEntry, 1)
	good.CreatedAt = time.Now().Add(-time.Hour).UTC()
	good.Seal()
	if err := store.Save(ctx, good); err != nil {
		t.Fatal(err)
	}

	bad := newCheckpoint("cp-new", "task-1", "c", KindStageExit, 1)
	bad.CreatedAt = time.Now().UTC()
	bad.Checksum = "broken"
	store.mu.Lock()
	store.byID["cp-new"] = bad
	store.byTask["task-1"] = append(store.byTask["task-1"], "cp-new")
	store.mu.Unlock()

	task := TaskSnapshot{Stage: "c", AttemptNo: 1, Consumed: good.Consumed, EventMaxSeq: 50}
	r := NewRestorer(store)
	res, err := r.RestoreLatest(ctx, "task-1", task, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore latest: %v", err)
	}
	if res.Checkpoint.CheckpointID != "cp-old" {
		t.Fatalf("must fall back to previous checkpoint, got %s", res.Checkpoint.CheckpointID)
	}
	if res.FellBackFrom != "cp-new" {
		t.Fatalf("fallback source = %q", res.FellBackFrom)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("corrupted checkpoint must produce a warning")
	}
}

func TestRestoreNoUsableCheckpoint(t *testing.T) {
	store := NewMemoryStore()
	r := NewRestorer(store)
	if _, err := r.RestoreLatest(context.Background(), "task-x", TaskSnapshot{}, RestoreOptions{}); !errors.Is(err, ErrNoUsableCheckpoint) {
		t.Fatalf("expected ErrNoUsableCheckpoint, got %v", err)
	}
}

func TestRetentionPlan(t *testing.T) {
	mk := func(id, stage string, minutes int) *Checkpoint {
		cp := newCheckpoint(id, "task-1", stage, KindInterval, 1)
		cp.CreatedAt = time.Now().Add(-time.Duration(minutes) * time.Minute).UTC()
		cp.Seal()
		return cp
	}
	list := []*Checkpoint{
		mk("cp-a5", "a", 5),
		mk("cp-b3", "b", 3),
		mk("cp-c2", "c", 2),
		mk("cp-c1", "c", 1),
		mk("cp-a9", "a", 9),
	}
	keep, remove := RetentionPlan(list, 2)
	keepSet := map[string]bool{}
	for _, id := range keep {
		keepSet[id] = true
	}
	for _, id := range []string{"cp-a5", "cp-b3"} {
		if !keepSet[id] {
			t.Fatalf("recent checkpoint %s must be kept: keep=%v", id, keep)
		}
	}
	for _, id := range []string{"cp-a5", "cp-b3", "cp-c2"} {
		if !keepSet[id] {
			t.Fatalf("last checkpoint of a stage must be kept: %s keep=%v", id, keep)
		}
	}
	if keepSet["cp-c1"] || keepSet["cp-a9"] {
		t.Fatalf("older same-stage duplicates should be removed: %v", keep)
	}
	if len(remove) != 2 {
		t.Fatalf("remove = %v", remove)
	}
}

func TestApplyRetention(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	for i := 0; i < 5; i++ {
		cp := newCheckpoint("cp-"+string(rune('a'+i)), "task-1", "c", KindInterval, 1)
		cp.CreatedAt = time.Now().Add(-time.Duration(i) * time.Minute).UTC()
		cp.Seal()
		if err := store.Save(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := store.ApplyRetention(ctx, "task-1", 2)
	if err != nil {
		t.Fatalf("apply retention: %v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("removed = %v", removed)
	}
	left, err := store.List(ctx, "task-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("left = %d", len(left))
	}
}
