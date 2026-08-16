-- +goose Up
-- Rename repo_profiles to repositories.
--
-- The table was named for what it originally held: a cache of AI-generated
-- profile text for repos the user had configured. It has since become the
-- registry of the repositories TF works with — it carries the provider that
-- issued each one, that provider's own id for it, the tracked-set membership
-- every reconcile writes through, the clone state, and the poller's
-- conditional-request cursor. The profile is now one column group among
-- several, and a name that promises a profile cache mis-describes every other
-- reader of the row.
--
-- The rename is the whole change. No column is added, dropped, retyped or
-- reordered, no row is copied, and no id moves: ALTER TABLE ... RENAME TO
-- rewrites the schema entry and leaves the rows exactly where they are, so an
-- existing local install crosses this migration without touching a page of
-- data.
--
-- Nothing else in the schema has to follow. No foreign key, view or trigger
-- references this table in SQLite — the slug-keyed references elsewhere
-- (team_github_repos, conversation_worktrees.repo_id, projects.pinned_repos,
-- entities.source_id) are text columns holding "owner/repo", not FKs, so they
-- neither know nor care what the table they describe is called.
ALTER TABLE repo_profiles RENAME TO repositories;

-- The indexes survive the rename attached to the renamed table, but keep their
-- old names — SQLite has no ALTER INDEX. Dropping and recreating them renames
-- them without touching a row: the folded natural key first, then the slug
-- lookup the poller reads through. Both are recreated with the definitions the
-- identity migration gave them, verbatim apart from the table's new name.
DROP INDEX repo_profiles_identity;
CREATE UNIQUE INDEX repositories_identity
    ON repositories(org_id, source, LOWER(owner), LOWER(repo));

DROP INDEX idx_repo_profiles_owner_repo;
CREATE INDEX idx_repositories_owner_repo ON repositories(owner, repo);

-- +goose Down
SELECT 'down not supported';
