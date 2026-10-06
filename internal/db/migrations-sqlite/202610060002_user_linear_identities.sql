-- +goose Up
-- user_linear_identities — workspace-scoped Linear identity bindings, the
-- Linear sibling of user_github_identities / user_jira_identities.
--
-- Linear is cloud-only, so the scope a person's identity lives in is not a
-- host but a workspace: one human holds an independent, workspace-scoped User
-- record in every Linear workspace they belong to, and a user id from one
-- workspace means nothing in another. The natural key is therefore (user_id,
-- workspace_id), where workspace_id is the Linear organization id of the
-- workspace the org's Linear credential belongs to — learned from that
-- credential, never typed. The per-user Linear credential is custodied under
-- the same id ("linear_token/<workspace_id>"), so identity and access are
-- keyed alike.
--
-- linear_user_id is the workspace-scoped User UUID (viewer.id), the match key
-- for assignee and author predicates; display_name is viewer.displayName.
-- source records how the binding was captured: 'api_key' (a pasted personal
-- API key — Linear has no PAT), 'connect_oauth' (one-click Connect), or
-- 'scim' (reserved; no writer). The value set is closed because identity
-- provenance is security-relevant. verified_at is the last authenticated
-- viewer confirmation, nullable for a directory sync that learns an identity
-- without one. An absent row is a supported state: the reader degrades
-- exactly as it does for the GitHub and Jira siblings.
CREATE TABLE user_linear_identities (
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id   TEXT NOT NULL,
    linear_user_id TEXT NOT NULL,
    display_name   TEXT,
    source         TEXT NOT NULL
                       CHECK (source IN ('api_key', 'connect_oauth', 'scim')),
    verified_at    TIMESTAMP,
    created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, workspace_id)
);

-- +goose Down
SELECT 'down not supported';
