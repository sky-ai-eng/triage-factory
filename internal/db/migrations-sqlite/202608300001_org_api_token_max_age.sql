-- +goose Up
-- Ceiling on how long any of the org's API tokens may live, in days. NULL means
-- uncapped, which is the default and the only value a local install will ever
-- hold: API tokens are a multi-mode credential (the table behind them is
-- PG-only, like sessions), and local mode's synthetic identity is already
-- headless, so nothing here reads this column.
--
-- It lands on SQLite anyway because org_settings is dual-dialect: a PG-only
-- column would fork the store code that projects this row, and one column list
-- per dialect is how a read shape drifts from its twin.
--
-- Nullable with no default, and NULL is the whole vocabulary of "no cap" — a 0
-- sentinel would collide with the 1..365 range the setting accepts, and a
-- default would be TF picking an expiry policy for an org that never asked for
-- one. The 1..365 range rides a CHECK on the Postgres twin and nothing here:
-- SQLite cannot add a CHECK by ALTER TABLE without rebuilding the table, and
-- rebuilding org_settings to constrain a column local mode never reads would
-- buy nothing.
ALTER TABLE org_settings ADD COLUMN api_token_max_age_days INTEGER;

-- +goose Down
SELECT 'down not supported';
