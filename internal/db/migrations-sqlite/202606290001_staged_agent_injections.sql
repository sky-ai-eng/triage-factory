-- +goose Up
-- staged_agent_injections (TFAC-501): the durable, producer-agnostic "stage for next
-- resume" agent-injection queue — the generic terminal/parked half of the
-- staged-injection delivery seam TFAC-493 shipped live-only. A producer that can't
-- re-derive its injection from a durable row of its own (the new-commits notifier,
-- the first consumer) appends a row here when the target run has no warm
-- process; the delegate spawner flushes a run's pending rows (DELETE …
-- RETURNING) into one <system-note> block ahead of the user's text on the next
-- resume, so the injection is delivered exactly once.
--
-- body is the bare, already-rendered injection line (the flush wraps + bundles).
-- producer is a free-text origin tag (domain.StagedInjectionProducer*, extensible,
-- no CHECK) kept for audit only. run_id ON DELETE CASCADE retires a parked run's
-- undelivered injections with it (the agent will never resume to read them). org_id
-- is a plain column with the local default for parity with the Postgres baseline
-- (SQLite is N=1, no RLS).
CREATE TABLE staged_agent_injections (
    id         TEXT PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    org_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    producer   TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- The index covers the per-run claim's run_id lookup + the created_at sort,
-- mirroring run_messages' idx_run_messages_run (also run_id-leading, no org_id).
-- org_id is applied as a residual filter, NOT indexed: run_id alone is already
-- selective (a run holds O(few) injections) and functionally determines org_id,
-- so a leading org_id would buy nothing — and local-mode is N=1 regardless.
CREATE INDEX idx_staged_agent_injections_run ON staged_agent_injections(run_id, created_at);

-- +goose Down
SELECT 'down not supported';
