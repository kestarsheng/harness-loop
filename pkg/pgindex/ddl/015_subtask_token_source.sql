-- T-P2-27: record whether consumed_tokens came from the provider usage block
-- ("actual") or the local character estimate ("estimate").
ALTER TABLE hlp_task.subtasks
    ADD COLUMN IF NOT EXISTS token_source text NOT NULL DEFAULT 'estimate';
