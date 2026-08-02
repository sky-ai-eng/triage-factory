-- +goose Up
--
-- Re-key the one-active-auto-run backstop from the entity to the task.
--
-- The task is the system's unit of "one situation that needs attention" —
-- that is exactly what its (entity_id, event_type, dedup_key) dedup index
-- means. Two tasks on one pull request are two different situations and may
-- each have an agent in flight; the entity was never the right unit for
-- "is something already working on this", it was a proxy for workspace
-- contention that per-run branches already handle.
--
-- It also stopped being safe. A conversation-level stop parks its
-- conversation and leaves the blueprint 'running' deliberately, so under the
-- entity key one stopped run held the only slot for the whole entity and
-- silently halted every future automated firing on it.
--
-- Still scoped to trigger_type='event': manual delegations have no such
-- constraint today (a second manual run on one task is deliberately allowed),
-- and widening this to cover them requires re-delegation to first replace its
-- predecessor.
--
-- blueprint_runs.entity_id stays and stays populated. It is no longer this
-- index's key, but it is the cheap entity lens for reads that want one.

DROP INDEX IF EXISTS blueprint_runs_one_active_auto_run_per_entity;

CREATE UNIQUE INDEX blueprint_runs_one_active_auto_run_per_task
    ON blueprint_runs (org_id, task_id)
    WHERE (trigger_type = 'event' AND status = 'running');

-- +goose Down
SELECT 'down not supported';
