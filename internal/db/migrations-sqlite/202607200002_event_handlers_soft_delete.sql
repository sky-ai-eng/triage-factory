-- +goose Up
-- Event handlers (task rules + prompt triggers) join prompts/blueprints as
-- shipped-content sync targets: user_modified is the sync's "never clobber a
-- user edit" signal, deleted_at lets a shipped row soft-delete so its
-- (org_id, team_id, system_slug) identity stays occupied and sync never
-- resurrects it.
ALTER TABLE event_handlers ADD COLUMN user_modified INTEGER NOT NULL DEFAULT 0;
ALTER TABLE event_handlers ADD COLUMN deleted_at DATETIME;

-- Re-cut the one-trigger-per-blueprint dedup index so a soft-deleted trigger
-- frees its blueprint slot instead of permanently blocking a replacement —
-- the mirror of "soft-deleted rows keep their system_slug slot occupied but
-- never block unrelated new work."
DROP INDEX event_handlers_one_trigger_per_blueprint;
CREATE UNIQUE INDEX event_handlers_one_trigger_per_blueprint ON event_handlers(blueprint_id) WHERE blueprint_id IS NOT NULL AND deleted_at IS NULL;

-- +goose Down
SELECT 'down not supported';
