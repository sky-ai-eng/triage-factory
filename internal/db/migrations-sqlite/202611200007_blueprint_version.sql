-- +goose Up
-- A blueprint's optimistic-concurrency token. A write that changes its name or
-- its steps raises it in the same UPDATE; a write to any other column
-- (usage_count, hidden, user_modified alone, a plain soft delete) leaves it. A
-- steps PUT names the version it read and lands only on a blueprint still at
-- it.
--
-- Rows saved before this migration start at 1, as a new row does: no caller
-- holds a version from before it.

ALTER TABLE blueprints ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

-- +goose Down
SELECT 'down not supported';
