package subtask

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"agent-platform/pkg/pgindex"
)

func TestSubtaskStatusSet(t *testing.T) {
	want := []Status{StatusPending, StatusRunning, StatusWaitingHandoff, StatusAccepted, StatusRejected, StatusFailed, StatusCancelled}
	if len(allStatuses) != 7 {
		t.Fatalf("status count = %d", len(allStatuses))
	}
	for _, s := range want {
		if !s.Valid() {
			t.Fatalf("%s must be valid", s)
		}
	}
	if Status("done").Valid() {
		t.Fatal("unknown status must be invalid")
	}
}

func TestSubtaskTerminalStates(t *testing.T) {
	for _, s := range []Status{StatusAccepted, StatusRejected, StatusFailed, StatusCancelled} {
		if !s.Terminal() {
			t.Fatalf("%s must be terminal", s)
		}
	}
	for _, s := range []Status{StatusPending, StatusRunning, StatusWaitingHandoff} {
		if s.Terminal() {
			t.Fatalf("%s must not be terminal", s)
		}
	}
}

func TestSubtaskAllowedTransitions(t *testing.T) {
	legal := [][2]Status{
		{StatusPending, StatusRunning},
		{StatusPending, StatusCancelled},
		{StatusRunning, StatusRunning},
		{StatusRunning, StatusWaitingHandoff},
		{StatusRunning, StatusAccepted},
		{StatusRunning, StatusRejected},
		{StatusRunning, StatusFailed},
		{StatusRunning, StatusCancelled},
		{StatusWaitingHandoff, StatusRunning},
		{StatusWaitingHandoff, StatusCancelled},
	}
	for _, pair := range legal {
		if !AllowedTransition(pair[0], pair[1]) {
			t.Fatalf("%s->%s must be legal", pair[0], pair[1])
		}
	}

	illegal := [][2]Status{
		{StatusPending, StatusAccepted},
		{StatusPending, StatusFailed},
		{StatusWaitingHandoff, StatusAccepted},
		{StatusAccepted, StatusRunning},
		{StatusRejected, StatusPending},
		{StatusFailed, StatusCancelled},
		{StatusCancelled, StatusPending},
	}
	for _, pair := range illegal {
		if AllowedTransition(pair[0], pair[1]) {
			t.Fatalf("%s->%s must be illegal", pair[0], pair[1])
		}
	}

	for _, from := range []Status{StatusAccepted, StatusRejected, StatusFailed, StatusCancelled} {
		for _, to := range allStatuses {
			if AllowedTransition(from, to) {
				t.Fatalf("terminal %s must have no outgoing edge to %s", from, to)
			}
		}
	}
}

func TestSubtaskAcceptanceRequired(t *testing.T) {
	_, err := Transition(StatusRunning, StatusAccepted, "", "")
	var target *AcceptanceRequiredError
	if !errors.As(err, &target) {
		t.Fatalf("expected E_SUBTASK_ACCEPTANCE_REQUIRED, got %v", err)
	}
	if _, err := Transition(StatusRunning, StatusAccepted, "acc-1", ""); err != nil {
		t.Fatalf("accepted with acceptance id must pass: %v", err)
	}

	if _, err := Transition(StatusRunning, StatusWaitingHandoff, "", ""); err == nil {
		t.Fatal("waiting_handoff without handoff id must fail")
	}
	if _, err := Transition(StatusRunning, StatusWaitingHandoff, "", "h-1"); err != nil {
		t.Fatalf("waiting_handoff with handoff id must pass: %v", err)
	}
}

func TestSubtaskTransitionRejectsInvalid(t *testing.T) {
	if _, err := Transition(Status("bogus"), StatusRunning, "", ""); err == nil {
		t.Fatal("invalid from status must fail")
	}
	if _, err := Transition(StatusPending, Status("bogus"), "", ""); err == nil {
		t.Fatal("invalid to status must fail")
	}
	if _, err := Transition(StatusPending, StatusAccepted, "acc", ""); !errors.Is(err, ErrIllegalSubtaskTransition) {
		t.Fatalf("illegal transition must wrap sentinel: %v", err)
	}
	if _, err := ParseStatus("done"); err == nil {
		t.Fatal("ParseStatus must reject unknown")
	}
}

