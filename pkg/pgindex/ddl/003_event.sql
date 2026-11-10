CREATE SCHEMA IF NOT EXISTS hlp_event;

CREATE TABLE IF NOT EXISTS hlp_event.events (
  event_id       text PRIMARY KEY,
  task_id        text NOT NULL,
  tenant_id      text NOT NULL,
  project_id     text NOT NULL,
  seq            bigint NOT NULL,
  event_type     text NOT NULL,
  schema_version text NOT NULL,
  stage          char(1),
  attempt        integer,
  severity       text NOT NULL,
  trace_id       text,
  span_id        text,
  ts             timestamptz NOT NULL,
  payload        jsonb NOT NULL,
  payload_ref    text,
  parent_task_id text,
  subtask_id     text,
  handoff_id     text,
  dag_id         text,
  dag_node_id    text,
  UNIQUE (task_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_events_task_seq  ON hlp_event.events (task_id, seq);
CREATE INDEX IF NOT EXISTS idx_events_tenant_ts ON hlp_event.events (tenant_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_events_type_ts   ON hlp_event.events (tenant_id, event_type, ts DESC);
CREATE INDEX IF NOT EXISTS idx_events_trace     ON hlp_event.events (trace_id);
CREATE INDEX IF NOT EXISTS idx_events_payload   ON hlp_event.events USING GIN (payload jsonb_path_ops);
CREATE INDEX IF NOT EXISTS idx_events_subtask   ON hlp_event.events (subtask_id) WHERE subtask_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS hlp_event.tool_idempotency (
  idempotency_key text PRIMARY KEY,
  task_id         text NOT NULL,
  tool_name       text NOT NULL,
  call_id         text NOT NULL,
  result_ref      text,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tool_idem_task ON hlp_event.tool_idempotency (task_id, created_at DESC);

CREATE TABLE IF NOT EXISTS hlp_event.audit (
  audit_id   text PRIMARY KEY,
  tenant_id  text NOT NULL,
  task_id    text,
  kind       text NOT NULL,
  actor      text,
  action     text NOT NULL,
  target     text,
  decision   text,
  rule_id    text,
  payload    jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_tenant_ts ON hlp_event.audit (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_task      ON hlp_event.audit (task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_rule      ON hlp_event.audit (rule_id) WHERE rule_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_audit_payload   ON hlp_event.audit USING GIN (payload jsonb_path_ops);