-- +goose Up
-- The model the three headless system jobs — scorer, project classifier, repo
-- profiler — run on. One knob for all three: they are the same kind of work
-- (short, toolless, no transcript) bought from the same budget, so a per-job
-- column would be three ways to answer one question.
--
-- A modelcatalog key, validated app-side against the catalog and against the
-- providers the org has connected — not CHECK-constrained, the
-- max_llm_model_tier precedent: the accepted set is the build's catalog, which
-- changes with the binary rather than with the schema.
--
-- Empty means unset, and unset means those jobs do not run. TF ships no
-- fallback model, so a job with no model skips its cycle and says so rather
-- than spending on a model nobody chose.
--
-- The DEFAULT is the local pre-fill, and it does double duty: SQLite backfills
-- every existing row with it when the column is added, so an install that
-- predates the setting keeps running its background jobs exactly as before, and
-- a tenant seeded afterwards (INSERT INTO org_settings (org_id)) materializes
-- with it too. Local mode is single-user and zero-configuration; there is no
-- setup step to make someone choose here. Postgres/multi deliberately defaults
-- to empty instead — an org there picks its model during setup.
ALTER TABLE org_settings ADD COLUMN background_jobs_model TEXT NOT NULL DEFAULT 'claude-haiku-4-5-20251001';

-- +goose Down
SELECT 'down not supported';
