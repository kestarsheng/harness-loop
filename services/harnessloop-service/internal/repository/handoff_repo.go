package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agent-platform/pkg/pgindex"
)

// HandoffRow is the aggregated view of one handoff. There is no dedicated
// handoff table in the schema yet; the durable source of truth is the
// hlp_event.events stream, where every handoff.* event carries handoff_id and
// the correlation fields required by design 4.12.2. This repo folds those
// events into one row per handoff_id.
type HandoffRow struct {
	HandoffID    string
	ParentTaskID string
	SubtaskID    string
	TenantID     string
	ProjectID    string
	FromAgentID  string
	ToAgentID    string
	Status       string
	Reason       string
	InputDigest  string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type HandoffRepo struct {
	db *pgindex.DB
}

func NewHandoffRepo(db *pgindex.DB) *HandoffRepo { return &HandoffRepo{db: db} }

// List returns handoffs scoped by tenant. taskID matches either the parent task
// or the event task_id, so callers can list a parent task's handoffs.
func (r *HandoffRepo) List(ctx context.Context, tenantID, projectID, taskID string, limit int) ([]*HandoffRow, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("handoff_repo: tenant is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := r.db.Query(ctx, `
		SELECT handoff_id, task_id, COALESCE(parent_task_id,''), COALESCE(subtask_id,''),
		       tenant_id, project_id, event_type, payload, ts
		FROM hlp_event.events
		WHERE tenant_id = $1
		  AND handoff_id IS NOT NULL
		  AND event_type LIKE 'handoff.%'
		  AND ($2 = '' OR project_id = $2)
		  AND ($3 = '' OR task_id = $3 OR parent_task_id = $3)
		ORDER BY handoff_id, seq`, tenantID, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("handoff_repo: list: %w", err)
	}
	defer rows.Close()

	type eventRow struct {
		handoffID    string
		taskID       string
		parentTaskID string
		subtaskID    string
		tenantID     string
		projectID    string
		eventType    string
		payload      []byte
		ts           time.Time
	}

	byID := make(map[string]*HandoffRow)
	order := make([]string, 0)
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(&e.handoffID, &e.taskID, &e.parentTaskID, &e.subtaskID,
			&e.tenantID, &e.projectID, &e.eventType, &e.payload, &e.ts); err != nil {
			return nil, fmt.Errorf("handoff_repo: scan: %w", err)
		}
		row, ok := byID[e.handoffID]
		if !ok {
			row = &HandoffRow{
				HandoffID:    e.handoffID,
				ParentTaskID: firstNonEmpty(e.parentTaskID, e.taskID),
				SubtaskID:    e.subtaskID,
				TenantID:     e.tenantID,
				ProjectID:    e.projectID,
				Status:       "created",
				CreatedAt:    e.ts,
				UpdatedAt:    e.ts,
			}
			byID[e.handoffID] = row
			order = append(order, e.handoffID)
		}
		if row.ParentTaskID == "" {
			row.ParentTaskID = firstNonEmpty(e.parentTaskID, e.taskID)
		}
		if row.SubtaskID == "" {
			row.SubtaskID = e.subtaskID
		}
		if e.ts.Before(row.CreatedAt) {
			row.CreatedAt = e.ts
		}
		if e.ts.After(row.UpdatedAt) {
			row.UpdatedAt = e.ts
		}
		if e.eventType == "handoff.accepted" || e.eventType == "handoff.rejected" {
			row.Status = strings.TrimPrefix(e.eventType, "handoff.")
		}

		var payload map[string]any
		if len(e.payload) > 0 {
			_ = json.Unmarshal(e.payload, &payload)
		}
		if v, ok := payload["from_agent_id"].(string); ok && v != "" {
			row.FromAgentID = v
		}
		if v, ok := payload["to_agent_id"].(string); ok && v != "" {
			row.ToAgentID = v
		}
		if v, ok := payload["input_digest"].(string); ok && v != "" {
			row.InputDigest = v
		}
		if v, ok := payload["reason"].(string); ok && v != "" {
			row.Reason = v
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]*HandoffRow, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].HandoffID < out[j].HandoffID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
