-- +goose NO TRANSACTION
-- +goose Up
-- Give every entity a scope and room for the provider's own id.
--
-- An entity was identified by (source, source_id) alone. source_id is a key a
-- provider lets change (a Jira project move, a Linear team move or team key
-- rename) and repeats across providers' namespaces (two Jira sites, two Linear
-- workspaces), so it identified an object only by coincidence. Two columns fix
-- that, on the repository model:
--
--   scope        the namespace a key and an id are unique within: the GitHub
--                host, the Jira site, the Linear workspace id, the Slack
--                workspace id.
--   external_id  the provider's id for the object, which a key change does not
--                move. NULL is "not learned yet".
--
-- source_id stays the key everything reads and displays. A key change becomes a
-- rename of the row that carries the id, rather than a second row.
--
-- The UNIQUE(source, source_id) constraint goes with it. Keys are unique among
-- ACTIVE rows of one scope only: a key a provider frees and hands to a new
-- object (a project deleted and recreated, a team key reused) must not collide
-- with the closed history it used to name. The constraint is part of the table
-- definition, so the table is rebuilt; foreign keys are off for the rebuild
-- because dropping a parent table with them on runs an implicit DELETE that
-- would cascade into every task, event and memory row.

PRAGMA foreign_keys = off;

CREATE TABLE entities_new (
    id                       TEXT PRIMARY KEY,
    source                   TEXT NOT NULL,
    scope                    TEXT NOT NULL,
    source_id                TEXT NOT NULL,
    external_id              TEXT,
    kind                     TEXT NOT NULL,
    title                    TEXT,
    url                      TEXT,
    snapshot_json            TEXT,
    description              TEXT NOT NULL DEFAULT '',
    state                    TEXT NOT NULL DEFAULT 'active',
    created_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_polled_at           DATETIME,
    closed_at                DATETIME,
    owning_team_id           TEXT REFERENCES teams(id) ON DELETE SET NULL,
    org_id                   TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    poll_seq                 INTEGER NOT NULL DEFAULT 0,
    commissioned_by_user_id  TEXT REFERENCES users(id) ON DELETE SET NULL
);

-- The scope each existing row was written under, from data already stored. Each
-- arm reproduces the Go canonicalizer the source's scope comes from
-- (domain.EntityScope), and migration_entity_scope_test.go pins the two
-- against each other.
--
--   github  the org's GitHub base URL with domain.GitHubHost's two rules:
--           trailing slashes trimmed, and empty is the deployment default.
--           The default here is https://github.com: TF_DEFAULT_GITHUB_HOST is
--           an environment variable a migration cannot read, and it is a
--           multi-mode deployment setting, where this file never runs.
--   jira    the row's own url up to '/browse/', which is the base URL the
--           tracker built it from — the site the issue actually came from,
--           even if the org has since been pointed at another one. A stub with
--           no such url falls back to the org's current Jira base URL with
--           domain.JiraHost's trim and its http(s) check. A stub with neither
--           names no site, and its scope is empty, which no lookup asks for.
--
-- Slack rows cannot exist here (Slack is multi-only) and Linear rows have
-- never shipped, so neither has an arm.
INSERT INTO entities_new (
    id, source, scope, source_id, external_id, kind, title, url, snapshot_json,
    description, state, created_at, last_polled_at, closed_at, owning_team_id,
    org_id, poll_seq, commissioned_by_user_id
)
SELECT
    e.id, e.source,
    CASE e.source
        WHEN 'github' THEN COALESCE(
            NULLIF(rtrim((SELECT s.base_url FROM org_event_sources s
                           WHERE s.org_id = e.org_id AND s.kind = 'github'), '/'), ''),
            'https://github.com')
        WHEN 'jira' THEN COALESCE(
            CASE WHEN instr(COALESCE(e.url, ''), '/browse/') > 1
                 THEN substr(e.url, 1, instr(e.url, '/browse/') - 1) END,
            (SELECT rtrim(trim(s.base_url), '/') FROM org_event_sources s
              WHERE s.org_id = e.org_id AND s.kind = 'jira'
                AND (lower(trim(s.base_url)) LIKE 'http://_%'
                     OR lower(trim(s.base_url)) LIKE 'https://_%')),
            '')
        ELSE ''
    END,
    e.source_id, NULL, e.kind, e.title, e.url, e.snapshot_json,
    e.description, e.state, e.created_at, e.last_polled_at, e.closed_at, e.owning_team_id,
    e.org_id, e.poll_seq, e.commissioned_by_user_id
FROM entities e;

DROP TABLE entities;

PRAGMA foreign_keys = on;

ALTER TABLE entities_new RENAME TO entities;

CREATE INDEX        idx_entities_state           ON entities(state);
CREATE INDEX        idx_entities_source_polled   ON entities(source, last_polled_at);
CREATE INDEX        idx_entities_closed_at       ON entities(closed_at) WHERE closed_at IS NOT NULL;
CREATE UNIQUE INDEX entities_id_org_unique       ON entities (id, org_id);
CREATE INDEX        idx_entities_github_author
    ON entities (json_extract(snapshot_json, '$.author'))
    WHERE source = 'github' AND snapshot_json IS NOT NULL AND snapshot_json != ''
      AND json_valid(snapshot_json);
CREATE INDEX        idx_entities_commissioned_by
    ON entities (commissioned_by_user_id)
    WHERE commissioned_by_user_id IS NOT NULL;

-- An entity's identity: one row per provider id in a scope.
CREATE UNIQUE INDEX entities_identity
    ON entities (org_id, source, scope, external_id)
    WHERE external_id IS NOT NULL;

-- A key names one live object in a scope; closed rows may share it.
CREATE UNIQUE INDEX entities_active_key
    ON entities (org_id, source, scope, source_id)
    WHERE state = 'active';

-- The key lookup, which reads the active row or else the most recently closed.
CREATE INDEX        idx_entities_key
    ON entities (org_id, source, scope, source_id);

-- +goose Down
SELECT 'down not supported';
