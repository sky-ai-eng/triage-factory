-- +goose Up
--
-- TFAC-425 (epic TFAC-422): the TF-owned SSO binding tables. Two tables,
-- both multi-org from the start because app.triagefactory.com runs the
-- identical image as a self-host and its orgs are mutually-distrusting
-- paying customers:
--
--   - sso_connections — an org↔IdP-provider binding. Protocol-agnostic
--     core (SAML today, OIDC a sibling later). The provider_id column is
--     the ONE bridge to GoTrue (its sso_providers.id, a UUID — confirmed
--     by the TFAC-423 spike). TF owns authorization (the org + default
--     role); GoTrue owns authN + the provider registry. The binding lives
--     HERE, not in auth.users.app_metadata, because RLS reads roles from
--     TF tables, roles are per-(user,org), and authz writes must be
--     transactional.
--
--   - sso_domains — verified email domains used to route an identifier-
--     first login (email → connection). DNS-TXT verification (the WorkOS
--     pattern); the token column is published publicly by design. The
--     load-bearing isolation guarantee is sso_domains_verified_global_uniq:
--     a *verified* domain belongs to at most one org across the whole
--     deployment. Pending claims are non-exclusive; first-to-verify wins.
--
-- Postgres only: SSO is a multi-mode concept. Local mode (N=1) never
-- registers a connection, so there is no SQLite twin — the SQLite store
-- impls are stubs returning ErrNotApplicableInLocal.
--
-- Follows the org_invites / org_secrets style: goose Up/Down, Down is a
-- no-op, public-schema default grants stripped down to tf_app + the admin
-- pool.

CREATE TABLE public.sso_connections (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid NOT NULL REFERENCES public.orgs(id) ON DELETE CASCADE,
    kind         text NOT NULL DEFAULT 'saml',
    -- GoTrue sso_providers.id (a UUID), stored as text: TF treats it as an
    -- opaque handle — no FK into GoTrue's schema, no UUID operations on it.
    -- This is the ONLY bridge to GoTrue; everything else (org, role) is
    -- TF-owned. Globally unique (the index below) so one GoTrue provider
    -- maps to exactly one org — the JIT isolation boundary (TFAC-426).
    provider_id  text NOT NULL,
    -- The role JIT provisioning grants a first-time SSO user. Everyone
    -- starts 'member'; promotion is manual via the roster (TFAC-417). The
    -- CHECK encodes the ceiling at the schema: 'owner' is the orgs.owner_
    -- user_id sentinel, never granted via SSO.
    default_role public.org_role NOT NULL DEFAULT 'member',
    enabled      boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT sso_connections_kind_chk       CHECK (kind IN ('saml','oidc')),
    CONSTRAINT sso_connections_role_not_owner CHECK (default_role <> 'owner')
);

-- One org per GoTrue provider, deployment-wide. The login-time JIT read
-- (GetByProviderID) looks up by this key; uniqueness is what makes
-- "provider_id → org" a function, not a relation.
CREATE UNIQUE INDEX sso_connections_provider_uniq ON public.sso_connections (provider_id);

-- (id, org_id) is trivially unique (id is already the PK), declared so
-- sso_domains can FK the *pair* and pin a domain to its connection's own org
-- at the schema level — the same (id, org_id)-unique + composite-FK pattern
-- the rest of the schema uses (agents, entities, runs, tasks, blueprint_runs,
-- teams). RLS gates the org_id *column* on write, but only this FK keeps a
-- domain's connection_id in the same org as its org_id.
ALTER TABLE public.sso_connections ADD CONSTRAINT sso_connections_id_org_id_key UNIQUE (id, org_id);

