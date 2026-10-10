-- +goose Up
-- Store every GitHub host and Jira site in one spelling.
--
-- A host or site is compared by exact string everywhere it is a key: an
-- entity's scope, repositories.host, a team's GitHub group mapping, an App
-- installation's host, the reachable-repo cache, a user's GitHub or Jira
-- identity. The writers kept the case the user typed, so changing
-- https://GHE.acme.com to https://ghe.acme.com read as a move to another server.
-- The canonical form (ghbase.CanonicalBaseURL) lowercases the scheme and the
-- authority, keeps the path's case (a GHES or Jira Data Center context path may
-- be case-sensitive), and trims whitespace and trailing slashes. This rewrites
-- every stored value into it: the org's GitHub and Jira base URLs themselves,
-- and every column holding a host or site taken from them.
--
-- SQLite has no URL parser, so the form is computed with string functions, once
-- per distinct value, into host_case. lower() folds ASCII only, as the Go side
-- does. migration_host_case_test.go pins the two against each other.
--
-- Lowercasing can make two rows one key. What happens then depends on the
-- table:
--
--   user_github_identities, user_jira_identities
--       The most recently updated row for the user and site is kept and the
--       others are deleted; a tie keeps the row already in canonical form.
--   team_github_groups, reachable_repositories, reachable_scopes
--       The same, by created_at, observed_at and refreshed_at. The deleted
--       rows repeat a mapping or a cached observation the kept row holds.
--   repositories, entities, org_github_app_installations
--       Other rows reference these, so none is deleted. A row whose canonical
--       key another row already holds keeps its old spelling, which no read
--       asks for; the poller retires a GitHub entity left there as it retires
--       any entity on a host the org has left. Rows on the org's current
--       spelling are rewritten first, so of two non-canonical spellings the
--       one the org is on now takes the key.

