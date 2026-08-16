-- +goose Up
-- Reference a repository by the registry row's own id.
--
-- A repository's slug is a display handle GitHub lets you change. Every other
-- table referred to one by that slug, which cost two things: a rename had to be
-- *propagated* to each of them, and none of the references was checked — a slug
-- naming a repository nobody has a row for was indistinguishable from a valid
-- one, so the rows rotted quietly instead of failing.
--
-- Three references become foreign keys here: the tracked set, the worktree
-- ledger, and a project's pinned repos. Everything else that holds a slug keeps
-- it, because it is not a reference: entities.source_id is a pull request's
-- natural key ("owner/repo#18"), artifacts.dedup_key is a contract between two
-- writers, placement_overrides.key_value is polymorphic, and events.payload_json
-- is history.
--
-- # The surrogate id comes first
--
-- The FK target is the registry row's primary key, and on this dialect that key
-- WAS the slug. An FK against it would have bought referential integrity and
-- none of the rename immunity that is the point — and it would not have matched
-- Postgres, where the same column is a uuid. So the table is rebuilt with a
-- surrogate id and the slug demoted to an ordinary column, which is also where
-- the identity migration's deferred narrowing gets resolved: the slug primary
-- key was the constraint that would have collided two providers spelling one
-- slug, and it is gone.
--
-- The store's contract does not move. db.RepositoryStore still surfaces every
-- repository as "owner/repo" in domain.Repository.ID, on both dialects — the
-- Postgres impl has always translated, and the SQLite impl now does the same.
-- No HTTP payload, CLI argument or filesystem path changes: those are read by
-- humans and agents, and the slug is the handle they use.

-- ===========================================================================
-- 1. repositories: surrogate id, slug demoted to an ordinary column
-- ===========================================================================
--
-- Column-for-column identical to what the identity migration built, except
-- that id no longer holds the slug. The rebuild is the same pattern that
-- migration used, and for the same reason: SQLite cannot re-type a primary key
-- in place.
CREATE TABLE repositories_new (
    -- id: a surrogate uuid, generated per row below. Never rendered, never
    -- accepted on the wire, and never rewritten — a rename moves owner/repo
    -- and leaves this alone, which is the whole point of the column.
    id              TEXT PRIMARY KEY,
    owner           TEXT NOT NULL,
    repo            TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'github',
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
    pulls_etag      TEXT,
    pulls_polled_at DATETIME
);

-- randomblob()/random() are non-deterministic, so SQLite evaluates them once
-- per row rather than folding them to a constant — each copied repository gets
-- its own id. The shape is a v4 uuid because that is what the Postgres column
-- holds and what every other id in this schema looks like; nothing parses it.
INSERT INTO repositories_new (
    id, owner, repo, source, external_id, description,
    has_readme, has_claude_md, has_agents_md, profile_text,
    clone_url, default_branch, base_branch, profiled_at, updated_at,
    clone_status, clone_error, clone_error_kind, org_id,
    pulls_etag, pulls_polled_at
)
SELECT
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
    substr(lower(hex(randomblob(2))), 2) || '-' ||
    substr('89ab', abs(random()) % 4 + 1, 1) ||
    substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
    r.owner, r.repo, r.source, r.external_id, r.description,
    r.has_readme, r.has_claude_md, r.has_agents_md, r.profile_text,
    r.clone_url, r.default_branch, r.base_branch, r.profiled_at, r.updated_at,
    r.clone_status, r.clone_error, r.clone_error_kind, r.org_id,
    r.pulls_etag, r.pulls_polled_at
FROM repositories r;

DROP TABLE repositories;
ALTER TABLE repositories_new RENAME TO repositories;

-- The folded natural key, unchanged — it was already the constraint that
-- decided what "one repository" means, and demoting the slug out of the
-- primary key leaves it as the only one.
CREATE UNIQUE INDEX repositories_identity
    ON repositories(org_id, source, LOWER(owner), LOWER(repo));
CREATE INDEX idx_repositories_owner_repo ON repositories(owner, repo);

