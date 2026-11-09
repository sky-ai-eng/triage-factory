-- +goose Up
-- Pending refresh marks: one row per target in a tracked repository that a
-- GitHub webhook delivery said changed, written in the transaction that
-- records the delivery in github_webhook_deliveries. kind says what target
-- holds: a pull request number in decimal, a head SHA, or a branch name
-- without refs/heads/. Deliveries naming the same target land on one row:
-- each bumps revision and marked_at, and first_marked_at stays. A pass clears
-- a mark only at the revision it read, so a delivery landing mid-pass keeps
-- its mark.
--
-- first_marked_at and marked_at are written from Go as UTC, the way
-- github_webhook_deliveries.received_at is, so every value in them is in one
-- format.
CREATE TABLE github_refresh_marks (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    repository_id   TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('pull_request', 'head_sha', 'branch')),
    target          TEXT NOT NULL CHECK (target <> ''),
    revision        INTEGER NOT NULL DEFAULT 1,
    first_marked_at DATETIME NOT NULL,
    marked_at       DATETIME NOT NULL,
    UNIQUE (repository_id, kind, target),
    FOREIGN KEY (repository_id, org_id) REFERENCES repositories(id, org_id) ON DELETE CASCADE
);

-- The poll reads an org's marks oldest first.
CREATE INDEX idx_github_refresh_marks_org ON github_refresh_marks (org_id, first_marked_at, id);

-- +goose Down
SELECT 'down not supported';
