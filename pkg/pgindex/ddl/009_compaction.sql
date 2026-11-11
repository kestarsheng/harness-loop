CREATE TABLE IF NOT EXISTS hlp_task.compaction_runs (
  run_id                       text PRIMARY KEY,
  tenant_id                    text NOT NULL,
  project_id                   text NOT NULL,
  task_id                      text NOT NULL,
  variant                      text NOT NULL,
  strategy_id                  text NOT NULL,
  prompt_version               text NOT NULL,
  trigger_reason               text NOT NULL,
  tokens_before                integer NOT NULL DEFAULT 0,
  tokens_after                 integer NOT NULL DEFAULT 0,
  ratio                        double precision NOT NULL DEFAULT 0,
  preserved_rate               double precision NOT NULL DEFAULT 0,
  downstream_acceptance_rate   double precision NOT NULL DEFAULT 0,
  degraded                     boolean NOT NULL DEFAULT false,
  degrade_level                text CHECK (degrade_level IN ('L1','L2')),
  output_ref                   text,
  duration_ms                  bigint NOT NULL DEFAULT 0,
  created_at                   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_compaction_task   ON hlp_task.compaction_runs (task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_compaction_variant ON hlp_task.compaction_runs (variant, created_at DESC);