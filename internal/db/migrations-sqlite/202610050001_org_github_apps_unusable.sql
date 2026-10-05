-- +goose Up
-- Whether GitHub still accepts the org's own GitHub App. An App can be deleted
-- on GitHub, or have its private key deleted or regenerated there, without TF
-- being told: no webhook announces either, and both look like any other failed
-- request from inside a poll cycle. The installation reconcile asks GitHub
-- directly (GET /app with the App's JWT) when its listing is refused, and
-- records the answer here so the Settings panel, the poller and credential
-- resolution all read the same fact.
--
-- unusable_reason is NULL while GitHub accepts the App, else 'missing' (no App
-- with this id exists) or 'key_rejected' (the stored key no longer signs for
-- it). unusable_since is when the current reason was first observed, NULL
-- whenever the reason is. App-validated, not CHECK-constrained, matching the
-- other ADD COLUMN text columns: widening a SQLite CHECK means a full table
-- rebuild.
--
-- Postgres has no companion migration: that schema is unreleased, so the
-- columns were added directly to the baseline (202605130001_pg_baseline.sql,
-- org_github_apps table).
ALTER TABLE org_github_apps ADD COLUMN unusable_reason TEXT;
ALTER TABLE org_github_apps ADD COLUMN unusable_since DATETIME;

-- +goose Down
SELECT 'down not supported';
