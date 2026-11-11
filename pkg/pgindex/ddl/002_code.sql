CREATE SCHEMA IF NOT EXISTS hlp_code;

CREATE TABLE IF NOT EXISTS hlp_code.indexed_files (
  tenant_id     text NOT NULL,
  project_id    text NOT NULL,
  file_path     text NOT NULL,
  language      text NOT NULL,
  content_hash  text NOT NULL,
  mtime         timestamptz NOT NULL,
  indexed_at    timestamptz,
  status        text NOT NULL CHECK (status IN ('pending','indexing','indexed','failed')),
  failure_count integer NOT NULL DEFAULT 0,
  last_error    text,
  parser_id     text,
  degraded      boolean NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, project_id, file_path)
);

CREATE TABLE IF NOT EXISTS hlp_code.code_chunks (
  chunk_id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        text NOT NULL,
  project_id       text NOT NULL,
  file_path        text NOT NULL,
  language         text NOT NULL,
  chunk_type       text NOT NULL CHECK (chunk_type IN ('function','method','header','function_part','file','class')),
  symbol           text,
  parent_symbol    text,
  start_line       integer NOT NULL,
  end_line         integer NOT NULL,
  content          text NOT NULL,
  content_hash     text NOT NULL,
  parser_id        text NOT NULL,
  embedding        vector(1024),
  content_tokens   text,
  content_tsvector tsvector,
  indexed_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_code_chunks_file ON hlp_code.code_chunks (tenant_id, project_id, file_path);
CREATE INDEX IF NOT EXISTS idx_code_chunks_tsv  ON hlp_code.code_chunks USING GIN (content_tsvector);
CREATE INDEX IF NOT EXISTS idx_code_chunks_vec  ON hlp_code.code_chunks
  USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

CREATE TABLE IF NOT EXISTS hlp_code.code_edges (
  edge_id       bigserial PRIMARY KEY,
  tenant_id     text NOT NULL,
  project_id    text NOT NULL,
  edge_type     text NOT NULL CHECK (edge_type IN ('call','import')),
  source_symbol text NOT NULL,
  target_symbol text NOT NULL,
  source_file   text NOT NULL,
  target_file   text,
  start_line    integer,
  UNIQUE (tenant_id, project_id, edge_type, source_symbol, target_symbol, source_file)
);

CREATE INDEX IF NOT EXISTS idx_code_edges_source_file ON hlp_code.code_edges (tenant_id, project_id, source_file);
CREATE INDEX IF NOT EXISTS idx_code_edges_target_file ON hlp_code.code_edges (tenant_id, project_id, target_file);