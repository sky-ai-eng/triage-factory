-- +goose Up
-- The GitHub deployment an installation lives on. A numeric installation id is
-- unique within one GitHub deployment, not universally: a self-host
-- aggregating orgs across two GHES instances can see the same id twice,
-- meaning two unrelated installations. The composite PK already assumes this
-- ("per-org GHES bases" in the baseline), but the row itself could not say
-- which GitHub it came from — the host was reachable only by joining the
-- owning org's org_settings.github_base_url, which is one join too many for
-- any comparison that spans orgs.
--
-- Normalized exactly the way every other GitHub host key in this schema is
-- (EffectiveGitHubHost, what user_github_identities.github_base_url stores):
-- trailing slashes trimmed, an unconfigured base URL resolving to
-- https://github.com, and any path component KEPT — a bare-origin derivation
-- would strip a GHES mount path and key a different host than the identity
-- rows for that same GitHub.
--
-- The DEFAULT is that rule rather than a placeholder: an org that configured
-- no base URL is on github.com, so a row written without a host is a
-- github.com row. The CHECK keeps the column from degrading into the empty
-- string, which is not a host anything can be compared against.
ALTER TABLE org_github_app_installations
    ADD COLUMN github_host TEXT NOT NULL DEFAULT 'https://github.com'
        CHECK (github_host <> '');

-- Backfill from the owning org's configured base URL under that same
-- normalization: rtrim of trailing slashes, and an unset or empty setting
-- resolving to the public host. An org row that somehow has no org_settings
-- row lands on the public host too, which is what the resolver would have
-- answered for it anyway.
UPDATE org_github_app_installations
   SET github_host = COALESCE(
           NULLIF(
               rtrim((SELECT s.github_base_url
                        FROM org_settings s
                       WHERE s.org_id = org_github_app_installations.org_id), '/'),
               ''),
           'https://github.com');

-- What this column deliberately does NOT get is a
-- UNIQUE (github_host, installation_id) index.
--
-- The policy it would enforce is real: an installation belongs to exactly one
-- workspace per host. The grant is not divisible, `installation.deleted` fires
-- once, the rate budget is shared, and the account owner cannot enumerate who
-- rides their installation. What makes it unenforceable-by-index today is
-- structural rather than caution — every workspace owns its own App key, so
-- GET /app/installations under one org's key returns only that org's
-- installations, and an upsert runs only for a delivery HMAC-verified against
-- that org's own webhook secret. No write path exists by which one workspace
-- claims another's installation id, and a row that somehow claimed one would
-- be inert: the claiming org's App key cannot mint a token against an
-- installation it does not own, so GitHub answers 404.
--
-- The index becomes load-bearing the moment a deployment-level App exists,
-- because one PEM then lists every tenant's installations. It has to land
-- there PAIRED with scoping that reconcile to installations the org actually
-- bound — an advisory lock is not a substitute, because the failure to prevent
-- is a background job attributing every tenant's installation to one org, and
-- a lock orders writes rather than validating them.
-- TODO(TFAC-802): ship the uniqueness index together with a scoped reconcile.

-- +goose Down
SELECT 'down not supported';
