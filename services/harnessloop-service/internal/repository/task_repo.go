package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/pgindex"
)

var (
	ErrTaskNotFound     = errors.New("task not found")
	ErrVersionConflict  = errors.New("version conflict")
	ErrInvalidTaskState = errors.New("invalid task state")
)

type TaskStatus string

const (
	TaskRunning   TaskStatus = "running"
	TaskPaused    TaskStatus = "paused"
	TaskCompleted TaskStatus = "completed"
	TaskAborted   TaskStatus = "aborted"
	TaskFailed    TaskStatus = "failed"
)

func (s TaskStatus) Valid() bool {
	switch s {
	case TaskRunning, TaskPaused, TaskCompleted, TaskAborted, TaskFailed:
		return true
	}
	return false
}

type TaskRun struct {
	TaskID         string
	TenantID       string
	ProjectID      string
	WorkspaceRoot  string
	Goal           string
	GoalHash       string
	CurrentStage   fsm.Stage
	Status         TaskStatus
	TerminalReason string
	AttemptNo      map[string]int
	RetryCount     map[string]int
	AcceptanceFail map[string]int
	BudgetTokens   int64
	BudgetCost     float64
	ConsumedTokens int64
	ConsumedCost   float64
	FSMEnabled     bool
	PolicySnapshot []byte
	Version        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type EventInsert struct {
	EventID       string
	Seq           int64
	EventType     string
	SchemaVersion string
	Severity      string
	Stage         fsm.Stage
	Attempt       int
	Payload       []byte
}

type ReconcileResult struct {
	SnapshotStage fsm.Stage
	EventStage    fsm.Stage
	MaxSeq        int64
	Drift         bool
}

type TaskRepo struct {
	db *pgindex.DB
}

func NewTaskRepo(db *pgindex.DB) *TaskRepo { return &TaskRepo{db: db} }

func nullJSON(m map[string]int) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func (r *TaskRepo) Create(ctx context.Context, t *TaskRun) error {
	if t.TaskID == "" || t.TenantID == "" || t.ProjectID == "" {
		return fmt.Errorf("task_repo: task_id/tenant_id/project_id are required")
	}
	if !t.CurrentStage.Valid() {
		return fmt.Errorf("task_repo: invalid stage %q", t.CurrentStage)
	}
	if t.Status == "" {
		t.Status = TaskRunning
	}
	if !t.Status.Valid() {
		return fmt.Errorf("task_repo: invalid status %q", t.Status)
	}
	if len(t.PolicySnapshot) == 0 {
		t.PolicySnapshot = []byte("{}")
	}
	if t.AttemptNo == nil {
		t.AttemptNo = map[string]int{}
	}
	if t.RetryCount == nil {
		t.RetryCount = map[string]int{}
	}
	if t.AcceptanceFail == nil {
		t.AcceptanceFail = map[string]int{}
	}

	_, err := r.db.Exec(ctx, `
		INSERT INTO hlp_task.task_runs
			(task_id, tenant_id, project_id, workspace_root, goal, goal_hash, current_stage, status,
			 terminal_reason, attempt_no, retry_count, acceptance_fail, budget_tokens, budget_cost,
			 consumed_tokens, consumed_cost, fsm_enabled, policy_snapshot, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,0)`,
		t.TaskID, t.TenantID, t.ProjectID, t.WorkspaceRoot, t.Goal, t.GoalHash,
		string(t.CurrentStage), string(t.Status), nullable(t.TerminalReason),
		nullJSON(t.AttemptNo), nullJSON(t.RetryCount), nullJSON(t.AcceptanceFail),
		t.BudgetTokens, t.BudgetCost, t.ConsumedTokens, t.ConsumedCost,
		t.FSMEnabled, t.PolicySnapshot)
	if err != nil {
		return fmt.Errorf("task_repo: create %s: %w", t.TaskID, err)
	}
	t.Version = 0
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const taskColumns = `task_id, tenant_id, project_id, workspace_root, goal, goal_hash,
	current_stage, status, COALESCE(terminal_reason, ''), attempt_no, retry_count, acceptance_fail,
	budget_tokens, budget_cost, consumed_tokens, consumed_cost, fsm_enabled, policy_snapshot,
	version, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(s rowScanner) (*TaskRun, error) {
	var (
		t              TaskRun
		stage, status  string
		attempt, retry []byte
		fail           []byte
		policy         []byte
	)
	if err := s.Scan(&t.TaskID, &t.TenantID, &t.ProjectID, &t.WorkspaceRoot, &t.Goal, &t.GoalHash,
		&stage, &status, &t.TerminalReason, &attempt, &retry, &fail,
		&t.BudgetTokens, &t.BudgetCost, &t.ConsumedTokens, &t.ConsumedCost, &t.FSMEnabled, &policy,
		&t.Version, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.CurrentStage = fsm.Stage(stage)
	t.Status = TaskStatus(status)
	t.PolicySnapshot = policy
	t.AttemptNo = decodeCounts(attempt)
	t.RetryCount = decodeCounts(retry)
	t.AcceptanceFail = decodeCounts(fail)
	return &t, nil
}

func decodeCounts(raw []byte) map[string]int {
	out := map[string]int{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func (r *TaskRepo) Get(ctx context.Context, tenantID, taskID string) (*TaskRun, error) {
	row := r.db.QueryRow(ctx, `SELECT `+taskColumns+` FROM hlp_task.task_runs WHERE task_id = $1 AND tenant_id = $2`, taskID, tenantID)
	t, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("task_repo: get %s: %w", taskID, err)
	}
	return t, nil
}

func (r *TaskRepo) List(ctx context.Context, tenantID, projectID string, statuses []string, limit int) ([]*TaskRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+taskColumns+`
		FROM hlp_task.task_runs
		WHERE tenant_id = $1 AND ($2 = '' OR project_id = $2)
		  AND (COALESCE(cardinality($3::text[]), 0) = 0 OR status = ANY($3))
		ORDER BY updated_at DESC
		LIMIT $4`, tenantID, projectID, statuses, limit)
	if err != nil {
		return nil, fmt.Errorf("task_repo: list: %w", err)
	}
	defer rows.Close()

	var out []*TaskRun
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("task_repo: scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *TaskRepo) Transfer(ctx context.Context, tenantID, taskID string, to fsm.Stage, expectedVersion int64, events []EventInsert) (*TaskRun, error) {
	if !to.Valid() {
		return nil, fmt.Errorf("task_repo: invalid target stage %q", to)
	}
	tx, err := r.db.Pool().Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("task_repo: begin transfer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current int64
	var stage string
	if err := tx.QueryRow(ctx, `
		SELECT version, current_stage FROM hlp_task.task_runs
		WHERE task_id = $1 AND tenant_id = $2
		FOR UPDATE`, taskID, tenantID).Scan(&current, &stage); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("task_repo: lock task: %w", err)
	}
	if current != expectedVersion {
		return nil, fmt.Errorf("%w: expected %d, actual %d", ErrVersionConflict, expectedVersion, current)
	}

	for _, e := range events {
		if err := insertEvent(ctx, tx, tenantID, taskID, e); err != nil {
			return nil, err
		}
	}

	tag, err := tx.Exec(ctx, `
		UPDATE hlp_task.task_runs
		SET current_stage = $3, version = version + 1, updated_at = now()
		WHERE task_id = $1 AND tenant_id = $2 AND version = $4`,
		taskID, tenantID, string(to), expectedVersion)
	if err != nil {
		return nil, fmt.Errorf("task_repo: update stage: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrVersionConflict
	}

	t, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM hlp_task.task_runs WHERE task_id = $1 AND tenant_id = $2`, taskID, tenantID))
	if err != nil {
		return nil, fmt.Errorf("task_repo: reload after transfer: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("task_repo: commit transfer: %w", err)
	}
	return t, nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, tenantID, taskID string, e EventInsert) error {
	if e.EventID == "" || e.EventType == "" {
		return fmt.Errorf("task_repo: event_id and event_type are required")
	}
	severity := e.Severity
	if severity == "" {
		severity = "info"
	}
	schema := e.SchemaVersion
	if schema == "" {
		schema = "1.0.0"
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO hlp_event.events
			(event_id, task_id, tenant_id, project_id, seq, event_type, schema_version, stage, attempt, severity, ts, payload)
		SELECT $1, $2, $3, project_id, $4, $5, $6, NULLIF($7, ''), NULLIF($8, 0), $9, now(), $10
		FROM hlp_task.task_runs WHERE task_id = $2 AND tenant_id = $3`,
		e.EventID, taskID, tenantID, e.Seq, e.EventType, schema, string(e.Stage), e.Attempt, severity, payload)
	if err != nil {
		return fmt.Errorf("task_repo: insert event %s: %w", e.EventID, err)
	}
	return nil
}

func (r *TaskRepo) UpdateStatus(ctx context.Context, tenantID, taskID string, status TaskStatus, terminalReason string, expectedVersion int64) (*TaskRun, error) {
	if !status.Valid() {
		return nil, fmt.Errorf("task_repo: invalid status %q", status)
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE hlp_task.task_runs
		SET status = $3, terminal_reason = $4, version = version + 1, updated_at = now()
		WHERE task_id = $1 AND tenant_id = $2 AND version = $5`,
		taskID, tenantID, string(status), nullable(terminalReason), expectedVersion)
	if err != nil {
		return nil, fmt.Errorf("task_repo: update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrVersionConflict
	}
	return r.Get(ctx, tenantID, taskID)
}

func (r *TaskRepo) UpdateAttemptNo(ctx context.Context, tenantID, taskID string, stage fsm.Stage, no int, expectedVersion int64) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE hlp_task.task_runs
		SET attempt_no = jsonb_set(attempt_no, ARRAY[$3], to_jsonb($4::int), true),
		    version = version + 1, updated_at = now()
		WHERE task_id = $1 AND tenant_id = $2 AND version = $5`,
		taskID, tenantID, string(stage), no, expectedVersion)
	if err != nil {
		return fmt.Errorf("task_repo: update attempt no: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrVersionConflict
	}
	return nil
}

func (r *TaskRepo) AddConsumed(ctx context.Context, tenantID, taskID string, tokens int64, cost float64) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE hlp_task.task_runs
		SET consumed_tokens = consumed_tokens + $3, consumed_cost = consumed_cost + $4, updated_at = now()
		WHERE task_id = $1 AND tenant_id = $2`, taskID, tenantID, tokens, cost)
	if err != nil {
		return fmt.Errorf("task_repo: add consumed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotFound
	}
	return nil
}

