-- +goose Up
-- Scope repositories and GitHub team mappings by the GitHub host they live on.
--
-- A repository was identified by (org, source, owner, repo), with its provider
-- id beside it. Neither half is unique across GitHub deployments: an org that
-- points github_base_url at another host meets the same names again, and
-- repository ids are per-deployment sequences, so a new host's repository can
-- carry an old host's id. host joins both keys, so a GitHub object's identity is
-- (org, host, ...) and nothing matches across hosts. It is the same value GitHub
-- entities are scoped under (domain.EntityScope / domain.GitHubHost).
--
-- team_github_repos and conversation_worktrees reference a repository by row
-- id, so they take the scope from the row and get no column. team_github_groups
-- names a GitHub organization by login, which another host can reuse, so it
-- carries the host itself.
--
-- Every existing row takes the org's current GitHub host: a local install has
-- only ever had one GitHub at a time. The value is domain.GitHubHost's rule in
-- SQL — trailing slashes trimmed, and empty is the deployment default, which a
-- migration cannot read from TF_DEFAULT_GITHUB_HOST and which is a multi-mode
-- setting anyway, so it is https://github.com here.
-- migration_github_host_scope_test.go pins this against the Go canonicalizer.

-- ===========================================================================
-- 1. repositories.host
-- ===========================================================================
--
-- SQLite adds a NOT NULL column only with a default. The store never writes an
-- empty host (it refuses one), so the default only ever reaches the rows the
-- backfill below overwrites.
ALTER TABLE repositories ADD COLUMN host TEXT NOT NULL DEFAULT '';

UPDATE repositories
   SET host = COALESCE(
       NULLIF(rtrim((SELECT s.base_url FROM org_event_sources s
                      WHERE s.org_id = repositories.org_id AND s.kind = 'github'), '/'), ''),
       'https://github.com');

DROP INDEX repositories_identity;
CREATE UNIQUE INDEX repositories_identity
    ON repositories(org_id, source, host, LOWER(owner), LOWER(repo));

-- A provider id names one repository on its host. Nothing enforced that before,
-- so two rows may already share one; the id stays on the most recently updated
-- of them and the others lose it. A row without an id is a supported state: it
-- is not renamable until it learns one, and the poller and the profiler record
-- ids only where no other row on the host holds them.
UPDATE repositories
   SET external_id = NULL
 WHERE external_id IS NOT NULL
   AND EXISTS (
       SELECT 1 FROM repositories o
        WHERE o.org_id = repositories.org_id
          AND o.source = repositories.source
          AND o.host = repositories.host
          AND o.external_id = repositories.external_id
          AND (COALESCE(o.updated_at, '') > COALESCE(repositories.updated_at, '')
               OR (COALESCE(o.updated_at, '') = COALESCE(repositories.updated_at, '')
                   AND o.id > repositories.id)));

CREATE UNIQUE INDEX repositories_external_identity
    ON repositories(org_id, source, host, external_id)
    WHERE external_id IS NOT NULL;

-- ===========================================================================
-- 2. team_github_groups.host
-- ===========================================================================
--
-- Rebuilt because host joins the primary key. Nothing references this table, so
-- the drop cascades nowhere.
CREATE TABLE team_github_groups_new (
    team_id          TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    host             TEXT NOT NULL,
    github_org_login TEXT NOT NULL,
    github_team_slug TEXT NOT NULL,
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, host, github_org_login, github_team_slug),
    CONSTRAINT tgg_host_populated CHECK (host <> ''),
    CONSTRAINT tgg_org_login_populated CHECK (github_org_login <> ''),
    CONSTRAINT tgg_team_slug_populated CHECK (github_team_slug <> '')
);

INSERT INTO team_github_groups_new (team_id, host, github_org_login, github_team_slug, created_at)
SELECT g.team_id,
       COALESCE(
           NULLIF(rtrim((SELECT s.base_url FROM org_event_sources s
                          WHERE s.org_id = t.org_id AND s.kind = 'github'), '/'), ''),
           'https://github.com'),
       g.github_org_login, g.github_team_slug, g.created_at
  FROM team_github_groups g
  JOIN teams t ON t.id = g.team_id;

DROP TABLE team_github_groups;
ALTER TABLE team_github_groups_new RENAME TO team_github_groups;

-- +goose Down
SELECT 'down not supported';
