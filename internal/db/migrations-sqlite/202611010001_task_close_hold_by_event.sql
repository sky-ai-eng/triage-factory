-- +goose Up
-- A held close names only its closing event: the close waits for the task's
-- working conversations, not for one blueprint run. A task holding one keeps
-- its event, and the router's sweep settles it.
DROP TRIGGER tasks_close_deferral_paired_insert;
DROP TRIGGER tasks_close_deferral_paired_update;
ALTER TABLE tasks DROP COLUMN close_deferred_blueprint_run_id;

-- +goose Down
SELECT 'down not supported';
