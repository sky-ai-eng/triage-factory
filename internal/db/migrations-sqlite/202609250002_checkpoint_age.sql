-- +goose Up
-- When the engagement's workspace was last covered by a stored checkpoint, on
-- database time: the last checkpoint written or found unchanged, or the agent
-- loop's start before the first. NULL for an engagement that does not
-- checkpoint. Stamped by the renewal only. No backfill: a live claim across
-- the upgrade shows none until its next renewal.
ALTER TABLE claims ADD COLUMN last_checkpoint_at DATETIME;
-- +goose Down
SELECT 'down not supported';
