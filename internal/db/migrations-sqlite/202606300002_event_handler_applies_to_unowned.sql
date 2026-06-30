-- +goose Up
-- event_handlers.applies_to_unowned: the explicit, per-rule routing-scope flag
-- (TFAC-517) that replaces the source-as-scope heuristic. When true, the rule's
-- team is added to a task's visibility even for entities the team doesn't own
-- (external/ambiguous authors) — the deliberate "watch" opt-in. When false (the
-- default), visibility rides ownership only: the team sees a task off this rule
-- only when the owning-team ladder resolves the team as owner.
--
-- DEFAULT false IS the migration: the as-designed behavior was always
-- "visibility rides ownership," so every existing row is correct on creation
-- (no backfill). Provenance (`source`) no longer doubles as a scope proxy — a
-- shipped rule can now opt in and a user rule can opt out, both impossible
-- before. Routing gates explicitWatchTeams on this column, not on source.
ALTER TABLE event_handlers ADD COLUMN applies_to_unowned BOOLEAN NOT NULL DEFAULT 0;

-- +goose Down
SELECT 'down not supported';
