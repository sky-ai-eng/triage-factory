-- +goose NO TRANSACTION
-- +goose Up
-- An installation is identified by (org_id, github_host, installation_id).
--
-- GitHub numbers installations per deployment, so an id names one installation
-- only together with its host. The key was (org_id, installation_id), which let
-- an installation on one host be written over another host's row of the same
-- number, and every reach row and scope marker hung off that key, so the
-- other host's cached reach was then read as the new installation's.
--
-- Three tables change, and each is rebuilt because SQLite cannot alter a
-- primary key, a CHECK or a foreign key in place:
--
--   org_github_app_installations  github_host joins the primary key. Its
--       DEFAULT goes: a row names the host it was listed on, and nothing
--       stands in for one.
--   reachable_repositories        every row carries its host. An App row's is
--       its installation's, and the foreign key to the installation runs over
--       (org_id, host, installation_id), so a row can only ever hang off the
--       installation on its own host.
--   reachable_scopes              every marker carries its host, the PAT
--       tier's scope already being one. An App marker whose installation no
--       longer exists vouches for nothing and is not copied.
--
-- Foreign keys are off for the rebuild because dropping a parent table with
-- them on runs an implicit DELETE that would cascade into its reach rows.

PRAGMA foreign_keys = off;

CREATE TABLE org_github_app_installations_new (
    installation_id      TEXT NOT NULL,
    org_id               TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    account_type         TEXT NOT NULL CHECK (account_type IN ('Organization','User')),
    account_login        TEXT NOT NULL,
    installed_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    removed_at           TIMESTAMP,
    account_id           TEXT,
    suspended_at         TIMESTAMP,
    suspended_by         TEXT,
    github_host          TEXT NOT NULL CHECK (github_host <> ''),
    repository_selection TEXT
        CHECK (repository_selection IS NULL OR repository_selection IN ('all', 'selected')),
    PRIMARY KEY (org_id, github_host, installation_id)
);

INSERT INTO org_github_app_installations_new
    (installation_id, org_id, account_type, account_login, installed_at, removed_at,
     account_id, suspended_at, suspended_by, github_host, repository_selection)
SELECT installation_id, org_id, account_type, account_login, installed_at, removed_at,
       account_id, suspended_at, suspended_by, github_host, repository_selection
  FROM org_github_app_installations;

CREATE TABLE reachable_repositories_new (
    org_id           TEXT NOT NULL,
    credential_class TEXT NOT NULL
        CHECK (credential_class IN ('pat', 'byo_app', 'managed_app')),
    -- installation_id: the installation an App row was observed through, which
    -- with host is the scope one refresh replaces. A PAT row has none, and its
    -- host alone is its scope.
    installation_id  TEXT,
    host             TEXT NOT NULL CHECK (host <> ''),
    source           TEXT NOT NULL DEFAULT 'github',
    owner            TEXT NOT NULL,
    repo             TEXT NOT NULL,
    external_id      TEXT,
    description      TEXT NOT NULL DEFAULT '',
    language         TEXT NOT NULL DEFAULT '',
    html_url         TEXT NOT NULL DEFAULT '',
    pushed_at        TEXT NOT NULL DEFAULT '',
    private          INTEGER NOT NULL DEFAULT 0,
    observed_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK ((credential_class IN ('byo_app', 'managed_app') AND installation_id IS NOT NULL)
        OR (credential_class = 'pat' AND installation_id IS NULL)),
    FOREIGN KEY (org_id, host, installation_id)
        REFERENCES org_github_app_installations (org_id, github_host, installation_id)
        ON DELETE CASCADE
);

-- An App row takes its installation's host. Under the old key an org held one
-- row per installation id, so the lookup finds exactly one.
INSERT INTO reachable_repositories_new
    (org_id, credential_class, installation_id, host, source, owner, repo, external_id,
     description, language, html_url, pushed_at, private, observed_at)
SELECT r.org_id, r.credential_class, r.installation_id,
       COALESCE(r.host, (SELECT i.github_host FROM org_github_app_installations i
                          WHERE i.org_id = r.org_id AND i.installation_id = r.installation_id)),
       r.source, r.owner, r.repo, r.external_id,
       r.description, r.language, r.html_url, r.pushed_at, r.private, r.observed_at
  FROM reachable_repositories r
 WHERE r.host IS NOT NULL
    OR EXISTS (SELECT 1 FROM org_github_app_installations i
                WHERE i.org_id = r.org_id AND i.installation_id = r.installation_id);

CREATE TABLE reachable_scopes_new (
    org_id           TEXT NOT NULL,
    credential_class TEXT NOT NULL
        CHECK (credential_class IN ('pat', 'byo_app', 'managed_app')),
    -- host: the GitHub the scope is on. scope: what one refresh replaces on
    -- it, the installation id for the App classes and the host again for pat.
    host             TEXT NOT NULL CHECK (host <> ''),
    scope            TEXT NOT NULL,
    refreshed_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, credential_class, host, scope)
);

INSERT INTO reachable_scopes_new (org_id, credential_class, host, scope, refreshed_at)
SELECT org_id, credential_class, scope, scope, refreshed_at
  FROM reachable_scopes
 WHERE credential_class = 'pat';

INSERT INTO reachable_scopes_new (org_id, credential_class, host, scope, refreshed_at)
SELECT sc.org_id, sc.credential_class, i.github_host, sc.scope, sc.refreshed_at
  FROM reachable_scopes sc
  JOIN org_github_app_installations i
    ON i.org_id = sc.org_id AND i.installation_id = sc.scope
 WHERE sc.credential_class <> 'pat';

DROP TABLE reachable_scopes;
DROP TABLE reachable_repositories;
DROP TABLE org_github_app_installations;

PRAGMA foreign_keys = on;

ALTER TABLE org_github_app_installations_new RENAME TO org_github_app_installations;
ALTER TABLE reachable_repositories_new RENAME TO reachable_repositories;
ALTER TABLE reachable_scopes_new RENAME TO reachable_scopes;

CREATE UNIQUE INDEX org_github_app_installations_active_account_key
    ON org_github_app_installations (org_id, github_host, account_login)
    WHERE removed_at IS NULL;
CREATE UNIQUE INDEX org_github_app_installations_active_host_installation_key
    ON org_github_app_installations (github_host, installation_id)
    WHERE removed_at IS NULL;
CREATE INDEX org_github_app_installations_org_idx
    ON org_github_app_installations (org_id);

CREATE UNIQUE INDEX reachable_repositories_app_identity
    ON reachable_repositories (org_id, host, installation_id, LOWER(owner), LOWER(repo))
    WHERE credential_class IN ('byo_app', 'managed_app');
CREATE UNIQUE INDEX reachable_repositories_pat_identity
    ON reachable_repositories (org_id, host, LOWER(owner), LOWER(repo))
    WHERE credential_class = 'pat';
CREATE INDEX reachable_repositories_registry_join
    ON reachable_repositories (org_id, source, LOWER(owner), LOWER(repo));
CREATE INDEX reachable_repositories_picker
    ON reachable_repositories (org_id, credential_class, LOWER(owner), LOWER(repo));

-- +goose Down
SELECT 'down not supported';
