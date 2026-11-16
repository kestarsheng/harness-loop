-- 014_event_global_seq.sql — T-P2-17 事件游标语义修复
--
-- 缺陷：hlp_event.events.seq 是「任务内自增」（NextSeq 用 MAX(seq) WHERE task_id），
-- 但 WatchEvents / MonitorStream / 前端 last_seq 把它当跨任务游标用。新任务的
-- 第一个事件 seq 又是 1，于是 `seq > since` 会把新任务的事件整段永久滤掉——
-- 50 并发时只收到 11% 的事件。
--
-- 修复：新增租户级 global_seq 作为唯一游标，seq 保持任务内序号不变（唯一约束
-- (task_id, seq) 不动，Checkpoint 的 event_seq_watermark 仍按任务内水位比较）。
--
-- 幂等：ADD COLUMN IF NOT EXISTS；回填只处理 global_seq IS NULL 的行；
--       约束用 pg_constraint 存在性判断后再生效；重复执行不产生新变更。
-- 可空：本迁移不设 NOT NULL。滚动升级期间旧版本服务仍在写事件，NOT NULL 会让
--       这些写入直接失败（丢事件比暂缺游标更难排查）；写入侧（event_repo.Append
--       与 task_repo 的状态转移事件）已保证新行必带 global_seq，回填兜住存量，
--       全量升级完成后再单独一步 SET NOT NULL 由运维执行。
--
-- 回填口径：同租户内按 (ts, event_id) 排序，ROW_NUMBER 从 1 起连续编号；
-- 若租户已有部分 global_seq（增量升级路径），从该租户 max(global_seq) 继续，
-- 因此不会与既有值撞号。空库全量重放时租户无存量行，编号从 1 开始。

ALTER TABLE hlp_event.events ADD COLUMN IF NOT EXISTS global_seq bigint;

WITH ranked AS (
  SELECT e.event_id,
         ROW_NUMBER() OVER (PARTITION BY e.tenant_id ORDER BY e.ts, e.event_id) AS rn,
         COALESCE(b.max_seq, 0) AS base_seq
  FROM hlp_event.events e
  JOIN (
    SELECT tenant_id, MAX(global_seq) AS max_seq
    FROM hlp_event.events
    GROUP BY tenant_id
  ) b ON b.tenant_id = e.tenant_id
  WHERE e.global_seq IS NULL
)
UPDATE hlp_event.events t
SET global_seq = ranked.base_seq + ranked.rn
FROM ranked
WHERE t.event_id = ranked.event_id;

-- 唯一约束同时提供 (tenant_id, global_seq) 的 btree 索引：游标查询
-- `WHERE tenant_id = $1 AND global_seq > $2 ORDER BY global_seq` 与写入侧
-- `MAX(global_seq) WHERE tenant_id = $1` 都走它。不再重复建同列普通索引，
-- 否则每次写入要维护两份同样的索引。
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'uq_events_tenant_global_seq'
      AND conrelid = 'hlp_event.events'::regclass
  ) THEN
    ALTER TABLE hlp_event.events
      ADD CONSTRAINT uq_events_tenant_global_seq UNIQUE (tenant_id, global_seq);
  END IF;
END $$;
