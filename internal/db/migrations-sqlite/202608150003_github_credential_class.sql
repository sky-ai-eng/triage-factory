-- +goose Up
-- Which credential system an org's GitHub access belongs to, stated rather
-- than inferred.
--
-- The inference this replaces was "no org_github_apps row ⇒ PAT", made
-- independently at seven call sites. It is right today only because PAT is the
-- one rowless shape that exists; org_github_apps holds one row per org keyed on
-- a UNIQUE app_id, so an org riding a deployment-level shared App would also
-- have no row and every one of those sites would read it as PAT — handing the
-- org a credential it does not have, silently, in the direction no one watches.
--
-- The class names WHICH CREDENTIAL SYSTEM the org is in. org_github_apps.active
-- continues to name WHICH CREDENTIAL IS LIVE. The staged window of a PAT→App
-- switch is where the two visibly differ, and the backfill below preserves that:
-- a staged App (active = false) with a live PAT backfills to byo_app.
--
-- App-validated, not CHECK-constrained, matching the convention the other
-- open-set text columns in this schema follow (prompts.source,
-- org_settings.max_llm_model_tier). A CHECK would have to be migrated to admit
-- a new class, and widening a SQLite CHECK means a full table rebuild.
ALTER TABLE org_settings
    ADD COLUMN github_credential_class TEXT NOT NULL DEFAULT 'pat';

-- Backfill from the fact the code was inferring from, so the column starts out
-- agreeing with reality on an existing laptop install. An org with a
-- registration row is in the App system whether or not that App is live yet;
-- every other org is on the PAT the DEFAULT already gave it.
UPDATE org_settings
   SET github_credential_class = 'byo_app'
 WHERE org_id IN (SELECT org_id FROM org_github_apps);

-- +goose Down
SELECT 'down not supported';
