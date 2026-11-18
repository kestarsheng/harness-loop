package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
	"agent-platform/pkg/pgindex"
)

// SubtaskRow mirrors hlp_task.subtasks. JSON columns are kept as raw bytes so
// the API returns exactly what the orchestrator persisted.
type SubtaskRow struct {
	SubtaskID          string
	ParentTaskID       string
	TenantID           string
	ProjectID          string
	DagID              string
	DagNodeID          string
	Depth              int
	IndexInParent      int
	ParallelSetID      string
	Role               string
	Status             string
	DependsOn          []string
	InputContract      []byte
	OutputContract     []byte
	AcceptanceCriteria []byte
	AcceptanceID       string
	FailureStrategy    string
	AttemptNo          int
	ArtifactRefs       []byte
	HandoffID          string
	AllocTokens        int64
	ConsumedTokens     int64
	TokenSource        string
	ConsumedCost       float64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type SubtaskRepo struct {
	db *pgindex.DB
}

func NewSubtaskRepo(db *pgindex.DB) *SubtaskRepo { return &SubtaskRepo{db: db} }

const subtaskColumns = `subtask_id, parent_task_id, tenant_id, project_id, dag_id, dag_node_id,
	depth, index_in_parent, COALESCE(parallel_set_id,''), role, status, depends_on,
	input_contract, output_contract, acceptance_criteria, COALESCE(acceptance_id,''),
	failure_strategy, attempt_no, artifact_refs, COALESCE(handoff_id,''),
	alloc_tokens, consumed_tokens, token_source, consumed_cost::float8, created_at, updated_at`

func scanSubtask(s rowScanner) (*SubtaskRow, error) {
	var (
		row     SubtaskRow
		depends []byte
	)
	if err := s.Scan(&row.SubtaskID, &row.ParentTaskID, &row.TenantID, &row.ProjectID,
		&row.DagID, &row.DagNodeID, &row.Depth, &row.IndexInParent, &row.ParallelSetID,
		&row.Role, &row.Status, &depends, &row.InputContract, &row.OutputContract,
		&row.AcceptanceCriteria, &row.AcceptanceID, &row.FailureStrategy, &row.AttemptNo,
		&row.ArtifactRefs, &row.HandoffID, &row.AllocTokens, &row.ConsumedTokens,
		&row.TokenSource, &row.ConsumedCost, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return nil, err
	}
	if len(depends) > 0 {
		if err := json.Unmarshal(depends, &row.DependsOn); err != nil {
			return nil, fmt.Errorf("subtask_repo: decode depends_on: %w", err)
		}
	}
	return &row, nil
}

// List returns the subtasks of one parent task, always scoped by tenant. An
// empty projectID means "any project within the tenant".
func (r *SubtaskRepo) List(ctx context.Context, tenantID, projectID, parentTaskID string, limit int) ([]*SubtaskRow, error) {
	if tenantID == "" || parentTaskID == "" {
		return nil, fmt.Errorf("subtask_repo: tenant and parent task are required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := r.db.Query(ctx, `SELECT `+subtaskColumns+`
		FROM hlp_task.subtasks
		WHERE tenant_id = $1
		  AND ($2 = '' OR project_id = $2)
		  AND parent_task_id = $3
		ORDER BY dag_id, index_in_parent, subtask_id
		LIMIT $4`, tenantID, projectID, parentTaskID, limit)
	if err != nil {
		return nil, fmt.Errorf("subtask_repo: list: %w", err)
	}
	defer rows.Close()

	var out []*SubtaskRow
	for rows.Next() {
		row, err := scanSubtask(rows)
		if err != nil {
			return nil, fmt.Errorf("subtask_repo: scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// InsertBatch upserts the subtasks produced by the b-stage planner. The unique
// key is (parent_task_id, dag_id, dag_node_id), so replanning the same node
// updates it instead of duplicating rows.
func (r *SubtaskRepo) InsertBatch(ctx context.Context, rows []*SubtaskRow) error {
	if len(rows) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for _, row := range rows {
		if row == nil {
			continue
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = now
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = now
		}
		depends := row.DependsOn
		if depends == nil {
			depends = []string{}
		}
		dependsJSON, err := json.Marshal(depends)
		if err != nil {
			return fmt.Errorf("subtask_repo: marshal depends_on: %w", err)
		}
		input := row.InputContract
		if len(input) == 0 {
			input = []byte("{}")
		}
		output := row.OutputContract
		if len(output) == 0 {
			output = []byte("{}")
		}
		acceptance := row.AcceptanceCriteria
		if len(acceptance) == 0 {
			acceptance = []byte("[]")
		}
		artifacts := row.ArtifactRefs
		if len(artifacts) == 0 {
			artifacts = []byte("[]")
		}
		if _, err := r.db.Exec(ctx, `INSERT INTO hlp_task.subtasks (
				subtask_id, parent_task_id, tenant_id, project_id, dag_id, dag_node_id,
				depth, index_in_parent, parallel_set_id, role, status, depends_on,
				input_contract, output_contract, acceptance_criteria, acceptance_id,
				failure_strategy, attempt_no, artifact_refs, handoff_id,
				alloc_tokens, consumed_tokens, token_source, consumed_cost, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)
			ON CONFLICT (parent_task_id, dag_id, dag_node_id) DO UPDATE SET
				subtask_id = EXCLUDED.subtask_id,
				status = EXCLUDED.status,
				role = EXCLUDED.role,
				parallel_set_id = EXCLUDED.parallel_set_id,
				depends_on = EXCLUDED.depends_on,
				input_contract = EXCLUDED.input_contract,
				output_contract = EXCLUDED.output_contract,
				acceptance_criteria = EXCLUDED.acceptance_criteria,
				failure_strategy = EXCLUDED.failure_strategy,
				updated_at = EXCLUDED.updated_at`,
			row.SubtaskID, row.ParentTaskID, row.TenantID, row.ProjectID, row.DagID, row.DagNodeID,
			row.Depth, row.IndexInParent, row.ParallelSetID, row.Role, row.Status, dependsJSON,
			input, output, acceptance, nullIfEmpty(row.AcceptanceID),
			row.FailureStrategy, row.AttemptNo, artifacts, nullIfEmpty(row.HandoffID),
			row.AllocTokens, row.ConsumedTokens, tokenSource(row.TokenSource), row.ConsumedCost, row.CreatedAt, row.UpdatedAt); err != nil {
			return fmt.Errorf("subtask_repo: insert %s: %w", row.SubtaskID, err)
		}
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var (
	// ErrSubtaskStatusConflict means the row is not in the status the caller
	// observed; the write is refused instead of rewinding a terminal state.
	ErrSubtaskStatusConflict = errors.New("subtask_repo: status conflict")
	// ErrSubtaskTransitionIllegal mirrors the orchestrator state machine for
	// callers that bypass it.
	ErrSubtaskTransitionIllegal = errors.New("subtask_repo: illegal status transition")
)

// SubtaskExecution is the write-back payload of one subtask transition. The
// executor's consumption numbers and artifacts belong to the same row update as
// the status: splitting them lets a row say "accepted" with zero evidence.
type SubtaskExecution struct {
	SubtaskID      string
	FromStatus     string
	ToStatus       string
	AttemptNo      int
	ConsumedTokens int64
	TokenSource    string
	ConsumedCost   float64
	ArtifactRefs   []byte
	AcceptanceID   string
}

// UpdateStatus persists one status change. It is a thin wrapper over
// UpdateExecution for callers that only move the state machine.
func (r *SubtaskRepo) UpdateStatus(ctx context.Context, subtaskID, fromStatus, toStatus string) error {
	return r.UpdateExecution(ctx, SubtaskExecution{
		SubtaskID:  subtaskID,
		FromStatus: fromStatus,
		ToStatus:   toStatus,
	})
}

// UpdateExecution persists a subtask transition with its execution outcome.
//
// The state machine is enforced twice: in Go for a readable error, and in the
// UPDATE's WHERE clause (`status = from_status`) so a concurrent writer cannot
// pass both checks and rewind a terminal row. Requiring from_status also means a
// zero-row result is reported instead of silently updating the wrong state.
func (r *SubtaskRepo) UpdateExecution(ctx context.Context, ex SubtaskExecution) error {
	if ex.SubtaskID == "" {
		return fmt.Errorf("subtask_repo: subtask_id is required")
	}
	from, err := subtask.ParseStatus(ex.FromStatus)
	if err != nil {
		return fmt.Errorf("subtask_repo: from_status: %w", err)
	}
	to, err := subtask.ParseStatus(ex.ToStatus)
	if err != nil {
		return fmt.Errorf("subtask_repo: to_status: %w", err)
	}
	if !subtask.AllowedTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrSubtaskTransitionIllegal, from, to)
	}
	if to == subtask.StatusAccepted && ex.AcceptanceID == "" {
		return fmt.Errorf("subtask_repo: accepted subtask %s requires acceptance_id", ex.SubtaskID)
	}
	artifacts := ex.ArtifactRefs
	if len(artifacts) == 0 {
		artifacts = []byte("[]")
	}
	tag, err := r.db.Exec(ctx, `UPDATE hlp_task.subtasks SET
			status = $3,
			attempt_no = $4,
			consumed_tokens = $5,
			consumed_cost = $6,
			artifact_refs = $7::jsonb,
			acceptance_id = COALESCE($8, acceptance_id),
			token_source = $9,
			updated_at = now()
		WHERE subtask_id = $1 AND status = $2`,
		ex.SubtaskID, ex.FromStatus, ex.ToStatus, ex.AttemptNo,
		ex.ConsumedTokens, ex.ConsumedCost, artifacts, nullIfEmpty(ex.AcceptanceID), tokenSource(ex.TokenSource))
	if err != nil {
		return fmt.Errorf("subtask_repo: update %s: %w", ex.SubtaskID, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// Zero rows: either the row is already at to_status (a retried write is a
	// no-op, not a failure) or a concurrent writer moved it elsewhere.
	var current string
	if qerr := r.db.QueryRow(ctx, `SELECT status FROM hlp_task.subtasks WHERE subtask_id = $1`, ex.SubtaskID).Scan(&current); qerr != nil {
		return fmt.Errorf("subtask_repo: read %s status: %w", ex.SubtaskID, qerr)
	}
	if current == ex.ToStatus {
		return nil
	}
	return fmt.Errorf("%w: %s is %q, expected %q", ErrSubtaskStatusConflict, ex.SubtaskID, current, ex.FromStatus)
}

// tokenSource normalizes the consumption provenance written to the row.
// Anything other than an explicit "actual" is labelled "estimate" so a missing
// or malformed label can never claim billed tokens.
func tokenSource(s string) string {
	if s == "actual" {
		return "actual"
	}
	return "estimate"
}
