-- +goose Up
-- Which inference providers a team may spend against. An org admin's
-- restriction, not a team's own preference: the team-settings write path
-- round-trips this column untouched, the same discipline max_daily_cost_usd
-- already holds, and only the org-admin surgical write (on the admin pool,
-- since an org admin need not be a member of the team they are restricting)
-- changes it.
--
-- Empty means unrestricted — every provider the org configured. Absent and
-- "all of them" therefore store the same value, which is correct here: an
-- admin who has narrowed nothing has narrowed nothing, and a team whose
-- restriction happened to name every provider would silently become restricted
-- the day the org connects a third.
--
-- text[] with a literal DEFAULT, matching jira_projects on this table: the
-- surgical writes materialize a row without naming this column, so the default
-- is what an unrestricted team actually stores.
ALTER TABLE public.team_settings
    ADD COLUMN allowed_providers text[] DEFAULT '{}'::text[] NOT NULL;

-- +goose Down
SELECT 'down not supported';
