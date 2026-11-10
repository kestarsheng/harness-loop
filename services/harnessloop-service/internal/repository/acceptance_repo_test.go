package repository

import (
	"context"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/acceptance"
)

func TestAcceptanceRepoCreateAndGet(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewAcceptanceRepo(db)
	ctx := context.Background()

	rec := AcceptanceRecord{
		AcceptanceID:   "acc-" + task.TaskID,
		TaskID:         task.TaskID,
		Stage:          "c",
		AttemptID:      task.TaskID + "-c-1",
		Verdict:        acceptance.VerdictFail,
		VerifiedBy:     acceptance.VerifiedByProgram,
		Reason:         "tests failed",
		Evidence:       []acceptance.Evidence{{Kind: "test", Ref: "go test ./...", Excerpt: "FAIL"}},
		FailedCriteria: []acceptance.FailedCriterion{{CriterionID: "c-1", Category: "impl_gap"}},
		Unverified:     []string{"c-2"},
		RollbackTarget: "b",
		PromptVersion:  acceptance.PromptVersion,
		Model:          "qwen-max",
		InputDigest:    "digest-1",
		TokensIn:       100,
		TokensOut:      50,
		DurationMs:     1200,
	}
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Get(ctx, rec.AcceptanceID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("record not found")
	}
	if got.Verdict != acceptance.VerdictFail || got.RollbackTarget != "b" {
		t.Fatalf("unexpected record: %+v", got)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Kind != "test" {
		t.Fatalf("evidence = %+v", got.Evidence)
	}
	if len(got.FailedCriteria) != 1 || got.FailedCriteria[0].Category != "impl_gap" {
		t.Fatalf("failed = %+v", got.FailedCriteria)
	}
	if len(got.Unverified) != 1 || got.Unverified[0] != "c-2" {
		t.Fatalf("unverified = %v", got.Unverified)
	}
	if got.PromptVersion != acceptance.PromptVersion || got.InputDigest != "digest-1" {
		t.Fatalf("audit fields = %+v", got)
	}

	if err := repo.Create(ctx, rec); err == nil {
		t.Fatal("duplicate acceptance_id must be rejected")
	}
}

func TestAcceptanceRepoRequiresIdentity(t *testing.T) {
	db, _, _ := setupTask(t)
	repo := NewAcceptanceRepo(db)
	ctx := context.Background()
	if err := repo.Create(ctx, AcceptanceRecord{AcceptanceID: "x"}); err == nil {
		t.Fatal("missing task/attempt must be rejected")
	}
}

func TestAcceptanceRepoListAndLatestEvidence(t *testing.T) {
	db, _, task := setupTask(t)
	repo := NewAcceptanceRepo(db)
	ctx := context.Background()

	fail := AcceptanceRecord{
		AcceptanceID:   "acc-fail-" + task.TaskID,
		TaskID:         task.TaskID,
		Stage:          "c",
		AttemptID:      task.TaskID + "-c-1",
		Verdict:        acceptance.VerdictFail,
		VerifiedBy:     acceptance.VerifiedByProgram,
		Reason:         "build failed",
		Evidence:       []acceptance.Evidence{{Kind: "command", Ref: "go build ./..."}},
		FailedCriteria: []acceptance.FailedCriterion{{CriterionID: "c-2", Category: "impl_gap"}},
		RollbackTarget: "b",
		PromptVersion:  acceptance.PromptVersion,
		InputDigest:    "digest-fail",
	}
	if err := repo.Create(ctx, fail); err != nil {
		t.Fatalf("create fail: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	pass := AcceptanceRecord{
		AcceptanceID:  "acc-pass-" + task.TaskID,
		TaskID:        task.TaskID,
		Stage:         "c",
		AttemptID:     task.TaskID + "-c-2",
		Verdict:       acceptance.VerdictPass,
		VerifiedBy:    acceptance.VerifiedByMixed,
		Reason:        "all good",
		Evidence:      []acceptance.Evidence{{Kind: "test", Ref: "go test ./..."}},
		PromptVersion: acceptance.PromptVersion,
		InputDigest:   "digest-pass",
	}
	if err := repo.Create(ctx, pass); err != nil {
		t.Fatalf("create pass: %v", err)
	}

	list, err := repo.ListByTaskStage(ctx, task.TaskID, "c", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d", len(list))
	}

	ev, fc, err := repo.LatestEvidence(ctx, task.TaskID, "c")
	if err != nil {
		t.Fatalf("latest evidence: %v", err)
	}
	if len(ev) != 1 || ev[0].Kind != "command" {
		t.Fatalf("evidence = %+v", ev)
	}
	if len(fc) != 1 || fc[0].CriterionID != "c-2" {
		t.Fatalf("failed = %+v", fc)
	}

	ev, fc, err = repo.LatestEvidence(ctx, "no-such-task", "c")
	if err != nil || ev != nil || fc != nil {
		t.Fatalf("missing task: %v %v %v", err, ev, fc)
	}

	if got, err := repo.Get(ctx, "missing"); err != nil || got != nil {
		t.Fatalf("missing get: %v %v", err, got)
	}
}
