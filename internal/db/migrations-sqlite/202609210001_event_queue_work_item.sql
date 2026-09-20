-- +goose Up
-- event_queue adopts the shared work-item block (internal/db/workitem): the
-- lifecycle columns the queue carried on its own — status pending | processing
-- | done | failed, attempts, an executor stamp, claimed_at, processed_at — are
-- replaced by the block's status ready | leased | done | parked | cancelled,
-- attempt budget, lease with a generation fence, retry time, cancellation
-- intent and uniqueness key. ALTER TABLE ADD COLUMN cannot add a NOT NULL
-- column without a constant default, cannot add the status CHECK, and cannot
-- drop the old columns cleanly, so the table is rebuilt: a new table in the
-- block's shape, the rows copied across under the data mapping below, the old
-- table dropped, the new one renamed into place. id is copied, so a queue id
-- logged before the upgrade names the same row after it.
--
-- Data mapping, per old status:
--   pending    -> ready,  attempt = attempts, lease_generation = 0
--   processing -> leased, attempt = attempts, lease_generation = 1, the owner
--                 stamp carried onto the lease, and lease_expires_at already
--                 one second in the past — the next claim reclaims it through
--                 the expired-lease arm, with no special recovery path
--   done       -> done,   done_at = processed_at
--   failed     -> parked, attempt = attempts, last_error kept,
--                 done_at = processed_at, last_outcome NULL (the old row
--                 records no typed outcome, and inventing one would be wrong)
-- Every row: max_attempts = 5, first_enqueued_at = created_at = enqueued_at,
-- next_attempt_at NULL, cancellation columns NULL, superseded_by NULL, and
-- unique_key = 'close_owed:' || entity_id for a close obligation, NULL
-- otherwise.
--
-- Timestamps are normalized into the block's layout as they are copied.
-- Existing rows carry three layouts (CURRENT_TIMESTAMP's second-resolution
-- text, the driver's nanosecond-with-offset text, and rows written by earlier
-- builds); strftime parses all of them and renders one, and the COALESCE
-- keeps a value it cannot parse rather than failing an installed database.
CREATE TABLE event_queue_new (
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
    event_id             TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    entity_id            TEXT REFERENCES entities(id),
    event_type           TEXT NOT NULL,
    traceparent          TEXT,
    entity_poll_seq      INTEGER
);

INSERT INTO event_queue_new (
    id, org_id, status, attempt, max_attempts, next_attempt_at,
    lease_generation, lease_owner, lease_epoch, leased_at, lease_expires_at,
    cancel_requested_at, cancel_requested_by, cancel_reason,
    last_error, last_outcome, unique_key, superseded_by,
    first_enqueued_at, created_at, done_at,
    event_id, entity_id, event_type, traceparent, entity_poll_seq
)
SELECT
    id,
    org_id,
    CASE status
        WHEN 'pending'    THEN 'ready'
        WHEN 'processing' THEN 'leased'
        WHEN 'done'       THEN 'done'
        ELSE                   'parked'
    END,
    attempts,
    5,
    NULL,
    CASE status WHEN 'processing' THEN 1 ELSE 0 END,
    CASE status WHEN 'processing' THEN executor_id ELSE NULL END,
    CASE status WHEN 'processing' THEN boot_epoch  ELSE NULL END,
    CASE status WHEN 'processing' THEN COALESCE(strftime('%Y-%m-%d %H:%M:%f', claimed_at), claimed_at) ELSE NULL END,
    CASE status WHEN 'processing' THEN strftime('%Y-%m-%d %H:%M:%f', 'now', '-1 seconds') ELSE NULL END,
    NULL, NULL, NULL,
    last_error,
    NULL,
    CASE event_type WHEN 'system:entity:close_owed' THEN 'close_owed:' || entity_id ELSE NULL END,
    NULL,
    COALESCE(strftime('%Y-%m-%d %H:%M:%f', enqueued_at), enqueued_at),
    COALESCE(strftime('%Y-%m-%d %H:%M:%f', enqueued_at), enqueued_at),
    CASE status
        WHEN 'done'   THEN COALESCE(strftime('%Y-%m-%d %H:%M:%f', processed_at), processed_at)
        WHEN 'failed' THEN COALESCE(strftime('%Y-%m-%d %H:%M:%f', processed_at), processed_at)
        ELSE NULL
    END,
    event_id, entity_id, event_type, traceparent, entity_poll_seq
FROM event_queue
ORDER BY id;

-- Duplicate obligations are collapsed before the unique index exists to
-- refuse them: the previous lifecycle could park several close obligations
-- for one entity, one per poll cycle. For each entity with more than one
-- obligation among the unsettled statuses, the highest id keeps its status
-- and every other one is settled as superseded by it, its key kept, its
-- lease released.
UPDATE event_queue_new
SET status = 'cancelled',
    last_outcome = 'superseded',
    superseded_by = (
        SELECT MAX(k.id) FROM event_queue_new k
        WHERE k.entity_id = event_queue_new.entity_id
          AND k.event_type = 'system:entity:close_owed'
          AND k.status IN ('ready','leased','parked')
    ),
    done_at = strftime('%Y-%m-%d %H:%M:%f', 'now'),
    lease_owner = NULL, lease_epoch = NULL, leased_at = NULL, lease_expires_at = NULL
WHERE event_type = 'system:entity:close_owed'
  AND status IN ('ready','leased','parked')
  AND id < (
        SELECT MAX(k.id) FROM event_queue_new k
        WHERE k.entity_id = event_queue_new.entity_id
          AND k.event_type = 'system:entity:close_owed'
          AND k.status IN ('ready','leased','parked')
  );

-- Nothing references event_queue by foreign key, trigger or view, so the
-- swap needs no other table touched.
DROP TABLE event_queue;
ALTER TABLE event_queue_new RENAME TO event_queue;

CREATE INDEX        idx_event_queue_entity  ON event_queue(entity_id);
CREATE UNIQUE INDEX idx_event_queue_event   ON event_queue(event_id);
-- The prune's scan: settled rows older than the retention cutoff.
CREATE INDEX        idx_event_queue_done_at ON event_queue(done_at) WHERE status IN ('done','cancelled');
-- The work-item indexes, verbatim as workitem.IndexDDL renders them for this
-- kind; a test asserts each is present by name.
CREATE INDEX IF NOT EXISTS idx_event_queue_ready_next ON event_queue (next_attempt_at, id) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_event_queue_ready_cancel ON event_queue (id) WHERE status = 'ready' AND cancel_requested_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_event_queue_leased_expiry ON event_queue (lease_expires_at) WHERE status = 'leased';
CREATE INDEX IF NOT EXISTS idx_event_queue_parked ON event_queue (org_id, id) WHERE status = 'parked';
CREATE UNIQUE INDEX IF NOT EXISTS uq_event_queue_unique_key ON event_queue (org_id, unique_key) WHERE unique_key IS NOT NULL AND status IN ('ready','leased','parked');

-- +goose Down
SELECT 'down not supported';
