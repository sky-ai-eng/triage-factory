-- +goose Up
-- slack:mention (TFAC-530): the events_catalog seed baked into the SQLite
-- baseline is frozen for already-migrated installs (goose only replays
-- migrations forward from where a DB left off), so a new event type ships
-- as a forward migration here rather than an in-place baseline edit — see
-- the baseline's events_catalog seed comment. Mirrors the equivalent row
-- added directly to the Postgres baseline (multi-mode is still
-- pre-launch, so that tree still consolidates into its single baseline
-- file). Schema + ownership registration live in ee/slack; this is just
-- the universal catalog row every install needs regardless of licensing
-- (TFAC-524 gates at access, not at seed).
INSERT OR IGNORE INTO events_catalog (id, source, category, label, description) VALUES
  ('slack:mention', 'slack', 'message', 'Bot Mentioned', 'The TF bot was @mentioned in a Slack channel');

-- +goose Down
SELECT 'down not supported';
