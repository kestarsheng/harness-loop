package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/pkg/pgindex"
)

// AuditRepo writes permission decisions to hlp_event.audit. The 011 CHECK
// constraint requires kind=tool_decision rows to carry a non-null rule_id and a
// payload with the "layer" key, so both are normalised here.
type AuditRepo struct {
	db *pgindex.DB
}

func NewAuditRepo(db *pgindex.DB) *AuditRepo { return &AuditRepo{db: db} }

func (r *AuditRepo) AppendToolDecision(ctx context.Context, rec policy.AuditRecord) error {
	if rec.TenantID == "" {
		return fmt.Errorf("audit_repo: tenant_id is required")
	}
	ruleID := rec.RuleID
	if ruleID == "" {
		ruleID = "policy.rule.unspecified"
	}
	layer := string(rec.Layer)
	if layer == "" {
		layer = string(policy.LayerSystem)
	}
	payload := map[string]any{
		"layer":       layer,
		"code":        rec.Code,
		"stage":       rec.Stage,
		"role":        rec.Role,
		"agent_id":    rec.AgentID,
		"args_digest": rec.ArgsDigest,
		"path":        rec.PathNormalized,
		"duration_us": rec.DurationUS,
		"tags":        rec.Tags,
		"decision":    string(rec.Decision),
	}
	if len(rec.ArgsSummary) > 0 {
		payload["args_summary"] = rec.ArgsSummary
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("audit_repo: marshal payload: %w", err)
	}
	auditID := rec.AuditID
	if auditID == "" {
		auditID = fmt.Sprintf("audit-%s-%d", rec.Tool, time.Now().UnixNano())
	}
	ts := rec.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	target := rec.PathNormalized
	actor := rec.AgentID
	if actor == "" {
		actor = rec.Role
	}
	_, err = r.db.Pool().Exec(ctx, `
		INSERT INTO hlp_event.audit
			(audit_id, tenant_id, task_id, kind, actor, action, target, decision, rule_id, payload, created_at)
		VALUES ($1,$2,NULLIF($3,''),'tool_decision',NULLIF($4,''),$5,NULLIF($6,''),$7,$8,$9,$10)
		ON CONFLICT (audit_id) DO NOTHING`,
		auditID, rec.TenantID, rec.TaskID, actor, rec.Tool, target, string(rec.Decision), ruleID, raw, ts)
	if err != nil {
		return fmt.Errorf("audit_repo: insert: %w", err)
	}
	return nil
}
