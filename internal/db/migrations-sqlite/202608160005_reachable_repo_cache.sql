-- +goose Up
-- The reachable-repo cache: one mirror of "which repositories can this org
-- reach", populated by BOTH credential classes and read by every consumer.
--
-- This generalizes installation_repositories, which mirrored the same fact for
-- the App tier alone. Its framing is unchanged and carries over verbatim: the
-- reachable set is a CACHE of an external fact, rebuilt in full per refresh,
-- with no durable identity, surviving nothing — the opposite of `repositories`,
-- which is a REGISTRY of TF entities that worktrees, entities, clone state and a
-- hand-set base_branch hang off. A reachable row is deliberately not a registry
-- row: the cache contains repositories nobody tracks, and that is the entire
-- content of the reach-without-purpose finding.
--
-- # Why generalizing it is the point
--
-- Two consumers were asking GitHub the same question live, differently, per
-- credential class:
--
--   * the repository picker (POST /api/github/repos/list) proxied
--     GET /user/repos on a PAT org and GET /installation/repositories on an App
--     org. Because it was a proxy list it could not report a total — and it
--     could not for two DIFFERENT reasons per tier, which is the tell that the
--     shape was wrong. /user/repos returns a bare array with no count at all;
--     /installation/repositories does return total_count, but one installation's
--     total, and the picker serves the union across all of them.
--   * the team-repos write gate re-asked, and kept its own in-process,
--     per-(org, user) map to avoid asking twice — a cache that died on restart
--     and, in multi mode, lived on whichever pod happened to serve the picker
--     rather than the one that later served the write.
--
-- A table answers both, identically on both tiers, and can COUNT(*).
--
-- # Rebuilt, not migrated
--
-- The old table is dropped rather than altered. Three of its columns change
-- shape at once (installation_id becomes nullable, the identity key splits per
-- class, the display fields the picker renders are new), which on this dialect
-- means a full rebuild anyway — and there is nothing to preserve, because every
-- row is re-derived by the next refresh pass. The cost of the drop is one
-- refresh, which is the cost the cache is designed around.

DROP TABLE IF EXISTS installation_repositories;

CREATE TABLE reachable_repositories (
    org_id           TEXT NOT NULL,
    -- credential_class: which credential system observed this reach. It is part
    -- of the key rather than a derived attribute because an org that switches
    -- tiers has, for a moment, both answers on file, and serving the union of
    -- them would be serving a reach the org no longer has. Every read filters on
    -- the org's CURRENT class, so the other tier's rows are inert until their
    -- next refresh replaces them.
    credential_class TEXT NOT NULL
        CHECK (credential_class IN ('pat', 'byo_app')),
    -- installation_id / host: the credential INSTANCE the reach was observed
    -- through — the scope one refresh replaces atomically.
    --
    -- The App tier keeps the installation FK it always had, which is also how it
    -- keeps inheriting host scoping (an installation id is unique per GitHub
    -- deployment, and the installation row records which deployment that is) and
    -- how an uninstall still takes the mirror with it.
    --
    -- The PAT tier has no installation row to hang off, so it carries the host
    -- directly. That is not bookkeeping: an org that repoints GitHubBaseURL is
    -- looking at a different deployment's repositories, and a mirror that did
    -- not record which one it read would answer the new host with the old host's
    -- set.
    installation_id  TEXT,
    host             TEXT,
    -- source: the provider that issued this repository, carried so the join to
    -- the registry is written against the same key the registry is keyed under
    -- (org_id, source, LOWER(owner), LOWER(repo)) rather than against a literal.
    -- Not part of this table's own key: every row here is reached through a
    -- GitHub credential, so 'github' is the only value it can hold.
    source           TEXT NOT NULL DEFAULT 'github',
    owner            TEXT NOT NULL,
    repo             TEXT NOT NULL,
    -- external_id: the provider's own repository id. It is what lets a cache
    -- entry and a registry row be matched by id even when their slugs
    -- momentarily disagree mid-rename. Nullable, and NULL is a supported state
    -- rather than a gap to backfill: a row without an id is matched by slug
    -- exactly as it would be anyway.
    external_id      TEXT,
    -- The picker's display fields, mirrored because the picker is now served
    -- from here rather than proxied. Both enumerations return whole repository
    -- objects, so recording them costs nothing beyond the width of the row, and
    -- the alternative — a slug-only cache plus a live fetch to decorate it —
    -- would put the upstream call back on the read this table exists to take it
    -- off. Empty string rather than NULL throughout: these are display strings
    -- with no meaningful absent state, and "" renders the same as a missing
    -- field without every reader having to say so.
    description      TEXT NOT NULL DEFAULT '',
    language         TEXT NOT NULL DEFAULT '',
    html_url         TEXT NOT NULL DEFAULT '',
    -- pushed_at: GitHub's own timestamp string, stored verbatim as text. It is
    -- passed through to the client and never compared, sorted on, or arithmetic
    -- done to here — parsing it into a real timestamp would be claiming a
    -- precision this column does not need and inviting a NULL for the repos that
    -- have never been pushed to.
    pushed_at        TEXT NOT NULL DEFAULT '',
    private          INTEGER NOT NULL DEFAULT 0,
    -- observed_at: when the refresh that wrote this row ran. Unlike the App-only
    -- mirror this replaces, it IS a staleness gate here as well as display
    -- provenance: the TTL that bounds how often a 3,000-repo account is
    -- re-enumerated reads it, and so does the write gate deciding whether to
    -- trust the cache or probe upstream. One refresh replaces a whole scope
    -- atomically, so every row in one scope carries the same instant.
    observed_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- Exactly one scope column per class, and it is the database that says so.
    -- The two are alternatives, never both and never neither: a row with both
    -- would have two answers to "what does one refresh replace", and a row with
    -- neither could not be replaced at all.
    CHECK ((credential_class = 'byo_app' AND installation_id IS NOT NULL AND host IS NULL)
        OR (credential_class = 'pat'     AND installation_id IS NULL     AND host IS NOT NULL)),
    FOREIGN KEY (org_id, installation_id)
        REFERENCES org_github_app_installations (org_id, installation_id)
        ON DELETE CASCADE
);

