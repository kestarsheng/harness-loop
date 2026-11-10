DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'chk_attempt_acceptance_after_finish'
  ) THEN
    ALTER TABLE hlp_task.stage_attempts
      ADD CONSTRAINT chk_attempt_acceptance_after_finish
      CHECK (finish_received OR acceptance_id IS NULL);
  END IF;
END
$$;