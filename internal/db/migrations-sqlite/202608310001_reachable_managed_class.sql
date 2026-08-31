-- +goose Up
-- The reachable-repo mirror learns 'managed_app': a workspace riding the
-- deployment's shared GitHub App keys its reach exactly as a BYO workspace does
-- — per installation — and until now these two tables refused the value.
--
-- Three constraints stand in the way, and widening only the obvious one is
-- worse than doing nothing:
--
--   1. The class CHECK on each table, which names the accepted values.
--   2. The scope CHECK on reachable_repositories, which pairs each class with
--      the scope column its rows must carry. managed_app joins the App arm, and
--      nothing is restructured to admit it: a managed org's reach is enumerated
--      per installation, that installation lives in the same
--      org_github_app_installations table the foreign key already points at,
--      and an uninstall takes its entries through the same cascade. PAT is the
--      odd tier — no installation to hang from — not managed.
--   3. reachable_repositories_app_identity, the App tier's uniqueness index,
--      which is PARTIAL on a literal class. This is the half that would have
--      failed silently: a managed_app row falls outside both partial indexes
--      and would carry no uniqueness constraint at all, while the writer's
--      ON CONFLICT DO NOTHING needs an index to have a conflict to detect and
--      degrades to a plain insert without one. GitHub's paginated listings
--      legitimately return the same repository twice when repos are created
--      mid-walk, so the result would be duplicate rows in the picker and a
--      wrong total_count — COUNT(*) being answerable is the entire reason this
--      table exists.
--
-- The app_identity predicate is WIDENED rather than joined by a third index.
-- The key already contains installation_id, an installation belongs to exactly
-- one App, and an org holds one class at a time, so a cross-class collision on
-- the same installation id is not constructible — and one index is strictly
-- stronger than two.
--
-- Every change here admits values the previous constraints refused, so no
-- stored row can violate the new ones and nothing is rewritten or cleaned up.
-- SQLite cannot ALTER a CHECK or re-predicate an index in place, so both tables
-- are rebuilt and their rows copied verbatim. Neither is a parent of anything,
-- so no child foreign key has to be toggled; reachable_repositories' own key
-- into org_github_app_installations is preserved in the new definition.

CREATE TABLE reachable_repositories_new (
    org_id           TEXT NOT NULL,
    -- Which credential system observed this reach. Part of the key rather than
    -- a derived attribute because an org that switches tiers has, for a moment,
    -- both answers on file, and serving the union of them would be serving a
    -- reach the org no longer has.
    credential_class TEXT NOT NULL
        CHECK (credential_class IN ('pat', 'byo_app', 'managed_app')),
    -- installation_id / host: the credential INSTANCE the reach was observed
    -- through — the scope one refresh replaces atomically. Both App classes
    -- hang off the installation, which is how they inherit host scoping and how
    -- an uninstall takes their entries with it; the PAT tier has no
    -- installation row to hang off, so it carries the host directly.
    installation_id  TEXT,
    host             TEXT,
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
    -- Exactly one scope column per class, and it is the database that says so.
    -- The two are alternatives, never both and never neither: a row with both
    -- would have two answers to "what does one refresh replace", and a row with
    -- neither could not be replaced at all.
    CHECK ((credential_class IN ('byo_app', 'managed_app') AND installation_id IS NOT NULL AND host IS NULL)
        OR (credential_class = 'pat' AND installation_id IS NULL AND host IS NOT NULL)),
    FOREIGN KEY (org_id, installation_id)
        REFERENCES org_github_app_installations (org_id, installation_id)
        ON DELETE CASCADE
);

INSERT INTO reachable_repositories_new
    (org_id, credential_class, installation_id, host, source, owner, repo, external_id,
     description, language, html_url, pushed_at, private, observed_at)
SELECT org_id, credential_class, installation_id, host, source, owner, repo, external_id,
       description, language, html_url, pushed_at, private, observed_at
FROM reachable_repositories;

DROP TABLE reachable_repositories;
ALTER TABLE reachable_repositories_new RENAME TO reachable_repositories;

-- The natural key, per scope shape, folded. Folded because GitHub identifiers
-- are case-insensitive, so a case-sensitive index behind a case-insensitive
-- guard admits duplicates whenever two writers race — neither sees the other's
-- uncommitted row and their keys then differ. The database has to be the thing
-- that refuses.
--
-- Two partial indexes rather than one over COALESCE(installation_id, host): the
-- scope columns are genuinely different columns with different nullability, and
-- a coalesced key would silently collide an installation whose id equals another
-- org's host string.
CREATE UNIQUE INDEX reachable_repositories_app_identity
    ON reachable_repositories (org_id, installation_id, LOWER(owner), LOWER(repo))
    WHERE credential_class IN ('byo_app', 'managed_app');

CREATE UNIQUE INDEX reachable_repositories_pat_identity
    ON reachable_repositories (org_id, host, LOWER(owner), LOWER(repo))
    WHERE credential_class = 'pat';

-- The registry join, and the drift queries that run over it. Deliberately the
-- same shape as repo_profiles_identity — (org_id, source, LOWER(owner),
-- LOWER(repo)) — so the two sides of "is this reachable repository tracked?" are
-- keyed identically and either can drive the join.
CREATE INDEX reachable_repositories_registry_join
    ON reachable_repositories (org_id, source, LOWER(owner), LOWER(repo));

-- The picker's own read: one org's current class, ordered by folded slug. The
-- order is part of the index because offset paging over an unordered set drops
-- and repeats rows, and the folded slug is a total order (the per-scope unique
-- index above proves no two rows tie on it).
CREATE INDEX reachable_repositories_picker
    ON reachable_repositories (org_id, credential_class, LOWER(owner), LOWER(repo));

-- The refresh markers, one row per scope: what makes "we have looked and found
-- nothing" distinguishable from "we have not looked", which no count of
-- repository rows can express. A managed org stores its installation id here
-- exactly as a BYO org does, so only the class CHECK changes.
CREATE TABLE reachable_scopes_new (
    org_id           TEXT NOT NULL,
    credential_class TEXT NOT NULL
        CHECK (credential_class IN ('pat', 'byo_app', 'managed_app')),
    -- scope: the credential instance one refresh replaces — the installation id
    -- for the App classes, the host for pat. Recorded as opaque text: this table
    -- records THAT a scope was refreshed and when, and never joins on it.
    scope            TEXT NOT NULL,
    refreshed_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, credential_class, scope)
);

INSERT INTO reachable_scopes_new (org_id, credential_class, scope, refreshed_at)
SELECT org_id, credential_class, scope, refreshed_at FROM reachable_scopes;

DROP TABLE reachable_scopes;
ALTER TABLE reachable_scopes_new RENAME TO reachable_scopes;

-- +goose Down
SELECT 'down not supported';