CREATE TABLE public.sso_domains (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- connection_id carries no standalone FK: it's the lead column of the
    -- composite FK below, which ties (connection_id, org_id) to the parent's
    -- (id, org_id) so a domain can only ever point at a connection in its own
    -- org.
    connection_id uuid NOT NULL,
    -- Denormalized from the parent connection for two reasons: the RLS
    -- policies gate directly on org_id (no join to the parent on every row
    -- check), and the routing read carries it without a second hop. RLS's
    -- INSERT WITH CHECK pins this column to tf.current_org_id() (the writer's
    -- own org) — but that alone does NOT stop a self-org row from pointing
    -- connection_id at another org's connection (a UUID RLS hides but which
    -- an admin/system writer could still supply). The composite FK below is
    -- what closes that gap: it's the load-bearing cross-org guard, and it
    -- holds on every write path including any future admin-pool / JIT / SCIM
    -- writer, the same backstop org_invites' composite target_team FK gives.
    org_id        uuid NOT NULL REFERENCES public.orgs(id) ON DELETE CASCADE,
    domain        text NOT NULL,
    -- DNS-TXT verification token (_triagefactory-verification.<domain>).
    -- Published publicly by design — it proves control of the DNS zone,
    -- it is not a secret.
    token         text NOT NULL,
    -- NULL until the DNS-TXT check passes (TFAC-428). A row is inert until
    -- verified: pending claims don't route and don't reserve the domain.
    verified_at   timestamptz NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT sso_domains_domain_nonempty CHECK (domain <> ''),
    -- Tenant-scoped composite FK: a domain's (connection_id, org_id) must
    -- match a real sso_connections (id, org_id) pair, so connection_id is
    -- forced into the same org as org_id. Both columns are NOT NULL, so
    -- MATCH SIMPLE always checks. ON DELETE CASCADE retires a connection's
    -- domains with it (the behavior the old single-column FK had).
    CONSTRAINT sso_domains_connection_fkey
        FOREIGN KEY (connection_id, org_id) REFERENCES public.sso_connections (id, org_id)
        ON DELETE CASCADE
);

-- One pending/active claim per (org, domain) — dedups a re-claim within
-- an org. Case-insensitive (lower(domain)) because domains are.
CREATE UNIQUE INDEX sso_domains_org_domain_uniq ON public.sso_domains (org_id, lower(domain));

-- THE isolation guarantee: a VERIFIED domain belongs to <=1 org across the
-- whole deployment. Pending claims are non-exclusive (two orgs can both
-- hold a pending row for the same domain via the per-org index above);
-- first-to-verify wins, and the loser's set-verified trips THIS partial
-- index. Enforced at the index/heap level regardless of RLS, so the
-- guarantee holds even though neither org can SELECT the other's row — the
-- collision is opaque (a generic unique violation; the routing/claim
-- handlers map it to a "domain already claimed" without naming the holder).
CREATE UNIQUE INDEX sso_domains_verified_global_uniq
    ON public.sso_domains (lower(domain)) WHERE verified_at IS NOT NULL;

-- BEFORE-UPDATE triggers keep updated_at fresh, matching every other table.
CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.sso_connections
    FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.sso_domains
    FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();

ALTER TABLE public.sso_connections ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sso_domains ENABLE ROW LEVEL SECURITY;

-- Management surface (the TFAC-424 connection CRUD + TFAC-428 domain
-- claim/verify) is org-admin-gated in the current org, mirroring
-- org_invites / org_memberships. SELECT is admin-gated too (an SSO config
-- is an admin concern, not a member-visible one).
--
-- DELETE has its own policy because both stores expose a delete primitive
-- (domain removal; connection removal) that runs on the app pool — without
-- a DELETE policy those would silently affect zero rows under RLS.
--
-- NOT expressible in RLS, and deliberately absent here: the two login-time
-- reads. sso_connections.GetByProviderID (JIT, TFAC-426) and
-- sso_domains.GetVerifiedByDomain (routing, TFAC-427) cross the tenant
-- boundary — the actor has no membership yet — so they run on the admin
-- pool (BYPASSRLS), with the provider_id / verified domain itself as the
-- lookup authorization, not an RLS predicate.

CREATE POLICY sso_connections_select ON public.sso_connections FOR SELECT
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY sso_connections_insert ON public.sso_connections FOR INSERT
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY sso_connections_update ON public.sso_connections FOR UPDATE
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY sso_connections_delete ON public.sso_connections FOR DELETE
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY sso_domains_select ON public.sso_domains FOR SELECT
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY sso_domains_insert ON public.sso_domains FOR INSERT
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY sso_domains_update ON public.sso_domains FOR UPDATE
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY sso_domains_delete ON public.sso_domains FOR DELETE
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

-- supabase_admin's ALTER DEFAULT PRIVILEGES auto-grants public-schema
-- tables to anon/authenticated/service_role at CREATE time. Strip them so
-- each table is reachable only by tf_app (under RLS) and the admin pool
-- (which owns it as superuser and bypasses RLS for the login-time reads) —
-- same posture as org_invites / org_secrets.
REVOKE ALL ON public.sso_connections FROM PUBLIC;
REVOKE ALL ON public.sso_connections FROM anon, authenticated, service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.sso_connections TO tf_app;

REVOKE ALL ON public.sso_domains FROM PUBLIC;
REVOKE ALL ON public.sso_domains FROM anon, authenticated, service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.sso_domains TO tf_app;

-- +goose Down
SELECT 'down not supported';
