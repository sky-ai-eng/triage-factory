-- +goose Up
-- Which inference providers a team may spend against. An org admin's
-- restriction, not a team's own preference: the team-settings write path
-- round-trips this column untouched, the same discipline max_daily_cost_usd
-- already holds, and only the org-admin surgical write changes it.
--
-- Empty means unrestricted — every provider the org configured. Absent and
-- "all of them" therefore store the same value, which is correct here: an
-- admin who has narrowed nothing has narrowed nothing, and a team whose
-- restriction happened to name every provider would silently become restricted
-- the day the org connects a third.
--
-- JSON text, matching jira_projects on this table: SQLite has no array type,
-- and the store already marshals a []string through that shape.
ALTER TABLE team_settings ADD COLUMN allowed_providers TEXT NOT NULL DEFAULT '[]';

-- +goose Down
SELECT 'down not supported';