func TestSubtaskModelValidation(t *testing.T) {
	base := Subtask{
		SubtaskID: "st-1", ParentTaskID: "task-1", TenantID: "t", ProjectID: "p",
		DagID: "dag-1", DagNodeID: "n1", Depth: 1, Role: "coder",
		Status: StatusPending, FailureStrategy: StrategyFailFast,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid subtask rejected: %v", err)
	}

	cases := []func(*Subtask){
		func(s *Subtask) { s.SubtaskID = "" },
		func(s *Subtask) { s.Depth = 0 },
		func(s *Subtask) { s.Depth = 3 },
		func(s *Subtask) { s.Role = "" },
		func(s *Subtask) { s.Status = Status("done") },
		func(s *Subtask) { s.FailureStrategy = FailureStrategy("bogus") },
		func(s *Subtask) { s.Status = StatusAccepted; s.AcceptanceID = "" },
		func(s *Subtask) { s.Status = StatusWaitingHandoff; s.HandoffID = "" },
	}
	for i, mut := range cases {
		bad := base
		mut(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("case %d must fail validation: %+v", i, bad)
		}
	}
}

func setupRepo(t *testing.T) (*Repo, string) {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	taskID := "parent-" + time.Now().Format("150405.000000000")
	if _, err := db.Exec(ctx, `
		INSERT INTO hlp_task.task_runs
			(task_id, tenant_id, project_id, workspace_root, goal, goal_hash, current_stage, status, policy_snapshot)
		VALUES ($1,'tenant-st','proj-st','/w','g','h','b','running','{}'::jsonb)`, taskID); err != nil {
		t.Fatalf("insert parent: %v", err)
	}
	return NewRepo(db), taskID
}

func TestSubtaskRepoCrossTenantRejected(t *testing.T) {
	repo, parent := setupRepo(t)
	err := repo.Insert(context.Background(), &Subtask{
		SubtaskID: "st-cross", ParentTaskID: parent, TenantID: "other", ProjectID: "proj-st",
		DagID: "dag-1", DagNodeID: "n1", Depth: 1, Role: "coder", Status: StatusPending,
	})
	if err == nil {
		t.Fatal("cross-tenant insert must fail")
	}
}

func TestSubtaskRepoDBRejectsUnaccepted(t *testing.T) {
	repo, parent := setupRepo(t)
	ctx := context.Background()
	subtaskID := "st-db-" + parent
	if err := repo.Insert(ctx, &Subtask{
		SubtaskID: subtaskID, ParentTaskID: parent, TenantID: "tenant-st", ProjectID: "proj-st",
		DagID: "dag-1", DagNodeID: "n1", Depth: 1, Role: "coder", Status: StatusPending,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.Transition(ctx, subtaskID, StatusRunning, "", ""); err != nil {
		t.Fatalf("pending->running: %v", err)
	}
	// Direct DB update bypassing the API must be rejected by the CHECK constraint.
	err := repo.Transition(ctx, subtaskID, StatusAccepted, "", "")
	var ae *AcceptanceRequiredError
	if !errors.As(err, &ae) {
		t.Fatalf("api must reject accepted without acceptance id: %v", err)
	}

	// Bypassing the API must also be rejected by the DB CHECK constraint.
	if _, dbErr := repo.db.Exec(ctx, `UPDATE hlp_task.subtasks SET status='accepted' WHERE subtask_id = $1`, subtaskID); dbErr == nil {
		t.Fatal("database constraint must reject accepted without acceptance_id")
	}
}

func TestSubtaskRepoAttemptAndConsumed(t *testing.T) {
	repo, parent := setupRepo(t)
	ctx := context.Background()
	subtaskID := "st-att-" + parent
	st := &Subtask{
		SubtaskID: subtaskID, ParentTaskID: parent, TenantID: "tenant-st", ProjectID: "proj-st",
		DagID: "dag-1", DagNodeID: "n1", Depth: 1, Role: "coder", Status: StatusPending,
	}
	if err := repo.Insert(ctx, st); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for want := 1; want <= 3; want++ {
		got, err := repo.NextAttemptNo(ctx, subtaskID)
		if err != nil {
			t.Fatalf("attempt: %v", err)
		}
		if got != want {
			t.Fatalf("attempt_no = %d, want %d", got, want)
		}
	}
	if err := repo.AddConsumed(ctx, subtaskID, 100, 0.5); err != nil {
		t.Fatalf("add consumed: %v", err)
	}
	got, err := repo.Get(ctx, subtaskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ConsumedTokens != 100 || got.ConsumedCost != 0.5 {
		t.Fatalf("subtask consumed = %+v", got)
	}
	var parentTokens int64
	if err := repo.db.QueryRow(ctx, `SELECT consumed_tokens FROM hlp_task.task_runs WHERE task_id = $1`, parent).Scan(&parentTokens); err != nil {
		t.Fatalf("load parent consumed: %v", err)
	}
	if parentTokens != 100 {
		t.Fatalf("consumed must roll up to parent budget pool, got %d", parentTokens)
	}
}
