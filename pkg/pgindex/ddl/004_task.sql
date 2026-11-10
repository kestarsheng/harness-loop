CREATE SCHEMA IF NOT EXISTS hlp_task;

CREATE TABLE IF NOT EXISTS hlp_task.task_runs (
  task_id         text PRIMARY KEY,
  tenant_id       text NOT NULL,
  project_id      text NOT NULL,
  workspace_root  text NOT NULL,
  goal            text NOT NULL,
  goal_hash       text NOT NULL,
  current_stage   char(1) NOT NULL CHECK (current_stage IN ('a','b','c','d','e')),
  status          text NOT NULL CHECK (status IN ('running','paused','completed','aborted','failed')),
  terminal_reason text CHECK (terminal_reason IN ('completed','aborted','failed')),
  attempt_no      jsonb NOT NULL DEFAULT '{}',
  retry_count     jsonb NOT NULL DEFAULT '{}',
  acceptance_fail jsonb NOT NULL DEFAULT '{}',
  budget_tokens   bigint NOT NULL DEFAULT 0,
  budget_cost     numeric(12,4) NOT NULL DEFAULT 0,
  consumed_tokens bigint NOT NULL DEFAULT 0,
  consumed_cost   numeric(12,4) NOT NULL DEFAULT 0,
  fsm_enabled     boolean NOT NULL DEFAULT false,
  policy_snapshot jsonb NOT NULL,
  version         bigint NOT NULL DEFAULT 0,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_tenant_status ON hlp_task.task_runs (tenant_id, status, updated_at DESC);

CREATE TABLE IF NOT EXISTS hlp_task.stage_attempts (
  attempt_id             text PRIMARY KEY,
  task_id                text NOT NULL REFERENCES hlp_task.task_runs(task_id) ON DELETE CASCADE,
  stage                  char(1) NOT NULL CHECK (stage IN ('a','b','c','d')),
  attempt_no             integer NOT NULL,
  entry_reason           text NOT NULL CHECK (entry_reason IN ('initial','acceptance_failed','retry','resume')),
  started_at             timestamptz NOT NULL,
  ended_at               timestamptz,
  finish_received        boolean NOT NULL DEFAULT false,
  acceptance_id          text,
  retry_count            integer NOT NULL DEFAULT 0,
  acceptance_fail_count  integer NOT NULL DEFAULT 0,
  UNIQUE (task_id, stage, attempt_no)
);

CREATE TABLE IF NOT EXISTS hlp_task.acceptance_records (
  acceptance_id   text PRIMARY KEY,
  task_id         text NOT NULL,
  stage           char(1) NOT NULL,
  attempt_id      text NOT NULL,
  verdict         text NOT NULL CHECK (verdict IN ('pass','fail','blocked')),
  verified_by     text NOT NULL CHECK (verified_by IN ('program','model','mixed')),
  reason          text NOT NULL,
  evidence        jsonb NOT NULL,
  failed_criteria jsonb,
  unverified      jsonb,
  rollback_target char(1),
  prompt_version  text NOT NULL,
  model           text,
  input_digest    text NOT NULL,
  tokens_in       integer,
  tokens_out      integer,
  duration_ms     bigint,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_accept_task_stage ON hlp_task.acceptance_records (task_id, stage, created_at DESC);