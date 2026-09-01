-- +goose Up
-- An installation belongs to exactly one workspace per GitHub deployment. The
-- grant is not divisible, `installation.deleted` fires once, the rate budget is
-- shared, and the account owner cannot enumerate who rides their installation:
-- one installation in two workspaces is not a state anything downstream can
-- represent, let alone unwind.
--
-- The policy needs an index because the credential no longer carries it. Where a
-- workspace owns its own App key, that key IS the boundary — it lists the
-- installations of its own App and no others — so nothing can claim an
-- installation it was not granted. A deployment-level App, one key serving many
-- workspaces, has no such property, and this index is what stands in its place:
-- the backstop under the reconcile scoping that keeps a managed workspace's
-- refresh to installations it has already bound. An advisory lock substitutes
-- for neither — it orders writes, it does not validate them.
--
-- github_host is in the key and must stay. GitHub numbers installations per
-- deployment rather than universally, so a self-host aggregating orgs across two
-- GHES instances legitimately sees id 456 twice, meaning two unrelated
-- installations; keying on installation_id alone would refuse the second.
--
-- Partial on removed_at IS NULL, like the active-account index beside it. An
-- uninstalled installation reaches nothing and therefore holds nothing, so
-- binding that id in another workspace is an ordinary bind, not a collision.
--
-- Adding a unique index to a populated SQLite table fails if duplicates are
-- present. There can be none here: producing one takes two orgs holding one
-- installation on one host, and local mode is single-org. Nothing de-duplicates
-- ahead of this statement on purpose — a de-duplicating pass could only silently
-- destroy a row that a failure would have surfaced.
CREATE UNIQUE INDEX org_github_app_installations_active_host_installation_key
    ON org_github_app_installations (github_host, installation_id)
    WHERE removed_at IS NULL;

-- +goose Down
SELECT 'down not supported';
