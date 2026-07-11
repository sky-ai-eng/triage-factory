-- +goose Up
-- run_pending_input (TFAC-585): the durable half of resume-by-enqueue — the
-- message (+ acting user) recorded before a parked run's continuation is
-- re-queued as ordinary claimable work, so a crash between "message
-- recorded" and "process spawned" is recoverable by the standard boot
-- sweep instead of an ad-hoc path. See
-- docs/for-agents/specs/horizontal-scaling/README.md §5.2 and the Postgres twin in
-- internal/db/migrations-postgres/202605130001_pg_baseline.sql.
--
-- Both dialects (unlike the Postgres-only run_signals outbox): local
-- mode's dispatcher claims its own resumed runs through the identical
-- queue path — resume-by-enqueue applies in every mode, TF_ROLE=all
-- included (decision log #7). local mode is N=1, so this table's only job
-- here is closing the same crash window a restart could otherwise hit.
--
-- One row per run (PK run_id): a second message recorded before the first
-- is claimed REPLACES it — an idempotent upsert keyed by run_id. Consumed
-- destructively (DELETE … RETURNING) by the claiming dispatcher's resume
-- path, exactly like staged_agent_injections' flush — delivered exactly
-- once. run_id ON DELETE CASCADE retires a purged run's pending input with
-- it. org_id is a plain column with the local default for parity with the
-- Postgres baseline (SQLite is N=1, no RLS).
CREATE TABLE run_pending_input (
    run_id     TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    org_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    message    TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
SELECT 'down not supported';
