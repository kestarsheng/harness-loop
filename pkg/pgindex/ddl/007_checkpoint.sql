CREATE TABLE IF NOT EXISTS hlp_task.checkpoints (
  checkpoint_id        text PRIMARY KEY,
  task_id              text NOT NULL,
  tenant_id            text NOT NULL,
  kind                 text NOT NULL,
  stage                char(1) NOT NULL,
  attempt_no           integer NOT NULL,
  event_seq_watermark  bigint NOT NULL DEFAULT 0,
  payload              jsonb NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_task_created ON hlp_task.checkpoints (task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkpoints_task_stage   ON hlp_task.checkpoints (task_id, stage, created_at DESC);