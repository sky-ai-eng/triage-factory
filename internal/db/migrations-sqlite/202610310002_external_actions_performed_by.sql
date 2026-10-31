-- +goose Up
-- performed_by: who performed the act. 'agent' is a run's agent, and every
-- refusal of one of its attempts; 'person' is a person acting through TF (an
-- approval or edit of a run's deliverable, a board drag, a task close);
-- 'system' is TF on its own (the board mirrors, an event-driven teardown). A
-- person's approval of a run's deliverable carries that run's
-- conversation_id, and actor_user_id is set on a manual run's agent rows too,
-- so this column is what tells them apart.
--
-- SQLite adds a NOT NULL column only with a default. The stores refuse a row
-- that does not name its performer, so the default only reaches the rows
-- below.
ALTER TABLE external_actions ADD COLUMN performed_by TEXT NOT NULL DEFAULT 'agent'
    CHECK (performed_by IN ('agent', 'person', 'system'));

-- A row with no run is never an agent's. With no actor either it is TF's own:
-- the Jira and Linear board mirrors, a commit-signing refusal outside a run.
UPDATE external_actions
   SET performed_by = 'system'
 WHERE conversation_id IS NULL AND actor_user_id IS NULL;

-- With an actor it is a person's: the board drag.
UPDATE external_actions
   SET performed_by = 'person'
 WHERE conversation_id IS NULL AND actor_user_id IS NOT NULL;

-- Every row with a run stays 'agent'. A person's approval recorded against a
-- run carries the same columns as the run's own rows, so it cannot be told
-- apart here.

-- +goose Down
SELECT 'down not supported';
