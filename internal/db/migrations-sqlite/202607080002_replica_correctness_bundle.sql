-- +goose Up
-- Replica-correctness bundle (TFAC-579), SQLite-relevant half. Local mode is
-- N=1 (single process, single connection) so the cross-process races this
-- ticket closes on Postgres (claiming pop, DB-enforced one-auto-run-per-
-- entity, became_atomic locking, snapshot CAS, advisory-lock RMW guards)
-- don't apply here — those stay Postgres-only. These two columns exist so
-- the shared store interfaces (EntityStore, SystemLLMRunStore) stay
-- conformant across both backends and the dbtest suite exercises identical
-- behavior:
--
--   - entities.poll_seq: bumped by UpdateSnapshotCASSystem alongside the
--     Postgres impl. Real CAS semantics even at N=1 (harmless — there's
--     only ever one writer) rather than special-casing SQLite to ignore
--     the expected value.
--   - system_llm_runs.trace_id: the same idempotency key as the Postgres
--     baseline, guarding against an accidental duplicate Record() call
--     (retry, double-call) independent of process count.
ALTER TABLE entities ADD COLUMN poll_seq INTEGER NOT NULL DEFAULT 0;

ALTER TABLE system_llm_runs ADD COLUMN trace_id TEXT;
CREATE UNIQUE INDEX idx_system_llm_runs_trace_id ON system_llm_runs(trace_id) WHERE trace_id IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