-- ===========================================================================
-- 2. A registry row for every slug that is about to become a reference
-- ===========================================================================
--
-- The tracked set, the worktree ledger and the pinned lists are converted
-- against these rows, so any slug they hold without one would be dropped by the
-- conversion. Minting the row instead keeps the reference and makes it
-- checkable — which is the state the schema is moving to anyway: a registry row
-- outlives the tracking decision that created it.
--
-- The tracked set is the case where a row is expected to exist already (the
-- reconcile has created one on every save). The other two are the cases where
-- one may not: untracking a repository used to delete its registry row while
-- keeping the worktree ledger entries that named it.
--
-- Each source is folded to one row per repository before the insert, because
-- the identity index folds: two rows spelling one repository differently are
-- one repository, and inserting both would raise. MIN() picks a casing per
-- column, and every row in a group is already case-equal on both.
INSERT INTO repositories (id, owner, repo, source, updated_at)
SELECT
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
    substr(lower(hex(randomblob(2))), 2) || '-' ||
    substr('89ab', abs(random()) % 4 + 1, 1) ||
    substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
    s.owner, s.repo, 'github', CURRENT_TIMESTAMP
FROM (
    SELECT MIN(owner) AS owner, MIN(repo) AS repo
      FROM team_github_repos
     GROUP BY LOWER(owner), LOWER(repo)
    UNION
    SELECT MIN(owner), MIN(repo) FROM (
        SELECT SUBSTR(repo_id, 1, INSTR(repo_id, '/') - 1) AS owner,
               SUBSTR(repo_id, INSTR(repo_id, '/') + 1)    AS repo
          FROM conversation_worktrees
         WHERE INSTR(repo_id, '/') > 1
    ) GROUP BY LOWER(owner), LOWER(repo)
    UNION
    SELECT MIN(owner), MIN(repo) FROM (
        SELECT SUBSTR(e.value, 1, INSTR(e.value, '/') - 1) AS owner,
               SUBSTR(e.value, INSTR(e.value, '/') + 1)    AS repo
          FROM projects p, json_each(p.pinned_repos) e
         WHERE json_valid(p.pinned_repos) AND INSTR(e.value, '/') > 1
    ) GROUP BY LOWER(owner), LOWER(repo)
) s
WHERE NOT EXISTS (
    SELECT 1 FROM repositories r
     WHERE r.source = 'github'
       AND LOWER(r.owner) = LOWER(s.owner)
       AND LOWER(r.repo) = LOWER(s.repo)
)
-- The UNION above folds nothing across its three arms, so one repository named
-- by two of them arrives twice. The conflict target is the identity index, and
-- the second arrival is the same repository as the first.
ON CONFLICT DO NOTHING;

-- ===========================================================================
-- 3. team_github_repos: (team_id, owner, repo) -> (team_id, repository_id)
-- ===========================================================================
--
-- The tracking source of truth, and the highest-value conversion: this is the
-- table a rename used to have to rewrite, and the one every tracked-set
-- semi-join is built on.
--
-- ON DELETE CASCADE is correct here and nowhere else in this migration: a
-- tracking row is a *statement about* a repository, so it cannot outlive the
-- registry row it points at. The converse does not hold — untracking a
-- repository is a delete on THIS table and leaves the registry row standing.
CREATE TABLE team_github_repos_new (
    team_id       TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, repository_id)
);

-- INSERT OR IGNORE collapses a team that tracked one repository under two
-- casings onto the single row the folded join resolves both to — the absorb
-- the rename rewrite used to perform inline, now done once, here.
INSERT OR IGNORE INTO team_github_repos_new (team_id, repository_id, created_at)
SELECT g.team_id, r.id, g.created_at
  FROM team_github_repos g
  JOIN repositories r
    ON r.source = 'github'
   AND LOWER(r.owner) = LOWER(g.owner)
   AND LOWER(r.repo) = LOWER(g.repo);

DROP TABLE team_github_repos;
ALTER TABLE team_github_repos_new RENAME TO team_github_repos;

