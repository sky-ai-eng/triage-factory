-- +goose Up
-- The pending-bind record: one row per initiated deployment-App bind ceremony.
--
-- When one App key serves many workspaces, the org<->installation mapping has
-- no source. GitHub's post-install redirect cannot be that source — it is an
-- unsigned GET whose installation_id is sequential, and GitHub's own
-- documentation says not to rely on it — so TF asserts the mapping itself, and
-- this row plus a cookie is what proves the assertion belongs to the workspace
-- that asked for it.
--
-- Durable rather than in-process because the pod that serves the Connect click
-- need not be the pod that serves the callback. An in-memory map would work in
-- development and fail intermittently under load.
--
-- nonce_hash is the SHA-256 of the nonce in lowercase hex; the plaintext lives
-- only in the browser's cookie, so reading this table yields nothing that can
-- complete a bind. It is the primary key because it is also the lookup: the
-- callback knows the nonce and nothing else, which is why the org and the
-- initiating user are columns here rather than parameters the callback could
-- supply for itself.
--
-- Single-use is enforced by the consume, a conditional UPDATE that matches only
-- an unspent, unexpired row and reports what it changed — never a read followed
-- by a write, which would let two concurrent callbacks both proceed.
--
-- The ceremony is multi-mode only (a distributed local binary ships no shared
-- App key), so nothing writes this table in a local install. The table is real
-- here anyway: the store is dialect-neutral and its conformance suite proves the
-- atomic-consume guarantee against both backends with the same assertions.
--
-- user_id is a soft reference with no foreign key, the same shape
-- users.last_acting_team_id takes: the callback compares it against the
-- returning session's own subject, so an id naming nobody can only fail the
-- comparison — it is never dereferenced, and a cascade would buy nothing a
-- fifteen-minute expiry does not.
--
-- Timestamps are written from Go on every insert with no CURRENT_TIMESTAMP
-- default, for the reason github_webhook_deliveries states next door: this
-- driver renders a bound time.Time in Go's own string form while
-- CURRENT_TIMESTAMP renders 'YYYY-MM-DD HH:MM:SS', and the consume compares
-- these columns against another bound time.Time. One writer, one format.
CREATE TABLE github_pending_binds (
    nonce_hash  TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL,
    expires_at  TIMESTAMP NOT NULL,
    consumed_at TIMESTAMP
);

-- Serves the prune, the only query that doesn't go through the primary key.
CREATE INDEX github_pending_binds_expires_at_idx
    ON github_pending_binds (expires_at);

-- +goose Down
SELECT 'down not supported';