func (r *TaskRepo) AbortLock(ctx context.Context, tenantID, taskID string, fn func(tx pgx.Tx, t *TaskRun) error) error {
	tx, err := r.db.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("task_repo: begin abort: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	t, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM hlp_task.task_runs WHERE task_id = $1 AND tenant_id = $2 FOR UPDATE`, taskID, tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTaskNotFound
		}
		return fmt.Errorf("task_repo: abort lock: %w", err)
	}
	if err := fn(tx, t); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *TaskRepo) Reconcile(ctx context.Context, tenantID, taskID string) (ReconcileResult, error) {
	var res ReconcileResult
	row := r.db.QueryRow(ctx, `
		SELECT t.current_stage, COALESCE(MAX(e.seq), 0)
		FROM hlp_task.task_runs t
		LEFT JOIN hlp_event.events e ON e.task_id = t.task_id AND e.tenant_id = t.tenant_id
		WHERE t.task_id = $1 AND t.tenant_id = $2
		GROUP BY t.current_stage`, taskID, tenantID)
	if err := row.Scan(&res.SnapshotStage, &res.MaxSeq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return res, ErrTaskNotFound
		}
		return res, fmt.Errorf("task_repo: reconcile snapshot: %w", err)
	}

	var eventStage string
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(payload->>'to', '')
		FROM hlp_event.events
		WHERE task_id = $1 AND tenant_id = $2 AND event_type = 'stage.transition'
		ORDER BY seq DESC LIMIT 1`, taskID, tenantID).Scan(&eventStage)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("task_repo: reconcile events: %w", err)
	}
	res.EventStage = fsm.Stage(eventStage)
	res.Drift = res.EventStage != "" && res.EventStage != res.SnapshotStage
	return res, nil
}
