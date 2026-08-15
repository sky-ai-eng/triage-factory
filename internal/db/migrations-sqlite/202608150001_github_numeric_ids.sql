-- +goose Up
-- GitHub's numeric ids for the two things credential resolution and identity
-- binding key on. Both are stored as TEXT, matching how every other
-- GitHub-issued id already lands in this schema (entities.source_id,
-- org_github_app_installations.installation_id) rather than introducing a
-- second convention for the same kind of value.
--
-- Both are nullable, and NULL is a supported state rather than a gap to
-- backfill in bulk: a row written before this existed resolves exactly as it
-- did (by login), and the next reconcile or webhook that names the account
-- fills the id in.

-- The account the App is installed on. A GitHub account login is renameable
-- and the numeric id is not, so the id is what an installation match should
-- turn on; account_login stays as the field the UI renders, kept current by
-- the same writers.
ALTER TABLE org_github_app_installations ADD COLUMN account_id TEXT;

-- The authenticating human's GitHub account, same reasoning: login records
-- what they are currently called, github_user_id records who they are.
ALTER TABLE user_github_identities ADD COLUMN github_user_id TEXT;

-- Serves the reverse (host, login) → user lookup the routing subscribers run.
-- lower(login) because GitHub logins are case-insensitive while the writers
-- persist them as captured, so a verbatim compare silently misses on
-- capitalisation. github_base_url leads: identity is host-scoped, so every
-- lookup pins it.
CREATE INDEX user_github_identities_login_lookup_idx
    ON user_github_identities (github_base_url, lower(login));

-- +goose Down
SELECT 'down not supported';
