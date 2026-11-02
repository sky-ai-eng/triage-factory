-- +goose NO TRANSACTION
-- +goose Up
-- Durable notifications, and notify rules as a third kind of event handler
-- whose notifications name the entity their event is about. The artifacts
-- table becomes deliverables, which hold only the pull requests and reviews a
-- run prepared for a person, each recording whether the run is known to have
-- opened it. Every external_actions row says who performed it, and a row
-- that changed an object names it. A task holds a close on its closing event
-- while the task's work finishes. A pull request's mergeability moves out of
-- its entity's snapshot into its own table, read through a probe queue on the
-- work-item block, and the poll records the last tip of each base branch it
-- watches. The org's newest generated memory gets an index.
--
-- NO TRANSACTION because widening event_handlers' kind CHECK rebuilds a table
-- other tables reference (pending_firings, conversations, blueprint_runs), and
-- PRAGMA foreign_keys is a no-op inside a transaction: with them on, dropping
-- the parent runs an implicit DELETE that cascades into every row referencing
-- it. The pragma is set first and everything else runs in one explicit
-- transaction, so a failure part way commits nothing and the next boot retries
-- the whole file. The pool is capped at one connection, so the pragma and the
-- transaction hold for every statement here. No table references the
-- deliverable rows the DML below deletes, so no cascade is skipped.

PRAGMA foreign_keys = off;

BEGIN IMMEDIATE;

-- ===========================================================================
-- 1. Notifications and notify rules
-- ===========================================================================

-- Durable notifications (specs/durable-notifications.md). Local mode is N=1:
-- the one user is the recipient of every audience, and there is no RLS.
-- entity_id is the entity an event notification is about, set exactly on
-- origin = 'event' rows.
CREATE TABLE notifications (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    origin          TEXT NOT NULL CHECK (origin IN ('system', 'event')),
    lifecycle       TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind <> ''),
    key             TEXT NOT NULL,
    audience_scope  TEXT NOT NULL,
    audience_role   TEXT,
    team_id         TEXT REFERENCES teams(id) ON DELETE CASCADE,
    user_id         TEXT REFERENCES users(id) ON DELETE CASCADE,
    source          TEXT,
    priority        TEXT NOT NULL CHECK (priority IN ('urgent', 'high', 'low')),
    dismissible     BOOLEAN NOT NULL,
    title           TEXT NOT NULL,
    body            TEXT NOT NULL DEFAULT '',
    actions         TEXT NOT NULL DEFAULT '[]',
    params          TEXT NOT NULL DEFAULT '{}',
    first_seen_at   DATETIME NOT NULL,
    last_seen_at    DATETIME NOT NULL,
    seen_count      INTEGER NOT NULL DEFAULT 1 CHECK (seen_count >= 1),
    escalated_at    DATETIME NOT NULL,
    resolved_at     DATETIME,
    resolved_reason TEXT CHECK (resolved_reason IN ('cleared', 'source_off', 'audience_removed')),
    expires_at      DATETIME,
    entity_id       TEXT REFERENCES entities(id) ON DELETE CASCADE
        CHECK ((origin = 'event') = (entity_id IS NOT NULL)),
    CHECK (
        (audience_scope = 'org' AND audience_role IN ('members', 'admins') AND team_id IS NULL AND user_id IS NULL)
        OR (audience_scope = 'team' AND audience_role IN ('members', 'admins') AND team_id IS NOT NULL AND user_id IS NULL)
        OR (audience_scope = 'user' AND audience_role IS NULL AND team_id IS NULL AND user_id IS NOT NULL)
    ),
    CHECK (
        (lifecycle = 'condition' AND expires_at IS NULL)
        OR (lifecycle = 'notice' AND expires_at IS NOT NULL)
    ),
    CHECK (lifecycle = 'condition' OR (dismissible AND resolved_at IS NULL)),
    CHECK ((resolved_at IS NULL) = (resolved_reason IS NULL))
);

