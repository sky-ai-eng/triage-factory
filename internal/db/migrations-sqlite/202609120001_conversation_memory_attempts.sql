-- +goose Up
-- conversation_memory_attempts: one row per try at generating a memory for a
-- conversation that ended owing one. The memory row alone cannot tell "nothing
-- was owed" from "something was owed and generation failed", and a reader who
-- cannot tell those apart either retries forever or gives up silently — this
-- ledger is what separates them.
--
-- completed_at is NULL while the attempt runs, and stays NULL for an attempt
-- whose brain died before it could close out: nothing rewrites it later,
-- because a row that never completed is the honest record of a brain that
-- stopped. outcome ('generated' | 'empty' | 'failed') is NULL exactly then;
-- error_kind ('no_model' | 'provider_backoff' | 'provider_error' | 'timeout' |
-- 'other') and error_message are NULL unless the outcome is 'failed'. All three
-- are app-validated with no CHECK (the conversations type/origin pattern), and
-- error_message is TF's own wording of the failure, never an upstream response
-- body.
--
-- system_llm_run_id is nullable and ON DELETE SET NULL: the spend-ledger insert
-- is best-effort, so the writer binds the id through a subselect and a dangling
-- one stores NULL rather than failing the attempt's own record. The window
-- counters say how much of the transcript the attempt fed the model, so a thin
-- memory reads as a truncated window rather than as a model with nothing to
-- say.
--
-- SQLite is N=1 (local mode) so there is no RLS; the org_id default mirrors the
-- Postgres baseline, and both FKs are single-column here (the composite parent
-- keys the Postgres FKs target have no SQLite twin).
CREATE TABLE conversation_memory_attempts (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    started_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at      DATETIME,
    outcome           TEXT,
    error_kind        TEXT,
    error_message     TEXT,
    system_llm_run_id TEXT REFERENCES system_llm_runs(id) ON DELETE SET NULL,
    window_rows_total INTEGER NOT NULL DEFAULT 0,
    window_rows_sent  INTEGER NOT NULL DEFAULT 0
);

-- The newest-attempt read.
CREATE INDEX idx_conversation_memory_attempts_conversation
    ON conversation_memory_attempts(conversation_id, started_at DESC);

-- +goose Down
SELECT 'down not supported';
