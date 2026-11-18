-- T-P2-29: the parent may finish as "completed" or, when tolerate sets left
-- failed children behind but everything closed, "completed_with_failed_subtasks".
-- The status column stays 'completed'; only the reason is more truthful.
ALTER TABLE hlp_task.task_runs
    DROP CONSTRAINT IF EXISTS task_runs_terminal_reason_check;

ALTER TABLE hlp_task.task_runs
    ADD CONSTRAINT task_runs_terminal_reason_check
    CHECK (terminal_reason IN ('completed', 'completed_with_failed_subtasks', 'aborted', 'failed'));
