-- +goose Up
--
-- TFAC-416: TF-owned, link-based org invites. An org admin creates an
-- invite (email + role + optional target team) and gets back a one-time
-- accept_url; the recipient authenticates (GitHub OAuth today, SSO later)
-- and redeems the raw token, which mints their org_memberships row via the
-- shared grantOrgMembership primitive. GoTrue stays a pure identity
-- provider — TF owns the org/role/team binding.
--
-- Postgres only: local mode (N=1) never mounts the invite routes, so there
-- is no SQLite twin.
--
-- The raw token is NEVER stored. create-invite generates 32 random bytes,
-- hands the base64url token back once in the accept_url, and persists only
-- sha256(token) in token_hash. Redeem hashes the presented token and looks
-- it up by that hash.

CREATE TABLE public.org_invites (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         uuid NOT NULL REFERENCES public.orgs(id)  ON DELETE CASCADE,
    email          text NOT NULL,               -- stored lower-cased (app normalizes)
    role           public.org_role NOT NULL DEFAULT 'member',
    target_team_id uuid NULL REFERENCES public.teams(id) ON DELETE SET NULL, -- NULL = org-only
    token_hash     bytea NOT NULL,              -- sha256(raw token); raw token is NEVER stored
    invited_by     uuid NULL REFERENCES public.users(id) ON DELETE SET NULL, -- audit only
    expires_at     timestamptz NOT NULL,
    accepted_at    timestamptz NULL,
    accepted_by    uuid NULL REFERENCES public.users(id) ON DELETE SET NULL, -- who redeemed (audit)
    revoked_at     timestamptz NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT org_invites_email_nonempty CHECK (email <> ''),
    -- owner is the orgs.owner_user_id sentinel, managed by the ownership-transfer
    -- ticket (#5), never granted via invite. Encodes the role ceiling at the schema.
    CONSTRAINT org_invites_role_not_owner CHECK (role <> 'owner')
);

-- Redeem lookup is by token hash.
CREATE UNIQUE INDEX org_invites_token_hash_uniq ON public.org_invites (token_hash);

-- At most ONE active (un-accepted, un-revoked) invite per (org, email). Re-inviting
-- an accepted/revoked address is allowed (a new row); a second *pending* invite to
-- the same address conflicts. Matches the tasks-dedup partial-unique pattern.
CREATE UNIQUE INDEX org_invites_active_uniq
    ON public.org_invites (org_id, lower(email))
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

-- BEFORE-UPDATE trigger keeps updated_at fresh, matching other tables.
CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.org_invites
    FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();

ALTER TABLE public.org_invites ENABLE ROW LEVEL SECURITY;

-- The admin-facing surface (create/list/revoke) is gated to org-admin in
-- the current org, mirroring the org_memberships admin policies. The redeem
-- surface (preview/accept) is NOT expressible in RLS — the actor is an
-- outsider holding a token, with no membership — so those run on the admin
-- pool (BYPASSRLS), with the token itself as the authorization.
CREATE POLICY org_invites_select ON public.org_invites FOR SELECT
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY org_invites_insert ON public.org_invites FOR INSERT
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY org_invites_update ON public.org_invites FOR UPDATE
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

-- supabase_admin's ALTER DEFAULT PRIVILEGES auto-grants public-schema
-- tables to anon/authenticated/service_role at CREATE time. Strip them so
-- the table is reachable only by tf_app (under RLS) and the admin pool
-- (which owns it as superuser and bypasses RLS for the redeem paths) —
-- same posture as org_secrets.
REVOKE ALL ON public.org_invites FROM PUBLIC;
REVOKE ALL ON public.org_invites FROM anon, authenticated, service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.org_invites TO tf_app;

-- +goose Down
SELECT 'down not supported';
