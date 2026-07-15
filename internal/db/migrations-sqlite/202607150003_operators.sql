-- +goose Up
-- operators: the deployment-operator identity — the CLI-managed flag that
-- gates the fleet console (fleet administration is deployment-scoped, so an
-- operator is org-less: it is not a member role). Managed only through
-- `triagefactory operator add|remove|list <email>`; the bootstrap trust
-- boundary is shell access to the deployment, the same boundary as jwk-init.
--
-- Keyed on the lower-cased email rather than a user id on purpose: the email
-- is what the CLI names, it is present on every verified session (claims.Email
-- from GoTrue), and keying on it lets an operator be authorized before their
-- first login (no user row exists yet) and survive user-row churn. The gate
-- is a plain lookup of the request's verified email against this table.
--
-- System table, admin-pool-only in Postgres (an operator is deployment
-- config, not tenant data); SQLite is unscoped N=1 (the single local user is
-- implicitly the operator, so this table is effectively unused in local mode).
CREATE TABLE operators (
    email     TEXT PRIMARY KEY,
    added_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- added_by: the OS user that ran the CLI, best-effort provenance for the
    -- `operator list` read-out. Not an authorization input.
    added_by  TEXT
);

-- +goose Down
SELECT 'down not supported';
