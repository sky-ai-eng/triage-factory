-- +goose Up
-- At most one active install per account on each GitHub deployment. The key was
-- (org_id, account_login), which treats a login as naming one account
-- everywhere, but a login is unique only within one deployment: an org that
-- moves to another GitHub keeps the old host's rows live, and binding the new
-- host's installation for an account of the same name collided with them.
-- github_host joins the key, as it already keys the installation id.
--
-- Widening a unique index cannot fail on existing rows: every set of rows
-- distinct on (org_id, account_login) is distinct on the wider key too.
DROP INDEX org_github_app_installations_active_account_key;
CREATE UNIQUE INDEX org_github_app_installations_active_account_key
    ON org_github_app_installations (org_id, github_host, account_login)
    WHERE removed_at IS NULL;

-- +goose Down
SELECT 'down not supported';
