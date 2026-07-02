-- +goose Up
-- slack:mention (TFAC-530) originally shipped its events_catalog row as a
-- static INSERT here, back when a new event type needed a forward
-- migration (the baseline seed was frozen for already-migrated installs).
-- TFAC-532 replaced that with db.SeedEventTypes, which reconciles
-- events_catalog from domain.AllEventTypes() on every db.Migrate call —
-- so this migration is now a no-op, kept only so its version_id stays in
-- the applied history for installs that already ran it.
SELECT 1;

-- +goose Down
SELECT 'down not supported';
