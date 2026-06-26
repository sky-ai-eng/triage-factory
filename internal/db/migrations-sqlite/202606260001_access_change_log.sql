-- +goose Up
-- access_change_log (TFAC-471): small, low-volume audit log of governance
-- actions that have no external entity — org/team membership & role
-- grants/changes/revokes, and credential bind/rotate (GitHub PAT, Jira org +
-- per-user, Anthropic key). Capture only; the read/display surface is the future
-- team-activity / org-governance view (TFAC-449 bucket C/D). Written in the SAME
-- transaction as the action it records, so the log can't diverge from reality.
--
-- Mirrors the Postgres baseline's public.access_change_log (TEXT id is
-- app-generated here; TEXT for the uuid columns; DATETIME created_at). SQLite is
-- N=1 (local mode) so there is no RLS; the org_id column + LocalDefaultOrgID
-- default mirror the Postgres baseline so a future multi-user SQLite shape needs
-- no rewiring. action is a free-text discriminator (org_member_granted,
-- org_role_changed, credential_set, ... — extensible, no CHECK). actor_user_id
-- is the request's authenticated user (NULL for system/bootstrap); detail_json
-- carries the per-action payload ({old_role,new_role} | {kind,host} | {invite_id}).
CREATE TABLE access_change_log (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    actor_user_id  TEXT,
    action         TEXT NOT NULL,
    target_user_id TEXT,
    team_id        TEXT,
    detail_json    TEXT,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_access_change_log_org_created ON access_change_log(org_id, created_at DESC);

-- +goose Down
SELECT 'down not supported';
