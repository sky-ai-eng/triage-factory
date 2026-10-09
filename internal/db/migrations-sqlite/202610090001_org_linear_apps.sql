-- +goose Up
-- org_linear_apps — the org's own Linear OAuth app, the Linear sibling of
-- org_jira_apps. The install ceremony and the per-user Connect run against
-- it. The row is the per-org override in the app precedence: an org with no
-- row falls back to the deployment app, or has none. client_secret_ref names
-- the org secret "linear_oauth_client_secret"; the secret never lives here.
CREATE TABLE org_linear_apps (
    org_id                TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    client_id             TEXT NOT NULL,
    client_secret_ref     TEXT NOT NULL,
    registered_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    registered_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL
);

-- org_linear_installs — one row per org: the Linear workspace that installed
-- the org's resolved app (actor=app), whichever app that was. The refresh
-- token lives in the org secret "linear_app_install"; this row is the
-- queryable half.
--
-- install_id is minted per install and stored in the secret too, so a writer
-- holding one names exactly the install it read and cannot remove a newer one.
-- app_user_id is viewer.id under the app token, the identity TF acts as in the
-- workspace. app_client_id is the app that minted the install: a refresh
-- needs that app's secret. installed_by_user_id is a soft reference, so the
-- row outlives the admin who installed it. removed_reason is set exactly when
-- removed_at is.
CREATE TABLE org_linear_installs (
    org_id               TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    install_id           TEXT NOT NULL CHECK (install_id <> ''),
    workspace_id         TEXT NOT NULL,
    workspace_url_key    TEXT NOT NULL,
    app_user_id          TEXT NOT NULL,
    app_client_id        TEXT NOT NULL,
    installed_by_user_id TEXT,
    installed_at         TIMESTAMP NOT NULL,
    removed_at           TIMESTAMP,
    removed_reason       TEXT
                             CHECK (removed_reason IN ('disconnected', 'install_revoked', 'install_failed')),
    CHECK ((removed_at IS NULL) = (removed_reason IS NULL))
);

-- One Linear workspace installs into at most one TF org. The credential is a
-- per-workspace token, so nothing else can enforce it. A removed install holds
-- nothing, so the workspace can be installed again, here or elsewhere.
CREATE UNIQUE INDEX org_linear_installs_workspace_live
    ON org_linear_installs (workspace_id) WHERE removed_at IS NULL;

-- +goose Down
SELECT 'down not supported';