-- The open occurrence a raise coalesces into.
CREATE UNIQUE INDEX notifications_open_occurrence_idx ON notifications (
    org_id, origin, kind, key, audience_scope,
    coalesce(audience_role, ''), coalesce(team_id, ''), coalesce(user_id, '')
) WHERE resolved_at IS NULL;

CREATE INDEX notifications_list_idx ON notifications (org_id, resolved_at, priority, escalated_at, id);

-- An entity's open event notifications: what the close phase resolves.
CREATE INDEX notifications_open_entity_idx ON notifications (org_id, entity_id, kind)
    WHERE entity_id IS NOT NULL AND resolved_at IS NULL;

CREATE TABLE notification_marks (
    notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    read_at         DATETIME,
    dismissed_at    DATETIME,
    PRIMARY KEY (notification_id, user_id)
);

-- A notify rule is a third kind of event_handlers row, and the notifications
-- it raises have origin 'event'. Widening kind's CHECK rebuilds the table.
CREATE TABLE event_handlers_new (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    creator_user_id TEXT,
    -- team_id is NOT NULL: every handler is team-owned. The LocalDefaultTeamID
    -- sentinel is the DEFAULT so local-mode inserts and direct INSERTs that
    -- omit it land on the sole team.
    team_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000010',

    kind            TEXT NOT NULL CHECK (kind IN ('rule', 'trigger', 'notify')),

    event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,
    enabled              BOOLEAN NOT NULL DEFAULT 1,
    -- source is app-validated, not CHECK-constrained: 'system' | 'user' today,
    -- and a new provenance value must be addable without a table rebuild.
    source               TEXT NOT NULL DEFAULT 'user',
    -- The stable shipped identifier for source='system' rows; NULL for user
    -- handlers. Re-seed idempotency keys on (org_id, team_id, system_slug).
    system_slug          TEXT,

    -- Rule and notify fields. NULL for triggers; a notify rule has a name
    -- and no priority or sort order.
    name             TEXT,
    default_priority REAL,
    sort_order       INTEGER,

    -- Trigger-only fields. NULL for rules and notify rules.
    blueprint_id             TEXT,
    breaker_threshold        INTEGER,
    min_autonomy_suitability REAL,

    -- Notify-only: who a notify rule tells, its creator ('creator') or its
    -- team's members ('team').
    notify_audience TEXT,

    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    applies_to_unowned BOOLEAN NOT NULL DEFAULT 0,
    user_modified INTEGER NOT NULL DEFAULT 0,
    deleted_at DATETIME,

    FOREIGN KEY (blueprint_id, org_id) REFERENCES blueprints (id, org_id) ON DELETE CASCADE,
    -- Same-team guard: a trigger may only fire a blueprint its own team owns.
    FOREIGN KEY (blueprint_id, team_id) REFERENCES blueprints (id, team_id) ON DELETE CASCADE,

    CHECK (kind <> 'rule' OR (
        blueprint_id IS NULL
        AND breaker_threshold IS NULL
        AND min_autonomy_suitability IS NULL
        AND name IS NOT NULL
        AND default_priority IS NOT NULL
        AND sort_order IS NOT NULL
    )),
    CHECK (kind <> 'trigger' OR (
        blueprint_id IS NOT NULL
        AND breaker_threshold IS NOT NULL
        AND min_autonomy_suitability IS NOT NULL
        AND default_priority IS NULL
        AND sort_order IS NULL
        AND name IS NULL
    )),
    -- A notify rule is a person's or a team's, never shipped content, and
    -- tells its audience about an event without touching tasks.
    CHECK (kind <> 'notify' OR (
        blueprint_id IS NULL
        AND breaker_threshold IS NULL
        AND min_autonomy_suitability IS NULL
        AND default_priority IS NULL
        AND sort_order IS NULL
        AND name IS NOT NULL
        AND notify_audience IN ('creator', 'team')
        AND source <> 'system'
        AND NOT applies_to_unowned
    )),
    CHECK ((kind = 'notify') = (notify_audience IS NOT NULL)),
    -- Any non-system provenance requires a creator, matching the prompts /
    -- blueprints pairing.
    CONSTRAINT event_handlers_system_has_no_creator CHECK (
        (source = 'system' AND creator_user_id IS NULL)
        OR (source <> 'system' AND creator_user_id IS NOT NULL)
    ),
    -- Per-team re-seed idempotency key: each team gets one copy of each
    -- shipped handler. NULLs distinct, so user handlers never collide.
    CONSTRAINT event_handlers_org_team_slug_unique UNIQUE (org_id, team_id, system_slug)
);

