-- +goose Up
-- curator_messages gets the same two fidelity columns run_messages gained in
-- 202607220001 (Postgres twin: internal/db/migrations-postgres/202605130001_pg_baseline.sql).
-- Curator turns run through the same SDK stream parser as delegated runs, so
-- they need reasoning/content_blocks for the same display-parity reason.
-- delivered/window_state/seq stay run_messages-only — those are native-loop
-- assembly mechanics the interactive curator (staying on agentproc) never
-- uses. NULL on every historical row and every row today's SDK runtime
-- writes without either field — no backfill.
ALTER TABLE curator_messages ADD COLUMN reasoning TEXT;
ALTER TABLE curator_messages ADD COLUMN content_blocks TEXT;

-- +goose Down
SELECT 'down not supported';
