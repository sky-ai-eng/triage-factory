-- +goose Up
-- The task's optimistic-concurrency token. A write that changes a column a
-- person acts on (status, either claim, snooze_until, closed_at, close_reason,
-- close_event_type, team_id) raises it in the same UPDATE, through
-- db.TaskSetSQL; a write to any other column leaves it. A person's write names
-- the version it read and lands only on a task still at it.
--
-- The bump is in each writer's SET rather than in a trigger: SQLite's
-- RETURNING reports the row as the statement itself wrote it, without what a
-- trigger changed afterwards, so a writer's returned row would carry the old
-- version.
ALTER TABLE tasks ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

-- +goose Down
SELECT 'down not supported';
