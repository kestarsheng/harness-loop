-- T-P2-13: symbol lookup index for graph expansion.
-- 列名以 002_code.sql 的实际定义为锚：tenant_id / project_id / symbol。
-- 承载 pkg/codeindex 的 chunksBySymbols 查询：
--   WHERE tenant_id = $1 AND project_id = $2 AND symbol = ANY($3::text[])
-- 无此索引时上述查询退化为按 tenant_id+project_id 全量回表（单种子 50ms+）。

CREATE INDEX IF NOT EXISTS idx_code_chunks_symbol
  ON hlp_code.code_chunks (tenant_id, project_id, symbol);
