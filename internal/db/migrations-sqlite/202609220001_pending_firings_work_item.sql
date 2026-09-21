-- +goose Up
-- pending_firings adopts the shared work-item block (internal/db/workitem):
-- the lifecycle columns the queue carried on its own — status pending |
-- draining | fired | skipped_stale, queued_at, claimed_at, drained_at — are
-- replaced by the block's status ready | leased | done | parked | cancelled,
-- attempt budget, lease with a generation fence, retry time, cancellation
-- intent and uniqueness key. ALTER TABLE ADD COLUMN cannot add a NOT NULL
-- column without a constant default, cannot add the status CHECK, and cannot
-- drop the old columns cleanly, so the table is rebuilt: a new table in the
-- block's shape, the rows copied across under the data mapping below, the old
-- table dropped, the new one renamed into place. id is copied, so a firing id
-- logged before the upgrade names the same row after it.
--
-- Data mapping, per old status:
--   pending       -> ready,  attempt = 0, lease_generation = 0
--   draining      -> leased, attempt = 1 (a lease charges the attempt it is in
--                    the middle of), lease_generation = 1, no owner (the old
--                    row records none), leased_at = claimed_at, and
--                    lease_expires_at already one second in the past — the
--                    next claim reclaims it through the expired-lease arm,
--                    with no special recovery path
--   fired         -> done,   done_at = drained_at, fired_run_id kept
--   skipped_stale -> done,   done_at = drained_at, skip_reason kept
-- Every row: max_attempts = 5, first_enqueued_at = created_at = queued_at,
-- next_attempt_at NULL, cancellation columns NULL, superseded_by NULL,
-- last_error and last_outcome NULL, and unique_key = task_id || ':' ||
-- trigger_id, the key workkinds.PendingFiringKey spells. No collapse is
-- needed: the previous dedup index allowed one pending-or-draining row per
-- (task, trigger), and those are exactly the rows that map to the unsettled
-- statuses the new unique index covers.
--
-- Timestamps are normalized into the block's layout as they are copied, and
-- the COALESCE keeps a value strftime cannot parse rather than failing an
-- installed database.
CREATE TABLE pending_firings_new (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id               TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    status               TEXT NOT NULL CHECK (status IN ('ready','leased','done','parked','cancelled')),
    attempt              INTEGER NOT NULL DEFAULT 0,
    max_attempts         INTEGER NOT NULL,
    next_attempt_at      TEXT NULL,
    lease_generation     INTEGER NOT NULL DEFAULT 0,
    lease_owner          TEXT NULL,
    lease_epoch          INTEGER NULL,
    leased_at            TEXT NULL,
    lease_expires_at     TEXT NULL,
    cancel_requested_at  TEXT NULL,
    cancel_requested_by  TEXT NULL,
    cancel_reason        TEXT NULL,
    last_error           TEXT NULL,
    last_outcome         TEXT NULL,
    unique_key           TEXT NULL,
    superseded_by        INTEGER NULL,
    first_enqueued_at    TEXT NOT NULL,
    created_at           TEXT NOT NULL,
    done_at              TEXT NULL,
    entity_id            TEXT NOT NULL REFERENCES entities(id)       ON DELETE CASCADE,
    task_id              TEXT NOT NULL REFERENCES tasks(id)          ON DELETE CASCADE,
    trigger_id           TEXT NOT NULL REFERENCES event_handlers(id) ON DELETE CASCADE,
    triggering_event_id  TEXT NOT NULL REFERENCES events(id),
    skip_reason          TEXT,
    fired_run_id         TEXT REFERENCES blueprint_runs(id)
);

INSERT INTO pending_firings_new (
    id, org_id, status, attempt, max_attempts, next_attempt_at,
    lease_generation, lease_owner, lease_epoch, leased_at, lease_expires_at,
    cancel_requested_at, cancel_requested_by, cancel_reason,
    last_error, last_outcome, unique_key, superseded_by,
    first_enqueued_at, created_at, done_at,
    entity_id, task_id, trigger_id, triggering_event_id, skip_reason, fired_run_id
)
SELECT
    id,
    org_id,
    CASE status
        WHEN 'pending'  THEN 'ready'
        WHEN 'draining' THEN 'leased'
        ELSE                 'done'
    END,
    CASE status WHEN 'draining' THEN 1 ELSE 0 END,
    5,
    NULL,
    CASE status WHEN 'draining' THEN 1 ELSE 0 END,
    NULL,
    NULL,
    CASE status WHEN 'draining' THEN COALESCE(strftime('%Y-%m-%d %H:%M:%f', claimed_at), claimed_at) ELSE NULL END,
    CASE status WHEN 'draining' THEN strftime('%Y-%m-%d %H:%M:%f', 'now', '-1 seconds') ELSE NULL END,
    NULL, NULL, NULL,
    NULL,
    NULL,
    task_id || ':' || trigger_id,
    NULL,
    COALESCE(strftime('%Y-%m-%d %H:%M:%f', queued_at), queued_at),
    COALESCE(strftime('%Y-%m-%d %H:%M:%f', queued_at), queued_at),
    CASE status
        WHEN 'fired'         THEN COALESCE(strftime('%Y-%m-%d %H:%M:%f', drained_at), drained_at)
        WHEN 'skipped_stale' THEN COALESCE(strftime('%Y-%m-%d %H:%M:%f', drained_at), drained_at)
        ELSE NULL
    END,
    entity_id, task_id, trigger_id, triggering_event_id, skip_reason, fired_run_id
FROM pending_firings
ORDER BY id;

-- Nothing references pending_firings by foreign key, trigger or view, so the
-- swap needs no other table touched.
DROP TABLE pending_firings;
ALTER TABLE pending_firings_new RENAME TO pending_firings;

-- The gate read: does the task have an unsettled firing? Parked is in it,
-- because a parked row still holds its key.
CREATE INDEX idx_pending_firings_task_unsettled ON pending_firings (task_id) WHERE status IN ('ready','leased','parked');
-- The work-item indexes, verbatim as workitem.IndexDDL renders them for this
-- kind; a test asserts each is present by name.
CREATE INDEX IF NOT EXISTS idx_pending_firings_ready_next ON pending_firings (next_attempt_at, id) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_pending_firings_ready_cancel ON pending_firings (id) WHERE status = 'ready' AND cancel_requested_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_pending_firings_leased_expiry ON pending_firings (lease_expires_at) WHERE status = 'leased';
CREATE INDEX IF NOT EXISTS idx_pending_firings_parked ON pending_firings (org_id, id) WHERE status = 'parked';
CREATE UNIQUE INDEX IF NOT EXISTS uq_pending_firings_unique_key ON pending_firings (org_id, unique_key) WHERE unique_key IS NOT NULL AND status IN ('ready','leased','parked');

-- +goose Down
SELECT 'down not supported';
