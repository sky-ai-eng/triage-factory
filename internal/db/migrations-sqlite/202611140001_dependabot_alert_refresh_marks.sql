-- +goose Up
-- A refresh mark can name a repository's Dependabot alerts: kind
-- dependabot_alerts, with an empty target, since one listing reads every
-- alert of the repository. Every other kind still needs a target. SQLite
-- cannot change a CHECK in place, so the table is rebuilt with its rows.
CREATE TABLE github_refresh_marks_new (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    repository_id   TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('pull_request', 'head_sha', 'branch', 'dependabot_alerts')),
    target          TEXT NOT NULL,
    revision        INTEGER NOT NULL DEFAULT 1,
    first_marked_at DATETIME NOT NULL,
    marked_at       DATETIME NOT NULL,
    CHECK ((kind = 'dependabot_alerts') = (target = '')),
    UNIQUE (repository_id, kind, target),
    FOREIGN KEY (repository_id, org_id) REFERENCES repositories(id, org_id) ON DELETE CASCADE
);

INSERT INTO github_refresh_marks_new (id, org_id, repository_id, kind, target, revision, first_marked_at, marked_at)
SELECT id, org_id, repository_id, kind, target, revision, first_marked_at, marked_at
FROM github_refresh_marks;

DROP TABLE github_refresh_marks;
ALTER TABLE github_refresh_marks_new RENAME TO github_refresh_marks;

-- The poll reads an org's marks oldest first.
CREATE INDEX idx_github_refresh_marks_org ON github_refresh_marks (org_id, first_marked_at, id);

-- +goose Down
SELECT 'down not supported';
