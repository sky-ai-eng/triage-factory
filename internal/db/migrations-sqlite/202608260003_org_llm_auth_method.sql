-- +goose Up
-- Where an org's Claude credentials come from, stated rather than inferred.
--
-- The inference this replaces was "no anthropic_api_key_ref and no
-- bedrock_credentials_ref ⇒ run on whatever the host supplies". That is right
-- for a zero-configuration local install and wrong for every other reading of
-- the same emptiness: an admin who removed a key, one who has not bound one
-- yet, one who picked "bring your own" and stopped halfway. All four are the
-- same absence, so nothing downstream could tell "deliberately on the host's
-- credentials" from "not set up", and the setup picker's own selection was
-- stored as two DELETEs and re-guessed on the next load.
--
-- 'system' is the host's credentials: TF supplies the subprocess nothing and
-- the Claude Code SDK resolves auth from the inherited environment — a
-- subscription login, an exported ANTHROPIC_API_KEY, Bedrock or Vertex vars.
-- 'byok' is the org's own bound material, whichever provider serves the model.
-- The refs stay the record of WHICH providers are bound; this column answers
-- what it means when none are.
--
-- App-validated, not CHECK-constrained, matching the other ADD COLUMN text
-- columns on this table (github_credential_class, background_jobs_model): a
-- CHECK would have to be migrated to admit a new value, and widening a SQLite
-- CHECK means a full table rebuild.
--
-- The DEFAULT is the local answer, and it is what makes the choice a default
-- rather than a demand: local mode is single-user and zero-configuration, so a
-- fresh install arrives already on the host's credentials and never has to
-- pick. Postgres/multi defaults to 'byok' instead — a multi-mode deployment
-- has no host credentials to lend (the operator's environment would be shared
-- by every tenant, and credential resolution refuses outright there), so 'byok'
-- is not a default so much as the only value multi has.
ALTER TABLE org_settings
    ADD COLUMN llm_auth_method TEXT NOT NULL DEFAULT 'system';

-- Backfill from the fact the code was inferring from, so the column starts out
-- agreeing with reality on an existing install. An org holding material for
-- either provider is on its own credentials; every other org is on the host's,
-- which the DEFAULT already gave it.
UPDATE org_settings
   SET llm_auth_method = 'byok'
 WHERE COALESCE(anthropic_api_key_ref, '') <> ''
    OR COALESCE(bedrock_credentials_ref, '') <> '';

-- +goose Down
SELECT 'down not supported';