-- The natural key, per class, folded. Folded for the reason the repository
-- identity migration states at length: GitHub identifiers are case-insensitive,
-- so a case-sensitive index behind a case-insensitive guard admits duplicates
-- whenever two writers race — neither sees the other's uncommitted row and their
-- keys then differ. The database has to be the thing that refuses.
--
-- Two partial indexes rather than one over COALESCE(installation_id, host): the
-- scope columns are genuinely different columns with different nullability, and
-- a coalesced key would silently collide an installation whose id equals another
-- org's host string.
CREATE UNIQUE INDEX reachable_repositories_app_identity
    ON reachable_repositories (org_id, installation_id, LOWER(owner), LOWER(repo))
    WHERE credential_class = 'byo_app';

CREATE UNIQUE INDEX reachable_repositories_pat_identity
    ON reachable_repositories (org_id, host, LOWER(owner), LOWER(repo))
    WHERE credential_class = 'pat';

-- The registry join, and the drift queries that run over it. Deliberately the
-- same shape as repositories_identity — (org_id, source, LOWER(owner),
-- LOWER(repo)) — so the two sides of "is this reachable repository tracked?" are
-- keyed identically and either can drive the join.
CREATE INDEX reachable_repositories_registry_join
    ON reachable_repositories (org_id, source, LOWER(owner), LOWER(repo));

-- The picker's own read: one org's current class, ordered by folded slug. The
-- order is part of the index because offset paging over an unordered set drops
-- and repeats rows, and the folded slug is a total order (the per-class unique
-- index above proves no two rows tie on it).
CREATE INDEX reachable_repositories_picker
    ON reachable_repositories (org_id, credential_class, LOWER(owner), LOWER(repo));

-- The refreshes themselves, one row per scope. Its whole job is to make "we have
-- not looked yet" representable, because the repository rows cannot: a scope that
-- genuinely reaches NOTHING writes zero of them, so row count alone reads a
-- legitimately empty grant as an un-run refresh — the picker would say
-- "discovering repositories…" forever and kick a refresh on every open, which is
-- exactly the unbounded enumeration the TTL exists to prevent.
--
-- No FK, deliberately. The scope column holds an installation id for one class
-- and a host for the other, so there is no single parent to point at; the two
-- writers that retire an installation (the store's soft-removal, and the
-- standalone clear) delete the matching row in the same transaction as the
-- repository rows.
--
-- What it does NOT try to represent is a scope that has never been refreshed at
-- all — a freshly-installed installation has no row here, so it does not drag the
-- org's staleness back. That case is covered by the forced refresh every
-- credential change fires, which is the mechanism for reach moving without a
-- timestamp moving, and it is why nothing here has to enumerate the scopes it
-- expects to see.
CREATE TABLE reachable_scopes (
    org_id           TEXT NOT NULL,
    credential_class TEXT NOT NULL
        CHECK (credential_class IN ('pat', 'byo_app')),
    -- scope: the credential instance one refresh replaces — the installation id
    -- for byo_app, the host for pat. Recorded as opaque text: this table records
    -- THAT a scope was refreshed and when, and never joins on it.
    scope            TEXT NOT NULL,
    refreshed_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, credential_class, scope)
);

-- +goose Down
SELECT 'down not supported';