-- The reverse lookup: "which teams track this repository", which is the
-- direction every tracked-set semi-join reads. It replaces the folded
-- (owner, repo, team_id) index the slug columns needed.
CREATE INDEX team_github_repos_repository_idx
    ON team_github_repos(repository_id, team_id);

-- ===========================================================================
-- 4. conversation_worktrees.repo_id -> repository_id
-- ===========================================================================
--
-- A slug despite the name. The agenthost surface still takes one from an
-- agent's argv (`tfac exec workspace add owner/repo`); the store resolves it to
-- a row and stores the id.
--
-- No ON DELETE action, so the reference is refused rather than followed: a
-- worktree ledger entry records that a run materialized a checkout, and outlives
-- the tracking decision that created it. Deleting a registry row that a run
-- still names is the state this refuses — the alternative is a ledger with a
-- hole in it, which is the rot the FK exists to prevent.
CREATE TABLE conversation_worktrees_new (
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    repository_id   TEXT NOT NULL REFERENCES repositories(id),
    path            TEXT NOT NULL,
    ref             TEXT NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    PRIMARY KEY (conversation_id, repository_id, ref)
);

-- A row whose repo_id was never a slug at all (no '/') has no repository to
-- point at and is dropped: it named a path on a disk that no longer exists, in
-- a run that has long since exited.
INSERT OR IGNORE INTO conversation_worktrees_new
    (conversation_id, repository_id, path, ref, created_at, org_id)
SELECT w.conversation_id, r.id, w.path, w.ref, w.created_at, w.org_id
  FROM conversation_worktrees w
  JOIN repositories r
    ON r.source = 'github'
   AND LOWER(r.owner) = LOWER(SUBSTR(w.repo_id, 1, INSTR(w.repo_id, '/') - 1))
   AND LOWER(r.repo) = LOWER(SUBSTR(w.repo_id, INSTR(w.repo_id, '/') + 1))
 WHERE INSTR(w.repo_id, '/') > 1;

DROP TABLE conversation_worktrees;
ALTER TABLE conversation_worktrees_new RENAME TO conversation_worktrees;

CREATE INDEX idx_conversation_worktrees_run ON conversation_worktrees(conversation_id);

-- ===========================================================================
-- 5. projects.pinned_repos -> project_pinned_repos
-- ===========================================================================
--
-- A JSON array of slug strings cannot carry a foreign key at all, so the
-- reference was the one in this schema that nothing could check. A join table
-- is what makes it checkable.
--
-- position preserves the order the user pinned them in. The column exists
-- because the API surfaces pinned_repos as an ordered list and a project
-- round-trips through export/import; ordering by slug instead would silently
-- reshuffle a list somebody arranged.
--
-- ON DELETE CASCADE on both sides: a pin is a piece of project configuration,
-- not durable work, so it follows whichever end goes away.
CREATE TABLE project_pinned_repos (
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    PRIMARY KEY (project_id, repository_id)
);

-- json_each yields one row per array element; e.key is its index. A project
-- whose column is not a JSON array is skipped rather than failing the
-- migration — it is already broken in a way this cannot fix, and the same
-- posture the rename rewrite took.
INSERT OR IGNORE INTO project_pinned_repos (project_id, repository_id, position)
SELECT p.id, r.id, e.key
  FROM projects p
  JOIN json_each(p.pinned_repos) e
  JOIN repositories r
    ON r.source = 'github'
   AND LOWER(r.owner) = LOWER(SUBSTR(e.value, 1, INSTR(e.value, '/') - 1))
   AND LOWER(r.repo) = LOWER(SUBSTR(e.value, INSTR(e.value, '/') + 1))
 WHERE json_valid(p.pinned_repos) AND INSTR(e.value, '/') > 1;

ALTER TABLE projects DROP COLUMN pinned_repos;

CREATE INDEX project_pinned_repos_repository_idx ON project_pinned_repos(repository_id);

-- +goose Down
SELECT 'down not supported';