CREATE TEMP TABLE host_case (
    raw       TEXT PRIMARY KEY,
    trimmed   TEXT,
    -- cut: the length of the prefix that folds, scheme "://" authority. 0 when
    -- the value has no "://" and so no authority to find.
    cut       INTEGER,
    canonical TEXT,
    -- preferred: the value is the spelling of an org's current base URL.
    preferred INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO host_case (raw)
SELECT base_url FROM org_event_sources WHERE kind IN ('github', 'jira') AND base_url IS NOT NULL
UNION SELECT host FROM repositories
UNION SELECT host FROM team_github_groups
UNION SELECT scope FROM entities WHERE source IN ('github', 'jira')
UNION SELECT github_base_url FROM user_github_identities
UNION SELECT jira_base_url FROM user_jira_identities
UNION SELECT github_host FROM org_github_app_installations
UNION SELECT host FROM reachable_repositories WHERE host IS NOT NULL
UNION SELECT scope FROM reachable_scopes WHERE credential_class = 'pat';

-- strings.TrimRight(strings.TrimSpace(raw), "/"), with TrimSpace's ASCII set.
UPDATE host_case SET trimmed = rtrim(trim(raw, char(9, 10, 11, 12, 13, 32)), '/');

UPDATE host_case
   SET cut = CASE
       WHEN instr(trimmed, '://') = 0 THEN 0
       WHEN instr(substr(trimmed, instr(trimmed, '://') + 3), '/') = 0 THEN length(trimmed)
       ELSE instr(trimmed, '://') + 1 + instr(substr(trimmed, instr(trimmed, '://') + 3), '/')
   END;

UPDATE host_case SET canonical = lower(substr(trimmed, 1, cut)) || substr(trimmed, cut + 1);

-- A value of only whitespace and slashes canonicalizes to nothing. No writer
-- stores one, and rewriting it to '' would trip the CHECKs that refuse an empty
-- host, so it is left as it is.
DELETE FROM host_case WHERE canonical = '';

UPDATE host_case
   SET preferred = 1
 WHERE raw IN (SELECT rtrim(trim(base_url, char(9, 10, 11, 12, 13, 32)), '/')
                 FROM org_event_sources
                WHERE kind IN ('github', 'jira') AND base_url IS NOT NULL);

-- ===========================================================================
-- 1. The settings
-- ===========================================================================
UPDATE org_event_sources
   SET base_url = (SELECT c.canonical FROM host_case c WHERE c.raw = org_event_sources.base_url)
 WHERE kind IN ('github', 'jira')
   AND base_url IN (SELECT raw FROM host_case WHERE canonical <> raw);

-- ===========================================================================
-- 2. Identities: keep the most recently updated row per (user, site)
-- ===========================================================================
DELETE FROM user_github_identities
 WHERE EXISTS (
       SELECT 1
         FROM user_github_identities o
         JOIN host_case oc ON oc.raw = o.github_base_url
         JOIN host_case rc ON rc.raw = user_github_identities.github_base_url
        WHERE o.user_id = user_github_identities.user_id
          AND o.github_base_url <> user_github_identities.github_base_url
          AND oc.canonical = rc.canonical
          AND (o.updated_at, oc.raw = oc.canonical, o.github_base_url)
            > (user_github_identities.updated_at, rc.raw = rc.canonical, user_github_identities.github_base_url));

UPDATE user_github_identities
   SET github_base_url = (SELECT c.canonical FROM host_case c WHERE c.raw = user_github_identities.github_base_url)
 WHERE github_base_url IN (SELECT raw FROM host_case WHERE canonical <> raw);

DELETE FROM user_jira_identities
 WHERE EXISTS (
       SELECT 1
         FROM user_jira_identities o
         JOIN host_case oc ON oc.raw = o.jira_base_url
         JOIN host_case rc ON rc.raw = user_jira_identities.jira_base_url
        WHERE o.user_id = user_jira_identities.user_id
          AND o.jira_base_url <> user_jira_identities.jira_base_url
          AND oc.canonical = rc.canonical
          AND (o.updated_at, oc.raw = oc.canonical, o.jira_base_url)
            > (user_jira_identities.updated_at, rc.raw = rc.canonical, user_jira_identities.jira_base_url));

UPDATE user_jira_identities
   SET jira_base_url = (SELECT c.canonical FROM host_case c WHERE c.raw = user_jira_identities.jira_base_url)
 WHERE jira_base_url IN (SELECT raw FROM host_case WHERE canonical <> raw);

-- ===========================================================================
-- 3. Team GitHub group mappings and the reachable-repo cache
-- ===========================================================================
DELETE FROM team_github_groups
 WHERE EXISTS (
       SELECT 1
         FROM team_github_groups o
         JOIN host_case oc ON oc.raw = o.host
         JOIN host_case rc ON rc.raw = team_github_groups.host
        WHERE o.team_id = team_github_groups.team_id
          AND o.github_org_login = team_github_groups.github_org_login
          AND o.github_team_slug = team_github_groups.github_team_slug
          AND o.host <> team_github_groups.host
          AND oc.canonical = rc.canonical
          AND (o.created_at, oc.raw = oc.canonical, o.host)
            > (team_github_groups.created_at, rc.raw = rc.canonical, team_github_groups.host));

UPDATE team_github_groups
   SET host = (SELECT c.canonical FROM host_case c WHERE c.raw = team_github_groups.host)
 WHERE host IN (SELECT raw FROM host_case WHERE canonical <> raw);

DELETE FROM reachable_repositories
 WHERE credential_class = 'pat'
   AND EXISTS (
       SELECT 1
         FROM reachable_repositories o
         JOIN host_case oc ON oc.raw = o.host
         JOIN host_case rc ON rc.raw = reachable_repositories.host
        WHERE o.credential_class = 'pat'
          AND o.org_id = reachable_repositories.org_id
          AND lower(o.owner) = lower(reachable_repositories.owner)
          AND lower(o.repo) = lower(reachable_repositories.repo)
          AND o.host <> reachable_repositories.host
          AND oc.canonical = rc.canonical
          AND (o.observed_at, oc.raw = oc.canonical, o.host)
            > (reachable_repositories.observed_at, rc.raw = rc.canonical, reachable_repositories.host));

UPDATE reachable_repositories
   SET host = (SELECT c.canonical FROM host_case c WHERE c.raw = reachable_repositories.host)
 WHERE credential_class = 'pat'
   AND host IN (SELECT raw FROM host_case WHERE canonical <> raw);

DELETE FROM reachable_scopes
 WHERE credential_class = 'pat'
   AND EXISTS (
       SELECT 1
         FROM reachable_scopes o
         JOIN host_case oc ON oc.raw = o.scope
         JOIN host_case rc ON rc.raw = reachable_scopes.scope
        WHERE o.credential_class = 'pat'
          AND o.org_id = reachable_scopes.org_id
          AND o.scope <> reachable_scopes.scope
          AND oc.canonical = rc.canonical
          AND (o.refreshed_at, oc.raw = oc.canonical, o.scope)
            > (reachable_scopes.refreshed_at, rc.raw = rc.canonical, reachable_scopes.scope));

UPDATE reachable_scopes
   SET scope = (SELECT c.canonical FROM host_case c WHERE c.raw = reachable_scopes.scope)
 WHERE credential_class = 'pat'
   AND scope IN (SELECT raw FROM host_case WHERE canonical <> raw);

-- ===========================================================================
-- 4. Rows other rows reference: rewritten where the key is free
-- ===========================================================================
UPDATE OR IGNORE repositories
   SET host = (SELECT c.canonical FROM host_case c WHERE c.raw = repositories.host)
 WHERE host IN (SELECT raw FROM host_case WHERE canonical <> raw AND preferred = 1);

UPDATE OR IGNORE repositories
   SET host = (SELECT c.canonical FROM host_case c WHERE c.raw = repositories.host)
 WHERE host IN (SELECT raw FROM host_case WHERE canonical <> raw);

UPDATE OR IGNORE entities
   SET scope = (SELECT c.canonical FROM host_case c WHERE c.raw = entities.scope)
 WHERE source IN ('github', 'jira')
   AND scope IN (SELECT raw FROM host_case WHERE canonical <> raw AND preferred = 1);

UPDATE OR IGNORE entities
   SET scope = (SELECT c.canonical FROM host_case c WHERE c.raw = entities.scope)
 WHERE source IN ('github', 'jira')
   AND scope IN (SELECT raw FROM host_case WHERE canonical <> raw);

UPDATE OR IGNORE org_github_app_installations
   SET github_host = (SELECT c.canonical FROM host_case c WHERE c.raw = org_github_app_installations.github_host)
 WHERE github_host IN (SELECT raw FROM host_case WHERE canonical <> raw AND preferred = 1);

UPDATE OR IGNORE org_github_app_installations
   SET github_host = (SELECT c.canonical FROM host_case c WHERE c.raw = org_github_app_installations.github_host)
 WHERE github_host IN (SELECT raw FROM host_case WHERE canonical <> raw);

DROP TABLE host_case;

-- +goose Down
SELECT 'down not supported';
