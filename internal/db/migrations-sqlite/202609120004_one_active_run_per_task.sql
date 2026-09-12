-- +goose Up
-- One active blueprint_run per task, whatever minted it. The rule is that a
-- task has at most one live conversation; the mint doors can only check then
-- act, so this index is what actually holds it. The Postgres baseline carried
-- the auto-only half of it (trigger_type='event') and widens in place; local
-- mode had no backstop at all, and is where a person delegating by hand while
-- the pollers fire makes the collision routine.
--
-- No org_id in the key: local mode is N=1.

-- Name what the missing backstop already allowed, before touching anything: on
-- a task holding several running blueprint_runs, every one but the most
-- recently started. Ties break on id so the choice is total and the two updates
-- below agree on it.
CREATE TEMP TABLE superseded_blueprint_runs AS
SELECT id FROM (
    SELECT id, ROW_NUMBER() OVER (
        PARTITION BY task_id ORDER BY started_at DESC, id DESC
    ) AS rn
    FROM blueprint_runs
    WHERE status = 'running'
)
WHERE rn > 1;

-- Stamp the boundary on the LIVE conversations those runs carried, first. A
-- cancelled blueprint's steps are not drivable, so a conversation left
-- un-ended under one would read as its task's live conversation forever —
-- holding the gate shut over something nothing can wake, which is the one
-- shape the rule cannot recover from on its own. 'delegated' is what happened:
-- a later delegation on the same task superseded this one.
--
-- Narrower than the boundary door the app uses, which stamps a task's
-- conversations whatever their status, and deliberately: a boundary is a debt
-- (an ended conversation with no conversation_memory row makes its task
-- memory-pending), so this pays it only where not paying it would cost the
-- task its gate. A conversation that already reached a terminal, or that some
-- earlier boundary already ended, is nobody's live conversation and needs
-- nothing here. Subagent rows are excluded for the same reason the app's door
-- excludes them: a subagent ends with its spawner and is not the task's.
UPDATE conversations
SET ended_at     = CURRENT_TIMESTAMP,
    ended_reason = 'delegated'
WHERE ended_at IS NULL
  AND parent_conversation_id IS NULL
  AND (status IS NULL OR status NOT IN ('completed', 'failed'))
  AND blueprint_run_id IN (SELECT id FROM superseded_blueprint_runs);

-- Then cancel the runs themselves, as a teardown would have when they were
-- superseded. system_cancelled is the honest reason: no person asked for this.
UPDATE blueprint_runs
SET status       = 'cancelled',
    abort_reason = 'system_cancelled',
    completed_at = CURRENT_TIMESTAMP
WHERE id IN (SELECT id FROM superseded_blueprint_runs);

DROP TABLE superseded_blueprint_runs;

CREATE UNIQUE INDEX blueprint_runs_one_active_run_per_task
    ON blueprint_runs (task_id) WHERE status = 'running';

-- +goose Down
SELECT 'down not supported';
