-- +goose Up
-- Durable notifications (specs/durable-notifications.md). Local mode is N=1:
-- the one user is the recipient of every audience, and there is no RLS.
CREATE TABLE notifications (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    origin          TEXT NOT NULL CHECK (origin IN ('system', 'event')),
    lifecycle       TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind <> ''),
    key             TEXT NOT NULL,
    audience_scope  TEXT NOT NULL,
    audience_role   TEXT,
    team_id         TEXT REFERENCES teams(id) ON DELETE CASCADE,
    user_id         TEXT REFERENCES users(id) ON DELETE CASCADE,
    source          TEXT,
    priority        TEXT NOT NULL CHECK (priority IN ('urgent', 'high', 'low')),
    dismissible     BOOLEAN NOT NULL,
    title           TEXT NOT NULL,
    body            TEXT NOT NULL DEFAULT '',
    actions         TEXT NOT NULL DEFAULT '[]',
    params          TEXT NOT NULL DEFAULT '{}',
    first_seen_at   DATETIME NOT NULL,
    last_seen_at    DATETIME NOT NULL,
    seen_count      INTEGER NOT NULL DEFAULT 1 CHECK (seen_count >= 1),
    escalated_at    DATETIME NOT NULL,
    resolved_at     DATETIME,
    resolved_reason TEXT CHECK (resolved_reason IN ('cleared', 'source_off', 'audience_removed')),
    expires_at      DATETIME,
    CHECK (
        (audience_scope = 'org' AND audience_role IN ('members', 'admins') AND team_id IS NULL AND user_id IS NULL)
        OR (audience_scope = 'team' AND audience_role IN ('members', 'admins') AND team_id IS NOT NULL AND user_id IS NULL)
        OR (audience_scope = 'user' AND audience_role IS NULL AND team_id IS NULL AND user_id IS NOT NULL)
    ),
    CHECK (
        (lifecycle = 'condition' AND expires_at IS NULL)
        OR (lifecycle = 'notice' AND expires_at IS NOT NULL)
    ),
    CHECK (lifecycle = 'condition' OR (dismissible AND resolved_at IS NULL)),
    CHECK ((resolved_at IS NULL) = (resolved_reason IS NULL))
);

-- The open occurrence a raise coalesces into.
CREATE UNIQUE INDEX notifications_open_occurrence_idx ON notifications (
    org_id, origin, kind, key, audience_scope,
    coalesce(audience_role, ''), coalesce(team_id, ''), coalesce(user_id, '')
) WHERE resolved_at IS NULL;

CREATE INDEX notifications_list_idx ON notifications (org_id, resolved_at, priority, escalated_at, id);

CREATE TABLE notification_marks (
    notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    read_at         DATETIME,
    dismissed_at    DATETIME,
    PRIMARY KEY (notification_id, user_id)
);

-- +goose Down
SELECT 'down not supported';
