-- +goose Up
-- A pull request's mergeability, read only by the probe worker and stored
-- here rather than in the entity's snapshot. One row per GitHub pull request
-- entity the worker has an answer for: the last known answer (never UNKNOWN),
-- the head it was read at, and when. The probe worker is the only writer of
-- the three, and github:pr:conflicts / github:pr:conflicts_resolved are
-- emitted from the row's transitions in the transaction that writes it.
CREATE TABLE pull_request_mergeability (
    entity_id           TEXT PRIMARY KEY REFERENCES entities(id) ON DELETE CASCADE,
    org_id              TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    mergeable           TEXT NULL CHECK (mergeable IN ('MERGEABLE','CONFLICTING')),
    mergeable_head_sha  TEXT NULL,
    checked_at          DATETIME NULL
);

-- A pull request already conflicting before the upgrade has its conflicts
-- task already. Without a row its first probe would compare against nothing
-- and emit github:pr:conflicts again, so every active pull request whose
-- stored snapshot carries a known answer starts with that answer, at the head
-- and time the snapshot was read at.
INSERT INTO pull_request_mergeability (entity_id, org_id, mergeable, mergeable_head_sha, checked_at)
SELECT id, org_id,
       json_extract(snapshot_json, '$.mergeable'),
       NULLIF(json_extract(snapshot_json, '$.head_sha'), ''),
       last_polled_at
FROM entities
WHERE source = 'github'
  AND state = 'active'
  AND json_valid(snapshot_json)
  AND json_extract(snapshot_json, '$.mergeable') IN ('MERGEABLE','CONFLICTING');

-- The queue of pull requests whose mergeability is due to be read, as a work
-- kind on the shared work-item block (internal/db/workitem; the kind is
-- workkinds.PRMergeabilityProbes), keyed on the entity id while a row is
-- ready, leased or parked. The four columns after the block are this kind's
-- own: the pull request, why it was admitted, when it may first be read, and
-- how many UNKNOWN answers came before it.
CREATE TABLE pr_mergeability_probes (
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
    entity_id            TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    reason               TEXT NOT NULL,
    probe_after          TEXT NOT NULL,
    unknown_reads        INTEGER NOT NULL DEFAULT 0
);

-- The work-item indexes, verbatim as workitem.IndexDDL renders them for this
-- kind; a test asserts each is present by name.
CREATE INDEX IF NOT EXISTS idx_pr_mergeability_probes_ready_next ON pr_mergeability_probes (next_attempt_at, id) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_pr_mergeability_probes_ready_cancel ON pr_mergeability_probes (id) WHERE status = 'ready' AND cancel_requested_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_pr_mergeability_probes_leased_expiry ON pr_mergeability_probes (lease_expires_at) WHERE status = 'leased';
CREATE INDEX IF NOT EXISTS idx_pr_mergeability_probes_parked ON pr_mergeability_probes (org_id, id) WHERE status = 'parked';
CREATE UNIQUE INDEX IF NOT EXISTS uq_pr_mergeability_probes_unique_key ON pr_mergeability_probes (org_id, unique_key) WHERE unique_key IS NOT NULL AND status IN ('ready','leased','parked');

-- +goose Down
SELECT 'down not supported';
