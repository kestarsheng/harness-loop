package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/checkpoint"
)

func mkCheckpoint(id, taskID, tenantID, stage string) *checkpoint.Checkpoint {
	cp := &checkpoint.Checkpoint{
		CheckpointID: id,
		TaskID:       taskID,
		TenantID:     tenantID,
		Stage:        stage,
		AttemptNo:    1,
		Kind:         checkpoint.KindStageEntry,
		FSM: checkpoint.FSMSnapshot{
			CurrentStage: stage,
			Status:       "running",
		},
		Consumed:          checkpoint.Consumed{Tokens: 10, Cost: 0.01},
		EventSeqWatermark: 7,
		CreatedAt:         time.Now().UTC(),
	}
	return cp
}

func TestCheckpointRepoSaveLoad(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewCheckpointRepo(db)
	ctx := context.Background()

	cp := mkCheckpoint("cp-"+task.TaskID, task.TaskID, task.TenantID, "c")
	cp.Artifacts = []checkpoint.Artifact{{Path: "a.go", Hash: "sha256:aa", Size: 12}}
	if err := repo.Save(ctx, cp); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := repo.Load(ctx, cp.CheckpointID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.TaskID != task.TaskID || got.Stage != "c" || got.EventSeqWatermark != 7 {
		t.Fatalf("unexpected checkpoint: %+v", got)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Path != "a.go" {
		t.Fatalf("artifacts lost: %+v", got.Artifacts)
	}
	if got.Checksum != cp.Checksum {
		t.Fatalf("checksum changed in transit")
	}

	if _, err := repo.Load(ctx, "missing"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("missing load: %v", err)
	}
}

func TestCheckpointRepoLatestAndList(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewCheckpointRepo(db)
	ctx := context.Background()

	older := mkCheckpoint("cp-old-"+task.TaskID, task.TaskID, task.TenantID, "b")
	older.CreatedAt = time.Now().Add(-time.Hour).UTC()
	older.Seal()
	newer := mkCheckpoint("cp-new-"+task.TaskID, task.TaskID, task.TenantID, "c")
	newer.CreatedAt = time.Now().UTC()
	newer.Seal()

	for _, cp := range []*checkpoint.Checkpoint{older, newer} {
		if err := repo.Save(ctx, cp); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	latest, err := repo.Latest(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.CheckpointID != newer.CheckpointID {
		t.Fatalf("latest = %s", latest.CheckpointID)
	}

	list, err := repo.List(ctx, task.TaskID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].CheckpointID != newer.CheckpointID {
		t.Fatalf("list order = %+v", list)
	}

	if _, err := repo.Latest(ctx, "no-such-task"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("empty latest: %v", err)
	}
}

func TestCheckpointRepoDelete(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewCheckpointRepo(db)
	ctx := context.Background()

	cp := mkCheckpoint("cp-del-"+task.TaskID, task.TaskID, task.TenantID, "a")
	if err := repo.Save(ctx, cp); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := repo.Delete(ctx, cp.CheckpointID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Load(ctx, cp.CheckpointID); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("deleted checkpoint must be gone: %v", err)
	}
	if err := repo.Delete(ctx, cp.CheckpointID); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
}

func TestCheckpointRepoRejectsCorrupted(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewCheckpointRepo(db)
	ctx := context.Background()

	cp := mkCheckpoint("cp-bad-"+task.TaskID, task.TaskID, task.TenantID, "a")
	cp.Checksum = "broken"
	if err := repo.Save(ctx, cp); !errors.Is(err, checkpoint.ErrCorrupted) {
		t.Fatalf("corrupted checkpoint must be rejected: %v", err)
	}
	if err := repo.Save(ctx, nil); err == nil {
		t.Fatal("nil checkpoint must be rejected")
	}
}
