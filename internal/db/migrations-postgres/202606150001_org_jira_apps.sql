-- +goose Up
-- Per-org Atlassian OAuth (3LO) app registration — the (client_id,
-- client_secret) of the Atlassian app the per-user "Connect Jira"
-- authorize/token flow runs against. Mirrors org_github_apps but far simpler:
-- an Atlassian OAuth app has no installations, no PEM, and no webhook secret,
-- only the OAuth client credentials.
--
-- This row is the per-org OVERRIDE in credential precedence: an org with no
-- row uses the deployment first-party app (read from operator config). The
-- client_secret_ref column holds a Vault secret-name pointer (the actual
-- client_secret is written via the vault helpers) so the app secret never
-- lives in the relational schema — same discipline as org_github_apps.

CREATE TABLE public.org_jira_apps (
    org_id uuid NOT NULL,
    client_id text NOT NULL,
    client_secret_ref text NOT NULL,
    registered_at timestamp with time zone DEFAULT now() NOT NULL,
    registered_by_user_id uuid
);

ALTER TABLE ONLY public.org_jira_apps
    ADD CONSTRAINT org_jira_apps_pkey PRIMARY KEY (org_id);

ALTER TABLE ONLY public.org_jira_apps
    ADD CONSTRAINT org_jira_apps_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.org_jira_apps
    ADD CONSTRAINT org_jira_apps_registered_by_user_id_fkey FOREIGN KEY (registered_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;

ALTER TABLE public.org_jira_apps ENABLE ROW LEVEL SECURITY;

-- Writes are admin-only (registering / rotating an OAuth app is a sensitive
-- workspace gesture). Reads open to any org member so the OAuth-app resolver
-- running on the app pool can see whether the org has its own app on the read
-- path. Mirrors the org_github_apps policies.
CREATE POLICY org_jira_apps_select ON public.org_jira_apps FOR SELECT TO tf_app
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));

CREATE POLICY org_jira_apps_insert ON public.org_jira_apps FOR INSERT TO tf_app
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

CREATE POLICY org_jira_apps_update ON public.org_jira_apps FOR UPDATE TO tf_app
    USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)))
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

CREATE POLICY org_jira_apps_delete ON public.org_jira_apps FOR DELETE TO tf_app
    USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

GRANT ALL ON TABLE public.org_jira_apps TO postgres;
GRANT ALL ON TABLE public.org_jira_apps TO anon;
GRANT ALL ON TABLE public.org_jira_apps TO authenticated;
GRANT ALL ON TABLE public.org_jira_apps TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.org_jira_apps TO tf_app;

-- +goose Down
SELECT 'down not supported';
