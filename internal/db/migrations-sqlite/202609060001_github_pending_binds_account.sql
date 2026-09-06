-- +goose Up
-- The GitHub login an admin named when connecting an account that already has
-- the deployment App installed. GitHub's install page never returns to the
-- callback for such an account, so that ceremony runs over the OAuth authorize
-- leg instead and the callback has to learn which account it is about from
-- this row — the only place the name is carried, never a URL. '' is a ceremony
-- that goes through the install page and learns its installation from GitHub's
-- redirect.
ALTER TABLE github_pending_binds ADD COLUMN account_login TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'down not supported';
