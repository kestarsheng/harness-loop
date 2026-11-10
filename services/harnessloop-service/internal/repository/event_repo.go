package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	eventpkg "agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/pgindex"
)

type EventFilter struct {
	TenantID  string
	TaskID    string
	Stage     string
	EventType string
	Severity  string
	SinceSeq  int64
	Limit     int
}

type EventRepo struct {
	db *pgindex.DB
}

func NewEventRepo(db *pgindex.DB) *EventRepo { return &EventRepo{db: db} }

func (r *EventRepo) Append(ctx context.Context, tenantID, projectID string, events []*eventpkg.Event) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	tx, err := r.db.Pool().Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("event_repo: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inserted := 0
	for _, e := range events {
		if e == nil {
			continue
		}
		if e.TenantID == "" {
			e.TenantID = tenantID
		}
		if e.ProjectID == "" {
			e.ProjectID = projectID
		}
		if err := e.Validate(); err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO hlp_event.events
				(event_id, task_id, tenant_id, project_id, seq, event_type, schema_version, stage, attempt,
				 severity, trace_id, span_id, ts, payload, payload_ref, agent_id,
				 parent_task_id, subtask_id, handoff_id, dag_id, dag_node_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,0),$10,NULLIF($11,''),NULLIF($12,''),$13,$14,NULLIF($15,''),NULLIF($21,''),NULLIF($16,''),NULLIF($17,''),NULLIF($18,''),NULLIF($19,''),NULLIF($20,''))
			ON CONFLICT (event_id) DO NOTHING`,
			e.EventID, e.TaskID, e.TenantID, e.ProjectID, e.Seq, string(e.EventType), e.SchemaVersion,
			e.Stage, e.Attempt, string(e.Severity), e.TraceID, e.SpanID, e.Timestamp, e.Payload, e.PayloadRef,
			e.ParentTaskID, e.SubtaskID, e.HandoffID, e.DagID, e.DagNodeID, e.AgentID)
		if err != nil {
			return 0, fmt.Errorf("event_repo: append %s: %w", e.EventID, err)
		}
		inserted += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("event_repo: commit: %w", err)
	}
	return inserted, nil
}

const eventColumns = `event_id, event_type, schema_version, task_id, COALESCE(trace_id,''), COALESCE(span_id,''),
	COALESCE(stage,''), COALESCE(attempt,0), seq, ts, severity, tenant_id, project_id, COALESCE(agent_id,''),
	payload, COALESCE(payload_ref,''), COALESCE(parent_task_id,''), COALESCE(subtask_id,''), COALESCE(handoff_id,''),
	COALESCE(dag_id,''), COALESCE(dag_node_id,'')`

func scanEvent(s rowScanner) (*eventpkg.Event, error) {
	var (
		e        eventpkg.Event
		typ, sev string
	)
	if err := s.Scan(&e.EventID, &typ, &e.SchemaVersion, &e.TaskID, &e.TraceID, &e.SpanID,
		&e.Stage, &e.Attempt, &e.Seq, &e.Timestamp, &sev, &e.TenantID, &e.ProjectID, &e.AgentID,
		&e.Payload, &e.PayloadRef, &e.ParentTaskID, &e.SubtaskID, &e.HandoffID, &e.DagID, &e.DagNodeID); err != nil {
		return nil, err
	}
	e.EventType = eventpkg.EventType(typ)
	e.Severity = eventpkg.Severity(sev)
	return &e, nil
}

func (r *EventRepo) List(ctx context.Context, f EventFilter) ([]*eventpkg.Event, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.Query(ctx, `SELECT `+eventColumns+`
		FROM hlp_event.events
		WHERE tenant_id = $1
		  AND ($2 = '' OR task_id = $2)
		  AND ($3 = '' OR stage = $3)
		  AND ($4 = '' OR event_type = $4)
		  AND ($5 = '' OR severity = $5)
		  AND seq > $6
		ORDER BY seq
		LIMIT $7`,
		f.TenantID, f.TaskID, f.Stage, f.EventType, f.Severity, f.SinceSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("event_repo: list: %w", err)
	}
	defer rows.Close()

	var out []*eventpkg.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("event_repo: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *EventRepo) NextSeq(ctx context.Context, tenantID, taskID string) (int64, error) {
	var seq int64
	err := r.db.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM hlp_event.events WHERE tenant_id = $1 AND task_id = $2`, tenantID, taskID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("event_repo: next seq: %w", err)
	}
	return seq, nil
}

func (r *EventRepo) WatchFrom(ctx context.Context, tenantID string, taskIDs []string, sinceSeq int64, limit int) ([]*eventpkg.Event, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := r.db.Query(ctx, `SELECT `+eventColumns+`
		FROM hlp_event.events
		WHERE tenant_id = $1
		  AND seq > $2
		  AND (COALESCE(cardinality($3::text[]), 0) = 0 OR task_id = ANY($3))
		ORDER BY seq
		LIMIT $4`, tenantID, sinceSeq, taskIDs, limit)
	if err != nil {
		return nil, sinceSeq, fmt.Errorf("event_repo: watch: %w", err)
	}
	defer rows.Close()

	var (
		out  []*eventpkg.Event
		last = sinceSeq
	)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, sinceSeq, fmt.Errorf("event_repo: scan: %w", err)
		}
		out = append(out, e)
		if e.Seq > last {
			last = e.Seq
		}
	}
	return out, last, rows.Err()
}

func (r *EventRepo) Cleanup(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	tag, err := r.db.Exec(ctx, `
		DELETE FROM hlp_event.events
		WHERE ctid IN (
			SELECT ctid FROM hlp_event.events WHERE ts < $1 ORDER BY ts LIMIT $2
		)`, olderThan, limit)
	if err != nil {
		return 0, fmt.Errorf("event_repo: cleanup: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *EventRepo) GetByID(ctx context.Context, tenantID, eventID string) (*eventpkg.Event, error) {
	row := r.db.QueryRow(ctx, `SELECT `+eventColumns+` FROM hlp_event.events WHERE tenant_id = $1 AND event_id = $2`, tenantID, eventID)
	e, err := scanEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("event_repo: get %s: %w", eventID, err)
	}
	return e, nil
}
