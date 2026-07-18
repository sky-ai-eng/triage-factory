-- +goose Up
-- Marketplace V1 (TFAC-535) is multi-mode only — every marketplace_* table
-- lives in the Postgres baseline only. org_settings is itself a shared
-- both-dialect table, so this migration keeps column parity by adding the
-- toggle here too — unlike events_catalog, which needs no per-dialect
-- migration at all (db.SeedEventTypes reconciles it from
-- domain.AllEventTypes() on every db.Migrate call, both dialects,
-- independent of any migration file). It is permanently 0 in local mode —
-- no local UI ever flips it, and every marketplace endpoint 404s regardless
-- of its value (the SQLite MarketplaceStore is a stub; see
-- internal/db/sqlite/marketplace_store.go).
ALTER TABLE org_settings ADD COLUMN marketplace_enabled BOOLEAN NOT NULL DEFAULT 0;

-- +goose Down
SELECT 'down not supported';
