package subtask

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	pgdb "agent-platform/pkg/pgindex"
)

type Repo struct {
	db *pgdb.DB
}

func NewRepo(db *pgdb.DB) *Repo { return &Repo{db: db} }

func (r *Repo) Insert(ctx context.Context, s *Subtask) error {
	if err := s.Validate(); err != nil {
		return err
	}
	var parentTenant, parentProject string
	err := r.db.QueryRow(ctx, `
		SELECT tenant_id, project_id FROM hlp_task.task_runs WHERE task_id = $1`,
		s.ParentTaskID).Scan(&parentTenant, &parentProject)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("%w: parent task %s not found", ErrInvalidSubtask, s.ParentTaskID)
		}
		return fmt.Errorf("subtask: load parent: %w", err)
	}
	if parentTenant != s.TenantID || parentProject != s.ProjectID {
		return fmt.Errorf("%w: tenant/project must match parent", ErrInvalidSubtask)
	}
	if s.Status == "" {
		s.Status = StatusPending
	}
	if s.FailureStrategy == "" {
		s.FailureStrategy = StrategyFailFast
	}

	dependsOn, _ := json.Marshal(s.DependsOn)
	inputContract, _ := json.Marshal(s.InputContract)
	outputContract, _ := json.Marshal(s.OutputContract)
	criteria, _ := json.Marshal(s.AcceptanceCriteria)
	artifacts, _ := json.Marshal(s.ArtifactRefs)

	if _, err := r.db.Exec(ctx, `
		INSERT INTO hlp_task.subtasks
			(subtask_id, parent_task_id, tenant_id, project_id, dag_id, dag_node_id, depth,
			 index_in_parent, parallel_set_id, role, status, depends_on, input_contract,
			 output_contract, acceptance_criteria, acceptance_id, failure_strategy, attempt_no,
			 artifact_refs, handoff_id, alloc_tokens, consumed_tokens, consumed_cost)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13,$14,$15,NULLIF($16,''),
		        $17,$18,$19,NULLIF($20,''),$21,$22,$23)`,
		s.SubtaskID, s.ParentTaskID, s.TenantID, s.ProjectID, s.DagID, s.DagNodeID, s.Depth,
		s.IndexInParent, s.ParallelSetID, s.Role, string(s.Status), dependsOn, inputContract,
		outputContract, criteria, s.AcceptanceID, string(s.FailureStrategy), s.AttemptNo,
		artifacts, s.HandoffID, s.AllocTokens, s.ConsumedTokens, s.ConsumedCost); err != nil {
		return fmt.Errorf("subtask: insert %s: %w", s.SubtaskID, err)
	}
	return nil
}

