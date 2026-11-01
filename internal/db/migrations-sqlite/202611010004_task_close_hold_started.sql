-- +goose Up
-- When the task's close was first held: kept while later closing events take
-- the hold's place, cleared with the hold. The close-hold sweep stops the
-- event-started work still going once a hold is older than its limit. Text in
-- the layout every lease comparison on this dialect reads.
ALTER TABLE tasks ADD COLUMN close_deferred_at DATETIME;

-- A close held when this runs starts its time now.
UPDATE tasks SET close_deferred_at = strftime('%Y-%m-%d %H:%M:%f', 'now')
 WHERE close_deferred_event_id IS NOT NULL;

-- Both set or both NULL. Triggers rather than a CHECK because SQLite adds no
-- table constraint to an existing table without rebuilding it.
-- +goose StatementBegin
CREATE TRIGGER tasks_close_hold_paired_insert
BEFORE INSERT ON tasks
WHEN (NEW.close_deferred_event_id IS NULL) <> (NEW.close_deferred_at IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'tasks_close_hold_paired: a held close names both its event and when it was first held');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER tasks_close_hold_paired_update
BEFORE UPDATE OF close_deferred_event_id, close_deferred_at ON tasks
WHEN (NEW.close_deferred_event_id IS NULL) <> (NEW.close_deferred_at IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'tasks_close_hold_paired: a held close names both its event and when it was first held');
END;
-- +goose StatementEnd

-- +goose Down
SELECT 'down not supported';
