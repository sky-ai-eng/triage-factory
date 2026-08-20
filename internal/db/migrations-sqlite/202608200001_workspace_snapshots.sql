-- +goose Up
-- workspace_snapshots: the durable lifecycle of one snapshot key's workspace
-- blob — pending, written, or failed — and the engagement that owns the write.
--
-- The blob store answers "is there a blob" and nothing else, so its silence is
-- four different situations wearing one face: a persist still in flight, a
-- persist that failed, a conversation that never got far enough to have a
-- workspace, and a snapshot the retention reaper collected. A resume that
-- cannot tell them apart has to treat every one of them as gone forever. This
-- row is what separates them: no row is the last two (nothing was ever owed),
-- 'pending' names a write in flight and, through writer_claim_id, the
-- engagement to ask about it, and 'failed' is the durable answer that stops a
-- waiter waiting.
--
-- writer_claim_id is also the stale-write guard. A writer re-reads it just
-- before uploading and skips the upload when the key has moved to someone else,
-- so an older engagement's late blob never lands on top of a successor's newer
-- one; the completion CAS keys on the same column, so a displaced writer also
-- cannot close out a write it no longer owns. No claims FK on it — the column
-- identifies a writer, and a claim row's fate must not decide whether the
-- blob's state is knowable.
--
-- The key is the snapshot key: (org_id, blueprint_run_id), because a
-- blueprint's steps share one workspace tree and one blob. SQLite is N=1 (local
-- mode) so there is no RLS; the org_id default mirrors the Postgres baseline,
-- and the blueprint_run_id FK is single-column here (the composite parent key
-- the Postgres FK targets has no SQLite twin).
CREATE TABLE workspace_snapshots (
    org_id           TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    blueprint_run_id TEXT NOT NULL REFERENCES blueprint_runs(id) ON DELETE CASCADE,
    state            TEXT NOT NULL CHECK (state IN ('pending', 'written', 'failed')),
    writer_claim_id  TEXT NOT NULL,
    updated_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, blueprint_run_id)
);

-- +goose Down
SELECT 'down not supported';
