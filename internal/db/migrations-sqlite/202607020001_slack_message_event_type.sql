-- +goose Up
-- slack:message (renamed from slack:mention pre-release; mention-ness became event metadata rather than a distinct type)
--
-- This migration is a no-op. Event types are now seeded by db.SeedEventTypes,
-- which reconciles events_catalog from domain.AllEventTypes() on every
-- db.Migrate call.
--
-- Kept only so its version_id remains in the applied history for installs
-- that already ran it.
SELECT 1;

-- +goose Down
SELECT 'down not supported';
