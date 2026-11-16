-- T-P2-12: terminal identity for multi-terminal agents.
-- Columns stay nullable so tasks created before this migration remain readable.

ALTER TABLE hlp_task.task_runs ADD COLUMN IF NOT EXISTS session_id text;
ALTER TABLE hlp_task.task_runs ADD COLUMN IF NOT EXISTS terminal_label text;

CREATE INDEX IF NOT EXISTS idx_task_session
  ON hlp_task.task_runs (tenant_id, session_id)
  WHERE session_id IS NOT NULL;