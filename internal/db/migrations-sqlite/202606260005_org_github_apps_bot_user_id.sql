-- +goose Up
-- App-bot commit-author email, numeric-id noreply form (TFAC-474): a GitHub App
-- bot's commits only link to its account on github.com (contribution graph,
-- Verified co-author badge) when the committer email is the numeric-id noreply
-- form "<bot_user_id>+<slug>[bot]@users.noreply.github.com". The bot's numeric
-- *user-account* id was never stored — org_github_apps holds the App id + slug,
-- and neither registration response carries the bot user id (it comes from
-- GET /users/<slug>[bot]). The credential resolver's OrgIdentityFor reads this
-- column (DB-only, no per-run API call) to build that email for App-bot commits.
-- App-path only; the PAT path's plain "<login>@..." form already links.
-- Nullable, no default — an App registered before this ticket reads back 0 and
-- falls back to the plain "<slug>[bot]@..." form; it self-heals on re-register.
--
-- Postgres has no companion migration: that schema is unreleased, so the column
-- was added directly to the baseline (202605130001_pg_baseline.sql,
-- org_github_apps table). SQLite is shipped, hence this forward-only migration.
ALTER TABLE org_github_apps ADD COLUMN bot_user_id INTEGER;

-- +goose Down
SELECT 'down not supported';
