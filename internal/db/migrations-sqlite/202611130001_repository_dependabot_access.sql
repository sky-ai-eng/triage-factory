-- +goose Up
-- Whether the org's GitHub credential can read a repository's Dependabot
-- alerts, as GitHub last answered the poll's probe: readable, no_permission
-- (the credential was refused), or disabled (alerts are turned off on the
-- repository). One row per repository, replaced by each probe; a repository
-- the poll has not probed has no row. Only a readable repository is polled
-- for alerts.
--
-- checked_at is written from Go as UTC, like github_refresh_marks.marked_at.
CREATE TABLE repository_dependabot_access (
    repository_id TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    access        TEXT NOT NULL CHECK (access IN ('readable', 'no_permission', 'disabled')),
    checked_at    DATETIME NOT NULL,
    FOREIGN KEY (repository_id, org_id) REFERENCES repositories(id, org_id) ON DELETE CASCADE
);

-- +goose Down
SELECT 'down not supported';
