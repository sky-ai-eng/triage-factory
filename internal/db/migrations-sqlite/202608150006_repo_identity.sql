-- +goose Up
-- Repository identity: which provider issued a repository, that provider's own
-- id for it, and a natural key that matches the way GitHub matches.
--
-- Every repository reference in this schema is case-normalized (owner, repo)
-- text — team_github_repos' PK, repo_profiles' unique, run_worktrees.repo_id,
-- entities.source_id, teams.pinned_repos. A rename or a transfer moves all of
-- them at once and TF cannot even detect that it happened: under polling, a
-- renamed repository is indistinguishable from a 404 plus a brand-new one. The
-- id the provider issues is the half of a repository's identity a rename does
-- not move, and this is the column that records it.
--
-- source is app-validated rather than CHECK-constrained — the convention the
-- baseline states for the other source columns (prompts.source). Widening a
-- SQLite CHECK costs a full-table rebuild, so the headroom is the absence of
-- the CHECK; the app is the source of truth for the value set, which today is
-- 'github' alone.
--
-- external_id is deliberately not named github_repo_id: (source, external_id)
-- is the shape entities (source, source_id) already uses, so a second provider
-- costs a value rather than a column. It is nullable, and NULL is a supported
-- state rather than a gap to backfill in bulk — a row without an id behaves
-- exactly as it does today, everywhere. It fills in from GitHub payloads TF
-- already fetches (the /repos/{owner}/{repo} response the profiler reads),
-- never from a fetch added to obtain it.
--
-- # Why the key is case-folded
--
-- GitHub identifiers are case-insensitive, so "one repository" has always meant
-- one row per case-folded slug. Every lookup in the store already enforced that
-- by hand. A key that does not fold leaves the invariant to those callers, and
-- a caller is exactly what fails: an INSERT guarded by a case-insensitive
-- NOT EXISTS and backed by a case-sensitive index admits two rows for one
-- repository whenever two writers race, because neither sees the other's
-- uncommitted row and their keys then differ. Folding it here makes the
-- database the thing that refuses, so no writer has to remember to.
--
-- SQLite cannot ALTER a table-level UNIQUE in place, so the table is rebuilt
-- and the rows copied. Nothing references repo_profiles, so no child FK has to
-- be toggled, and the table has no FK of its own to preserve.
--
-- The rebuild keeps id as the "owner/repo" slug PK. It stays case-sensitive and
-- stays narrower on the source axis — two providers spelling the same slug
-- would still collide here — which costs nothing while local mode is
-- single-org and GitHub-only. Reshaping it belongs to whatever adds a second
-- provider.
CREATE TABLE repo_profiles_new (
    id              TEXT PRIMARY KEY,
    owner           TEXT NOT NULL,
    repo            TEXT NOT NULL,
    -- source: the provider that issued this repository. App-validated.
    source          TEXT NOT NULL DEFAULT 'github',
    -- external_id: the provider's own repository id, discriminated by source.
    -- NULL means "not learned yet", never "no such id".
    external_id     TEXT,
    description     TEXT,
    has_readme      BOOLEAN DEFAULT 0,
    has_claude_md   BOOLEAN DEFAULT 0,
    has_agents_md   BOOLEAN DEFAULT 0,
    profile_text    TEXT,
    clone_url       TEXT,
    default_branch  TEXT,
    base_branch     TEXT,
    profiled_at     DATETIME,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    clone_status    TEXT NOT NULL DEFAULT 'pending',
    clone_error     TEXT,
    clone_error_kind TEXT,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    -- pulls_etag / pulls_polled_at back the GitHub poller's conditional
    -- open-PR discovery: pulls_etag is the last ETag GitHub returned for
    -- GET /repos/{o}/{r}/pulls?state=open; pulls_polled_at is
    -- the last successful list (200 or 304). A 304 against the stored ETag
    -- means the open-PR set is unchanged and discovery skips the repo.
    pulls_etag      TEXT,
    pulls_polled_at DATETIME
);

