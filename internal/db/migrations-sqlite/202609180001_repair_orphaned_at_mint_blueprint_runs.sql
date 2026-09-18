-- +goose Up
-- One-time repair of a shape that can no longer be produced. A firing now
-- commits its blueprint_run and its first step conversation in one
-- transaction, so a 'running' parent with no child cannot appear; installed
-- databases may still hold one from the window where the two were separate
-- writes, and it keeps holding blueprint_runs_one_active_run_per_task against
-- its task forever.
--
-- No grace: migrations run before any worker starts, so no firing is in
-- flight to be mistaken for an orphan.
--
-- Failing rather than re-minting the missing step is what makes this
-- self-healing: the terminal write frees the index, and the task's own next
-- firing produces a fresh, fully-minted run.
UPDATE blueprint_runs
SET status       = 'failed',
    abort_reason = 'orphaned_at_mint',
    completed_at = CURRENT_TIMESTAMP
WHERE status = 'running'
  AND NOT EXISTS (
      SELECT 1 FROM conversations c WHERE c.blueprint_run_id = blueprint_runs.id
  );

-- +goose Down
SELECT 'down not supported';
