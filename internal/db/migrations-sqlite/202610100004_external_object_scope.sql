-- +goose Up
-- Record which provider namespace every artifact and audit-ledger row is about,
-- and which GitHub host every repository placement pin is for.
--
-- artifacts and external_actions were deduped on (org_id, dedup_key). A dedup
-- key and a target are only unique inside a provider namespace: the same
-- owner/repo#18 exists on two GitHub hosts, and two Jira sites or Linear
-- workspaces repeat keys. scope names that namespace, with exactly the value an
-- entity of the provider is scoped under (domain.ExternalObjectScope), and joins
-- the dedup key: (org_id, scope, dedup_key).
--
-- placement_overrides keys a repo pin on its owner/repo, which is likewise only
-- unique within one GitHub host, so host joins the primary key.
--
-- Every existing row takes the org's current scope for its provider: a local
-- install has only ever had one GitHub, one Jira site and one Linear workspace
-- at a time. The values are domain.ExternalObjectScope's rules in SQL:
--
--   github, git  the org's GitHub base URL in ghbase.CanonicalBaseURL form
--                (surrounding whitespace and trailing slashes trimmed, scheme
--                and authority lowercased, the path's case kept), and empty is
--                the deployment default. The default here is https://github.com:
--                TF_DEFAULT_GITHUB_HOST is an environment variable a migration
--                cannot read, and it is a multi-mode setting, where this file
--                never runs.
--   jira         the org's Jira base URL in the same canonical form, when it is
--                an http(s) URL with an authority (domain.JiraHost); empty when
--                no such URL is set, which no read asks for.
--   linear       the org's Linear workspace id; empty while none is bound.
--   slack        domain.SlackScope.
--   network      domain.NetworkScope.
--
-- A placement pin takes the org's GitHub host: repo is the only key kind TF
-- writes. migration_external_object_scope_test.go pins all of this against the
-- Go side.

-- ===========================================================================
-- 1. Each org's current scope per provider
-- ===========================================================================
CREATE TEMP TABLE external_object_scope_backfill (
    org_id   TEXT NOT NULL,
    provider TEXT NOT NULL,
    scope    TEXT NOT NULL,
    PRIMARY KEY (org_id, provider)
);

INSERT INTO external_object_scope_backfill (org_id, provider, scope)
WITH orgs_in_use(org_id) AS (
    SELECT org_id FROM artifacts
    UNION SELECT org_id FROM external_actions
    UNION SELECT org_id FROM placement_overrides
),
trimmed(org_id, kind, t) AS (
    SELECT u.org_id, k.kind,
           rtrim(trim(COALESCE((SELECT s.base_url FROM org_event_sources s
                                 WHERE s.org_id = u.org_id AND s.kind = k.kind), ''),
                      ' ' || char(9, 10, 11, 12, 13)), '/')
      FROM orgs_in_use u
     CROSS JOIN (SELECT 'github' AS kind UNION ALL SELECT 'jira') k
),
split(org_id, kind, t, sep, rest) AS (
    SELECT org_id, kind, t, instr(t, '://'), substr(t, instr(t, '://') + 3)
      FROM trimmed
),
authority(org_id, kind, t, sep, auth_len) AS (
    SELECT org_id, kind, t, sep,
           CASE WHEN instr(rest, '/') = 0 THEN length(rest) ELSE instr(rest, '/') - 1 END
      FROM split
),
canonical(org_id, kind, base, auth_len) AS (
    SELECT org_id, kind,
           CASE WHEN sep = 0 THEN t
                ELSE lower(substr(t, 1, sep + 2 + auth_len)) || substr(t, sep + 3 + auth_len)
           END,
           CASE WHEN sep = 0 THEN 0 ELSE auth_len END
      FROM authority
),
github(org_id, scope) AS (
    SELECT org_id, CASE WHEN base = '' THEN 'https://github.com' ELSE base END
      FROM canonical WHERE kind = 'github'
),
jira(org_id, scope) AS (
    SELECT org_id,
           CASE WHEN auth_len > 0 AND (base LIKE 'http://%' OR base LIKE 'https://%')
                THEN base ELSE '' END
      FROM canonical WHERE kind = 'jira'
)
SELECT org_id, 'github', scope FROM github
UNION ALL SELECT org_id, 'git', scope FROM github
UNION ALL SELECT org_id, 'jira', scope FROM jira
UNION ALL
SELECT u.org_id, 'linear',
       COALESCE((SELECT o.linear_workspace_id FROM org_settings o WHERE o.org_id = u.org_id), '')
  FROM orgs_in_use u
UNION ALL SELECT org_id, 'slack', 'slack.com' FROM orgs_in_use
UNION ALL SELECT org_id, 'network', 'internet' FROM orgs_in_use;

-- ===========================================================================
-- 2. artifacts.scope and external_actions.scope
-- ===========================================================================
--
-- SQLite adds a NOT NULL column only with a default. The stores never write an
-- empty scope (they refuse one), so the default only reaches the rows the
-- backfill below overwrites, and a provider this install has no scope for.
ALTER TABLE artifacts ADD COLUMN scope TEXT NOT NULL DEFAULT '';
ALTER TABLE external_actions ADD COLUMN scope TEXT NOT NULL DEFAULT '';

UPDATE artifacts
   SET scope = COALESCE((SELECT b.scope FROM external_object_scope_backfill b
                          WHERE b.org_id = artifacts.org_id AND b.provider = artifacts.provider), '');

UPDATE external_actions
   SET scope = COALESCE((SELECT b.scope FROM external_object_scope_backfill b
                          WHERE b.org_id = external_actions.org_id AND b.provider = external_actions.provider), '');

-- Adding a column to a unique key only relaxes it, so neither rebuild can fail
-- on rows the old index already held apart.
DROP INDEX idx_artifacts_dedup;
CREATE UNIQUE INDEX idx_artifacts_dedup ON artifacts (org_id, scope, dedup_key);

DROP INDEX idx_external_actions_dedup;
CREATE UNIQUE INDEX idx_external_actions_dedup ON external_actions (org_id, scope, dedup_key);

-- ===========================================================================
-- 3. placement_overrides.host
-- ===========================================================================
--
-- Rebuilt because host joins the primary key. Nothing references this table,
-- so the drop cascades nowhere.
CREATE TABLE placement_overrides_new (
    org_id             TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    key_kind           TEXT NOT NULL,
    host               TEXT NOT NULL,
    key_value          TEXT NOT NULL,
    pinned_instance_id TEXT,
    replicas           INTEGER NOT NULL DEFAULT 0,
    updated_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, key_kind, host, key_value)
);

INSERT INTO placement_overrides_new
    (org_id, key_kind, host, key_value, pinned_instance_id, replicas, updated_at)
SELECT p.org_id, p.key_kind,
       (SELECT b.scope FROM external_object_scope_backfill b
         WHERE b.org_id = p.org_id AND b.provider = 'github'),
       p.key_value, p.pinned_instance_id, p.replicas, p.updated_at
  FROM placement_overrides p;

DROP TABLE placement_overrides;
ALTER TABLE placement_overrides_new RENAME TO placement_overrides;

DROP TABLE external_object_scope_backfill;

-- +goose Down
SELECT 'down not supported';
