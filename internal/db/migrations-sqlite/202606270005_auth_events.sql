-- +goose Up
-- auth_events (TFAC-76): the SOC2 authentication audit log of record — durable,
-- append-only capture of every authentication / session outcome (login, logout,
-- refresh failure, JWT-verify failure, SSO enforcement, break-glass). The
-- authentication sibling of access_change_log (the authorization-CHANGE log);
-- together the two halves of the SOC2 access-controls surface (CC6.1 logical
-- access, CC7.2 monitoring).
--
-- Local/SQLite mode has no GoTrue and no sessions table, so it produces ~no auth
-- events in practice (there is no login flow). The table is PARITY-ONLY — kept
-- shape-identical to the Postgres baseline's public.auth_events so a future
-- multi-user SQLite needs no rewiring (mirrors the access_change_log SQLite
-- migration's rationale). event_type is a free-text discriminator
-- (domain.AuthEvent* — extensible, no CHECK). org_id / user_id / session_id are
-- all nullable (pre-auth / pre-identity failures carry none); session_id is a
-- plain TEXT column with NO FK — there is no sessions table in SQLite to
-- reference. detail_json carries the per-event payload. NO reaper: auth_events is
-- the durable record and is never auto-purged.
CREATE TABLE auth_events (
    id          TEXT PRIMARY KEY,
    org_id      TEXT,
    user_id     TEXT,
    session_id  TEXT,           -- plain column: SQLite has NO sessions table (local has no login)
    event_type  TEXT NOT NULL,
    ip_address  TEXT,
    user_agent  TEXT,
    detail_json TEXT,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_auth_events_org_ts  ON auth_events (org_id, created_at DESC);
CREATE INDEX idx_auth_events_user_ts ON auth_events (user_id, created_at DESC);
CREATE INDEX idx_auth_events_type_ts ON auth_events (event_type, created_at DESC);

-- +goose Down
SELECT 'down not supported';
