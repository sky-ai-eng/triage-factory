-- +goose Up
-- Any number of live triggers may reference one blueprint. No row changes.
DROP INDEX event_handlers_one_trigger_per_blueprint;

-- +goose Down
SELECT 'down not supported';
