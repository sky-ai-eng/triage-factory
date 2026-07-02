-- +goose Up
-- Marketplace V1 (TFAC-535) is multi-mode only — every marketplace_* table
-- lives in the Postgres baseline only (house pattern: cf. the Slack tables,
-- which are PG-only with a SQLite migration only for the shared event type
-- in 202607020001_slack_mention_event_type.sql). org_settings is a shared
-- both-dialect table, so this migration keeps column parity by adding the
-- toggle here too. It is permanently 0 in local mode — no local UI ever
-- flips it, and every marketplace endpoint 404s regardless of its value
-- (the SQLite MarketplaceStore is a stub; see internal/db/sqlite/marketplace_store.go).
ALTER TABLE org_settings ADD COLUMN marketplace_enabled BOOLEAN NOT NULL DEFAULT 0;

-- +goose Down
SELECT 'down not supported';
