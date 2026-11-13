-- 权限决策审计索引（REQ-166/167，design 4.15.9 / 5.1.5）
-- 复用 hlp_event.audit，不新建表。

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'ck_audit_tool_decision'
  ) THEN
    ALTER TABLE hlp_event.audit
      ADD CONSTRAINT ck_audit_tool_decision CHECK (
        kind <> 'tool_decision' OR (
          decision IN ('allow', 'deny', 'require_approval')
          AND rule_id IS NOT NULL
          AND payload ? 'layer'
        )
      );
  END IF;
END
$$;

CREATE INDEX IF NOT EXISTS idx_audit_rule_ts
  ON hlp_event.audit (rule_id, created_at DESC)
  WHERE rule_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_audit_task_rule_ts
  ON hlp_event.audit (task_id, rule_id, created_at DESC)
  WHERE task_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_audit_decision_rule
  ON hlp_event.audit (decision, rule_id, created_at DESC)
  WHERE kind = 'tool_decision';

CREATE INDEX IF NOT EXISTS idx_audit_kind_ts
  ON hlp_event.audit (kind, created_at DESC);