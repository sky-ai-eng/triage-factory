-- +goose Up
-- Repository identity: which provider issued a repository, and that
-- provider's own id for it.
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
-- The unique widens from (owner, repo) to (org_id, source, owner, repo).
-- SQLite cannot ALTER a table-level UNIQUE in place, so the table is rebuilt
-- and every row copied across. Nothing references repo_profiles, so no child
-- FK has to be toggled, and the table has no FK of its own to preserve.
--
-- The rebuild keeps id as the "owner/repo" slug PK, which remains the narrower
-- constraint in this dialect: two sources spelling the same slug would still
-- collide here. That costs nothing today — local mode is single-org and
-- GitHub-only, so slug uniqueness and the widened key select the same rows —
-- and reshaping the slug PK is the table-rename ticket's business, not this
-- one's.
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
    pulls_polled_at DATETIME,
    UNIQUE(org_id, source, owner, repo)
);

-- Every existing row is a GitHub repository with no id learned yet. The copy
-- is column-for-column otherwise: a row that was profiled stays profiled, its
-- clone state, base branch, and poll ETag intact, and — because the widened
-- key is a superset of the old one on a single-source table — no row splits
-- and none merges. Same count out as in.
INSERT INTO repo_profiles_new (
    id, owner, repo, source, external_id, description,
    has_readme, has_claude_md, has_agents_md, profile_text,
    clone_url, default_branch, base_branch, profiled_at, updated_at,
    clone_status, clone_error, clone_error_kind, org_id,
    pulls_etag, pulls_polled_at
)
SELECT
    id, owner, repo, 'github', NULL, description,
    has_readme, has_claude_md, has_agents_md, profile_text,
    clone_url, default_branch, base_branch, profiled_at, updated_at,
    clone_status, clone_error, clone_error_kind, org_id,
    pulls_etag, pulls_polled_at
FROM repo_profiles;

DROP TABLE repo_profiles;
ALTER TABLE repo_profiles_new RENAME TO repo_profiles;

-- Dropping the table dropped its indexes with it; the slug lookup index is
-- recreated verbatim.
CREATE INDEX idx_repo_profiles_owner_repo ON repo_profiles(owner, repo);

-- +goose Down
SELECT 'down not supported';