-- Copy one row per repository. Every existing row is a GitHub repository with
-- no id learned yet, so the copy is column-for-column otherwise: a row that was
-- profiled stays profiled, its clone state, base branch, and poll ETag intact.
--
-- "One row per repository" is where the old key and the new one can disagree.
-- UNIQUE(owner, repo) was case-SENSITIVE, so a database could in principle hold
-- 'Acme/Api' and 'acme/api' as separate rows; the folded key says those are one
-- repository. The winner is the profiled row, then the lowest id — deterministic
-- either way, and the survivor keeps its own casing (which is what the store
-- has always done: casing is sticky).
--
-- This is defensive rather than expected. The tracked-set reconcile case-folds
-- the org-wide union before it inserts, so no writer that ships today can
-- produce such a pair; it would take history from the pre-derived-cache
-- configured-repos PUT plus GitHub answering with two different casings for one
-- repo. The fold exists because the alternative to a no-op branch here is a
-- CREATE UNIQUE INDEX that raises and takes the process down at boot.
INSERT INTO repo_profiles_new (
    id, owner, repo, source, external_id, description,
    has_readme, has_claude_md, has_agents_md, profile_text,
    clone_url, default_branch, base_branch, profiled_at, updated_at,
    clone_status, clone_error, clone_error_kind, org_id,
    pulls_etag, pulls_polled_at
)
SELECT
    p.id, p.owner, p.repo, 'github', NULL, p.description,
    p.has_readme, p.has_claude_md, p.has_agents_md, p.profile_text,
    p.clone_url, p.default_branch, p.base_branch, p.profiled_at, p.updated_at,
    p.clone_status, p.clone_error, p.clone_error_kind, p.org_id,
    p.pulls_etag, p.pulls_polled_at
FROM repo_profiles p
WHERE p.id = (
    SELECT p2.id
      FROM repo_profiles p2
     WHERE p2.org_id = p.org_id
       AND LOWER(p2.owner) = LOWER(p.owner)
       AND LOWER(p2.repo) = LOWER(p.repo)
     ORDER BY (p2.profiled_at IS NULL), p2.id
     LIMIT 1
);

-- Carry a discarded twin's base branch onto the survivor if the survivor has
-- none. base_branch is the only column on this table a person set by hand;
-- everything else a twin held (profile text, clone URL, clone status, ETag) is
-- a cache the profiler and poller rebuild within a cycle, so re-deriving it
-- costs a poll and losing a branch setting costs a surprise.
UPDATE repo_profiles_new
   SET base_branch = (
        SELECT p.base_branch
          FROM repo_profiles p
         WHERE p.org_id = repo_profiles_new.org_id
           AND LOWER(p.owner) = LOWER(repo_profiles_new.owner)
           AND LOWER(p.repo) = LOWER(repo_profiles_new.repo)
           AND p.base_branch IS NOT NULL
         ORDER BY p.id
         LIMIT 1)
 WHERE base_branch IS NULL;

DROP TABLE repo_profiles;
ALTER TABLE repo_profiles_new RENAME TO repo_profiles;

-- The natural key, expressed as a folded unique index because a table-level
-- UNIQUE cannot hold an expression. This is what makes a duplicate impossible
-- rather than merely avoided: a second writer inserting the same repository
-- under any casing conflicts here, and ON CONFLICT DO NOTHING turns that into
-- the no-op the create path wants.
CREATE UNIQUE INDEX repo_profiles_identity
    ON repo_profiles(org_id, source, LOWER(owner), LOWER(repo));

-- Dropping the table dropped its indexes with it; the slug lookup index is
-- recreated verbatim.
CREATE INDEX idx_repo_profiles_owner_repo ON repo_profiles(owner, repo);

-- +goose Down
SELECT 'down not supported';
