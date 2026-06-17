-- +goose Up
--
-- TFAC-402: replace Supabase Vault / pgsodium secret storage with
-- app-layer AES-256-GCM. The Vault root key lives in the postgres
-- container filesystem (/etc/postgresql-custom/pgsodium_root.key), NOT
-- on the pg-data volume, so any container recreate regenerates it and
-- renders every previously-stored secret permanently undecryptable —
-- operator data-loss on a routine `docker compose down/up`.
--
-- The fix is structural: stop delegating secret encryption to Postgres.
-- The TF binary now encrypts org/user integration secrets app-side with
-- TF_SECRET_ENCRYPTION_KEY (a .env key, same model as
-- TF_SESSION_ENCRYPTION_KEY) and stores opaque ciphertext in this normal
-- RLS-gated table. A DB dump then yields only AES ciphertext, and there
-- is no Postgres-side key left to lose.
--
-- Fresh-installs-only (multi mode hasn't shipped): no data migration. The
-- supabase_vault / pgsodium extensions stay loaded (image-managed,
-- harmless) — TF simply stops using them, so the vault_* wrapper
-- functions are dropped at the bottom of this migration.

CREATE TABLE public.org_secrets (
    org_id      uuid NOT NULL REFERENCES public.orgs(id) ON DELETE CASCADE,
    -- NULL for org-scope secrets; the owning user's id for per-user
    -- secrets (the Jira "act as yourself" credential). The two scopes
    -- share one table, discriminated by user_id, mirroring the
    -- vault_*_org_secret / vault_*_user_secret split.
    user_id     uuid NULL REFERENCES public.users(id) ON DELETE CASCADE,
    key         text NOT NULL,
    -- AES-256-GCM ciphertext + its 12-byte nonce, produced by
    -- internal/aead. Stored as separate columns so the schema is
    -- self-describing (the nonce is not prefixed onto the ciphertext).
    ciphertext  bytea NOT NULL,
    nonce       bytea NOT NULL,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- At most one row per (scope, key). user_id NULL (org scope) collapses to
-- the all-zero sentinel UUID so org-scope and per-user rows can't collide
-- and a partial unique index is unnecessary. The Put/PutUser UPSERTs name
-- this exact expression in their ON CONFLICT target.
CREATE UNIQUE INDEX org_secrets_scope_key_uniq
    ON public.org_secrets (
        org_id,
        COALESCE(user_id, '00000000-0000-0000-0000-000000000000'::uuid),
        key
    );

ALTER TABLE public.org_secrets ENABLE ROW LEVEL SECURITY;

-- Two permissive policies, scoped to tf_app, that reproduce the dropped
-- vault_* gates exactly. Permissive policies combine with OR, so the
-- effective gate is:
--   (org-scope row AND org matches) OR (own user-scope row AND org matches)
-- The admin pool (supabase_admin) bypasses RLS entirely and is the only
-- path to the *System reads/writes — it trusts the explicit org_id /
-- user_id args, exactly as vault_get_org_secret_system did.

-- Org-scope rows (user_id IS NULL): gate on org only, mirroring
-- vault_get_org_secret's `p_org_id = current_org_id()` check.
CREATE POLICY org_secrets_org ON public.org_secrets
    FOR ALL
    TO tf_app
    USING (user_id IS NULL AND org_id = tf.current_org_id())
    WITH CHECK (user_id IS NULL AND org_id = tf.current_org_id());

-- Per-user rows: gate on org AND user, mirroring vault_get_user_secret's
-- additional `p_user_id = current_user_id()` check. This is the
-- load-bearing cross-user boundary — a session acting as user A cannot
-- see user B's secret.
CREATE POLICY org_secrets_user ON public.org_secrets
    FOR ALL
    TO tf_app
    USING (user_id = tf.current_user_id() AND org_id = tf.current_org_id())
    WITH CHECK (user_id = tf.current_user_id() AND org_id = tf.current_org_id());

-- supabase_admin's ALTER DEFAULT PRIVILEGES auto-grants public-schema
-- tables to anon/authenticated/service_role at CREATE time. Strip them: a
-- secrets table must be reachable only by tf_app, even if RLS were ever
-- misconfigured (the dropped vault_* wrappers were REVOKE'd from these
-- roles for exactly this reason). supabase_admin (admin pool) owns the
-- table as superuser and bypasses RLS for the *System methods.
REVOKE ALL ON public.org_secrets FROM PUBLIC;
REVOKE ALL ON public.org_secrets FROM anon, authenticated, service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.org_secrets TO tf_app;

-- Vault / pgsodium wrappers are dead after the rewrite. Drop all nine
-- (4 org + 5 user, including the two _system variants). IF EXISTS so the
-- migration expresses "ensure these are gone" idempotently — it won't
-- fail on a divergent DB where one was already removed. The
-- supabase_vault / pgsodium extensions and the vault schema stay (image-
-- managed); only TF's wrapper functions go.
DROP FUNCTION IF EXISTS public.vault_put_org_secret(uuid, text, text, text);
DROP FUNCTION IF EXISTS public.vault_get_org_secret(uuid, text);
DROP FUNCTION IF EXISTS public.vault_get_org_secret_system(uuid, text);
DROP FUNCTION IF EXISTS public.vault_delete_org_secret(uuid, text);
DROP FUNCTION IF EXISTS public.vault_put_user_secret(uuid, uuid, text, text, text);
DROP FUNCTION IF EXISTS public.vault_get_user_secret(uuid, uuid, text);
DROP FUNCTION IF EXISTS public.vault_get_user_secret_system(uuid, uuid, text);
DROP FUNCTION IF EXISTS public.vault_delete_user_secret(uuid, uuid, text);
DROP FUNCTION IF EXISTS public.vault_put_user_secret_system(uuid, uuid, text, text, text);

-- +goose Down
SELECT 'down not supported';
