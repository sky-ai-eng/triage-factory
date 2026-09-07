-- +goose Up
-- Which return the callback expects for a pending-bind record: 'authorize'
-- (GitHub's OAuth authorize, code and state) or 'install' (GitHub's install
-- redirect, code and installation_id). Every ceremony now starts on the
-- authorize leg with the account the admin named, and reaches the install leg
-- only from inside it, for an account found without the App — so the record
-- has to say which return it is waiting for, and the query string never gets
-- to. App-validated.
ALTER TABLE github_pending_binds ADD COLUMN leg TEXT NOT NULL DEFAULT 'authorize';

-- A row with no account was minted for the install leg: the only records
-- written without one are those, so the column says so rather than carrying
-- the default for a leg the row never was.
UPDATE github_pending_binds SET leg = 'install' WHERE account_login = '';

-- +goose Down
SELECT 'down not supported';
