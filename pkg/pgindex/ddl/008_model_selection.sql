CREATE TABLE IF NOT EXISTS hlp_task.model_selections (
  selection_id      text PRIMARY KEY,
  tenant_id         text NOT NULL,
  project_id        text NOT NULL,
  task_id           text NOT NULL REFERENCES hlp_task.task_runs(task_id) ON DELETE CASCADE,
  stage             char(1) NOT NULL CHECK (stage IN ('a','b','c','d','e')),
  attempt_no        integer NOT NULL,
  model_id          text NOT NULL,
  strategy          text NOT NULL CHECK (strategy IN ('cost_first','quality_first','balanced','explicit')),
  resolved_from     text NOT NULL CHECK (resolved_from IN ('override','explicit','strategy','default','fallback')),
  reason            text,
  candidates        jsonb,
  context_window    integer NOT NULL,
  threshold_tokens  bigint NOT NULL,
  input_price       numeric(12,6) NOT NULL,
  output_price      numeric(12,6) NOT NULL,
  tokens_in         bigint NOT NULL DEFAULT 0,
  tokens_out        bigint NOT NULL DEFAULT 0,
  cost              numeric(12,4) NOT NULL DEFAULT 0,
  is_fallback       boolean NOT NULL DEFAULT false,
  approval_id       text,
  operator          text,
  created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_model_selection_task_stage
  ON hlp_task.model_selections (task_id, stage, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_model_selection_cost
  ON hlp_task.model_selections (tenant_id, project_id, model_id, created_at DESC);

ALTER TABLE hlp_task.task_runs ADD COLUMN IF NOT EXISTS model_id text;
ALTER TABLE hlp_task.task_runs ADD COLUMN IF NOT EXISTS model_strategy text;
ALTER TABLE hlp_task.task_runs ADD COLUMN IF NOT EXISTS model_resolved_reason text;