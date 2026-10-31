-- +goose Up
-- A close owed to the task and held until the blueprint run whose own action
-- caused the closing event ends: the closing event, and that run. Written by
-- the router's close phase, cleared by its deferral sweep, and read only while
-- the task is active.
ALTER TABLE tasks ADD COLUMN close_deferred_event_id TEXT REFERENCES events(id);
ALTER TABLE tasks ADD COLUMN close_deferred_blueprint_run_id TEXT REFERENCES blueprint_runs(id);

-- The deferral sweep's read: the few tasks holding a deferred close.
CREATE INDEX idx_tasks_close_deferred ON tasks (id) WHERE close_deferred_event_id IS NOT NULL;

-- Both set or both NULL. Triggers rather than a CHECK because SQLite adds no
-- table constraint to an existing table without rebuilding it.
-- +goose StatementBegin
CREATE TRIGGER tasks_close_deferral_paired_insert
BEFORE INSERT ON tasks
WHEN (NEW.close_deferred_event_id IS NULL) <> (NEW.close_deferred_blueprint_run_id IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'tasks_close_deferral_paired: a deferred close names both its event and its blueprint run');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER tasks_close_deferral_paired_update
BEFORE UPDATE OF close_deferred_event_id, close_deferred_blueprint_run_id ON tasks
WHEN (NEW.close_deferred_event_id IS NULL) <> (NEW.close_deferred_blueprint_run_id IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'tasks_close_deferral_paired: a deferred close names both its event and its blueprint run');
END;
-- +goose StatementEnd

-- +goose Down
SELECT 'down not supported';
