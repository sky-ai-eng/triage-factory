-- +goose Up
-- SKY-414: durable, DB-backed event queue for the router.
--
-- The in-memory event bus (internal/eventbus) drops events for slow
-- subscribers under burst — Publish is a non-blocking send onto a
-- 256-deep per-subscriber channel. The router is the system of record:
-- it persists event rows and creates tasks. Riding the lossy bus meant a
-- discovery/backfill burst could silently drop an event row *and* its
-- task. This table is the transactional-outbox queue the router drains
-- instead: the events audit row and a queue row are inserted atomically
-- at ingest, so a recorded event is always routable, survives restarts,
-- and is processed exactly once.
--
-- Status lifecycle: pending -> processing -> done | failed. A transient
-- failure returns a claimed row to pending; a row that exhausts its
-- retry budget lands in failed (poison pill, retained for debugging).
-- 'processing' rows left by a crash are reset to pending at boot (single
-- worker) and replayed.
--
-- SQLite/local is single-worker (N=1). The status + claimed_at machinery
-- mirrors the Postgres impl so the two stores stay behaviorally
-- identical; Postgres ClaimNext uses FOR UPDATE SKIP LOCKED so multiple
-- router workers can drain concurrently without double-processing — the
-- groundwork for horizontal routing (actually running N workers is a
-- SKY-414 non-goal).
CREATE TABLE event_queue (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id     TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    entity_id    TEXT REFERENCES entities(id),
    event_type   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',   -- pending | processing | done | failed
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT,
    enqueued_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    claimed_at   DATETIME,
    processed_at DATETIME,
    org_id       TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);

-- FIFO drain over pending rows: the worker claims the oldest by id.
CREATE INDEX        idx_event_queue_pending          ON event_queue(id)                  WHERE status = 'pending';
-- Per-entity history for debug/audit + the entity-scoped reads.
CREATE INDEX        idx_event_queue_entity           ON event_queue(entity_id);
-- Retention prune of done rows + ops visibility by terminal state/time.
CREATE INDEX        idx_event_queue_status_processed ON event_queue(status, processed_at);
-- One queue row per event row — the outbox invariant (an event is
-- enqueued exactly once) and idempotent enqueue.
CREATE UNIQUE INDEX idx_event_queue_event            ON event_queue(event_id);

-- +goose Down
SELECT 'down not supported';
