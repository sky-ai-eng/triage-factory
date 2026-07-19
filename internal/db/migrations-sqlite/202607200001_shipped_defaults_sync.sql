-- +goose Up
-- Boot-time sync of shipped defaults into unmodified team copies replaces the
-- old versioning sidecar: the sync decides "unmodified rows equal current
-- shipped content" by direct content comparison, so the stored shipped-content
-- hash has no reader left. Drop the table; its writers (PromptStore.SeedOrUpdate
-- and the BlueprintStore counterpart) are deleted alongside it.
DROP TABLE system_prompt_versions;

-- shipped_defaults_backfilled_at is the durable, per-team marker that the
-- one-time grandfather backfill has run for this team. Pre-existing structural
-- edits to shipped blueprints were never stamped user_modified before the
-- stamping ticket, so before a team's first sync the backfill stamps
-- user_modified on any system-slugged blueprint whose step structure already
-- diverges from shipped, then sets this timestamp. NULL = backfill still owed;
-- a timestamp = done, so the clobber-guard runs at most once per team.
ALTER TABLE teams ADD COLUMN shipped_defaults_backfilled_at DATETIME;

-- +goose Down
SELECT 'down not supported';