func (r *Repo) Get(ctx context.Context, subtaskID string) (*Subtask, error) {
	row := r.db.QueryRow(ctx, `
		SELECT subtask_id, parent_task_id, tenant_id, project_id, dag_id, dag_node_id, depth,
		       index_in_parent, COALESCE(parallel_set_id,''), role, status, depends_on,
		       input_contract, output_contract, acceptance_criteria, COALESCE(acceptance_id,''),
		       failure_strategy, attempt_no, artifact_refs, COALESCE(handoff_id,''),
		       alloc_tokens, consumed_tokens, consumed_cost, created_at, updated_at
		FROM hlp_task.subtasks WHERE subtask_id = $1`, subtaskID)
	s, err := scanSubtask(row)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func scanSubtask(row rowScanner) (*Subtask, error) {
	var (
		s                                               Subtask
		status, strategy                                string
		dependsOn, inputC, outputC, criteria, artifacts []byte
	)
	if err := row.Scan(&s.SubtaskID, &s.ParentTaskID, &s.TenantID, &s.ProjectID, &s.DagID,
		&s.DagNodeID, &s.Depth, &s.IndexInParent, &s.ParallelSetID, &s.Role, &status,
		&dependsOn, &inputC, &outputC, &criteria, &s.AcceptanceID, &strategy, &s.AttemptNo,
		&artifacts, &s.HandoffID, &s.AllocTokens, &s.ConsumedTokens, &s.ConsumedCost,
		&s.CreatedAt, &s.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("E_SUBTASK_NOT_FOUND: %s", s.SubtaskID)
		}
		return nil, fmt.Errorf("subtask: scan: %w", err)
	}
	s.Status = Status(status)
	s.FailureStrategy = FailureStrategy(strategy)
	_ = json.Unmarshal(dependsOn, &s.DependsOn)
	_ = json.Unmarshal(inputC, &s.InputContract)
	_ = json.Unmarshal(outputC, &s.OutputContract)
	_ = json.Unmarshal(criteria, &s.AcceptanceCriteria)
	_ = json.Unmarshal(artifacts, &s.ArtifactRefs)
	return &s, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (r *Repo) ListByParent(ctx context.Context, parentTaskID, dagID string) ([]*Subtask, error) {
	rows, err := r.db.Query(ctx, `
		SELECT subtask_id, parent_task_id, tenant_id, project_id, dag_id, dag_node_id, depth,
		       index_in_parent, COALESCE(parallel_set_id,''), role, status, depends_on,
		       input_contract, output_contract, acceptance_criteria, COALESCE(acceptance_id,''),
		       failure_strategy, attempt_no, artifact_refs, COALESCE(handoff_id,''),
		       alloc_tokens, consumed_tokens, consumed_cost, created_at, updated_at
		FROM hlp_task.subtasks
		WHERE parent_task_id = $1 AND ($2 = '' OR dag_id = $2)
		ORDER BY dag_id, index_in_parent`, parentTaskID, dagID)
	if err != nil {
		return nil, fmt.Errorf("subtask: list: %w", err)
	}
	defer rows.Close()
	var out []*Subtask
	for rows.Next() {
		s, err := scanSubtask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repo) Transition(ctx context.Context, subtaskID string, to Status, acceptanceID, handoffID string) error {
	current, err := r.Get(ctx, subtaskID)
	if err != nil {
		return err
	}
	next, err := Transition(current.Status, to, acceptanceID, handoffID)
	if err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE hlp_task.subtasks
		SET status = $2, acceptance_id = NULLIF($3,''), handoff_id = NULLIF($4,''), updated_at = now()
		WHERE subtask_id = $1 AND status = $5`,
		subtaskID, string(next), acceptanceID, handoffID, string(current.Status))
	if err != nil {
		return fmt.Errorf("subtask: transition %s: %w", subtaskID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: concurrent status change", ErrIllegalSubtaskTransition)
	}
	return nil
}

func (r *Repo) NextAttemptNo(ctx context.Context, subtaskID string) (int, error) {
	var no int
	err := r.db.QueryRow(ctx, `
		UPDATE hlp_task.subtasks SET attempt_no = attempt_no + 1, updated_at = now()
		WHERE subtask_id = $1
		RETURNING attempt_no`, subtaskID).Scan(&no)
	if err != nil {
		return 0, fmt.Errorf("subtask: next attempt no: %w", err)
	}
	return no, nil
}

func (r *Repo) AddConsumed(ctx context.Context, subtaskID string, tokens int64, cost float64) error {
	tx, err := r.db.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("subtask: begin consumed: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var parentTaskID string
	if err := tx.QueryRow(ctx, `
		UPDATE hlp_task.subtasks
		SET consumed_tokens = consumed_tokens + $2, consumed_cost = consumed_cost + $3, updated_at = now()
		WHERE subtask_id = $1
		RETURNING parent_task_id`, subtaskID, tokens, cost).Scan(&parentTaskID); err != nil {
		return fmt.Errorf("subtask: add consumed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE hlp_task.task_runs
		SET consumed_tokens = consumed_tokens + $2, consumed_cost = consumed_cost + $3, updated_at = now()
		WHERE task_id = $1`, parentTaskID, tokens, cost); err != nil {
		return fmt.Errorf("subtask: rollup consumed: %w", err)
	}
	return tx.Commit(ctx)
}