INSERT INTO event_handlers_new (
    id, org_id, creator_user_id, team_id, kind, event_type, scope_predicate_json,
    enabled, source, system_slug, name, default_priority, sort_order,
    blueprint_id, breaker_threshold, min_autonomy_suitability,
    created_at, updated_at, applies_to_unowned, user_modified, deleted_at)
SELECT
    id, org_id, creator_user_id, team_id, kind, event_type, scope_predicate_json,
    enabled, source, system_slug, name, default_priority, sort_order,
    blueprint_id, breaker_threshold, min_autonomy_suitability,
    created_at, updated_at, applies_to_unowned, user_modified, deleted_at
FROM event_handlers;

DROP TABLE event_handlers;
ALTER TABLE event_handlers_new RENAME TO event_handlers;

CREATE UNIQUE INDEX event_handlers_id_org_unique          ON event_handlers (id, org_id);
CREATE INDEX        idx_event_handlers_event_type_enabled ON event_handlers(org_id, event_type) WHERE enabled = 1;
CREATE INDEX        idx_event_handlers_kind               ON event_handlers(org_id, kind);
CREATE INDEX        idx_event_handlers_blueprint          ON event_handlers(org_id, blueprint_id) WHERE blueprint_id IS NOT NULL;
-- A blueprint is fired by exactly one live trigger.
CREATE UNIQUE INDEX event_handlers_one_trigger_per_blueprint ON event_handlers(blueprint_id) WHERE blueprint_id IS NOT NULL AND deleted_at IS NULL;

-- ===========================================================================
-- 2. Deliverables
-- ===========================================================================

-- The artifacts table becomes deliverables: the pull requests and reviews a
-- run prepared for a person to act on. Every other kind (branch, issue,
-- comment, message) recorded what a run changed, which the external-action
-- ledger now records on its own (section 3), so those rows go. Of the ledger
-- rows already stored, only a pull request's and a review's can name their
-- object, so nothing stands in for the rows deleted here; that loss is
-- accepted. There is no CHECK on kind: SQLite adds no table constraint to an
-- existing table without rebuilding it, and the store refuses any other kind.
DELETE FROM artifacts WHERE kind NOT IN ('pull_request', 'review');

-- Rows, columns and keys are unchanged by the rename. Each index is recreated
-- under the table's name, and the one on conversation_id is named for that
-- column, as the Postgres baseline's is.
ALTER TABLE artifacts RENAME TO deliverables;

DROP INDEX idx_artifacts_dedup;
CREATE UNIQUE INDEX idx_deliverables_dedup ON deliverables (org_id, scope, dedup_key);

DROP INDEX idx_artifacts_run;
CREATE INDEX idx_deliverables_conversation ON deliverables (conversation_id);

DROP INDEX idx_artifacts_team_created;
CREATE INDEX idx_deliverables_team_created ON deliverables (team_id, created_at DESC);

DROP INDEX idx_artifacts_org_created;
CREATE INDEX idx_deliverables_org_created ON deliverables (org_id, created_at DESC);

DROP INDEX idx_artifacts_node_id;
CREATE INDEX idx_deliverables_node_id ON deliverables (org_id, scope, node_id) WHERE node_id IS NOT NULL;

DROP INDEX idx_artifacts_external_id;
CREATE INDEX idx_deliverables_external_id ON deliverables (org_id, scope, kind, external_id) WHERE external_id IS NOT NULL;

