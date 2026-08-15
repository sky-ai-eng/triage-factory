-- +goose Up
-- Installation suspension. A GitHub account owner can suspend an App's
-- installation without uninstalling it: the grant stays, every token minted
-- from it is refused, and nothing about the installation row says so. Without
-- these columns a suspended installation is indistinguishable from a working
-- one, and an already-minted installation token stays in the process cache
-- until its natural expiry — up to an hour of 403s on a row that reads as
-- connected.
--
-- Both nullable, and NULL is the ordinary state: an installation is suspended
-- iff suspended_at is non-NULL. Written by the two writers that already
-- maintain this mirror — the installation webhook (suspend / unsuspend) and
-- the /app/installations reconcile, which is the half that converges a
-- deployment that missed a delivery (GitHub does not auto-retry) and the only
-- half that runs at all in local mode.
ALTER TABLE org_github_app_installations ADD COLUMN suspended_at TIMESTAMP;

-- The GitHub login that suspended it, from the payload's suspended_by user
-- object. Display provenance only — nothing resolves on it — so it is text,
-- not a join key, and a payload that names no one leaves it NULL.
ALTER TABLE org_github_app_installations ADD COLUMN suspended_by TEXT;

-- +goose Down
SELECT 'down not supported';
