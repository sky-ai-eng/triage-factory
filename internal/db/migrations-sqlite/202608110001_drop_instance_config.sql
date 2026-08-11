-- +goose Up
-- instance_config was the host-level singleton (id=1) holding server_port,
-- written by the internal/config package's Load/Save pair. That package was
-- deleted when its call sites moved to the scope-specific settings stores,
-- which took the only writer with it — the row has held the schema default
-- ever since, and the single remaining reader fed a Server field nothing read.
-- Both are gone now, so the table follows.
--
-- SQLite-only: Postgres never had this table (multi mode takes the port from
-- container env), so there is no counterpart migration in migrations-postgres.
DROP TABLE IF EXISTS instance_config;

-- +goose Down
SELECT 'down not supported';
