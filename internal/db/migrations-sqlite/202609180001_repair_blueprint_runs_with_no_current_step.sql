-- +goose Up
-- One-time repair of a shape that can no longer be produced. A firing now
-- commits its blueprint_run and its first step conversation in one
-- transaction, and a step advance commits the current_step_index bump and the
-- conversation that pointer names in another, so a 'running' run with no
-- conversation at its current step cannot appear. Installed databases may
-- still hold one from the window where those were separate writes, and nothing
-- can drive it — the claim gate only ever drives the step the pointer names —
-- while it keeps holding blueprint_runs_one_active_run_per_task against its
-- task forever.
--
-- No grace: migrations run before any worker starts, so no firing or advance
-- is in flight to be mistaken for an orphan.
--
-- Failing rather than re-minting the missing step is what makes this
-- self-healing: the terminal write frees the index, and the task's own next
-- firing produces a fresh, fully-minted run. The steps that did run keep their
-- conversations; those are history, not the wedge.
UPDATE blueprint_runs
SET status       = 'failed',
    abort_reason = 'orphaned_step',
    completed_at = CURRENT_TIMESTAMP
WHERE status = 'running'
  AND NOT EXISTS (
      SELECT 1 FROM conversations c
      WHERE c.blueprint_run_id = blueprint_runs.id
        AND c.blueprint_step_index = blueprint_runs.current_step_index
  );

-- +goose Down
SELECT 'down not supported';
