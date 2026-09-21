-- +goose Up
-- The post-scoring re-evaluation becomes a work kind on the shared work-item
-- block (internal/db/workitem; the kind is workkinds.TaskReDerive). One row
-- per task whose scores have landed and whose deferred triggers have not yet
-- been evaluated against them, keyed on the task id while the row is ready,
-- leased or parked.
--
-- tasks.score_revision counts score writes: UpdateTaskScores raises it in the
-- transaction that writes the scores and, in the same transaction, raises the
-- queue row's requested_revision to match. A worker freezes requested_revision
-- into its receipt at claim and completes only when the row still carries
-- that value, so a score that lands mid-evaluation is never marked evaluated
-- by a pass that read the older one.
ALTER TABLE tasks ADD COLUMN score_revision INTEGER NOT NULL DEFAULT 0;

CREATE TABLE task_rederive_queue (
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
    task_id              TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    requested_revision   INTEGER NOT NULL DEFAULT 0
);

-- A task owed a pass before the upgrade is owed one after it: one ready row
-- per owed task, at revision 0, keyed on the task, enqueued now.
INSERT INTO task_rederive_queue (
    org_id, status, attempt, max_attempts, lease_generation,
    task_id, requested_revision, unique_key, first_enqueued_at, created_at
)
SELECT
    org_id, 'ready', 0, 5, 0,
    id, 0, id, strftime('%Y-%m-%d %H:%M:%f','now'), strftime('%Y-%m-%d %H:%M:%f','now')
FROM tasks
WHERE rederive_owed = 1
ORDER BY created_at, id;

-- SQLite refuses to drop a column an index names, so the index goes first.
DROP INDEX idx_tasks_rederive_owed;
ALTER TABLE tasks DROP COLUMN rederive_owed;

-- The work-item indexes, verbatim as workitem.IndexDDL renders them for this
-- kind; a test asserts each is present by name.
CREATE INDEX IF NOT EXISTS idx_task_rederive_queue_ready_next ON task_rederive_queue (next_attempt_at, id) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_task_rederive_queue_ready_cancel ON task_rederive_queue (id) WHERE status = 'ready' AND cancel_requested_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_task_rederive_queue_leased_expiry ON task_rederive_queue (lease_expires_at) WHERE status = 'leased';
CREATE INDEX IF NOT EXISTS idx_task_rederive_queue_parked ON task_rederive_queue (org_id, id) WHERE status = 'parked';
CREATE UNIQUE INDEX IF NOT EXISTS uq_task_rederive_queue_unique_key ON task_rederive_queue (org_id, unique_key) WHERE unique_key IS NOT NULL AND status IN ('ready','leased','parked');

-- +goose Down
SELECT 'down not supported';
