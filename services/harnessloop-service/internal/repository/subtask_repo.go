package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
	alloc_tokens, consumed_tokens, consumed_cost::float8, created_at, updated_at`

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
		&row.ConsumedCost, &row.CreatedAt, &row.UpdatedAt); err != nil {
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
				alloc_tokens, consumed_tokens, consumed_cost, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
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
			row.AllocTokens, row.ConsumedTokens, row.ConsumedCost, row.CreatedAt, row.UpdatedAt); err != nil {
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
