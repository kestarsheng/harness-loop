CREATE SCHEMA IF NOT EXISTS hlp_mem;

CREATE TABLE IF NOT EXISTS hlp_mem.memory_topics (
  topic_id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL,
  project_id    text NOT NULL,
  scope         text NOT NULL CHECK (scope IN ('global','project')),
  topic_key     text NOT NULL,
  category      text NOT NULL CHECK (category IN ('user_preference','long_term_constraint','project_fact','task_state')),
  title         text NOT NULL,
  body          text NOT NULL,
  pinned        boolean NOT NULL DEFAULT false,
  order_rank    integer NOT NULL,
  content_hash  text NOT NULL,
  source_file   text NOT NULL,
  updated_at    timestamptz NOT NULL,
  indexed_at    timestamptz NOT NULL DEFAULT now(),
  decayed       boolean NOT NULL DEFAULT false,
  tsv           tsvector,
  UNIQUE (tenant_id, project_id, scope, topic_key)
);

CREATE INDEX IF NOT EXISTS idx_mem_topics_tenant ON hlp_mem.memory_topics (tenant_id, project_id, scope, order_rank);
CREATE INDEX IF NOT EXISTS idx_mem_topics_cat    ON hlp_mem.memory_topics (tenant_id, project_id, category);
CREATE INDEX IF NOT EXISTS idx_mem_topics_tsv    ON hlp_mem.memory_topics USING GIN (tsv);
CREATE INDEX IF NOT EXISTS idx_mem_topics_hash   ON hlp_mem.memory_topics (tenant_id, project_id, content_hash);

CREATE TABLE IF NOT EXISTS hlp_mem.memory_chunks (
  chunk_id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  topic_id      uuid NOT NULL REFERENCES hlp_mem.memory_topics(topic_id) ON DELETE CASCADE,
  chunk_index   integer NOT NULL,
  content       text NOT NULL,
  content_hash  text NOT NULL,
  embedding     vector(1024),
  UNIQUE (topic_id, chunk_index)
);

CREATE INDEX IF NOT EXISTS idx_mem_chunks_hnsw ON hlp_mem.memory_chunks
  USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);