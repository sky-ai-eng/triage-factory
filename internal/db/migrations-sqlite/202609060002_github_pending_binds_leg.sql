-- +goose Up
-- Which return the callback expects for a pending-bind record: 'authorize'
-- (GitHub's OAuth authorize, code and state) or 'install' (GitHub's install
-- redirect, code and installation_id). Every ceremony now starts on the
-- authorize leg with the account the admin named, and reaches the install leg
-- only from inside it, for an account found without the App — so the record
-- has to say which return it is waiting for, and the query string never gets
-- to. App-validated.
ALTER TABLE github_pending_binds ADD COLUMN leg TEXT NOT NULL DEFAULT 'authorize';

-- +goose Down
SELECT 'down not supported';
