-- +goose Up
-- The set of repositories a GitHub App installation can reach, mirrored so it
-- can be compared against the set TF actually tracks.
--
-- GET /installation/repositories is already called once per installation per
-- poll cycle to compute the tracked-granted intersection, and the answer is
-- then discarded. Nothing can therefore compute, server-side, either of the two
-- states an operator needs to see:
--
--   * reach without purpose — the App can reach a repository no team tracks,
--     which means TF holds write access to code nobody asked it to touch;
--   * scope drift — a team tracks a repository outside the grant, which means
--     that repository is silently unpollable.
--
-- Both are security findings rather than cosmetics, and neither is derivable
-- from state TF keeps today.
--
-- # Why this is its own table and not a column on repo_profiles
--
-- The relationship really is 1:N — a repository belongs to one GitHub account,
-- an account has at most one installation of a given App, and a workspace has
-- exactly one App — so a nullable granted_installation_id on the repository row
-- would be relationally correct. It is still wrong, for four reasons:
--
--  1. It changes what a repository row means. That table is the repositories TF
--     works with; the grant deliberately contains repositories nobody tracks,
--     which is the entire content of reach without purpose. Merging them makes
--     an "all repositories" installation on a 3,000-repo org mint 3,000 rows
--     that the profiler and every context loader then iterate.
--  2. Two lifecycles, two owners, contradictory answers to "exists".
--     Repository identity is TF-owned and durable — worktrees, entities, clone
--     state and a hand-set base_branch hang off it. Grant membership is
--     GitHub-owned and ephemeral: revoked from a selective grant it must
--     vanish, while the repository row must survive because a task may
--     reference it.
--  3. The never-empty invariant gets harder. As its own table that invariant is
--     a scoped transaction over rows only the reconcile writes. As a column it
--     is a wide UPDATE nulling a field across a table five subsystems write.
--  4. It re-introduces the provider-specific column the repository-identity
--     migration designed out: an installation id is a GitHub App concept, and
--     GitLab has none.
--
-- The framing: the grant is a CACHE of an external fact; the repository row is a
-- REGISTRY of a TF entity. This table is rebuilt in full every pass, needs no
-- durable identity, and survives nothing.
CREATE TABLE installation_repositories (
    org_id          TEXT NOT NULL,
    installation_id TEXT NOT NULL,
    -- source: the provider that issued this repository, carried so the join to
    -- the registry can be written against the same key the registry is keyed
    -- under (org_id, source, LOWER(owner), LOWER(repo)) rather than against a
    -- literal. It is not part of this table's own key: every row here hangs off
    -- a GitHub App installation, so 'github' is the only value it can hold.
    source          TEXT NOT NULL DEFAULT 'github',
    owner           TEXT NOT NULL,
    repo            TEXT NOT NULL,
    -- external_id: GitHub's own repository id, present because
    -- GET /installation/repositories returns whole repository objects. It is
    -- what lets a grant entry and a registry row be matched by id even when
    -- their slugs momentarily disagree mid-rename. Nullable, and NULL is a
    -- supported state rather than a gap to backfill: a row without an id is
    -- matched by slug exactly as it would be anyway.
    external_id     TEXT,
    -- observed_at: when this pass recorded the row. Display provenance ("the
    -- grant as of…"), never a join key and never a staleness gate — the whole
    -- table is replaced atomically, so every row in one installation's mirror
    -- carries the same instant.
    observed_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- Host scoping is inherited through this FK rather than stored: an
    -- installation id is unique per GitHub deployment, and the installation row
    -- is the thing that records which deployment that is.
    FOREIGN KEY (org_id, installation_id)
        REFERENCES org_github_app_installations (org_id, installation_id)
        ON DELETE CASCADE
);

-- The natural key, folded, for the reason the repository-identity migration
-- states at length: GitHub identifiers are case-insensitive, so a
-- case-sensitive index behind a case-insensitive guard admits duplicates
-- whenever two writers race — neither sees the other's uncommitted row and
-- their keys then differ. The database has to be the thing that refuses.
CREATE UNIQUE INDEX installation_repositories_identity
    ON installation_repositories (org_id, installation_id, LOWER(owner), LOWER(repo));

-- The registry join, and the drift queries that run over it. Deliberately the
-- same shape as repo_profiles_identity — (org_id, source, LOWER(owner),
-- LOWER(repo)) — so the two sides of "is this granted repository tracked?" are
-- keyed identically and either can drive the join.
CREATE INDEX installation_repositories_registry_join
    ON installation_repositories (org_id, source, LOWER(owner), LOWER(repo));

-- Whether the grant is every repository on the account or an enumerated
-- selection. GitHub reports it on the installation object
-- (GET /app/installations and every installation webhook payload); TF has
-- always discarded it.
--
-- It is what says whether drift is even POSSIBLE for a given installation.
-- Without it the org page cannot distinguish "nothing is drifting" from "drift
-- is impossible here", and those need different copy: an 'all' installation
-- reaches everything the account owns, so a tracked repository on that account
-- can never sit outside the grant.
--
-- Nullable, and NULL means "not learned yet" — a row mirrored before the first
-- reconcile — never "no selection". The CHECK admits NULL for exactly that
-- reason and refuses anything outside GitHub's two values, so a typo cannot
-- reach the column and be read later as a third state.
ALTER TABLE org_github_app_installations
    ADD COLUMN repository_selection TEXT
        CHECK (repository_selection IS NULL OR repository_selection IN ('all', 'selected'));

-- +goose Down
SELECT 'down not supported';
