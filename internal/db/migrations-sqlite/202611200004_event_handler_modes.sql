-- +goose NO TRANSACTION
-- +goose Up
-- An event handler's kind and enabled flag become one mode: off, notify, watch
-- or automate. Every handler carries a name, a priority, a sort order, a
-- breaker threshold and an autonomy floor, and a blueprint only when it has
-- one, which it needs to automate.
--
-- The rows map as follows. "Live" is deleted_at IS NULL, and two predicates
-- are equal when both are NULL or json(a) = json(b).
--
--  1. Each team's six shipped rule and trigger pairs become one handler under
--     the new slug (eh_slugs below). With both live and equal predicates, the
--     trigger survives, because runs and queued firings reference its id: it
--     takes the rule's name, priority and sort order, its mode is automate if
--     it was enabled, else watch if the rule was, else off, it is
--     user_modified if either was, and the rule is deleted. With both live and
--     different predicates, both survive: the rule takes the slug, the trigger
--     loses its slug, and both are user_modified. With one live, it takes the
--     slug (a trigger also takes the rule's name, priority and sort order), a
--     rule left over is deleted and a trigger left over keeps deleted_at and
--     loses its slug. With neither live, the trigger takes the slug and keeps
--     deleted_at, so the sync does not bring back a handler the team deleted,
--     and the rule is deleted; a team with no trigger keeps the rule under
--     the slug for the same reason.
--  2. The four unpaired shipped rows take their new slugs.
--  3. Every other row takes a mode from its kind: a rule watch, a trigger
--     automate and a notify rule notify if it was enabled, and off if not. A
--     former trigger with no name takes its blueprint's, or its event type's
--     when the blueprint row is gone, and sorts after its team's rules in
--     created_at order; a former notify rule sorts after the triggers. A
--     missing priority is 0.5, breaker threshold 4 and autonomy floor 0.
--  4. A conflicts handler that was disabled and not user_modified makes a
--     task: watch, not off.
--
-- The boot-time sync then brings every unmodified shipped row to current
-- shipped content, so nothing here copies a shipped name or priority.
--
-- NO TRANSACTION because rebuilding event_handlers drops a table other tables
-- reference (pending_firings, conversations, blueprint_runs), and PRAGMA
-- foreign_keys is a no-op inside a transaction: with them on, dropping the
-- parent runs an implicit DELETE that cascades into every row referencing it.
-- The pragma is set first and the rebuild runs in one explicit transaction, so
-- a failure part way commits nothing and the next boot retries the whole file.
-- The pool is capped at one connection, so the pragma and the transaction hold
-- for every statement here.

PRAGMA foreign_keys = off;

BEGIN IMMEDIATE;

-- The rows as they are mapped. event_handlers is read for the rows as they
-- were until the rebuild at the end.
CREATE TABLE eh_stage (
    id                       TEXT PRIMARY KEY,
    org_id                   TEXT,
    creator_user_id          TEXT,
    team_id                  TEXT,
    kind                     TEXT,
    event_type               TEXT,
    scope_predicate_json     TEXT,
    enabled                  BOOLEAN,
    source                   TEXT,
    system_slug              TEXT,
    name                     TEXT,
    default_priority         REAL,
    sort_order               INTEGER,
    blueprint_id             TEXT,
    breaker_threshold        INTEGER,
    min_autonomy_suitability REAL,
    created_at               DATETIME,
    updated_at               DATETIME,
    applies_to_unowned       BOOLEAN,
    user_modified            INTEGER,
    deleted_at               DATETIME,
    mode                     TEXT
);

INSERT INTO eh_stage (
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

-- Step 1: the shipped pairs.
CREATE TABLE eh_slugs (new_slug TEXT PRIMARY KEY, r_slug TEXT NOT NULL, t_slug TEXT NOT NULL);
INSERT INTO eh_slugs (new_slug, r_slug, t_slug) VALUES
    ('system-handler-ci-check-failed',            'system-rule-ci-check-failed',            'system-trigger-ci-fix'),
    ('system-handler-review-changes-requested',   'system-rule-review-changes-requested',   'system-trigger-fix-review-feedback'),
    ('system-handler-review-requested',           'system-rule-review-requested',           'system-trigger-pr-review'),
    ('system-handler-jira-assigned',              'system-rule-jira-assigned',              'system-trigger-jira-implement'),
    ('system-handler-linear-assigned',            'system-rule-linear-assigned',            'system-trigger-linear-implement'),
    ('system-handler-vulnerability-alert-opened', 'system-rule-vulnerability-alert-opened', 'system-trigger-vulnerability-remediate');

-- One row per team and pair: the rule's and the trigger's ids, either NULL
-- when the team has no such row, and whether each is live.
CREATE TABLE eh_pairs (
    new_slug  TEXT NOT NULL,
    team_id   TEXT NOT NULL,
    r_id      TEXT,
    t_id      TEXT,
    r_live    INTEGER NOT NULL DEFAULT 0,
    t_live    INTEGER NOT NULL DEFAULT 0,
    same_pred INTEGER NOT NULL DEFAULT 0
);

INSERT INTO eh_pairs (new_slug, team_id, r_id, t_id)
SELECT p.new_slug, h.team_id,
       MAX(CASE WHEN h.system_slug = p.r_slug THEN h.id END),
       MAX(CASE WHEN h.system_slug = p.t_slug THEN h.id END)
FROM eh_slugs p
JOIN event_handlers h ON h.system_slug IN (p.r_slug, p.t_slug)
GROUP BY p.new_slug, h.team_id;

UPDATE eh_pairs SET
    r_live = EXISTS (SELECT 1 FROM event_handlers h WHERE h.id = eh_pairs.r_id AND h.deleted_at IS NULL),
    t_live = EXISTS (SELECT 1 FROM event_handlers h WHERE h.id = eh_pairs.t_id AND h.deleted_at IS NULL);

UPDATE eh_pairs SET same_pred = COALESCE((
    SELECT (r.scope_predicate_json IS NULL AND t.scope_predicate_json IS NULL)
           OR json(r.scope_predicate_json) = json(t.scope_predicate_json)
    FROM event_handlers r, event_handlers t
    WHERE r.id = eh_pairs.r_id AND t.id = eh_pairs.t_id), 0)
WHERE r_live AND t_live;

-- Both live, equal predicates: the trigger is the handler.
UPDATE eh_stage SET
    system_slug      = p.new_slug,
    name             = r.name,
    default_priority = r.default_priority,
    sort_order       = r.sort_order,
    mode             = CASE WHEN eh_stage.enabled THEN 'automate' WHEN r.enabled THEN 'watch' ELSE 'off' END,
    user_modified    = (eh_stage.user_modified OR r.user_modified)
FROM eh_pairs p
JOIN event_handlers r ON r.id = p.r_id
WHERE eh_stage.id = p.t_id AND p.r_live AND p.t_live AND p.same_pred;

DELETE FROM eh_stage WHERE id IN (SELECT r_id FROM eh_pairs WHERE r_live AND t_live AND same_pred);

-- Both live, different predicates: both stay, and step 3 gives the trigger
-- its mode, name and place.
UPDATE eh_stage SET system_slug = p.new_slug, user_modified = 1
FROM eh_pairs p
WHERE eh_stage.id = p.r_id AND p.r_live AND p.t_live AND NOT p.same_pred;

UPDATE eh_stage SET system_slug = NULL, user_modified = 1
FROM eh_pairs p
WHERE eh_stage.id = p.t_id AND p.r_live AND p.t_live AND NOT p.same_pred;

-- Only the rule live.
UPDATE eh_stage SET system_slug = p.new_slug
FROM eh_pairs p
WHERE eh_stage.id = p.r_id AND p.r_live AND NOT p.t_live;

UPDATE eh_stage SET system_slug = NULL
FROM eh_pairs p
WHERE eh_stage.id = p.t_id AND p.r_live AND NOT p.t_live;

-- Only the trigger live.
UPDATE eh_stage SET system_slug = p.new_slug
FROM eh_pairs p
WHERE eh_stage.id = p.t_id AND p.t_live AND NOT p.r_live;

UPDATE eh_stage SET
    name             = r.name,
    default_priority = r.default_priority,
    sort_order       = r.sort_order
FROM eh_pairs p
JOIN event_handlers r ON r.id = p.r_id
WHERE eh_stage.id = p.t_id AND p.t_live AND NOT p.r_live;

DELETE FROM eh_stage WHERE id IN (SELECT r_id FROM eh_pairs WHERE t_live AND NOT r_live AND r_id IS NOT NULL);

-- Neither live: the deleted trigger holds the slug, or the deleted rule when
-- the team has no trigger.
UPDATE eh_stage SET system_slug = p.new_slug
FROM eh_pairs p
WHERE eh_stage.id = p.t_id AND NOT p.r_live AND NOT p.t_live;

UPDATE eh_stage SET system_slug = p.new_slug
FROM eh_pairs p
WHERE eh_stage.id = p.r_id AND p.t_id IS NULL AND NOT p.r_live AND NOT p.t_live;

DELETE FROM eh_stage WHERE id IN (SELECT r_id FROM eh_pairs WHERE t_id IS NOT NULL AND NOT r_live AND NOT t_live AND r_id IS NOT NULL);

-- Step 2: the unpaired shipped rows.
UPDATE eh_stage SET system_slug = 'system-handler-review-commented'     WHERE system_slug = 'system-rule-review-commented';
UPDATE eh_stage SET system_slug = 'system-handler-jira-became-atomic'   WHERE system_slug = 'system-rule-jira-became-atomic';
UPDATE eh_stage SET system_slug = 'system-handler-linear-became-atomic' WHERE system_slug = 'system-rule-linear-became-atomic';
UPDATE eh_stage SET system_slug = 'system-handler-pr-conflicts'         WHERE system_slug = 'system-trigger-conflict-resolution';

-- Step 3: every other row.
UPDATE eh_stage SET mode = CASE kind
        WHEN 'rule'    THEN CASE WHEN enabled THEN 'watch'    ELSE 'off' END
        WHEN 'trigger' THEN CASE WHEN enabled THEN 'automate' ELSE 'off' END
        ELSE                CASE WHEN enabled THEN 'notify'   ELSE 'off' END
    END
WHERE mode IS NULL;

UPDATE eh_stage SET name = COALESCE((SELECT b.name FROM blueprints b WHERE b.id = eh_stage.blueprint_id), event_type)
WHERE name IS NULL;

UPDATE eh_stage SET default_priority = 0.5 WHERE default_priority IS NULL;
UPDATE eh_stage SET breaker_threshold = 4 WHERE breaker_threshold IS NULL;
UPDATE eh_stage SET min_autonomy_suitability = 0 WHERE min_autonomy_suitability IS NULL;

-- A former trigger sorts after every row of its team that has a place, which
-- is every former rule and every trigger that took a rule's place above.
CREATE TABLE eh_places (id TEXT PRIMARY KEY, sort_order INTEGER NOT NULL);

INSERT INTO eh_places (id, sort_order)
SELECT s.id,
       COALESCE((SELECT MAX(x.sort_order) FROM eh_stage x WHERE x.team_id = s.team_id), -1)
       + ROW_NUMBER() OVER (PARTITION BY s.team_id ORDER BY s.created_at, s.id)
FROM eh_stage s
WHERE s.kind = 'trigger' AND s.sort_order IS NULL;

UPDATE eh_stage SET sort_order = (SELECT p.sort_order FROM eh_places p WHERE p.id = eh_stage.id)
WHERE id IN (SELECT id FROM eh_places);

DELETE FROM eh_places;

INSERT INTO eh_places (id, sort_order)
SELECT s.id,
       COALESCE((SELECT MAX(x.sort_order) FROM eh_stage x WHERE x.team_id = s.team_id), -1)
       + ROW_NUMBER() OVER (PARTITION BY s.team_id ORDER BY s.created_at, s.id)
FROM eh_stage s
WHERE s.kind = 'notify' AND s.sort_order IS NULL;

UPDATE eh_stage SET sort_order = (SELECT p.sort_order FROM eh_places p WHERE p.id = eh_stage.id)
WHERE id IN (SELECT id FROM eh_places);

-- Step 4: merge conflicts make a task.
UPDATE eh_stage SET mode = 'watch'
WHERE system_slug = 'system-handler-pr-conflicts' AND NOT enabled AND NOT user_modified;

CREATE TABLE event_handlers_new (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    creator_user_id TEXT,
    -- team_id is NOT NULL: every handler is team-owned. The LocalDefaultTeamID
    -- sentinel is the DEFAULT so local-mode inserts and direct INSERTs that
    -- omit it land on the sole team.
    team_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000010',

    -- What a matching event does: off nothing, notify tells the team's
    -- members, watch makes a task, automate makes a task and runs the
    -- handler's blueprint on it.
    mode            TEXT NOT NULL CHECK (mode IN ('off', 'notify', 'watch', 'automate')),

    event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,
    -- source is app-validated, not CHECK-constrained: 'system' | 'user' today,
    -- and a new provenance value must be addable without a table rebuild.
    source               TEXT NOT NULL DEFAULT 'user',
    -- The stable shipped identifier for source='system' rows; NULL for user
    -- handlers. Re-seed idempotency keys on (org_id, team_id, system_slug).
    system_slug          TEXT,

    name                     TEXT NOT NULL,
    default_priority         REAL NOT NULL DEFAULT 0.5,
    sort_order               INTEGER NOT NULL DEFAULT 0,
    blueprint_id             TEXT,
    breaker_threshold        INTEGER NOT NULL DEFAULT 4,
    min_autonomy_suitability REAL NOT NULL DEFAULT 0,

    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    applies_to_unowned BOOLEAN NOT NULL DEFAULT 0,
    user_modified INTEGER NOT NULL DEFAULT 0,
    deleted_at DATETIME,

    FOREIGN KEY (blueprint_id, org_id) REFERENCES blueprints (id, org_id) ON DELETE CASCADE,
    -- Same-team guard: a handler may only reference a blueprint its own team
    -- owns.
    FOREIGN KEY (blueprint_id, team_id) REFERENCES blueprints (id, team_id) ON DELETE CASCADE,

    -- A handler runs a blueprint only in automate, and keeps one it has in
    -- every other mode.
    CHECK (mode <> 'automate' OR blueprint_id IS NOT NULL),
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
    id, org_id, creator_user_id, team_id, mode, event_type, scope_predicate_json,
    source, system_slug, name, default_priority, sort_order,
    blueprint_id, breaker_threshold, min_autonomy_suitability,
    created_at, updated_at, applies_to_unowned, user_modified, deleted_at)
SELECT
    id, org_id, creator_user_id, team_id, mode, event_type, scope_predicate_json,
    source, system_slug, name, default_priority, sort_order,
    blueprint_id, breaker_threshold, min_autonomy_suitability,
    created_at, updated_at, applies_to_unowned, user_modified, deleted_at
FROM eh_stage;

DROP TABLE eh_places;
DROP TABLE eh_pairs;
DROP TABLE eh_slugs;
DROP TABLE eh_stage;
DROP TABLE event_handlers;
ALTER TABLE event_handlers_new RENAME TO event_handlers;

CREATE UNIQUE INDEX event_handlers_id_org_unique         ON event_handlers (id, org_id);
CREATE INDEX        idx_event_handlers_event_type_active ON event_handlers(org_id, event_type) WHERE mode <> 'off';
CREATE INDEX        idx_event_handlers_mode              ON event_handlers(org_id, mode);
CREATE INDEX        idx_event_handlers_blueprint         ON event_handlers(org_id, blueprint_id) WHERE blueprint_id IS NOT NULL;

COMMIT;

PRAGMA foreign_keys = on;

-- +goose Down
SELECT 'down not supported';
