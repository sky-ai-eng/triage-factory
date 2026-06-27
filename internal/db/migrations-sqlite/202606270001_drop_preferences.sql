-- +goose Up
-- Drop preferences: the per-user dashboard-brief table (summary_md was "the
-- user's personalized dashboard brief") is retired alongside the Brief page and
-- the /api/brief + /api/preferences stub endpoints, none of which were ever
-- implemented — no code ever read or wrote this table. Forward-only per the
-- SQLite (shipped) migration rule — never edit the baseline; the Postgres
-- baseline drops it inline since multi-mode has not shipped.
DROP TABLE IF EXISTS preferences;

-- +goose Down
SELECT 'down not supported';
