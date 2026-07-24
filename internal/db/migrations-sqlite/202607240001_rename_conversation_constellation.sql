-- +goose Up
-- Wire rename fast-follow to the data-model refactor: the surviving
-- run-named constellation tables adopt conversation vocabulary. Their FK
-- column was already renamed to conversation_id by the refactor; this is the
-- table rename that finishes the job. run_signals is Postgres-only (the
-- SQLite twin is a stub with no table), so it does not appear here. Indexes
-- follow the table across a SQLite RENAME, so their names keep the run_ prefix
-- — a harmless internal label, not a wire or query surface.
ALTER TABLE run_memory RENAME TO conversation_memory;
ALTER TABLE run_memory_entities RENAME TO conversation_memory_entities;
ALTER TABLE run_worktrees RENAME TO conversation_worktrees;

-- +goose Down
SELECT 'down not supported';
