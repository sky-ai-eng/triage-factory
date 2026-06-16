-- +goose Up
-- Widen user_jira_identities.source to admit 'cloud_api_token' — the provenance
-- marker the Cloud per-user API-token bind records (the paste counterpart of the
-- Data Center 'pat'). The value set stays closed (identity provenance is
-- security-relevant); this only adds the one Cloud paste method. Cloud OAuth
-- ('connect_oauth') was already allowed by the baseline.
--
-- SQLite can't ALTER a CHECK constraint in place, so rebuild the table and copy
-- rows. Nothing references user_jira_identities, so no child FK has to be
-- toggled; the table's own FK to users(id) is preserved in the new definition.
CREATE TABLE user_jira_identities_new (
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    jira_base_url TEXT NOT NULL,
    account_id    TEXT NOT NULL,
    display_name  TEXT,
    source        TEXT NOT NULL
                      CHECK (source IN ('pat', 'connect_oauth', 'scim', 'cloud_api_token')),
    verified_at   TIMESTAMP,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, jira_base_url)
);

INSERT INTO user_jira_identities_new
    (user_id, jira_base_url, account_id, display_name, source, verified_at, created_at, updated_at)
SELECT user_id, jira_base_url, account_id, display_name, source, verified_at, created_at, updated_at
FROM user_jira_identities;

DROP TABLE user_jira_identities;
ALTER TABLE user_jira_identities_new RENAME TO user_jira_identities;

-- +goose Down
SELECT 'down not supported';