DROP INDEX idx_artifacts_entity;
CREATE INDEX idx_deliverables_entity ON deliverables (entity_id) WHERE entity_id IS NOT NULL;

-- verification: whether the recording run is known to have opened the pull
-- request. 'pending' on a pull request create's row until the reconciler
-- settles it from GitHub's answer: 'verified', or 'foreign' when GitHub shows
-- the run did not open it, which also closes a draft or open row. Only a
-- verified row is acted on through TF. Every other writer's row, and every
-- row already stored, is 'verified'. App-validated, like kind: ALTER TABLE
-- cannot add the CHECK the Postgres column carries.
ALTER TABLE deliverables ADD COLUMN verification TEXT NOT NULL DEFAULT 'verified';

-- ===========================================================================
-- 3. The external-action ledger
-- ===========================================================================

-- performed_by: who performed the act. 'agent' is a run's agent, and every
-- refusal of one of its attempts; 'person' is a person acting through TF (an
-- approval or edit of a run's deliverable, a board drag, a task close);
-- 'system' is TF on its own (the board mirrors, an event-driven teardown). A
-- person's approval of a run's deliverable carries that run's
-- conversation_id, and actor_user_id is set on a manual run's agent rows too,
-- so this column is what tells them apart.
--
-- SQLite adds a NOT NULL column only with a default. The stores refuse a row
-- that does not name its performer, so the default only reaches the rows
-- already stored.
ALTER TABLE external_actions ADD COLUMN performed_by TEXT NOT NULL DEFAULT 'agent'
    CHECK (performed_by IN ('agent', 'person', 'system'));

-- object_kind and object_key name the object an external action changed, set
-- exactly when the act landed a change and NULL on a refusal, a failure and a
-- catch-all row. The kind's identifying columns are read together
-- (domain.ObjectID): entity_id for a pull request or a GitHub issue,
-- repository_id for a repository, repository_id + object_key for a branch (the
-- ref), a label (its name), a release, a workflow or a workflow run (GitHub's
-- id), and scope + object_key for a Jira or Linear issue, a review, a comment
-- and a Slack message (the object's key).
ALTER TABLE external_actions ADD COLUMN object_kind TEXT
    CHECK (object_kind IN (
        'pull_request', 'issue', 'repository', 'branch', 'label', 'release',
        'workflow', 'workflow_run', 'review', 'comment', 'message'));
ALTER TABLE external_actions ADD COLUMN object_key TEXT;

-- A row with no run is never an agent's. With no actor either it is TF's own
-- (the Jira and Linear board mirrors, a commit-signing refusal outside a run),
-- and with an actor it is a person's (the board drag). Every row with a run
-- stays 'agent': a person's approval recorded against a run carries the same
-- columns as the run's own rows, so it cannot be told apart here.
UPDATE external_actions
   SET performed_by = CASE WHEN actor_user_id IS NULL THEN 'system' ELSE 'person' END
 WHERE conversation_id IS NULL;

-- A pull request's rows and a review's carry the object's identity in their
-- own columns, so those name it here, and their runs show the artifact and
-- their deliverables match it. No other kind's row carries its identity, and
-- those rows name no object.
--
-- A pull request is identified by its entity.
UPDATE external_actions
   SET object_kind = 'pull_request'
 WHERE provider = 'github'
   AND entity_id IS NOT NULL
   AND action IN ('pr_created', 'pr_marked_ready', 'pr_converted_to_draft', 'pr_edited', 'pr_closed',
                  'pr_reopened', 'pr_merged', 'pr_auto_merge_enabled', 'pr_auto_merge_disabled',
                  'pr_branch_updated', 'review_requested', 'review_request_removed');

-- A submitted review is identified by GitHub's review id, the row's
-- external_id, in the key domain.GitHubReviewObjectKey builds. An id that is
-- not a positive integer (a submit whose response carried none) names no
-- review.
UPDATE external_actions
   SET object_kind = 'review',
       object_key = 'github:review:' || external_id
 WHERE provider = 'github'
   AND action IN ('review_submitted', 'review_dismissed')
   AND external_id GLOB '[1-9]*'
   AND external_id NOT GLOB '*[^0-9]*';

-- The rows that changed one keyed object, found by its key within a scope:
-- the tracker's read of which comments TF posted.
CREATE INDEX idx_external_actions_object_key ON external_actions (org_id, scope, object_key)
    WHERE object_key IS NOT NULL;

-- A conversation's rows in one org, in time order: the conversation's action
-- list and its artifact reads filter on both columns. The single-column
-- conversation index serves the conversation foreign key's ON DELETE SET NULL.
CREATE INDEX idx_external_actions_org_conversation_occurred
    ON external_actions (org_id, conversation_id, occurred_at);

-- ===========================================================================
-- 4. Held task closes
-- ===========================================================================

-- A close owed to the task and held while the task's working conversations
-- finish: close_deferred_event_id is the closing event, and close_deferred_at
-- when the close was first held, kept while later closing events take the
-- hold's place. Written by the router's close phase, cleared with the hold by
-- its close-hold sweep, which also stops the event-started work still going
-- once a hold is older than its limit, and read only while the task is
-- active. close_deferred_at is text in the layout every lease comparison on
-- this dialect reads.
ALTER TABLE tasks ADD COLUMN close_deferred_event_id TEXT REFERENCES events(id);
ALTER TABLE tasks ADD COLUMN close_deferred_at DATETIME;

-- The close-hold sweep's read: the few tasks holding a close.
CREATE INDEX idx_tasks_close_deferred ON tasks (id) WHERE close_deferred_event_id IS NOT NULL;

-- Both set or both NULL. Triggers rather than a CHECK because SQLite adds no
-- table constraint to an existing table without rebuilding it.
-- +goose StatementBegin
CREATE TRIGGER tasks_close_hold_paired_insert
BEFORE INSERT ON tasks
WHEN (NEW.close_deferred_event_id IS NULL) <> (NEW.close_deferred_at IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'tasks_close_hold_paired: a held close names both its event and when it was first held');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER tasks_close_hold_paired_update
BEFORE UPDATE OF close_deferred_event_id, close_deferred_at ON tasks
WHEN (NEW.close_deferred_event_id IS NULL) <> (NEW.close_deferred_at IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'tasks_close_hold_paired: a held close names both its event and when it was first held');
END;
-- +goose StatementEnd

-- ===========================================================================
-- 5. Pull request mergeability
-- ===========================================================================

-- A pull request's mergeability, stored here rather than in the entity's
-- snapshot: one row per GitHub pull request entity the probe worker or the
-- branch watch has written.
CREATE TABLE pull_request_mergeability (
    entity_id           TEXT PRIMARY KEY REFERENCES entities(id) ON DELETE CASCADE,
    org_id              TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    -- Written only by the probe worker: the last known answer (never
    -- UNKNOWN, and NULL until a read answers), and the head, time and base
    -- branch of the last read that settled a probe, which is a known answer
    -- or the UNKNOWN that ended its retries. The base is kept because a
    -- retargeted pull request's answer is about a branch it no longer merges
    -- into. github:pr:conflicts / github:pr:conflicts_resolved are emitted
    -- from the answer's transitions in the transaction that writes it.
    mergeable           TEXT NULL CHECK (mergeable IN ('MERGEABLE','CONFLICTING')),
    mergeable_head_sha  TEXT NULL,
    checked_at          DATETIME NULL,
    mergeable_base_ref  TEXT NULL,
    -- The pull request's changed paths, written only by the branch watch,
    -- which reads them when the pull request's base branch moves to decide
    -- whether the move can have changed its mergeability. paths_json is a
    -- JSON array of {"path", "change", "previous"}; paths_head_sha and
    -- paths_base_ref the head and base the list was read at; paths_complete
    -- whether it holds every changed file. The probe worker's columns are
    -- untouched by the watch, and the watch's by the worker.
    paths_json          TEXT NULL,
    paths_head_sha      TEXT NULL,
    paths_base_ref      TEXT NULL,
    paths_complete      BOOLEAN NOT NULL DEFAULT 0,
    -- A base branch move the stored answer may predate, written by the branch
    -- watch for each pull request the move can have affected: base_move_reason
    -- is base_moved or base_reset, and base_moved_at when the watch wrote it,
    -- in the probe queue's leased_at shape. The poll admits a probe for it
    -- every cycle until a probe leased at or after base_moved_at settles a
    -- read, which clears both; a probe dropped or read before the move leaves
    -- them.
    base_move_reason    TEXT NULL CHECK (base_move_reason IN ('base_moved','base_reset')),
    base_moved_at       TEXT NULL,
    -- How many rounds of the probe worker's reads in a row ended in the
    -- UNKNOWN give-up (GitHub answered UNKNOWN on every retry), so mergeable,
    -- if set, is from an earlier read when it is above zero. The poll reads
    -- such a pull request again after a pause that doubles with each, rather
    -- than waiting for the sweep; a known answer resets it to zero.
    give_ups            INTEGER NOT NULL DEFAULT 0
);

-- A pull request already conflicting before the upgrade has its conflicts
-- task already. Without a row its first probe would compare against nothing
-- and emit github:pr:conflicts again, so every active pull request whose
-- stored snapshot carries a known answer starts with that answer, at the head,
-- base and time the snapshot was read at. A snapshot naming no base leaves
-- the row with none, and it is not probed on its base until its next read
-- records one.
INSERT INTO pull_request_mergeability (entity_id, org_id, mergeable, mergeable_head_sha, checked_at, mergeable_base_ref)
SELECT id, org_id,
       json_extract(snapshot_json, '$.mergeable'),
       NULLIF(json_extract(snapshot_json, '$.head_sha'), ''),
       last_polled_at,
       NULLIF(json_extract(snapshot_json, '$.base_ref'), '')
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
-- The entity delete's cascade, and the worker's hourly prune of settled
-- probes, which are a day of sweeps.
CREATE INDEX idx_pr_mergeability_probes_entity ON pr_mergeability_probes (entity_id);
CREATE INDEX idx_pr_mergeability_probes_done_at ON pr_mergeability_probes (done_at) WHERE status IN ('done','cancelled');

-- The last tip of each base branch the poll's branch watch processed: one row
-- per (repository, branch) an open pull request targets, written only by the
-- watch. tip_sha is the tip whose move has been recorded on the rows of the
-- pull requests it can have affected (pull_request_mergeability's
-- base_move_reason), etag the ref read's from its last 200, and observed_at
-- when the tip was last checked, a 304 included. compare_failing_since is
-- when comparing tip_sha with a newer tip first failed, in a run of failures
-- still going: a compare that has failed for long enough resets the branch.
-- Stored so a restart does not take every base branch for newly seen and
-- probe every pull request on it.
CREATE TABLE branch_tips (
    repository_id TEXT NOT NULL,
    branch        TEXT NOT NULL,
    org_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    tip_sha       TEXT NOT NULL,
    etag          TEXT NULL,
    observed_at   DATETIME NOT NULL,
    compare_failing_since DATETIME NULL,
    PRIMARY KEY (repository_id, branch),
    FOREIGN KEY (repository_id, org_id) REFERENCES repositories(id, org_id) ON DELETE CASCADE
);

-- ===========================================================================
-- 6. Memory generation
-- ===========================================================================

-- The org's newest generated memory, which the memory provisioner's
-- failing-generation read measures every failure against.
CREATE INDEX idx_conversation_memory_attempts_generated
    ON conversation_memory_attempts (org_id, completed_at DESC) WHERE outcome = 'generated';

COMMIT;

PRAGMA foreign_keys = on;

-- +goose Down
SELECT 'down not supported';
