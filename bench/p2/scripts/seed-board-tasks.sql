-- 大屏帧率压测的卡片数据源：50 个任务（DFX-P-08）。
--
-- 只写 hlp_task.task_runs：看板的卡片按 tenant 拉列表、按 status/stage 渲染，
-- 不依赖 stage_attempts（缺 attempt 时卡片按 attempt_no=0 渲染）。
-- 幂等：重复执行只刷新状态与时间戳，不会产生重复任务。
--
-- 用法：
--   docker exec -i hlp-pg-test psql -U agent -d agent_platform < bench/p2/scripts/seed-board-tasks.sql
--   （run-board-perf.sh 会自动带 -v n=<卡片数>）

\set ON_ERROR_STOP on

INSERT INTO hlp_task.task_runs (
    task_id, tenant_id, project_id, workspace_root, goal, goal_hash,
    current_stage, status, terminal_reason,
    attempt_no, retry_count, acceptance_fail, policy_snapshot,
    version, created_at, updated_at
)
SELECT
    'board-task-' || lpad(i::text, 3, '0'),
    'default',
    'proj-board',
    '/tmp/hlp-board',
    '大屏压测任务 #' || i || '：验证 50 卡片下的单帧耗时与交互延迟',
    md5('board-task-' || i),
    -- 阶段轮转，让卡片上的阶段进度条与泳道都有内容可画。
    (ARRAY['a','b','c','d','e'])[1 + (i % 5)],
    CASE
        WHEN i % 17 = 0 THEN 'failed'
        WHEN i % 23 = 0 THEN 'completed'
        WHEN i % 11 = 0 THEN 'paused'
        ELSE 'running'
    END,
    CASE
        WHEN i % 17 = 0 THEN 'failed'
        WHEN i % 23 = 0 THEN 'completed'
        ELSE NULL
    END,
    '{}'::jsonb,
    '{}'::jsonb,
    '{}'::jsonb,
    '{"policy":"board-perf"}'::jsonb,
    1,
    now() - ((60 - i) || ' minutes')::interval,
    now() - (i || ' seconds')::interval
FROM generate_series(1, 50) AS i
ON CONFLICT (task_id) DO UPDATE
SET status = EXCLUDED.status,
    terminal_reason = EXCLUDED.terminal_reason,
    current_stage = EXCLUDED.current_stage,
    updated_at = EXCLUDED.updated_at;

SELECT count(*) AS board_tasks
FROM hlp_task.task_runs
WHERE tenant_id = 'default' AND project_id = 'proj-board';
