-- +goose NO TRANSACTION
-- +goose Up
-- A notify rule tells its team's members, so event_handlers loses
-- notify_audience and the two CHECKs that read it. A rule that told its
-- creator becomes one that tells its team by losing the column; in local mode
-- the one user is told either way. Every other column of every row is copied
-- unchanged, and the notifications rules already raised are not touched.
--
-- NO TRANSACTION because dropping a column a CHECK names rebuilds a table
-- other tables reference (pending_firings, conversations, blueprint_runs), and
-- PRAGMA foreign_keys is a no-op inside a transaction: with them on, dropping
-- the parent runs an implicit DELETE that cascades into every row referencing
-- it. The pragma is set first and the rebuild runs in one explicit
-- transaction, so a failure part way commits nothing and the next boot retries
-- the whole file. The pool is capped at one connection, so the pragma and the
-- transaction hold for every statement here.

PRAGMA foreign_keys = off;

BEGIN IMMEDIATE;

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
    -- A notify rule is user-authored, never shipped content, and tells its
    -- team's members about an event without touching tasks.
    CHECK (kind <> 'notify' OR (
        blueprint_id IS NULL
        AND breaker_threshold IS NULL
        AND min_autonomy_suitability IS NULL
        AND default_priority IS NULL
        AND sort_order IS NULL
        AND name IS NOT NULL
        AND source <> 'system'
        AND NOT applies_to_unowned
    )),
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

COMMIT;

PRAGMA foreign_keys = on;

-- +goose Down
SELECT 'down not supported';
