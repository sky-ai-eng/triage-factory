-- +goose Up
-- Per-org Atlassian OAuth (3LO) app registration — the (client_id,
-- client_secret) of the Atlassian app the per-user "Connect Jira"
-- authorize/token flow runs against. Mirrors org_github_apps but far simpler:
-- an Atlassian OAuth app has no installations, no PEM, and no webhook secret,
-- only the OAuth client credentials.
--
-- This row is the per-org OVERRIDE in credential precedence: an org with no
-- row falls back to the deployment app (multi) or has no app at all (local —
-- where the BYO app IS this row). client_secret_ref is a pointer into the
-- secret store (the OS keychain in local mode); the secret itself never lives
-- in this table, the same shape org_github_apps uses.
CREATE TABLE org_jira_apps (
    org_id                 TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    client_id              TEXT NOT NULL,
    client_secret_ref      TEXT NOT NULL,
    registered_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    registered_by_user_id  TEXT REFERENCES users(id) ON DELETE SET NULL
);

-- +goose Down
SELECT 'down not supported';
