CREATE TABLE IF NOT EXISTS hlp_task.subtasks (
  subtask_id          text PRIMARY KEY,
  parent_task_id      text NOT NULL REFERENCES hlp_task.task_runs(task_id) ON DELETE CASCADE,
  tenant_id           text NOT NULL,
  project_id          text NOT NULL,
  dag_id              text NOT NULL,
  dag_node_id         text NOT NULL,
  depth               integer NOT NULL CHECK (depth BETWEEN 1 AND 2),
  index_in_parent     integer NOT NULL DEFAULT 0,
  parallel_set_id     text,
  role                text NOT NULL,
  status              text NOT NULL CHECK (status IN ('pending','running','waiting_handoff','accepted','rejected','failed','cancelled')),
  depends_on          jsonb NOT NULL DEFAULT '[]',
  input_contract      jsonb NOT NULL DEFAULT '{}',
  output_contract     jsonb NOT NULL DEFAULT '{}',
  acceptance_criteria jsonb NOT NULL DEFAULT '[]',
  acceptance_id       text,
  failure_strategy    text NOT NULL DEFAULT 'fail-fast' CHECK (failure_strategy IN ('fail-fast','tolerate','retry')),
  attempt_no          integer NOT NULL DEFAULT 0,
  artifact_refs       jsonb NOT NULL DEFAULT '[]',
  handoff_id          text,
  alloc_tokens        bigint NOT NULL DEFAULT 0,
  consumed_tokens     bigint NOT NULL DEFAULT 0,
  consumed_cost       numeric(12,4) NOT NULL DEFAULT 0,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (parent_task_id, dag_id, dag_node_id),
  CHECK (status <> 'accepted' OR acceptance_id IS NOT NULL),
  CHECK (status <> 'waiting_handoff' OR handoff_id IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_subtasks_parent ON hlp_task.subtasks (parent_task_id, dag_id, index_in_parent);
CREATE INDEX IF NOT EXISTS idx_subtasks_status ON hlp_task.subtasks (parent_task_id, status);
