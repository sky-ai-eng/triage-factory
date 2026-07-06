-- +goose Up
-- Consolidated Postgres baseline (2026-05-13).
--
-- This file is mechanically regenerated from `pg_dump --schema-only -n public
-- -n tf` of all 14 prior Postgres migrations applied to a fresh supabase
-- testcontainer. It collapses the SKY-247 D3 + SKY-246 D2 + SKY-249 D6/D7/D9
-- migration history into a single fresh-install baseline.
--
-- Brick policy: pre-baseline Postgres installs are refused at boot. (No such
-- installs exist in the wild today — multi-mode hasn't shipped — but the
-- contract is kept consistent with the SQLite baseline so this stays the
-- canonical Postgres entry point.)
--
-- Future Postgres schema changes go in NEW NNN-numbered migration files in
-- this directory. NEVER edit this baseline. Down is a no-op.
--
-- === Deliberate cross-dialect divergence (do NOT "fix" in a later migration) ======
-- This baseline and its SQLite twin
-- (internal/db/migrations-sqlite/202605130001_baseline.sql) differ in the
-- following ways ON PURPOSE. A mechanical (table,col,target,on_delete) or
-- CHECK diff will flag each as a "gap" — it is not. Later migration
-- authors must not converge these:
--
--   1. Type representation. Postgres uses native enum TYPES
--      (public.membership_role, public.org_role) plus uuid / timestamptz /
--      jsonb / interval / array column types; SQLite uses TEXT / INTEGER plus
--      CHECKs. A CHECK-only diff falsely reports PG "missing" the role CHECKs
--      on memberships.role / org_memberships.role — same domain, enum type.
--   2. RLS keys. Postgres carries composite (id, org_id) FKs and (id, org_id)
--      uniques so row-level security can pin tenant scope; SQLite uses
--      single-column FKs (one connection, no RLS). Every *_org_id composite FK
--      / unique here is this category, not a divergence.
--   3. Attribution refs (creator_user_id and the like — the "who authored
--      this" columns). Postgres FK-constrains these with ON DELETE CASCADE;
--      SQLite leaves them bare TEXT (or NO-ACTION on runs). Deliberate: local
--      mode is N=1 and the sentinel user is never deleted. NARROW scope — this
--      does NOT cover identity/ownership user_id PKs (user_settings,
--      user_github/jira_identities, memberships, org_memberships),
--      which carry the users(id) FK in BOTH dialects.
--   4. Tables that exist in one dialect only. instance_config is SQLite-only
--      (host port lives in container env under multi mode). sessions and
--      project_knowledge are Postgres-only (multi-mode auth + shared KB).
--   5. PG-only RLS apparatus. Row-level-security policies, the tf.* helper
--      functions, the auth.users FK, and admin-only columns
--      (orgs.owner_user_id, teams.created_by_user_id, users.default_org_id,
--      sessions.active_org_id) have no SQLite analog.
--   6. blueprint_runs.status. Postgres enforces the value set with a CHECK
--      (cheap to ALTER pre-tenant); SQLite leaves it app-validated (widening a
--      SQLite CHECK means a full-table rebuild, so the absence of the CHECK is
--      the headroom). Both accept the same values; the app is source of truth.
--
-- Target image: supabase/postgres:15.1.0.147 — pre-loads supabase_vault,
-- pgsodium, pgcrypto, pgjwt, uuid-ossp, pg_graphql via
-- shared_preload_libraries, and pre-creates the auth + vault + extensions
-- schemas. gen_random_uuid() lives in pg_catalog on PG 13+; no extension
-- dependency required.

CREATE SCHEMA IF NOT EXISTS tf;

-- Idempotent role creation. The image ships `authenticator` (LOGIN,
-- NOINHERIT); we add `tf_app` (NOLOGIN, NOINHERIT, BYPASSRLS=false) and let
-- authenticator switch into it via SET LOCAL ROLE.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tf_app') THEN
    CREATE ROLE tf_app NOLOGIN NOINHERIT;
  END IF;
END
$$;
-- +goose StatementEnd

GRANT tf_app TO authenticator;

GRANT USAGE ON SCHEMA public, tf TO tf_app;

-- Defensive — the image already loads supabase_vault.
CREATE EXTENSION IF NOT EXISTS supabase_vault WITH SCHEMA vault;

-- pg_dump emits functions before tables (and before the FKs / triggers that
-- close the loop). Some function bodies reference tables that don't exist
-- yet at CREATE FUNCTION time (e.g. tf.team_in_current_org → teams). Tell
-- the planner not to parse-check function bodies during this migration; the
-- bodies are still parsed at first invocation. pg_dump uses the same SET
-- when reloading its own output.
SET check_function_bodies = false;

--
-- Name: membership_role; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.membership_role AS ENUM (
    'admin',
    'member',
    'viewer'
);


--
-- Name: org_role; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.org_role AS ENUM (
    'owner',
    'admin',
    'member'
);


--
-- Name: update_project_knowledge(uuid, integer, text, uuid); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.update_project_knowledge(p_id uuid, p_expected_version integer, p_content text, p_updated_by_run uuid DEFAULT NULL::uuid) RETURNS integer
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_new_version INT;
  v_user_id     UUID := tf.current_user_id();
BEGIN
  IF v_user_id IS NULL THEN
    RAISE EXCEPTION 'no current_user_id (request.jwt.claims unset)'
      USING ERRCODE = '42501';
  END IF;

  -- If a run is being attributed, it must be one the caller can see
  -- through runs RLS (their own, in their current org). A forged
  -- p_updated_by_run from another user fails this check because runs
  -- has SELECT policy `org_id = current_org_id AND creator = current_user`.
  IF p_updated_by_run IS NOT NULL
     AND NOT EXISTS (SELECT 1 FROM runs WHERE id = p_updated_by_run) THEN
    RAISE EXCEPTION 'run % not accessible to caller', p_updated_by_run
      USING ERRCODE = '42501';
  END IF;

  UPDATE project_knowledge
     SET content = p_content,
         version = version + 1,
         last_updated_by = v_user_id,
         last_updated_by_run = p_updated_by_run,
         updated_at = now()
   WHERE id = p_id
     AND version = p_expected_version
  RETURNING version INTO v_new_version;

  IF v_new_version IS NULL THEN
    RAISE EXCEPTION 'concurrent update of project_knowledge %', p_id
      USING ERRCODE = '40001';
  END IF;
  RETURN v_new_version;
END;
$$;
-- +goose StatementEnd


--
-- Name: vault_delete_org_secret(uuid, text); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.vault_delete_org_secret(p_org_id uuid, p_key text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/' || p_key;
  v_existing  UUID;
BEGIN
  -- NULL p_org_id or NULL current_org_id would slip past IS DISTINCT
  -- FROM (both-NULL is "not distinct"). Refuse both explicitly so a
  -- claims-less session can't sneak through.
  IF p_org_id IS NULL OR tf.current_org_id() IS NULL THEN
    RAISE EXCEPTION 'Vault access denied: missing org context (p_org_id or request.jwt.claims.org_id is NULL)'
      USING ERRCODE = '42501';
  END IF;
  IF p_org_id <> tf.current_org_id() THEN
    RAISE EXCEPTION 'cross-org Vault access denied: p_org_id=% does not match request.jwt.claims.org_id', p_org_id
      USING ERRCODE = '42501';
  END IF;
  SELECT id INTO v_existing FROM vault.decrypted_secrets WHERE name = v_full_name;
  IF v_existing IS NULL THEN
    RETURN FALSE;
  END IF;
  DELETE FROM vault.secrets WHERE id = v_existing;
  RETURN TRUE;
END;
$$;
-- +goose StatementEnd


--
-- Name: vault_get_org_secret(uuid, text); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.vault_get_org_secret(p_org_id uuid, p_key text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/' || p_key;
  v_secret    TEXT;
BEGIN
  -- NULL p_org_id or NULL current_org_id would slip past IS DISTINCT
  -- FROM (both-NULL is "not distinct"). Refuse both explicitly so a
  -- claims-less session can't sneak through.
  IF p_org_id IS NULL OR tf.current_org_id() IS NULL THEN
    RAISE EXCEPTION 'Vault access denied: missing org context (p_org_id or request.jwt.claims.org_id is NULL)'
      USING ERRCODE = '42501';
  END IF;
  IF p_org_id <> tf.current_org_id() THEN
    RAISE EXCEPTION 'cross-org Vault access denied: p_org_id=% does not match request.jwt.claims.org_id', p_org_id
      USING ERRCODE = '42501';
  END IF;
  SELECT decrypted_secret INTO v_secret
    FROM vault.decrypted_secrets
   WHERE name = v_full_name;
  RETURN v_secret;
END;
$$;
-- +goose StatementEnd


--
-- Name: vault_get_org_secret_system(uuid, text); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.vault_get_org_secret_system(p_org_id uuid, p_key text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/' || p_key;
  v_secret    TEXT;
BEGIN
  -- System/background read path. No current_org_id() check: p_org_id
  -- is trusted (the EXECUTE grant restricts this to the admin/system
  -- pool — tf_app has none, so a request handler can't reach it; those
  -- use the claims-checked vault_get_org_secret instead). Same
  -- secret-name convention: 'org/<org_id>/<key>'. A NULL p_org_id is a
  -- caller bug, not a privilege failure — refuse it explicitly rather
  -- than silently looking up 'org//<key>'.
  IF p_org_id IS NULL THEN
    RAISE EXCEPTION 'system Vault access denied: p_org_id is NULL'
      USING ERRCODE = '22004';
  END IF;
  SELECT decrypted_secret INTO v_secret
    FROM vault.decrypted_secrets
   WHERE name = v_full_name;
  RETURN v_secret;
END;
$$;
-- +goose StatementEnd


--
-- Name: vault_put_org_secret(uuid, text, text, text); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.vault_put_org_secret(p_org_id uuid, p_key text, p_secret text, p_description text DEFAULT NULL::text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/' || p_key;
  v_existing  UUID;
  -- vault.secrets.description is NOT NULL; coalesce NULL → '' so callers
  -- can pass NULL ergonomically.
  v_desc      TEXT := COALESCE(p_description, '');
BEGIN
  -- DEFINER + arbitrary p_org_id would let any tf_app caller read/write
  -- ANY org's secrets; gate on the JWT-claims org so the wrapper only
  -- ever touches the active session's tenant.
  -- NULL p_org_id or NULL current_org_id would slip past IS DISTINCT
  -- FROM (both-NULL is "not distinct"). Refuse both explicitly so a
  -- claims-less session can't sneak through.
  IF p_org_id IS NULL OR tf.current_org_id() IS NULL THEN
    RAISE EXCEPTION 'Vault access denied: missing org context (p_org_id or request.jwt.claims.org_id is NULL)'
      USING ERRCODE = '42501';
  END IF;
  IF p_org_id <> tf.current_org_id() THEN
    RAISE EXCEPTION 'cross-org Vault access denied: p_org_id=% does not match request.jwt.claims.org_id', p_org_id
      USING ERRCODE = '42501';
  END IF;
  SELECT id INTO v_existing FROM vault.decrypted_secrets WHERE name = v_full_name;
  IF v_existing IS NOT NULL THEN
    PERFORM vault.update_secret(v_existing, p_secret, v_full_name, v_desc);
    RETURN v_existing;
  END IF;
  RETURN vault.create_secret(p_secret, v_full_name, v_desc);
END;
$$;
-- +goose StatementEnd


--
-- Name: current_org_id(); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.current_org_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$
  SELECT CASE
    WHEN current_setting('request.jwt.claims', true) IS NULL
      OR current_setting('request.jwt.claims', true) = ''
    THEN NULL
    ELSE NULLIF(current_setting('request.jwt.claims', true)::jsonb ->> 'org_id', '')::uuid
  END;
$$;
-- +goose StatementEnd


--
-- Name: current_user_id(); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.current_user_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$
  SELECT CASE
    WHEN current_setting('request.jwt.claims', true) IS NULL
      OR current_setting('request.jwt.claims', true) = ''
    THEN NULL
    ELSE NULLIF(current_setting('request.jwt.claims', true)::jsonb ->> 'sub', '')::uuid
  END;
$$;
-- +goose StatementEnd


--
-- Name: guard_org_owner_transfer(); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.guard_org_owner_transfer() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
  IF NEW.owner_user_id IS DISTINCT FROM OLD.owner_user_id THEN
    IF OLD.owner_user_id IS DISTINCT FROM tf.current_user_id() THEN
      RAISE EXCEPTION 'only the current org owner can transfer ownership'
        USING ERRCODE = '42501';
    END IF;
    IF NOT EXISTS (
      SELECT 1 FROM org_memberships
       WHERE user_id = NEW.owner_user_id
         AND org_id  = NEW.id
         AND role    = 'owner'
    ) THEN
      RAISE EXCEPTION 'new owner_user_id must already have role=owner in org_memberships'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd


--
-- Name: guard_org_owners(); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.guard_org_owners() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM affected ao
    WHERE NOT EXISTS (
      SELECT 1 FROM org_memberships
       WHERE org_id = ao.org_id AND role = 'owner'
    )
  ) THEN
    RAISE EXCEPTION 'each org must retain at least one owner role'
      USING ERRCODE = '23514';
  END IF;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd


--
-- Name: set_updated_at(); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.set_updated_at() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$;
-- +goose StatementEnd


--
-- Name: team_in_current_org(uuid); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.team_in_current_org(target_team uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
  SELECT EXISTS (
    SELECT 1 FROM teams
    WHERE id = target_team
      AND org_id = tf.current_org_id()
  );
$$;
-- +goose StatementEnd


--
-- Name: user_has_org_access(uuid); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.user_has_org_access(target_org uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
  SELECT EXISTS (
    SELECT 1 FROM org_memberships
    WHERE user_id = tf.current_user_id() AND org_id = target_org
  );
$$;
-- +goose StatementEnd


--
-- Name: org_tracked_repos(uuid); Type: FUNCTION; Schema: tf; Owner: -
--

-- The cross-team union read behind team_github_repos -> repo_profiles
-- reconciliation (SKY-375). repo_profiles is the org-wide UNION of every
-- team's tracked repos, but the team_github_repos SELECT policy is
-- team-membership-scoped, so a team admin's app-pool tx can't see sibling
-- teams' rows. This SECURITY DEFINER helper bypasses that per-team SELECT
-- RLS — a within-org, non-security boundary — to return the full org
-- union, while preserving the ORG boundary: with request claims it
-- requires p_org_id to equal the caller's org; with no claims
-- (admin-pool / system / test) the guard is skipped, mirroring the
-- vault_*_system split. The pinned search_path blocks definer hijacking.
-- +goose StatementBegin
CREATE FUNCTION tf.org_tracked_repos(p_org_id uuid) RETURNS TABLE(owner text, repo text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
  IF tf.current_org_id() IS NOT NULL AND p_org_id <> tf.current_org_id() THEN
    RAISE EXCEPTION 'org_tracked_repos: requested org % does not match caller org %', p_org_id, tf.current_org_id();
  END IF;
  RETURN QUERY
    SELECT DISTINCT g.owner, g.repo
    FROM team_github_repos g
    JOIN teams t ON t.id = g.team_id
    WHERE t.org_id = p_org_id
    ORDER BY g.owner, g.repo;
END;
$$;
-- +goose StatementEnd


--
-- Name: user_in_team(uuid); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.user_in_team(target_team uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
  SELECT EXISTS (
    SELECT 1 FROM memberships
    WHERE user_id = tf.current_user_id()
      AND team_id = target_team
  );
$$;
-- +goose StatementEnd


--
-- Name: user_can_write_team(uuid); Type: FUNCTION; Schema: tf; Owner: -
--
-- The write-path sibling of user_in_team (membership exists). A viewer is a
-- read-only member, so the team-scoped *write* RLS policies gate on this —
-- "membership exists AND the role can write" — while the *read* policies keep
-- user_in_team (viewers read). Splitting the two is what makes the viewer role
-- genuinely read-only end-to-end (TFAC-447); before this every write policy
-- checked only membership, so a viewer wrote like a member. Same shape as
-- user_in_team (SECURITY DEFINER STABLE, locked search_path) so it composes
-- into the existing policy predicates without changing their RLS posture.
--
-- An archived team (teams.deleted_at IS NOT NULL) is write-blocked end-to-end
-- (TFAC-448): the membership join to teams adds deleted_at IS NULL, so every
-- team-scoped write policy keyed on user_can_write_team (tasks, runs, prompts,
-- blueprints, event_handlers, projects, team_agents) — plus the
-- RequireTeamWrite / RequireTaskWrite handler gates that call it — reject the
-- write at the row level. This is the DB backstop that covers the task-scoped
-- mutations (swipe / snooze / requeue / advance) whose team is derived from the
-- task, not the URL. The team-settings family gates on user_is_team_admin
-- instead, so those handlers add the explicit authz.VerifyTeamNotArchived gate.
-- Archive itself stamps deleted_at via teams_update (org-admin) and reaps runs
-- on the admin pool (BYPASSRLS), so neither path is blocked by this filter.

-- +goose StatementBegin
CREATE FUNCTION tf.user_can_write_team(target_team uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
  SELECT EXISTS (
    SELECT 1 FROM memberships m
    JOIN teams t ON t.id = m.team_id
    WHERE m.user_id = tf.current_user_id()
      AND m.team_id = target_team
      AND m.role IN ('admin', 'member')
      AND t.deleted_at IS NULL
  );
$$;
-- +goose StatementEnd


--
-- Name: user_is_org_admin(uuid); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.user_is_org_admin(target_org uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
  SELECT EXISTS (
    SELECT 1 FROM org_memberships
    WHERE user_id = tf.current_user_id()
      AND org_id = target_org
      AND role IN ('owner', 'admin')
  );
$$;
-- +goose StatementEnd


--
-- Name: user_is_org_admin_via_team(uuid); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.user_is_org_admin_via_team(target_team uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
  SELECT EXISTS (
    SELECT 1 FROM teams t
    WHERE t.id = target_team
      AND tf.user_is_org_admin(t.org_id)
  );
$$;
-- +goose StatementEnd


--
-- Name: user_is_team_admin(uuid); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.user_is_team_admin(target_team uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
  SELECT EXISTS (
    SELECT 1 FROM memberships m
    WHERE m.user_id = tf.current_user_id()
      AND m.team_id = target_team
      AND m.role = 'admin'
  );
$$;
-- +goose StatementEnd


--
-- Name: user_owns_org(uuid); Type: FUNCTION; Schema: tf; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION tf.user_owns_org(target_org uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
  SELECT EXISTS (
    SELECT 1 FROM orgs WHERE id = target_org AND owner_user_id = tf.current_user_id()
  );
$$;
-- +goose StatementEnd


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: agents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agents (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    display_name text DEFAULT 'Triage Factory Bot'::text NOT NULL,
    default_model text,
    default_autonomy_suitability real,
    github_pat_user_id uuid,
    github_org_login text,
    jira_service_account_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: curator_messages; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.curator_messages (
    id bigint NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid NOT NULL,
    request_id uuid NOT NULL,
    role text NOT NULL,
    subtype text DEFAULT 'text'::text NOT NULL,
    content text DEFAULT ''::text NOT NULL,
    tool_calls jsonb,
    tool_call_id text,
    is_error boolean DEFAULT false NOT NULL,
    metadata jsonb,
    model text,
    input_tokens integer,
    output_tokens integer,
    cache_read_tokens integer,
    cache_creation_tokens integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: curator_messages_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.curator_messages_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: curator_messages_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.curator_messages_id_seq OWNED BY public.curator_messages.id;


--
-- Name: curator_pending_context; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.curator_pending_context (
    id bigint NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid NOT NULL,
    project_id uuid NOT NULL,
    curator_session_id text NOT NULL,
    change_type text NOT NULL,
    baseline_value text NOT NULL,
    consumed_at timestamp with time zone,
    consumed_by_request_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: curator_pending_context_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.curator_pending_context_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: curator_pending_context_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.curator_pending_context_id_seq OWNED BY public.curator_pending_context.id;


--
-- Name: curator_requests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.curator_requests (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid NOT NULL,
    project_id uuid NOT NULL,
    -- team_id snapshotted from the project at creation (point-in-time, nullable;
    -- mirrors runs.team_id). A Curator session is tied to a project and projects
    -- are team-scoped, so curator spend attributes to the project's team: team
    -- project -> team-attributed; private/org project -> NULL (still creator- and
    -- org-visible, just absent from team dashboards). Denormalized (no FK): a
    -- project later moving teams leaves past spend with the team that incurred it,
    -- and the security_invoker llm_spend view stays JOIN-free so it doesn't re-gate
    -- curator rows through projects' RLS. Every curator INSERT must populate this
    -- via (SELECT team_id FROM projects WHERE id = <project_id>). See TFAC-476.
    team_id uuid,
    status text DEFAULT 'queued'::text NOT NULL,
    user_input text NOT NULL,
    error_msg text,
    cost_usd real DEFAULT 0 NOT NULL,
    duration_ms integer DEFAULT 0 NOT NULL,
    num_turns integer DEFAULT 0 NOT NULL,
    -- Token breakdown denormalized onto the request at completion (TFAC-473):
    -- CompleteRequest SETs each to the absolute SUM over curator_messages
    -- (idempotent), the same roll-up runs uses over run_messages. Mirrors
    -- system_llm_runs' columns so the unified spend view (TFAC-472) reads
    -- tokens natively for curator turns.
    input_tokens integer DEFAULT 0 NOT NULL,
    output_tokens integer DEFAULT 0 NOT NULL,
    cache_read_tokens integer DEFAULT 0 NOT NULL,
    cache_creation_tokens integer DEFAULT 0 NOT NULL,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: system_llm_runs; Type: TABLE; Schema: public; Owner: -
--

-- One row per agentproc.Run made by a headless system job (the scorer,
-- repo-profiler, and project-classifier each run a Haiku call every poll
-- cycle). Captures the cost + token breakdown the subprocess already
-- computed so org spend reconciles with the Anthropic bill and a "system
-- overhead" line exists alongside runs.total_cost_usd / curator_requests.cost_usd.
-- Org-level, no team_id by design: scorer batches mix teams, and
-- repo_profiles/entities carry no team. System-written (admin pool); the
-- app pool only reads, gated by the org-scoped RLS policy below. See TFAC-451.
CREATE TABLE public.system_llm_runs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    job text NOT NULL,
    model text NOT NULL,
    total_cost_usd real DEFAULT 0 NOT NULL,
    input_tokens integer DEFAULT 0 NOT NULL,
    output_tokens integer DEFAULT 0 NOT NULL,
    cache_read_tokens integer DEFAULT 0 NOT NULL,
    cache_creation_tokens integer DEFAULT 0 NOT NULL,
    duration_ms integer DEFAULT 0 NOT NULL,
    num_turns integer DEFAULT 0 NOT NULL,
    is_error boolean DEFAULT false NOT NULL,
    metadata_json text,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: access_change_log; Type: TABLE; Schema: public; Owner: -
--

-- Small, low-volume audit log of governance actions that have no external
-- entity: org/team membership & role grants/changes/revokes, and credential
-- bind/rotate (GitHub PAT, Jira org + per-user, Anthropic key). Capture only —
-- the read/display surface is the future team-activity / org-governance view
-- (TFAC-449 bucket C/D). entities is entity-keyed and team-visible via the task
-- semi-join, so governance actions (no entity) get their own table rather than
-- polluting the hot router path.
--
-- Written on the APP pool in the SAME transaction as the action it records, so
-- the log can't diverge from reality — a log-write failure rolls the action
-- back. action is a free-text discriminator (org_member_granted, org_role_changed,
-- credential_set, ... — extensible, no CHECK). actor_user_id is the request's
-- authenticated user (NULL for system/bootstrap). target_user_id / team_id are
-- set for membership/role actions; detail_json carries the per-action payload
-- ({old_role,new_role} | {kind,host} | {invite_id} | ...). See TFAC-471.
CREATE TABLE public.access_change_log (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    actor_user_id uuid,
    action text NOT NULL,
    target_user_id uuid,
    team_id uuid,
    detail_json text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: external_actions; Type: TABLE; Schema: public; Owner: -
--

-- The append-only audit log of record: one row per external write TF performs
-- under an ORG-scoped credential (the org GitHub App / the org Jira service
-- account). Both autonomous (bot/system) and human-authorized-but-org-executed
-- (the GitHub approval flow, the board-drag draft toggle) writes land here;
-- writes under an individual user's OWN credential (the Jira claim/swipe/done/
-- requeue flows) are deliberately EXCLUDED and never instrumented. Event-grain
-- and immutable — the counterpart to the mutable, object-grain `artifacts` table
-- (which stays the agent-run state aid the reconciler maintains).
--
-- Written in the SAME transaction as the action it records wherever the action
-- has a DB state change to compose with (the server approval flips), so the log
-- can't diverge from the action. action is a free-text discriminator (no CHECK —
-- extensible, like access_change_log); credential names the org credential used
-- (github_app | jira_org); from_state/to_state carry a transition's endpoints;
-- run_id is the producing run (the agent's, or the drafter's for an approval, FK
-- ON DELETE SET NULL so it outlives a run purge); actor_user_id is the human
-- authorizer/initiator (NULL for an autonomous system write). dedup_key is the
-- natural per-action key — a branch push (the one true double-capture case: the
-- pre-push hook AND the git-proxy backstop both observe it) carries a
-- deterministic key so the twin collapses under ON CONFLICT DO NOTHING; every
-- other action gets a unique key, so DO NOTHING only ever collapses the twin.
-- See TFAC-483.
CREATE TABLE public.external_actions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    team_id uuid,
    provider text NOT NULL,
    action text NOT NULL,
    target text NOT NULL,
    external_id text,
    url text,
    from_state text,
    to_state text,
    run_id uuid,
    actor_user_id uuid,
    credential text NOT NULL,
    dedup_key text NOT NULL,
    detail_json text,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: entities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.entities (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    source text NOT NULL,
    source_id text NOT NULL,
    kind text NOT NULL,
    title text,
    url text,
    snapshot_json jsonb,
    description text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    project_id uuid,
    owning_team_id uuid,
    classified_at timestamp with time zone,
    classification_rationale text,
    last_polled_at timestamp with time zone,
    closed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: entity_links; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.entity_links (
    from_entity_id uuid NOT NULL,
    to_entity_id uuid NOT NULL,
    kind text NOT NULL,
    origin text NOT NULL,
    org_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: event_handlers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.event_handlers (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid,
    team_id uuid NOT NULL,
    kind text NOT NULL,
    event_type text NOT NULL,
    scope_predicate_json jsonb,
    enabled boolean DEFAULT true NOT NULL,
    source text DEFAULT 'user'::text NOT NULL,
    -- applies_to_unowned: the explicit, per-rule routing-scope flag (TFAC-517)
    -- that replaced the source-as-scope heuristic. TRUE adds the rule's team to a
    -- task's visibility even for entities the team doesn't own (external/ambiguous
    -- authors) — the deliberate "watch" opt-in; FALSE (default) keeps visibility
    -- riding ownership only. Routing gates explicitWatchTeams on this column, not
    -- on source. No multi-mode deployment exists yet, so this lands in the
    -- baseline directly (the SQLite side gets a forward migration instead).
    applies_to_unowned boolean DEFAULT false NOT NULL,
    system_slug text,
    name text,
    default_priority real,
    sort_order integer,
    blueprint_id text,
    breaker_threshold integer,
    min_autonomy_suitability real,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT event_handlers_kind_check CHECK ((kind = ANY (ARRAY['rule'::text, 'trigger'::text]))),
    CONSTRAINT event_handlers_rule_shape CHECK (((kind <> 'rule'::text) OR ((blueprint_id IS NULL) AND (breaker_threshold IS NULL) AND (min_autonomy_suitability IS NULL) AND (name IS NOT NULL) AND (default_priority IS NOT NULL) AND (sort_order IS NOT NULL)))),
    -- source is app-validated, not CHECK-constrained (the source_check was
    -- dropped in this baseline, both dialects) so new provenance values
    -- are addable without DDL. The creator pairing below was harmonized from
    -- source='user' to source<>'system' to tolerate any non-system source.
    CONSTRAINT event_handlers_system_has_no_creator CHECK ((((source = 'system'::text) AND (creator_user_id IS NULL)) OR ((source <> 'system'::text) AND (creator_user_id IS NOT NULL)))),
    CONSTRAINT event_handlers_trigger_shape CHECK (((kind <> 'trigger'::text) OR ((blueprint_id IS NOT NULL) AND (breaker_threshold IS NOT NULL) AND (min_autonomy_suitability IS NOT NULL) AND (default_priority IS NULL) AND (sort_order IS NULL) AND (name IS NULL))))
);


--
-- Name: events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    entity_id uuid,
    event_type text NOT NULL,
    dedup_key text DEFAULT ''::text NOT NULL,
    metadata_json jsonb,
    occurred_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: events_catalog; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.events_catalog (
    id text NOT NULL,
    source text NOT NULL,
    category text NOT NULL,
    label text NOT NULL,
    description text NOT NULL
);


-- goose_db_version is managed by goose itself — do NOT recreate.


--
-- Name: jira_project_status_rules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.jira_project_status_rules (
    team_id uuid NOT NULL,
    project_key text NOT NULL,
    pickup_members text[] DEFAULT '{}'::text[] NOT NULL,
    in_progress_members text[] DEFAULT '{}'::text[] NOT NULL,
    in_progress_canonical text,
    done_members text[] DEFAULT '{}'::text[] NOT NULL,
    done_canonical text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    -- Mirror of the SQLite CHECKs: every persisted row is fully
    -- configured. HTTP handler is the user-facing gate; these are
    -- defense-in-depth against any other write path (admin UI in
    -- multi mode, direct SQL, restore). "canonical is in members"
    -- stays in the HTTP validator because PG CHECK can't have
    -- subqueries.
    CONSTRAINT jpsr_pickup_populated CHECK (
        cardinality(pickup_members) > 0
    ),
    CONSTRAINT jpsr_in_progress_populated CHECK (
        cardinality(in_progress_members) > 0
        AND in_progress_canonical IS NOT NULL AND in_progress_canonical <> ''
    ),
    CONSTRAINT jpsr_done_populated CHECK (
        cardinality(done_members) > 0
        AND done_canonical IS NOT NULL AND done_canonical <> ''
    )
);


--
-- Name: team_github_groups; Type: TABLE; Schema: public; Owner: -
--

-- The GitHub twin of jira_project_status_rules: one row per
-- (team, github_org_login, github_team_slug), a team declaring "route
-- this GitHub team's review requests to me." Dumb string labels for
-- routing only — no membership resolution, no nested-team traversal,
-- no sync of GitHub's team graph. Fully-qualified with the org login so
-- @acme/frontend and @beta/frontend don't collide. Many GitHub teams
-- can sit under one TF team (the primary "funnel" direction); the
-- reverse (one GitHub team under many TF teams) stays permitted. Rows
-- are pure key tuples, so edits are replace-sets, never in-place
-- updates — hence no UPDATE policy below.
CREATE TABLE public.team_github_groups (
    team_id uuid NOT NULL,
    github_org_login text NOT NULL,
    github_team_slug text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tgg_org_login_populated CHECK (github_org_login <> ''),
    CONSTRAINT tgg_team_slug_populated CHECK (github_team_slug <> '')
);


--
-- Name: team_github_repos; Type: TABLE; Schema: public; Owner: -
--

-- One row per (team, owner, repo): a team declaring "I track this repo."
-- The GitHub *tracking-scope* twin of jira_project_status_rules and the
-- source of truth for which repos a team cares about. NOT the same as
-- team_github_groups above (CODEOWNERS review-routing teams) — this is
-- the tracking selection. repo_profiles is the org-shared UNION of every
-- team's rows here, a derived poll/profile/ETag cache reconciled on every
-- write and never user-written directly anymore. No org_id column: org
-- scope rides the teams FK, mirroring jira_project_status_rules. Local
-- mode (N=1) tracks every configured repo on the single default team, so
-- the router's team↔repo gate never drops anything there.
CREATE TABLE public.team_github_repos (
    team_id uuid NOT NULL,
    owner text NOT NULL,
    repo text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tgr_owner_populated CHECK (owner <> ''),
    CONSTRAINT tgr_repo_populated CHECK (repo <> '')
);


--
-- Name: memberships; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.memberships (
    user_id uuid NOT NULL,
    team_id uuid NOT NULL,
    role public.membership_role DEFAULT 'member'::public.membership_role NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: org_memberships; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.org_memberships (
    user_id uuid NOT NULL,
    org_id uuid NOT NULL,
    role public.org_role DEFAULT 'member'::public.org_role NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: org_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.org_settings (
    org_id uuid NOT NULL,
    github_base_url text,
    github_poll_interval interval DEFAULT '00:05:00'::interval NOT NULL,
    github_clone_protocol text DEFAULT 'ssh'::text NOT NULL,
    jira_base_url text,
    jira_poll_interval interval DEFAULT '00:05:00'::interval NOT NULL,
    -- Vault refs (not raw secrets) for Anthropic / Bedrock credentials.
    -- NULL means "use deployment default" on hosted SaaS or "not configured
    -- yet" on self-host. The SecretStore API resolves the ref to a live
    -- token at request time; rotation replaces the secret behind the ref
    -- without touching this row.
    anthropic_api_key_ref text,
    bedrock_credentials_ref text,
    -- Max model tier the org permits teams/users to pick. NULL means no cap.
    -- App-validated, not CHECK-constrained (the max_llm_model_tier_check was
    -- dropped in this baseline, both dialects): an opaque, provider-agnostic
    -- model/capability identifier. The app knows 'haiku'|'sonnet'|'opus' today
    -- (validated in the settings handler), but dropping the CHECK lets OpenAI /
    -- future families be added with zero DDL; a richer provider/model split stays
    -- additive (new columns). Column not renamed; app layer unchanged.
    max_llm_model_tier text,
    -- Org-wide daily LLM spend cap (TFAC-477). NULL = no cap; the app layer also
    -- treats 0 as "no cap". When the org's spend for the current UTC calendar day
    -- (summed across every category — autonomous + manual + curator + system
    -- overhead) is at or above this value, the delegation choke point
    -- (Spawner.Delegate) refuses all new agent runs, a runaway-spend fuse. Mirrors
    -- the nullable max_llm_model_tier shape above; in-flight runs are unaffected
    -- and the read fails open on error so a transient failure can't wedge delegation.
    max_daily_cost_usd double precision,
    -- Ship-dark org toggle for the within-org prompt marketplace (TFAC-535 /
    -- TFAC-92 scoping decision 4). Default false: rendered grayed out with
    -- "coming soon" in org settings until the TFAC-539 launch flip; UI +
    -- enforcement of this column are that ticket's concern, not this one's.
    marketplace_enabled boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT org_settings_github_clone_protocol_check CHECK ((github_clone_protocol = ANY (ARRAY['https'::text, 'ssh'::text])))
);


--
-- Name: orgs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.orgs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    description text,
    billing_email text,
    owner_user_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);


--
-- Name: pending_firings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.pending_firings (
    id bigint NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid NOT NULL,
    entity_id uuid NOT NULL,
    task_id uuid NOT NULL,
    trigger_id uuid NOT NULL,
    triggering_event_id uuid NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    skip_reason text,
    queued_at timestamp with time zone DEFAULT now() NOT NULL,
    drained_at timestamp with time zone,
    fired_run_id uuid
);


--
-- Name: pending_firings_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.pending_firings_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: pending_firings_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.pending_firings_id_seq OWNED BY public.pending_firings.id;


--
-- Name: poller_state; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.poller_state (
    org_id uuid NOT NULL,
    source text NOT NULL,
    source_id text NOT NULL,
    state_json jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: project_knowledge; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.project_knowledge (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    project_id uuid NOT NULL,
    key text NOT NULL,
    content text DEFAULT ''::text NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    last_updated_by uuid,
    last_updated_by_run uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: projects; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.projects (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid NOT NULL,
    team_id uuid,
    visibility text DEFAULT 'team'::text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    curator_session_id text,
    pinned_repos jsonb DEFAULT '[]'::jsonb NOT NULL,
    jira_project_key text,
    linear_project_key text,
    spec_authorship_blueprint_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT projects_team_visibility_requires_team CHECK (((visibility <> 'team'::text) OR (team_id IS NOT NULL))),
    CONSTRAINT projects_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'team'::text, 'org'::text])))
);


--
-- Name: prompts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.prompts (
    id text DEFAULT (gen_random_uuid())::text NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid,
    team_id uuid NOT NULL,
    name text NOT NULL,
    body text NOT NULL,
    source text DEFAULT 'user'::text NOT NULL,
    usage_count integer DEFAULT 0 NOT NULL,
    hidden boolean DEFAULT false NOT NULL,
    user_modified boolean DEFAULT false NOT NULL,
    allowed_tools text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    model text DEFAULT ''::text NOT NULL,
    system_slug text,
    -- deleted_at soft-deletes a prompt: the row + its runs stay (runs.prompt_id
    -- is RESTRICT, so a hard DELETE on a prompt with run history would error);
    -- request-facing reads filter deleted_at IS NULL, the ...System reads keep
    -- resolving it for in-flight runs + past-run timelines. Load-bearing once
    -- every new prompt is auto-wrapped as a 1-step blueprint (the step FK is
    -- RESTRICT), so hard-delete is impossible.
    deleted_at timestamp with time zone,
    -- source is app-validated, not CHECK-constrained (source_check dropped in
    -- this baseline, both dialects). The system_has_no_creator pairing is
    -- the only source invariant the DB still enforces.
    CONSTRAINT prompts_system_has_no_creator CHECK ((((source = 'system'::text) AND (creator_user_id IS NULL)) OR ((source <> 'system'::text) AND (creator_user_id IS NOT NULL))))
);


--
-- Name: repo_profiles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.repo_profiles (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    owner text NOT NULL,
    repo text NOT NULL,
    description text,
    has_readme boolean DEFAULT false NOT NULL,
    has_claude_md boolean DEFAULT false NOT NULL,
    has_agents_md boolean DEFAULT false NOT NULL,
    profile_text text,
    clone_url text,
    default_branch text,
    base_branch text,
    clone_status text DEFAULT 'pending'::text NOT NULL,
    clone_error text,
    clone_error_kind text,
    profiled_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    pulls_etag text,
    pulls_polled_at timestamp with time zone
);


--
-- Name: artifacts; Type: TABLE; Schema: public; Owner: -
--

-- artifacts (TFAC-455): the single durable, run-attributed, polymorphic
-- record of everything a run produces in an external system (a pushed
-- branch, a draft/open PR, a draft/submitted review, a Jira/Linear issue,
-- a comment). One row per external object; provider + kind discriminate
-- the shape. All capture writers UPSERT on (org_id, dedup_key) so the same
-- logical object is one row. Replaces the never-written run_artifacts
-- placeholder. team_id is denormalized from the run so reads scope by team
-- exactly like runs; run_id is nullable (ON DELETE SET NULL) so a row
-- survives a run purge for audit. state is per-kind lifecycle (domain
-- consts, no CHECK — extensible).
CREATE TABLE public.artifacts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    run_id uuid,
    org_id uuid NOT NULL,
    team_id uuid NOT NULL,
    provider text NOT NULL,
    kind text NOT NULL,
    target text NOT NULL,
    external_id text,
    url text,
    state text NOT NULL,
    dedup_key text NOT NULL,
    details_json text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: run_memory; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.run_memory (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    run_id uuid NOT NULL,
    entity_id uuid NOT NULL,
    blueprint_run_id uuid,
    agent_content text,
    human_content text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: run_messages; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.run_messages (
    id bigint NOT NULL,
    org_id uuid NOT NULL,
    run_id uuid NOT NULL,
    role text NOT NULL,
    content text,
    subtype text DEFAULT 'text'::text,
    tool_calls jsonb,
    tool_call_id text,
    is_error boolean DEFAULT false NOT NULL,
    metadata jsonb,
    model text,
    input_tokens integer,
    output_tokens integer,
    cache_read_tokens integer,
    cache_creation_tokens integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: run_messages_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.run_messages_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: run_messages_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.run_messages_id_seq OWNED BY public.run_messages.id;


--
-- Name: run_worktrees; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.run_worktrees (
    run_id uuid NOT NULL,
    org_id uuid NOT NULL,
    repo_id text NOT NULL,
    path text NOT NULL,
    ref text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.runs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid,
    team_id uuid NOT NULL,
    visibility text DEFAULT 'team'::text NOT NULL,
    -- task_id / prompt_id are NULLABLE (relaxed from NOT NULL in this
    -- baseline) so a run need not descend from a task or a saved prompt — headroom
    -- for a future user-initiated interactive run (no event, no task, no saved
    -- prompt). runs_origin_requires_parents pins them for origin 'blueprint'
    -- (every run today). The interactive parent (agent_sessions + a
    -- runs.agent_session_id FK) is deferred — additive in a later migration.
    task_id uuid,
    prompt_id text,
    trigger_id uuid,
    trigger_type text DEFAULT 'manual'::text NOT NULL,
    -- origin discriminates blueprint-step runs from a future interactive kind;
    -- app-validated (no value CHECK, mirrors source). See the SQLite twin.
    origin text DEFAULT 'blueprint'::text NOT NULL,
    status text DEFAULT 'cloning'::text NOT NULL,
    model text,
    session_id text,
    worktree_path text,
    result_summary text,
    outcome text,
    outcome_reason text,
    -- failure_kind is the machine-readable discriminator for WHY a run
    -- reached status='failed' (domain.RunFailureKind: memory_limit / crash /
    -- no_result / agent_error), written by markFailedIfActive / completeRun
    -- alongside the status flip. App-validated (no CHECK, same as
    -- repo_profiles.clone_error_kind); NULL === no specific classification
    -- (non-failed runs, legacy failed rows). See the SQLite twin.
    failure_kind text,
    stop_reason text,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    -- parked_at is stamped when the run enters the `open` parked state and
    -- cleared on resume, so the snapshot-retention sweep can key an open run off
    -- its last park rather than started_at (which never resets across resumes).
    -- NULL whenever the run is not parked open; the pending_approval and
    -- completed+abort terminals use completed_at for the same purpose.
    parked_at timestamp with time zone,
    duration_ms integer,
    num_turns integer,
    total_cost_usd real,
    -- Token breakdown denormalized onto the run at completion (TFAC-473):
    -- AgentRunStore.Complete SETs each to the absolute SUM over run_messages
    -- (idempotent across resumes). Mirrors system_llm_runs' columns so the
    -- unified spend view (TFAC-472) reads tokens natively for delegated runs.
    input_tokens integer DEFAULT 0 NOT NULL,
    output_tokens integer DEFAULT 0 NOT NULL,
    cache_read_tokens integer DEFAULT 0 NOT NULL,
    cache_creation_tokens integer DEFAULT 0 NOT NULL,
    actor_agent_id uuid,
    -- blueprint_run_id is NULLABLE (relaxed from NOT NULL at this baseline);
    -- pinned for origin 'blueprint' by runs_origin_requires_parents, so every
    -- run today still has it. See the SQLite twin + task_id above.
    blueprint_run_id uuid,
    blueprint_step_index integer,
    triggering_event_id uuid,
    -- Run-queue claim columns (mirror event_queue). A blueprint step is
    -- enqueued as a run row in status='queued'; the dispatcher claims it
    -- (queued -> running) via FOR UPDATE SKIP LOCKED, stamping claimed_at and
    -- bumping attempts. Both stay NULL/0 for the legacy never-queued shape.
    claimed_at timestamp with time zone,
    attempts integer DEFAULT 0 NOT NULL,
    -- executor_id records which executor instance owns a run's live
    -- process while it is running. Stamped when the run goes live; NULL
    -- for queued / never-live / terminal-parked rows. At N=1 it's a single
    -- per-process instance id (forward-compat ownership hook for
    -- horizontal scaling, where it becomes the lease the control plane
    -- signals through). Not a status — purely the run→executor pointer.
    executor_id text,
    CONSTRAINT runs_creator_matches_trigger_type CHECK ((((trigger_type = 'manual'::text) AND (creator_user_id IS NOT NULL)) OR ((trigger_type = 'event'::text) AND (creator_user_id IS NULL)))),
    CONSTRAINT runs_team_visibility_requires_team CHECK (((visibility <> 'team'::text) OR (team_id IS NOT NULL))),
    CONSTRAINT runs_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'team'::text, 'org'::text]))),
    -- A 'blueprint'-origin run carries its full parentage (blueprint_run + task
    -- + prompt), preserving the previous NOT-NULL invariant for every run
    -- today; origin <> 'blueprint' (a future interactive/ad-hoc run) is left
    -- unconstrained here. See the SQLite twin.
    CONSTRAINT runs_origin_requires_parents CHECK (((origin = 'blueprint'::text AND blueprint_run_id IS NOT NULL AND task_id IS NOT NULL AND prompt_id IS NOT NULL) OR (origin <> 'blueprint'::text)))
);


--
-- Name: llm_spend; Type: VIEW; Schema: public; Owner: -
--

-- Unified spend view (TFAC-472): one row-per-spend shape UNION-ing the three
-- source tables — delegated runs, curator turns, and headless system jobs —
-- onto a single category axis so the team dashboard + safety cap read from one
-- place and org totals reconcile with the Anthropic bill. Read-side only: no
-- normalized table, no write-path change, no backfill — the view IS the
-- abstraction boundary (materialize later only if it's ever measured too slow).
-- Placed here because it depends on all three source tables, of which runs is
-- the last created.
--
-- category derivation: runs split on trigger_type (manual → per-user, anything
-- else → autonomous; the runs_creator_matches_trigger_type CHECK guarantees
-- 'manual'/'event'); curator is 'curator'; system jobs are 'system_overhead'
-- with the job (scorer/repo_profiler/classifier) carried as subtype.
--
-- team_id: runs are team-scoped; curator is team-attributed via a point-in-time
-- snapshot of its project's team (curator_requests.team_id, TFAC-476) — a team
-- project → that team, a private/org project → NULL; system-overhead has no team
-- (org-level). So the dashboard sees runs + team-project curator by team, and
-- system + null-team curator at org scope. actor_agent_id is the org agent that
-- executed the run (audit passthrough) — only runs are agent-executed, so NULL
-- for curator (user-driven) + system (background). trigger_id is the
-- event_handler that fired an autonomous run (TFAC-478) — it backs the team
-- dashboard's by-rule breakdown; NULL for manual runs, curator, and system.
--
-- Tokens are read NATIVELY from all three tables — every token column is
-- INTEGER NOT NULL DEFAULT 0 (runs + curator via TFAC-473, system via
-- TFAC-451) — so the view does NO COALESCE on tokens; only runs.total_cost_usd
-- (nullable until a terminal write) is wrapped in COALESCE(…,0). The view
-- reflects *settled* spend: in-flight rows show 0 cost + 0 tokens until a
-- terminal write, consistently across cost and tokens. Every terminal write
-- (completion, cancel, infra-failure, and the boot-time orphan sweeps) rolls
-- the breakdown up from the per-message tables, so a cancelled/failed run or
-- curator turn still carries its real token spend.
--
-- security_invoker = true is LOAD-BEARING and mandatory (PG 15+): without it a
-- view evaluates base-table RLS as the view *owner*, bypassing the invoker's
-- row scoping → a cross-team / cross-org spend leak. With it, the base tables'
-- existing RLS (runs/curator_requests org+team, system_llm_runs org) applies
-- under the querying app-pool identity, exactly as if selecting the tables
-- directly. Deliberately NO separate RLS policy on the view, and NOT
-- security_definer.
CREATE VIEW public.llm_spend WITH (security_invoker='true') AS
 SELECT 'run'::text AS source,
    runs.id AS source_id,
    runs.org_id,
    runs.team_id,
        CASE runs.trigger_type
            WHEN 'manual'::text THEN 'manual'::text
            ELSE 'autonomous'::text
        END AS category,
    NULL::text AS subtype,
    runs.creator_user_id,
    runs.actor_agent_id,
    runs.trigger_id,
    runs.model,
    COALESCE(runs.total_cost_usd, (0)::real) AS total_cost_usd,
    runs.input_tokens,
    runs.output_tokens,
    runs.cache_read_tokens,
    runs.cache_creation_tokens,
    runs.started_at AS occurred_at
   FROM public.runs
UNION ALL
 SELECT 'curator'::text AS source,
    curator_requests.id AS source_id,
    curator_requests.org_id,
    curator_requests.team_id,
    'curator'::text AS category,
    NULL::text AS subtype,
    curator_requests.creator_user_id,
    NULL::uuid AS actor_agent_id,
    NULL::uuid AS trigger_id,
    NULL::text AS model,
    curator_requests.cost_usd AS total_cost_usd,
    curator_requests.input_tokens,
    curator_requests.output_tokens,
    curator_requests.cache_read_tokens,
    curator_requests.cache_creation_tokens,
    curator_requests.created_at AS occurred_at
   FROM public.curator_requests
UNION ALL
 SELECT 'system'::text AS source,
    system_llm_runs.id AS source_id,
    system_llm_runs.org_id,
    NULL::uuid AS team_id,
    'system_overhead'::text AS category,
    system_llm_runs.job AS subtype,
    NULL::uuid AS creator_user_id,
    NULL::uuid AS actor_agent_id,
    NULL::uuid AS trigger_id,
    system_llm_runs.model,
    system_llm_runs.total_cost_usd,
    system_llm_runs.input_tokens,
    system_llm_runs.output_tokens,
    system_llm_runs.cache_read_tokens,
    system_llm_runs.cache_creation_tokens,
    system_llm_runs.started_at AS occurred_at
   FROM public.system_llm_runs;


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    jwt_enc bytea NOT NULL,
    jwt_nonce bytea NOT NULL,
    refresh_token_enc bytea NOT NULL,
    refresh_nonce bytea NOT NULL,
    jwt_expires_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    user_agent text,
    ip_addr inet,
    active_org_id uuid,
    CONSTRAINT sessions_check CHECK ((expires_at > created_at)),
    CONSTRAINT sessions_check1 CHECK ((jwt_expires_at <= expires_at))
);


--
-- Name: swipe_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.swipe_events (
    id bigint NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid NOT NULL,
    task_id uuid NOT NULL,
    action text NOT NULL,
    hesitation_ms integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: swipe_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.swipe_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: swipe_events_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.swipe_events_id_seq OWNED BY public.swipe_events.id;


--
-- Name: system_prompt_versions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.system_prompt_versions (
    org_id uuid NOT NULL,
    prompt_id text NOT NULL,
    content_hash text NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: task_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.task_events (
    task_id uuid NOT NULL,
    event_id uuid NOT NULL,
    org_id uuid NOT NULL,
    kind text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tasks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tasks (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid NOT NULL,
    team_id uuid,
    visibility text DEFAULT 'team'::text NOT NULL,
    entity_id uuid NOT NULL,
    event_type text NOT NULL,
    dedup_key text DEFAULT ''::text NOT NULL,
    primary_event_id uuid NOT NULL,
    status text DEFAULT 'queued'::text NOT NULL,
    priority_score real,
    ai_summary text,
    autonomy_suitability real,
    priority_reasoning text,
    scoring_status text DEFAULT 'pending'::text NOT NULL,
    severity text,
    relevance_reason text,
    source_status text,
    snooze_until timestamp with time zone,
    close_reason text,
    close_event_type text,
    closed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    claimed_by_agent_id uuid,
    claimed_by_user_id uuid,
    CONSTRAINT tasks_claim_xor CHECK (((claimed_by_agent_id IS NULL) OR (claimed_by_user_id IS NULL))),
    CONSTRAINT tasks_claimed_requires_team CHECK ((((claimed_by_user_id IS NULL) AND (claimed_by_agent_id IS NULL)) OR (team_id IS NOT NULL))),
    CONSTRAINT tasks_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'team'::text, 'org'::text]))),
    CONSTRAINT tasks_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'in_progress'::text, 'in_review'::text, 'done'::text, 'dismissed'::text, 'snoozed'::text])))
);


--
-- Name: task_teams; Type: TABLE; Schema: public; Owner: -
--

-- task_teams is the visibility set: the teams whose handlers matched
-- the event that spawned the task. A team sees an unclaimed task iff it
-- is in task_teams (or it is the owning team_id); once claimed the card
-- consolidates to the owning team_id.
CREATE TABLE public.task_teams (
    task_id uuid NOT NULL,
    team_id uuid NOT NULL
);


--
-- Name: team_agents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.team_agents (
    team_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    per_team_model text,
    per_team_autonomy_suitability real,
    added_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: team_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.team_settings (
    team_id uuid NOT NULL,
    jira_projects text[] DEFAULT '{}'::text[] NOT NULL,
    ai_reprioritize_threshold integer DEFAULT 5 NOT NULL,
    ai_preference_update_interval integer DEFAULT 20 NOT NULL,
    -- Team-scope AI behavior policy. Moved off user_settings: in v1 the
    -- team owns the Claude tier used for scoring + agent runs (clamped by
    -- org_settings.max_llm_model_tier when set) and the master toggle for
    -- auto-delegation. auto_delegate_enabled defaults FALSE so new teams
    -- don't auto-spawn agents until explicitly opted in.
    default_model text DEFAULT 'sonnet'::text NOT NULL,
    auto_delegate_enabled boolean DEFAULT false NOT NULL,
    -- Per-team daily LLM spend cap (TFAC-482, EE/governance-gated). NULL = no cap;
    -- the app layer also treats 0 as "no cap". When the team's spend for the
    -- current UTC calendar day (summed over its team_id rows ONLY — system
    -- overhead + non-team curator carry a NULL team_id and never count toward a
    -- team cap) is at or above this value, the delegation choke point refuses new
    -- agent runs FOR THAT TEAM. Org-admin-configured (a team admin cannot set
    -- their own team's cap) and enforced only while the governance entitlement is
    -- active; unlicensed/lapsed → dormant, with the org-wide cap
    -- (org_settings.max_daily_cost_usd) remaining the safety net. Mirrors that org
    -- cap's nullable shape; in-flight runs are unaffected and the read fails open.
    max_daily_cost_usd double precision,
    -- Per-team branch-name template (TFAC-498): a free-text convention shown to
    -- the delegated agent as envelope guidance (NOT enforced — the push gate is
    -- the enforcement point). The literal "<ticket-id>" is substituted with the
    -- run's ticket id at prompt-render time. NOT NULL with a literal DEFAULT so
    -- partial upserts (e.g. SetDailyCostCapSystem) materialize it without the
    -- writer naming the column; the app coalesces an empty write to the default.
    -- Rolled into the baseline (not a forward migration) because multi-mode /
    -- Postgres is net-new and unshipped; the SQLite tree, which HAS shipped,
    -- carries the equivalent forward migration 202606280001_team_branch_template.sql.
    branch_template text DEFAULT 'tfac/<ticket-id>'::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: teams; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.teams (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    description text,
    created_by_user_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    -- deleted_at soft-deletes a team (TFAC-448; mirrors orgs.deleted_at +
    -- prompts.deleted_at). Archiving stamps now() here, force-stops the team's
    -- in-flight work, and blocks further writes; restore flips it back to NULL.
    -- Request-facing team reads filter deleted_at IS NULL (the team vanishes
    -- from selectors); the ...System reads omit the filter so the archive /
    -- restore / preview paths + in-flight reaping still resolve it. The team's
    -- durable work (tasks, runs, memory) is never hard-deleted.
    deleted_at timestamp with time zone
);


--
-- Name: user_settings; Type: TABLE; Schema: public; Owner: -
--

-- Reserved for future per-user prefs (theme, notification destinations,
-- swipe sensitivity, onboarding state). The ai_model + ai_auto_delegate_enabled
-- columns that used to live here moved to team_settings — the team owns
-- the AI behavior policy, users don't override in v1.
CREATE TABLE public.user_settings (
    user_id uuid NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    display_name text,
    avatar_url text,
    timezone text DEFAULT 'UTC'::text NOT NULL,
    default_org_id uuid,
    last_acting_team_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: user_github_identities; Type: TABLE; Schema: public; Owner: -
--

-- Host-scoped GitHub identity bindings (SKY-396). Replaces the single
-- users.github_username column: one human can hold a different login on each
-- GitHub host (github.com for one org, a corp GHES for another), so the
-- natural key is (user_id, github_base_url), not a lone column. For the first
-- self-deploy (one org, one host) this is exactly one row per user.
--
-- source records HOW the binding was captured ('pat' | 'connect_oauth' |
-- 'scim' | 'login_claim') — load-bearing for SKY-271's "verified against the
-- org's host, never typed-unverified" integrity rule. verified_at timestamps
-- the last authenticated /user confirmation against the host — the hook for
-- future drift re-checks. It is deliberately nullable: NULL is a meaningful
-- state, "login known but not yet host-verified" — the shape a future SCIM
-- sync (source='scim') would write when it learns a login from the directory
-- without an authenticated round-trip to the host. Today's writers (pat,
-- login_claim) always stamp it. An absent row is a durable, supported state
-- (the NULL-degrades-gracefully contract from SKY-264).
CREATE TABLE public.user_github_identities (
    user_id uuid NOT NULL,
    github_base_url text NOT NULL,
    login text NOT NULL,
    source text NOT NULL,
    verified_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_github_identities_source_check CHECK ((source = ANY (ARRAY['pat'::text, 'connect_oauth'::text, 'scim'::text, 'login_claim'::text])))
);


--
-- Name: user_jira_identities; Type: TABLE; Schema: public; Owner: -
--

-- Host-scoped Jira identity bindings (SKY-397). The Jira sibling of
-- user_github_identities: it replaces the single-valued users.jira_account_id
-- / users.jira_display_name columns. One human can hold a different Jira
-- account on each Jira site (a Cloud site for one org, a Server/DC host for
-- another), so the natural key is (user_id, jira_base_url), not a lone column.
-- For the first self-deploy (one org, one host) this is exactly one row per
-- user.
--
-- The access layer already keys per-(user, host): the per-user Jira PAT is
-- custodied as "jira_token/<host>" (SKY-442). This table makes IDENTITY
-- symmetric with that access — both on the same canonical host — so a second
-- Jira site can't overwrite the first's identity the way the single column did.
--
-- account_id is the Atlassian StableID (Cloud accountId, else Server/DC key);
-- display_name is the Jira-side display name. source records HOW the binding
-- was captured ('pat' | 'connect_oauth' | 'scim') — 'pat' is the only writer
-- today (DC paste-a-PAT); Cloud OAuth and SCIM are later tickets. verified_at
-- timestamps the last authenticated /myself confirmation (nullable for a
-- future SCIM directory sync; today's pat writer always stamps it). An absent
-- row is a durable, supported state (the NULL-degrades-gracefully contract
-- from SKY-264).
CREATE TABLE public.user_jira_identities (
    user_id uuid NOT NULL,
    jira_base_url text NOT NULL,
    account_id text NOT NULL,
    display_name text,
    source text NOT NULL,
    verified_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_jira_identities_source_check CHECK ((source = ANY (ARRAY['pat'::text, 'connect_oauth'::text, 'scim'::text])))
);


--
-- Name: curator_messages id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_messages ALTER COLUMN id SET DEFAULT nextval('public.curator_messages_id_seq'::regclass);


--
-- Name: curator_pending_context id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_pending_context ALTER COLUMN id SET DEFAULT nextval('public.curator_pending_context_id_seq'::regclass);


--
-- Name: pending_firings id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings ALTER COLUMN id SET DEFAULT nextval('public.pending_firings_id_seq'::regclass);


--
-- Name: run_messages id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_messages ALTER COLUMN id SET DEFAULT nextval('public.run_messages_id_seq'::regclass);


--
-- Name: swipe_events id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.swipe_events ALTER COLUMN id SET DEFAULT nextval('public.swipe_events_id_seq'::regclass);


--
-- Name: agents agents_id_org_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT agents_id_org_unique UNIQUE (id, org_id);


--
-- Name: agents agents_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT agents_org_id_key UNIQUE (org_id);


--
-- Name: agents agents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT agents_pkey PRIMARY KEY (id);


--
-- Name: curator_messages curator_messages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_messages
    ADD CONSTRAINT curator_messages_pkey PRIMARY KEY (id);


--
-- Name: curator_pending_context curator_pending_context_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_pending_context
    ADD CONSTRAINT curator_pending_context_pkey PRIMARY KEY (id);


--
-- Name: curator_requests curator_requests_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_requests
    ADD CONSTRAINT curator_requests_id_org_id_key UNIQUE (id, org_id);


--
-- Name: curator_requests curator_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_requests
    ADD CONSTRAINT curator_requests_pkey PRIMARY KEY (id);


--
-- Name: entities entities_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entities
    ADD CONSTRAINT entities_id_org_id_key UNIQUE (id, org_id);


--
-- Name: entities entities_org_id_source_source_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entities
    ADD CONSTRAINT entities_org_id_source_source_id_key UNIQUE (org_id, source, source_id);


--
-- Name: entities entities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entities
    ADD CONSTRAINT entities_pkey PRIMARY KEY (id);


--
-- Name: entity_links entity_links_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_links
    ADD CONSTRAINT entity_links_pkey PRIMARY KEY (from_entity_id, to_entity_id, kind);


--
-- Name: event_handlers event_handlers_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_id_org_id_key UNIQUE (id, org_id);


--
-- Name: event_handlers event_handlers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_pkey PRIMARY KEY (org_id, id);


--
-- Name: event_handlers event_handlers_org_team_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

-- Per-team re-seed idempotency key: one copy of each shipped handler per team
-- (same system_slug, distinct team_id); NULLs distinct so user handlers never
-- collide (SKY-380).
ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_org_team_slug_key UNIQUE (org_id, team_id, system_slug);


--
-- Name: events_catalog events_catalog_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.events_catalog
    ADD CONSTRAINT events_catalog_pkey PRIMARY KEY (id);


--
-- Name: events events_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_id_org_id_key UNIQUE (id, org_id);


--
-- Name: events events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_pkey PRIMARY KEY (id);


--
-- Name: jira_project_status_rules jira_project_status_rules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.jira_project_status_rules
    ADD CONSTRAINT jira_project_status_rules_pkey PRIMARY KEY (team_id, project_key);


--
-- Name: team_github_groups team_github_groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_github_groups
    ADD CONSTRAINT team_github_groups_pkey PRIMARY KEY (team_id, github_org_login, github_team_slug);


--
-- Name: team_github_repos team_github_repos_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_github_repos
    ADD CONSTRAINT team_github_repos_pkey PRIMARY KEY (team_id, owner, repo);


--
-- Name: memberships memberships_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memberships
    ADD CONSTRAINT memberships_pkey PRIMARY KEY (user_id, team_id);


--
-- Name: org_memberships org_memberships_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.org_memberships
    ADD CONSTRAINT org_memberships_pkey PRIMARY KEY (user_id, org_id);


--
-- Name: org_settings org_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.org_settings
    ADD CONSTRAINT org_settings_pkey PRIMARY KEY (org_id);


--
-- Name: orgs orgs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.orgs
    ADD CONSTRAINT orgs_pkey PRIMARY KEY (id);


--
-- Name: orgs orgs_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.orgs
    ADD CONSTRAINT orgs_slug_key UNIQUE (slug);


--
-- Name: pending_firings pending_firings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_pkey PRIMARY KEY (id);


--
-- Name: poller_state poller_state_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.poller_state
    ADD CONSTRAINT poller_state_pkey PRIMARY KEY (org_id, source, source_id);


--
-- Name: project_knowledge project_knowledge_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_knowledge
    ADD CONSTRAINT project_knowledge_pkey PRIMARY KEY (id);


--
-- Name: project_knowledge project_knowledge_project_id_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_knowledge
    ADD CONSTRAINT project_knowledge_project_id_key_key UNIQUE (project_id, key);


--
-- Name: projects projects_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_id_org_id_key UNIQUE (id, org_id);


--
-- Name: projects projects_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_pkey PRIMARY KEY (id);


--
-- Name: prompts prompts_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_id_org_id_key UNIQUE (id, org_id);


--
-- Name: prompts prompts_id_team_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

-- Parent key for the same-team composite FK (blueprint_steps.step_prompt_id)
-- so a blueprint step can only bind a prompt its own team owns (SKY-380).
ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_id_team_id_key UNIQUE (id, team_id);


--
-- Name: prompts prompts_org_team_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

-- Per-team re-seed idempotency key: one copy of each shipped prompt per team
-- (same system_slug, distinct team_id); NULLs distinct so user prompts never
-- collide (SKY-380).
ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_org_team_slug_key UNIQUE (org_id, team_id, system_slug);


--
-- Name: prompts prompts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_pkey PRIMARY KEY (org_id, id);


--
-- Name: repo_profiles repo_profiles_org_id_owner_repo_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.repo_profiles
    ADD CONSTRAINT repo_profiles_org_id_owner_repo_key UNIQUE (org_id, owner, repo);


--
-- Name: repo_profiles repo_profiles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.repo_profiles
    ADD CONSTRAINT repo_profiles_pkey PRIMARY KEY (id);


--
-- Name: system_llm_runs system_llm_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.system_llm_runs
    ADD CONSTRAINT system_llm_runs_pkey PRIMARY KEY (id);


--
-- Name: access_change_log access_change_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_change_log
    ADD CONSTRAINT access_change_log_pkey PRIMARY KEY (id);


--
-- Name: external_actions external_actions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_actions
    ADD CONSTRAINT external_actions_pkey PRIMARY KEY (id);


--
-- Name: artifacts artifacts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.artifacts
    ADD CONSTRAINT artifacts_pkey PRIMARY KEY (id);


--
-- Name: run_memory run_memory_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_memory
    ADD CONSTRAINT run_memory_pkey PRIMARY KEY (id);


--
-- Name: run_memory run_memory_run_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_memory
    ADD CONSTRAINT run_memory_run_id_key UNIQUE (run_id);


--
-- Name: run_messages run_messages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_messages
    ADD CONSTRAINT run_messages_pkey PRIMARY KEY (id);


--
-- Name: run_worktrees run_worktrees_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_worktrees
    ADD CONSTRAINT run_worktrees_pkey PRIMARY KEY (run_id, repo_id, ref);


--
-- Name: runs runs_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_id_org_id_key UNIQUE (id, org_id);


--
-- Name: runs runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);


--
-- Name: swipe_events swipe_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.swipe_events
    ADD CONSTRAINT swipe_events_pkey PRIMARY KEY (id);


--
-- Name: system_prompt_versions system_prompt_versions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.system_prompt_versions
    ADD CONSTRAINT system_prompt_versions_pkey PRIMARY KEY (org_id, prompt_id);


--
-- Name: task_events task_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_events
    ADD CONSTRAINT task_events_pkey PRIMARY KEY (task_id, event_id);


--
-- Name: tasks tasks_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_id_org_id_key UNIQUE (id, org_id);


--
-- Name: tasks tasks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_pkey PRIMARY KEY (id);


--
-- Name: task_teams task_teams_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_teams
    ADD CONSTRAINT task_teams_pkey PRIMARY KEY (task_id, team_id);


--
-- Name: team_agents team_agents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_agents
    ADD CONSTRAINT team_agents_pkey PRIMARY KEY (team_id, agent_id);


--
-- Name: team_settings team_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_settings
    ADD CONSTRAINT team_settings_pkey PRIMARY KEY (team_id);


--
-- Name: teams teams_org_id_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_org_id_slug_key UNIQUE (org_id, slug);


--
-- Name: teams teams_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_pkey PRIMARY KEY (id);


--
-- Name: user_github_identities user_github_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_github_identities
    ADD CONSTRAINT user_github_identities_pkey PRIMARY KEY (user_id, github_base_url);


--
-- Name: user_jira_identities user_jira_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_jira_identities
    ADD CONSTRAINT user_jira_identities_pkey PRIMARY KEY (user_id, jira_base_url);


--
-- Name: user_settings user_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_settings
    ADD CONSTRAINT user_settings_pkey PRIMARY KEY (user_id);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: agents_org_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX agents_org_idx ON public.agents USING btree (org_id);


--
-- Name: idx_curator_messages_request_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_curator_messages_request_created ON public.curator_messages USING btree (request_id, created_at, id);


--
-- Name: idx_curator_pending_context_consumer; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_curator_pending_context_consumer ON public.curator_pending_context USING btree (consumed_by_request_id) WHERE (consumed_by_request_id IS NOT NULL);


--
-- Name: idx_curator_pending_context_one_pending_per_type; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_curator_pending_context_one_pending_per_type ON public.curator_pending_context USING btree (project_id, curator_session_id, change_type) WHERE (consumed_at IS NULL);


--
-- Name: idx_curator_requests_in_flight; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_curator_requests_in_flight ON public.curator_requests USING btree (project_id) WHERE (status = ANY (ARRAY['queued'::text, 'running'::text]));


--
-- Name: idx_curator_requests_project_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_curator_requests_project_created ON public.curator_requests USING btree (project_id, created_at);


--
-- Name: idx_entities_closed_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entities_closed_at ON public.entities USING btree (closed_at) WHERE (closed_at IS NOT NULL);


--
-- Name: idx_entities_org_source_polled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entities_org_source_polled ON public.entities USING btree (org_id, source, last_polled_at);


--
-- Name: idx_entities_org_state; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entities_org_state ON public.entities USING btree (org_id, state);


--
-- Name: idx_entities_project_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entities_project_id ON public.entities USING btree (project_id) WHERE (project_id IS NOT NULL);


--
-- Name: idx_entity_links_from_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entity_links_from_kind ON public.entity_links USING btree (from_entity_id, kind);


--
-- Name: idx_entity_links_to_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entity_links_to_kind ON public.entity_links USING btree (to_entity_id, kind);


--
-- Name: idx_event_handlers_org_event_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_event_handlers_org_event_enabled ON public.event_handlers USING btree (org_id, event_type) WHERE (enabled = true);


--
-- Name: idx_event_handlers_org_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_event_handlers_org_kind ON public.event_handlers USING btree (org_id, kind);


--
-- Name: idx_event_handlers_blueprint; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_event_handlers_blueprint ON public.event_handlers USING btree (org_id, blueprint_id) WHERE (blueprint_id IS NOT NULL);


--
-- Name: event_handlers_one_trigger_per_blueprint; Type: INDEX; Schema: public; Owner: -
--
-- A blueprint is fired by exactly one event: at most one trigger may reference
-- a given blueprint. blueprint_id IS NOT NULL already implies kind='trigger'
-- (the rule-shape CHECK pins rules to a NULL blueprint_id), so the partial
-- predicate needs no kind clause. The mirror of the copy-only step index — that
-- one keeps a prompt in one blueprint, this one keeps a blueprint behind one
-- event. Events still fan out 1:many (one event_type may fire many blueprints
-- via many distinct trigger rows); this only bounds the per-blueprint side.

CREATE UNIQUE INDEX event_handlers_one_trigger_per_blueprint ON public.event_handlers USING btree (org_id, blueprint_id) WHERE (blueprint_id IS NOT NULL);


--
-- Name: idx_events_org_entity_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_events_org_entity_created ON public.events USING btree (org_id, entity_id, created_at DESC);


--
-- Name: idx_events_org_entity_occurred; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_events_org_entity_occurred ON public.events USING btree (org_id, entity_id, occurred_at DESC);


--
-- Name: idx_events_org_type_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_events_org_type_created ON public.events USING btree (org_id, event_type, created_at DESC);


--
-- Name: idx_events_org_type_entity; Type: INDEX; Schema: public; Owner: -
--
-- Supports the per-org SELECT event_type, COUNT(DISTINCT entity_id)
-- aggregate behind FactoryReadStore.LifetimeDistinctByEventType. The
-- row layout is sorted by (org_id, event_type, entity_id) so each
-- org's groups are contiguous and per-group entity_ids are adjacent —
-- DISTINCT collapses to adjacency dedup, no temp B-tree.
--

CREATE INDEX idx_events_org_type_entity ON public.events USING btree (org_id, event_type, entity_id) WHERE (entity_id IS NOT NULL);


--
-- Name: idx_pending_firings_dedup; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_pending_firings_dedup ON public.pending_firings USING btree (task_id, trigger_id) WHERE (status = 'pending'::text);


--
-- Name: idx_pending_firings_entity_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_pending_firings_entity_pending ON public.pending_firings USING btree (entity_id, queued_at) WHERE (status = 'pending'::text);


--
-- Name: idx_repo_profiles_org_owner_repo; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_repo_profiles_org_owner_repo ON public.repo_profiles USING btree (org_id, owner, repo);


--
-- Name: idx_system_llm_runs_org_started; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_system_llm_runs_org_started ON public.system_llm_runs USING btree (org_id, started_at DESC);


--
-- Name: idx_system_llm_runs_org_job_started; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_system_llm_runs_org_job_started ON public.system_llm_runs USING btree (org_id, job, started_at DESC);


--
-- Name: idx_access_change_log_org_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_access_change_log_org_created ON public.access_change_log USING btree (org_id, created_at DESC);


--
-- Name: idx_external_actions_dedup; Type: INDEX; Schema: public; Owner: -
-- The natural-key uniqueness the append-only dedup rides on: ON CONFLICT
-- (org_id, dedup_key) DO NOTHING collapses the branch hook+proxy twin (which
-- share a deterministic key) and rejects any other true duplicate, never a
-- mutation.
--

CREATE UNIQUE INDEX idx_external_actions_dedup ON public.external_actions USING btree (org_id, dedup_key);


--
-- Name: idx_external_actions_org_occurred; Type: INDEX; Schema: public; Owner: -
-- Backs the org-wide, newest-first scan of the action-log org feed
-- (ExternalActionStore.ListByOrgSystem) — the cross-team governance read.
--

CREATE INDEX idx_external_actions_org_occurred ON public.external_actions USING btree (org_id, occurred_at DESC);


--
-- Name: idx_external_actions_team_occurred; Type: INDEX; Schema: public; Owner: -
-- Backs the team-scoped action feed (ExternalActionStore.ListByTeam), the
-- team-grain sibling of the org index.
--

CREATE INDEX idx_external_actions_team_occurred ON public.external_actions USING btree (org_id, team_id, occurred_at DESC);


--
-- Name: idx_external_actions_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_external_actions_run ON public.external_actions USING btree (run_id);


--
-- Name: idx_artifacts_dedup; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_artifacts_dedup ON public.artifacts USING btree (org_id, dedup_key);


--
-- Name: idx_artifacts_team_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_artifacts_team_created ON public.artifacts USING btree (team_id, created_at DESC);


--
-- Name: idx_artifacts_org_created; Type: INDEX; Schema: public; Owner: -
-- Backs the org-wide, newest-first scan of the bot-activity audit feed
-- (ArtifactStore.ListByOrgSystem, TFAC-483) — the org-grain sibling of
-- idx_artifacts_team_created.
--

CREATE INDEX idx_artifacts_org_created ON public.artifacts USING btree (org_id, created_at DESC);


--
-- Name: idx_artifacts_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_artifacts_run ON public.artifacts USING btree (run_id);


--
-- Name: idx_run_memory_entity_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_run_memory_entity_created ON public.run_memory USING btree (entity_id, created_at);


--
-- Name: idx_run_memory_entity_blueprint; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_run_memory_entity_blueprint ON public.run_memory USING btree (entity_id, blueprint_run_id);


--
-- Name: idx_run_memory_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_run_memory_run ON public.run_memory USING btree (run_id);


--
-- Name: idx_run_messages_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_run_messages_run ON public.run_messages USING btree (run_id);


--
-- Name: idx_run_worktrees_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_run_worktrees_run ON public.run_worktrees USING btree (run_id);


--
-- Name: idx_runs_org_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_runs_org_status ON public.runs USING btree (org_id, status);


--
-- Name: idx_runs_prompt_started; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_runs_prompt_started ON public.runs USING btree (prompt_id, started_at DESC);


--
-- Name: idx_runs_task; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_runs_task ON public.runs USING btree (task_id);


--
-- Name: idx_runs_trigger; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_runs_trigger ON public.runs USING btree (trigger_id);


--
-- Name: idx_swipe_events_action_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_swipe_events_action_created ON public.swipe_events USING btree (action, created_at);


--
-- Name: idx_swipe_events_task; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_swipe_events_task ON public.swipe_events USING btree (task_id);


--
-- Name: idx_task_events_event; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_events_event ON public.task_events USING btree (event_id);


--
-- Name: idx_task_events_task; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_events_task ON public.task_events USING btree (task_id);


--
-- Name: idx_tasks_active_entity_event_dedup; Type: INDEX; Schema: public; Owner: -
--

-- One task per real situation: identity is (entity, event_type,
-- dedup_key), independent of team. The teams an event is relevant to
-- are visibility (task_teams), never count — one event matching N
-- teams' rules yields one task, not N.
CREATE UNIQUE INDEX idx_tasks_active_entity_event_dedup ON public.tasks USING btree (entity_id, event_type, dedup_key) WHERE (status <> ALL (ARRAY['done'::text, 'dismissed'::text]));


--
-- Name: idx_task_teams_team; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_teams_team ON public.task_teams USING btree (team_id);


--
-- Name: idx_tasks_entity; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tasks_entity ON public.tasks USING btree (entity_id);


--
-- Name: idx_tasks_org_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tasks_org_status ON public.tasks USING btree (org_id, status);


--
-- Name: idx_tasks_org_status_priority; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tasks_org_status_priority ON public.tasks USING btree (org_id, status, priority_score DESC);


--
-- Name: project_knowledge_org_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX project_knowledge_org_idx ON public.project_knowledge USING btree (org_id, project_id);


--
-- Name: runs_actor_agent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX runs_actor_agent_idx ON public.runs USING btree (actor_agent_id) WHERE (actor_agent_id IS NOT NULL);


--
-- Name: runs_event_trigger_fence; Type: INDEX; Schema: public; Owner: -
--
-- Fired-fence (SKY-424): one event firing one trigger materializes at most
-- one run. Partial WHERE triggering_event_id IS NOT NULL so manual and
-- blueprint-step runs (NULL) never participate — multiple manual runs of one
-- task stay allowed, and two distinct event instances still fire independently.

CREATE UNIQUE INDEX runs_event_trigger_fence ON public.runs USING btree (triggering_event_id, trigger_id) WHERE (triggering_event_id IS NOT NULL);


--
-- Name: tasks_claimed_agent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tasks_claimed_agent_idx ON public.tasks USING btree (claimed_by_agent_id) WHERE (claimed_by_agent_id IS NOT NULL);


--
-- Name: tasks_claimed_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tasks_claimed_user_idx ON public.tasks USING btree (claimed_by_user_id) WHERE (claimed_by_user_id IS NOT NULL);


--
-- Name: team_agents_agent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX team_agents_agent_idx ON public.team_agents USING btree (agent_id);


--
-- Name: team_agents_team_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX team_agents_team_idx ON public.team_agents USING btree (team_id);


--
-- Name: team_github_repos_lower_owner_repo_idx; Type: INDEX; Schema: public; Owner: -
--

-- Functional index backing the factory belt's tracked-repo semi-join
-- (TFAC-516, factoryGitHubRepoTrackedExists). The belt matches a GitHub
-- entity's repo against the tracked set by lower(owner)/lower(repo) across
-- *all* of the viewer's teams, not a single team_id, so the (team_id, owner,
-- repo) primary key can't serve the lookup (team_id isn't pinned). Without
-- this index that EXISTS scans the org's tracked repos per entity row — fine
-- for the old ever-tasked population, but the belt is intentionally larger
-- now. owner/repo lead (equality-filtered); team_id trails so the teams
-- join + RLS membership check read it straight from the index. lower() on
-- both axes mirrors TracksRepoSystem's case-insensitive match.
CREATE INDEX team_github_repos_lower_owner_repo_idx ON public.team_github_repos USING btree (lower(owner), lower(repo), team_id);


--
-- Name: org_memberships org_memberships_keep_owner_on_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER org_memberships_keep_owner_on_delete AFTER DELETE ON public.org_memberships REFERENCING OLD TABLE AS affected FOR EACH STATEMENT EXECUTE FUNCTION tf.guard_org_owners();


--
-- Name: org_memberships org_memberships_keep_owner_on_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER org_memberships_keep_owner_on_update AFTER UPDATE ON public.org_memberships REFERENCING OLD TABLE AS affected FOR EACH STATEMENT EXECUTE FUNCTION tf.guard_org_owners();


--
-- Name: orgs orgs_guard_owner_transfer; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER orgs_guard_owner_transfer BEFORE UPDATE OF owner_user_id ON public.orgs FOR EACH ROW EXECUTE FUNCTION tf.guard_org_owner_transfer();


--
-- Name: agents set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.agents FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: event_handlers set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.event_handlers FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: jira_project_status_rules set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.jira_project_status_rules FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: org_settings set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.org_settings FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: orgs set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.orgs FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: project_knowledge set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.project_knowledge FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: projects set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.projects FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: prompts set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.prompts FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: repo_profiles set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.repo_profiles FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: team_settings set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.team_settings FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: teams set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.teams FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: user_github_identities set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.user_github_identities FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: user_jira_identities set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.user_jira_identities FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: user_settings set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.user_settings FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: users set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.users FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: agents agents_github_pat_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT agents_github_pat_user_id_fkey FOREIGN KEY (github_pat_user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: agents agents_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT agents_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: curator_messages curator_messages_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_messages
    ADD CONSTRAINT curator_messages_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: curator_messages curator_messages_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_messages
    ADD CONSTRAINT curator_messages_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: curator_messages curator_messages_request_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_messages
    ADD CONSTRAINT curator_messages_request_id_org_id_fkey FOREIGN KEY (request_id, org_id) REFERENCES public.curator_requests(id, org_id) ON DELETE CASCADE;


--
-- Name: curator_pending_context curator_pending_context_consumed_by_request_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_pending_context
    ADD CONSTRAINT curator_pending_context_consumed_by_request_id_org_id_fkey FOREIGN KEY (consumed_by_request_id, org_id) REFERENCES public.curator_requests(id, org_id) ON DELETE SET NULL;


--
-- Name: curator_pending_context curator_pending_context_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_pending_context
    ADD CONSTRAINT curator_pending_context_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: curator_pending_context curator_pending_context_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_pending_context
    ADD CONSTRAINT curator_pending_context_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: curator_pending_context curator_pending_context_project_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_pending_context
    ADD CONSTRAINT curator_pending_context_project_id_org_id_fkey FOREIGN KEY (project_id, org_id) REFERENCES public.projects(id, org_id) ON DELETE CASCADE;


--
-- Name: curator_requests curator_requests_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_requests
    ADD CONSTRAINT curator_requests_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: curator_requests curator_requests_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_requests
    ADD CONSTRAINT curator_requests_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: curator_requests curator_requests_project_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.curator_requests
    ADD CONSTRAINT curator_requests_project_id_org_id_fkey FOREIGN KEY (project_id, org_id) REFERENCES public.projects(id, org_id) ON DELETE CASCADE;


--
-- Name: entities entities_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entities
    ADD CONSTRAINT entities_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: entities entities_project_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entities
    ADD CONSTRAINT entities_project_id_org_id_fkey FOREIGN KEY (project_id, org_id) REFERENCES public.projects(id, org_id) ON DELETE SET NULL;


--
-- Name: entities entities_owning_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entities
    ADD CONSTRAINT entities_owning_team_id_fkey FOREIGN KEY (owning_team_id) REFERENCES public.teams(id) ON DELETE SET NULL;


--
-- Name: entity_links entity_links_from_entity_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_links
    ADD CONSTRAINT entity_links_from_entity_id_org_id_fkey FOREIGN KEY (from_entity_id, org_id) REFERENCES public.entities(id, org_id) ON DELETE CASCADE;


--
-- Name: entity_links entity_links_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_links
    ADD CONSTRAINT entity_links_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: entity_links entity_links_to_entity_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_links
    ADD CONSTRAINT entity_links_to_entity_id_org_id_fkey FOREIGN KEY (to_entity_id, org_id) REFERENCES public.entities(id, org_id) ON DELETE CASCADE;


--
-- Name: event_handlers event_handlers_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: event_handlers event_handlers_event_type_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_event_type_fkey FOREIGN KEY (event_type) REFERENCES public.events_catalog(id) ON DELETE RESTRICT;


--
-- Name: event_handlers event_handlers_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


-- event_handlers trigger -> blueprints FK constraints
-- (event_handlers_blueprint_id_org_id_fkey / _team_id_fkey) are added in the
-- Blueprints section near the bottom of this file, after the blueprints table
-- exists.


--
-- Name: event_handlers event_handlers_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: events events_entity_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_entity_id_org_id_fkey FOREIGN KEY (entity_id, org_id) REFERENCES public.entities(id, org_id);


--
-- Name: events events_event_type_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_event_type_fkey FOREIGN KEY (event_type) REFERENCES public.events_catalog(id);


--
-- Name: events events_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: jira_project_status_rules jira_project_status_rules_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.jira_project_status_rules
    ADD CONSTRAINT jira_project_status_rules_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: team_github_groups team_github_groups_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_github_groups
    ADD CONSTRAINT team_github_groups_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: team_github_repos team_github_repos_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_github_repos
    ADD CONSTRAINT team_github_repos_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: memberships memberships_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memberships
    ADD CONSTRAINT memberships_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: memberships memberships_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memberships
    ADD CONSTRAINT memberships_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: org_memberships org_memberships_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.org_memberships
    ADD CONSTRAINT org_memberships_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: org_memberships org_memberships_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.org_memberships
    ADD CONSTRAINT org_memberships_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: org_settings org_settings_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.org_settings
    ADD CONSTRAINT org_settings_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: orgs orgs_owner_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.orgs
    ADD CONSTRAINT orgs_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES public.users(id);


--
-- Name: pending_firings pending_firings_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: pending_firings pending_firings_entity_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_entity_id_org_id_fkey FOREIGN KEY (entity_id, org_id) REFERENCES public.entities(id, org_id) ON DELETE CASCADE;


--
-- Name: pending_firings pending_firings_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: pending_firings pending_firings_task_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_task_id_org_id_fkey FOREIGN KEY (task_id, org_id) REFERENCES public.tasks(id, org_id) ON DELETE CASCADE;


--
-- Name: pending_firings pending_firings_trigger_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_trigger_id_org_id_fkey FOREIGN KEY (trigger_id, org_id) REFERENCES public.event_handlers(id, org_id) ON DELETE CASCADE;


--
-- Name: pending_firings pending_firings_triggering_event_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_triggering_event_id_org_id_fkey FOREIGN KEY (triggering_event_id, org_id) REFERENCES public.events(id, org_id);


--
-- Name: poller_state poller_state_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.poller_state
    ADD CONSTRAINT poller_state_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: project_knowledge project_knowledge_last_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_knowledge
    ADD CONSTRAINT project_knowledge_last_updated_by_fkey FOREIGN KEY (last_updated_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: project_knowledge project_knowledge_last_updated_by_run_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_knowledge
    ADD CONSTRAINT project_knowledge_last_updated_by_run_fkey FOREIGN KEY (last_updated_by_run, org_id) REFERENCES public.runs(id, org_id) ON DELETE SET NULL;


--
-- Name: project_knowledge project_knowledge_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_knowledge
    ADD CONSTRAINT project_knowledge_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: project_knowledge project_knowledge_project_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_knowledge
    ADD CONSTRAINT project_knowledge_project_id_org_id_fkey FOREIGN KEY (project_id, org_id) REFERENCES public.projects(id, org_id) ON DELETE CASCADE;


--
-- Name: projects projects_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: projects projects_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


-- projects.spec_authorship_blueprint_id -> blueprints FK
-- (projects_spec_authorship_blueprint_id_org_id_fkey) is added in the
-- Blueprints section near the bottom of this file, after the blueprints table
-- exists.


--
-- Name: projects projects_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;


--
-- Name: prompts prompts_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: prompts prompts_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: prompts prompts_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: repo_profiles repo_profiles_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.repo_profiles
    ADD CONSTRAINT repo_profiles_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: system_llm_runs system_llm_runs_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.system_llm_runs
    ADD CONSTRAINT system_llm_runs_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: access_change_log access_change_log_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

-- Only org_id carries an FK (ON DELETE CASCADE: drop an org's log with the org).
-- actor_user_id / target_user_id / team_id are deliberately FK-free so the audit
-- row survives the deletion of the user/team it references — an audit log must
-- outlive its subjects.
ALTER TABLE ONLY public.access_change_log
    ADD CONSTRAINT access_change_log_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: external_actions external_actions_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

-- org_id CASCADE (drop an org's log with the org); run_id SET NULL (the action
-- outlives a purged run for the audit trail, like artifacts). team_id and
-- actor_user_id are deliberately FK-free so the audit row outlives the team/user
-- it references — an audit log must outlive its subjects (same rule as
-- access_change_log).
ALTER TABLE ONLY public.external_actions
    ADD CONSTRAINT external_actions_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: external_actions external_actions_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_actions
    ADD CONSTRAINT external_actions_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE SET NULL;


--
-- Name: artifacts artifacts_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.artifacts
    ADD CONSTRAINT artifacts_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: artifacts artifacts_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

-- Single-column ref to runs(id) with ON DELETE SET NULL: the artifact
-- outlives a purged run for the audit ledger. A composite (run_id, org_id)
-- ref like the other run children can't SET NULL here because org_id is
-- NOT NULL — and the artifact's own org_id_fkey already pins the org.
ALTER TABLE ONLY public.artifacts
    ADD CONSTRAINT artifacts_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE SET NULL;


--
-- Name: artifacts artifacts_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.artifacts
    ADD CONSTRAINT artifacts_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;


--
-- Name: run_memory run_memory_entity_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_memory
    ADD CONSTRAINT run_memory_entity_id_org_id_fkey FOREIGN KEY (entity_id, org_id) REFERENCES public.entities(id, org_id);


--
-- Name: run_memory run_memory_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_memory
    ADD CONSTRAINT run_memory_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: run_memory run_memory_run_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_memory
    ADD CONSTRAINT run_memory_run_id_org_id_fkey FOREIGN KEY (run_id, org_id) REFERENCES public.runs(id, org_id) ON DELETE CASCADE;


--
-- Name: run_messages run_messages_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_messages
    ADD CONSTRAINT run_messages_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: run_messages run_messages_run_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_messages
    ADD CONSTRAINT run_messages_run_id_org_id_fkey FOREIGN KEY (run_id, org_id) REFERENCES public.runs(id, org_id) ON DELETE CASCADE;


--
-- Name: run_worktrees run_worktrees_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_worktrees
    ADD CONSTRAINT run_worktrees_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: run_worktrees run_worktrees_run_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_worktrees
    ADD CONSTRAINT run_worktrees_run_id_org_id_fkey FOREIGN KEY (run_id, org_id) REFERENCES public.runs(id, org_id) ON DELETE CASCADE;


--
-- Name: runs runs_actor_agent_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_actor_agent_fkey FOREIGN KEY (actor_agent_id, org_id) REFERENCES public.agents(id, org_id) ON DELETE SET NULL;


--
-- Name: runs runs_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: runs runs_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: runs runs_prompt_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_prompt_id_org_id_fkey FOREIGN KEY (prompt_id, org_id) REFERENCES public.prompts(id, org_id);


--
-- Name: runs runs_task_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_task_id_org_id_fkey FOREIGN KEY (task_id, org_id) REFERENCES public.tasks(id, org_id);


--
-- Name: runs runs_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;


--
-- Name: runs runs_trigger_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_trigger_id_org_id_fkey FOREIGN KEY (trigger_id, org_id) REFERENCES public.event_handlers(id, org_id);


--
-- Name: runs runs_triggering_event_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--
-- Composite FK mirrors pending_firings.triggering_event_id. NULL
-- triggering_event_id (manual / blueprint-step runs) skips the check under
-- MATCH SIMPLE, so only event-fired runs are tied to a real event row.

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_triggering_event_id_org_id_fkey FOREIGN KEY (triggering_event_id, org_id) REFERENCES public.events(id, org_id);


--
-- Name: sessions sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: sessions sessions_active_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--
-- ON DELETE SET NULL so deleting an org doesn't cascade-delete every
-- session that pointed at it; the next request from those sessions
-- falls through to "no active org" behavior (handler returns 409 and
-- the SPA prompts the user to pick another org).
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_active_org_id_fkey FOREIGN KEY (active_org_id) REFERENCES public.orgs(id) ON DELETE SET NULL;


--
-- Name: swipe_events swipe_events_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.swipe_events
    ADD CONSTRAINT swipe_events_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: swipe_events swipe_events_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.swipe_events
    ADD CONSTRAINT swipe_events_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: swipe_events swipe_events_task_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.swipe_events
    ADD CONSTRAINT swipe_events_task_id_org_id_fkey FOREIGN KEY (task_id, org_id) REFERENCES public.tasks(id, org_id);


--
-- Name: system_prompt_versions system_prompt_versions_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.system_prompt_versions
    ADD CONSTRAINT system_prompt_versions_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: system_prompt_versions system_prompt_versions_prompt_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.system_prompt_versions
    ADD CONSTRAINT system_prompt_versions_prompt_id_org_id_fkey FOREIGN KEY (prompt_id, org_id) REFERENCES public.prompts(id, org_id) ON DELETE CASCADE;


--
-- Name: task_events task_events_event_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_events
    ADD CONSTRAINT task_events_event_id_org_id_fkey FOREIGN KEY (event_id, org_id) REFERENCES public.events(id, org_id) ON DELETE CASCADE;


--
-- Name: task_events task_events_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_events
    ADD CONSTRAINT task_events_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: task_events task_events_task_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_events
    ADD CONSTRAINT task_events_task_id_org_id_fkey FOREIGN KEY (task_id, org_id) REFERENCES public.tasks(id, org_id) ON DELETE CASCADE;


--
-- Name: tasks tasks_claimed_agent_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_claimed_agent_fkey FOREIGN KEY (claimed_by_agent_id, org_id) REFERENCES public.agents(id, org_id) ON DELETE SET NULL;


--
-- Name: tasks tasks_claimed_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_claimed_by_user_id_fkey FOREIGN KEY (claimed_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: tasks tasks_close_event_type_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_close_event_type_fkey FOREIGN KEY (close_event_type) REFERENCES public.events_catalog(id);


--
-- Name: tasks tasks_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: tasks tasks_entity_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_entity_id_org_id_fkey FOREIGN KEY (entity_id, org_id) REFERENCES public.entities(id, org_id);


--
-- Name: tasks tasks_event_type_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_event_type_fkey FOREIGN KEY (event_type) REFERENCES public.events_catalog(id) ON DELETE RESTRICT;


--
-- Name: tasks tasks_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: tasks tasks_primary_event_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_primary_event_id_org_id_fkey FOREIGN KEY (primary_event_id, org_id) REFERENCES public.events(id, org_id);


--
-- Name: tasks tasks_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tasks
    ADD CONSTRAINT tasks_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;


--
-- Name: task_teams task_teams_task_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_teams
    ADD CONSTRAINT task_teams_task_id_fkey FOREIGN KEY (task_id) REFERENCES public.tasks(id) ON DELETE CASCADE;


--
-- Name: task_teams task_teams_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_teams
    ADD CONSTRAINT task_teams_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: team_agents team_agents_agent_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_agents
    ADD CONSTRAINT team_agents_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES public.agents(id) ON DELETE CASCADE;


--
-- Name: team_agents team_agents_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_agents
    ADD CONSTRAINT team_agents_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: team_settings team_settings_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_settings
    ADD CONSTRAINT team_settings_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: teams teams_created_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_created_by_user_id_fkey FOREIGN KEY (created_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: teams teams_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: user_github_identities user_github_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_github_identities
    ADD CONSTRAINT user_github_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_jira_identities user_jira_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_jira_identities
    ADD CONSTRAINT user_jira_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_settings user_settings_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_settings
    ADD CONSTRAINT user_settings_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: users users_default_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_default_org_id_fkey FOREIGN KEY (default_org_id) REFERENCES public.orgs(id) ON DELETE SET NULL;


--
-- public.users is the PRINCIPAL table (one row per human) and is intentionally
-- NOT foreign-keyed to auth.users: one principal can hold several GoTrue login
-- identities (GitHub + N SSO providers), since GoTrue mints a distinct
-- auth.users per SSO provider and refuses to link them. The bridge to GoTrue
-- lives on public.user_identities.auth_user_id; identities resolve to one
-- principal via verified-email linking at login. See the principal identity
-- model near the end of this file.
--


--
-- Name: agents; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.agents ENABLE ROW LEVEL SECURITY;

--
-- Name: agents agents_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY agents_delete ON public.agents FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));


--
-- Name: agents agents_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY agents_insert ON public.agents FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id) AND ((github_pat_user_id IS NULL) OR (EXISTS ( SELECT 1
   FROM public.org_memberships
  WHERE ((org_memberships.user_id = agents.github_pat_user_id) AND (org_memberships.org_id = agents.org_id)))))));


--
-- Name: agents agents_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY agents_select ON public.agents FOR SELECT TO tf_app USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: agents agents_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY agents_update ON public.agents FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id) AND ((github_pat_user_id IS NULL) OR (EXISTS ( SELECT 1
   FROM public.org_memberships
  WHERE ((org_memberships.user_id = agents.github_pat_user_id) AND (org_memberships.org_id = agents.org_id)))))));


--
-- Name: curator_messages; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.curator_messages ENABLE ROW LEVEL SECURITY;

--
-- Name: curator_messages curator_messages_modify; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY curator_messages_modify ON public.curator_messages USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()))) WITH CHECK (((org_id = tf.current_org_id()) AND (creator_user_id = tf.current_user_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: curator_messages curator_messages_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY curator_messages_select ON public.curator_messages FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id())));


--
-- Name: curator_pending_context; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.curator_pending_context ENABLE ROW LEVEL SECURITY;

--
-- Name: curator_pending_context curator_pending_context_modify; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY curator_pending_context_modify ON public.curator_pending_context USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()))) WITH CHECK (((org_id = tf.current_org_id()) AND (creator_user_id = tf.current_user_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: curator_pending_context curator_pending_context_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY curator_pending_context_select ON public.curator_pending_context FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id())));


--
-- Name: curator_requests; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.curator_requests ENABLE ROW LEVEL SECURITY;

--
-- Name: curator_requests curator_requests_modify; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY curator_requests_modify ON public.curator_requests USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()))) WITH CHECK (((org_id = tf.current_org_id()) AND (creator_user_id = tf.current_user_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: curator_requests curator_requests_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY curator_requests_select ON public.curator_requests FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id())));


--
-- Name: entities; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.entities ENABLE ROW LEVEL SECURITY;

--
-- Name: entities entities_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY entities_all ON public.entities USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: entity_links; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.entity_links ENABLE ROW LEVEL SECURITY;

--
-- Name: entity_links entity_links_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY entity_links_all ON public.entity_links USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: event_handlers; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.event_handlers ENABLE ROW LEVEL SECURITY;

--
-- Name: event_handlers event_handlers_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY event_handlers_delete ON public.event_handlers FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)));


--
-- Name: event_handlers event_handlers_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY event_handlers_insert ON public.event_handlers FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND tf.user_can_write_team(team_id)));


--
-- Name: event_handlers event_handlers_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY event_handlers_select ON public.event_handlers FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND ((creator_user_id = tf.current_user_id()) OR (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.user_id = tf.current_user_id()) AND (m.team_id = event_handlers.team_id)))))));


--
-- Name: event_handlers event_handlers_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY event_handlers_update ON public.event_handlers FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)));


--
-- Name: events; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.events ENABLE ROW LEVEL SECURITY;

--
-- Name: events events_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY events_all ON public.events USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: jira_project_status_rules; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.jira_project_status_rules ENABLE ROW LEVEL SECURITY;

--
-- Name: jira_project_status_rules jira_rules_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY jira_rules_delete ON public.jira_project_status_rules FOR DELETE USING ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: jira_project_status_rules jira_rules_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY jira_rules_insert ON public.jira_project_status_rules FOR INSERT WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: jira_project_status_rules jira_rules_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY jira_rules_select ON public.jira_project_status_rules FOR SELECT USING ((tf.team_in_current_org(team_id) AND (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.team_id = jira_project_status_rules.team_id) AND (m.user_id = tf.current_user_id()))))));


--
-- Name: jira_project_status_rules jira_rules_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY jira_rules_update ON public.jira_project_status_rules FOR UPDATE USING ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id))) WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: team_github_groups; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.team_github_groups ENABLE ROW LEVEL SECURITY;

--
-- Name: team_github_groups team_github_groups_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_github_groups_select ON public.team_github_groups FOR SELECT USING ((tf.team_in_current_org(team_id) AND (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.team_id = team_github_groups.team_id) AND (m.user_id = tf.current_user_id()))))));


--
-- Name: team_github_groups team_github_groups_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_github_groups_insert ON public.team_github_groups FOR INSERT WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: team_github_groups team_github_groups_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_github_groups_delete ON public.team_github_groups FOR DELETE USING ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: team_github_repos; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.team_github_repos ENABLE ROW LEVEL SECURITY;

--
-- Name: team_github_repos team_github_repos_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_github_repos_select ON public.team_github_repos FOR SELECT USING ((tf.team_in_current_org(team_id) AND (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.team_id = team_github_repos.team_id) AND (m.user_id = tf.current_user_id()))))));


--
-- Name: team_github_repos team_github_repos_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_github_repos_insert ON public.team_github_repos FOR INSERT WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: team_github_repos team_github_repos_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_github_repos_update ON public.team_github_repos FOR UPDATE USING ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id))) WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: team_github_repos team_github_repos_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_github_repos_delete ON public.team_github_repos FOR DELETE USING ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: memberships; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.memberships ENABLE ROW LEVEL SECURITY;

--
-- Name: memberships memberships_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY memberships_delete ON public.memberships FOR DELETE USING (((user_id = tf.current_user_id()) OR (tf.team_in_current_org(team_id) AND (tf.user_is_team_admin(team_id) OR tf.user_is_org_admin_via_team(team_id)))));


--
-- Name: memberships memberships_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY memberships_insert ON public.memberships FOR INSERT WITH CHECK ((tf.team_in_current_org(team_id) AND (tf.user_is_team_admin(team_id) OR tf.user_is_org_admin_via_team(team_id))));


--
-- Name: memberships memberships_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY memberships_select ON public.memberships FOR SELECT USING (((user_id = tf.current_user_id()) OR (EXISTS ( SELECT 1
   FROM public.teams t
  WHERE ((t.id = memberships.team_id) AND (t.org_id = tf.current_org_id()) AND tf.user_has_org_access(t.org_id))))));


--
-- Name: memberships memberships_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY memberships_update ON public.memberships FOR UPDATE USING ((tf.team_in_current_org(team_id) AND (tf.user_is_team_admin(team_id) OR tf.user_is_org_admin_via_team(team_id)))) WITH CHECK ((tf.team_in_current_org(team_id) AND (tf.user_is_team_admin(team_id) OR tf.user_is_org_admin_via_team(team_id))));


--
-- Name: org_memberships; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.org_memberships ENABLE ROW LEVEL SECURITY;

--
-- Name: org_memberships org_memberships_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_memberships_delete ON public.org_memberships FOR DELETE USING (((user_id = tf.current_user_id()) OR ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))));


--
-- Name: org_memberships org_memberships_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_memberships_insert ON public.org_memberships FOR INSERT WITH CHECK ((((user_id = tf.current_user_id()) AND tf.user_owns_org(org_id)) OR ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))));


--
-- Name: org_memberships org_memberships_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_memberships_select ON public.org_memberships FOR SELECT USING (((user_id = tf.current_user_id()) OR tf.user_has_org_access(org_id)));


--
-- Name: org_memberships org_memberships_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_memberships_update ON public.org_memberships FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));


--
-- Name: org_settings; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.org_settings ENABLE ROW LEVEL SECURITY;

--
-- Name: org_settings org_settings_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_settings_delete ON public.org_settings FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));


--
-- Name: org_settings org_settings_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_settings_insert ON public.org_settings FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));


--
-- Name: org_settings org_settings_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_settings_select ON public.org_settings FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: org_settings org_settings_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_settings_update ON public.org_settings FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));


--
-- Name: orgs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.orgs ENABLE ROW LEVEL SECURITY;

--
-- Name: orgs orgs_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY orgs_insert ON public.orgs FOR INSERT WITH CHECK ((owner_user_id = tf.current_user_id()));


--
-- Name: orgs orgs_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY orgs_select ON public.orgs FOR SELECT USING ((((id = tf.current_org_id()) AND tf.user_has_org_access(id)) OR (owner_user_id = tf.current_user_id())));


--
-- Name: orgs orgs_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY orgs_update ON public.orgs FOR UPDATE USING (((id = tf.current_org_id()) AND tf.user_is_org_admin(id))) WITH CHECK (((id = tf.current_org_id()) AND tf.user_is_org_admin(id)));


--
-- Name: pending_firings; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.pending_firings ENABLE ROW LEVEL SECURITY;

--
-- Name: pending_firings pending_firings_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY pending_firings_all ON public.pending_firings USING ((EXISTS ( SELECT 1
   FROM public.tasks t
  WHERE (t.id = pending_firings.task_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.tasks t
  WHERE (t.id = pending_firings.task_id))));


--
-- Name: poller_state; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.poller_state ENABLE ROW LEVEL SECURITY;

--
-- Name: poller_state poller_state_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY poller_state_all ON public.poller_state USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: project_knowledge; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.project_knowledge ENABLE ROW LEVEL SECURITY;

--
-- Name: project_knowledge project_knowledge_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY project_knowledge_all ON public.project_knowledge USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: projects; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.projects ENABLE ROW LEVEL SECURITY;

--
-- Name: projects projects_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY projects_delete ON public.projects FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (team_id IS NOT NULL) AND tf.user_can_write_team(team_id)))));


--
-- Name: projects projects_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY projects_insert ON public.projects FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND ((visibility <> 'team'::text) OR ((team_id IS NOT NULL) AND tf.user_can_write_team(team_id))) AND ((visibility <> 'org'::text) OR tf.user_is_org_admin(org_id))));


--
-- Name: projects projects_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY projects_select ON public.projects FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND ((creator_user_id = tf.current_user_id()) OR ((visibility = 'team'::text) AND (team_id IS NOT NULL) AND (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.user_id = tf.current_user_id()) AND (m.team_id = projects.team_id))))) OR (visibility = 'org'::text))));


--
-- Name: projects projects_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY projects_update ON public.projects FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (team_id IS NOT NULL) AND tf.user_can_write_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id))))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (team_id IS NOT NULL) AND tf.user_can_write_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id)))));


--
-- Name: prompts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.prompts ENABLE ROW LEVEL SECURITY;

--
-- Name: prompts prompts_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY prompts_delete ON public.prompts FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)));


--
-- Name: prompts prompts_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY prompts_insert ON public.prompts FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND tf.user_can_write_team(team_id)));


--
-- Name: prompts prompts_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY prompts_select ON public.prompts FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND ((creator_user_id = tf.current_user_id()) OR (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.user_id = tf.current_user_id()) AND (m.team_id = prompts.team_id)))))));


--
-- Name: prompts prompts_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY prompts_update ON public.prompts FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)));


--
-- Name: repo_profiles; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.repo_profiles ENABLE ROW LEVEL SECURITY;

--
-- Name: repo_profiles repo_profiles_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY repo_profiles_all ON public.repo_profiles USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: system_llm_runs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.system_llm_runs ENABLE ROW LEVEL SECURITY;

--
-- Name: system_llm_runs system_llm_runs_all; Type: POLICY; Schema: public; Owner: -
--

-- Mirrors repo_profiles_all: org-scoped read/write under the app pool.
-- The table is system-written via the admin pool (BYPASSRLS), so in
-- practice only the org-scoped SELECT side is exercised by tf_app; the
-- WITH CHECK is retained for symmetry with the rest of the schema.
CREATE POLICY system_llm_runs_all ON public.system_llm_runs USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: access_change_log; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.access_change_log ENABLE ROW LEVEL SECURITY;

--
-- Name: access_change_log access_change_log_all; Type: POLICY; Schema: public; Owner: -
--

-- Mirrors system_llm_runs_all: org-scoped read/write under the app pool. Unlike
-- system_llm_runs (admin-written), this table IS written through tf_app — every
-- Record composes inside the claims-bearing WithTx that runs the audited
-- governance action, so the WITH CHECK side is exercised on write, and the
-- future org-admin audit view reads under the USING side. (The invite-accept
-- org_member_granted write is the one admin-pool exception — the invitee has no
-- RLS standing to insert their own membership — and bypasses this policy.)
CREATE POLICY access_change_log_all ON public.access_change_log USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: external_actions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.external_actions ENABLE ROW LEVEL SECURITY;

--
-- Name: external_actions external_actions_all; Type: POLICY; Schema: public; Owner: -
--

-- Org-scoped, like access_change_log (NOT team-scoped like artifacts). The USING
-- (SELECT) predicate gates on org MEMBERSHIP, not admin role: like artifacts and
-- access_change_log, the DB is the cross-ORG boundary, while the within-org
-- role check (FeatureGovernance + team-admin/org-admin) is enforced at the HTTP
-- handler. A direct-DB org member could therefore read the audit rows — the
-- repo's deliberate posture (RLS is the tenant boundary, not the in-app role
-- boundary), consistent across these audit/artifact tables. The WITH CHECK gates
-- the app-pool inserts (manual bot runs under synthetic claims + the server
-- approval/board handlers under the approver's claims) by org; the admin-pool
-- inserts (event-triggered bot runs + the Jira mirror — no JWT claims) bypass
-- this policy. ListByTeam filters team_id in the WHERE clause under this org gate;
-- ListByOrgSystem reads admin-pool, org-wide.
CREATE POLICY external_actions_all ON public.external_actions USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: artifacts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.artifacts ENABLE ROW LEVEL SECURITY;

-- artifacts mirror the runs team-visibility branch: team-scoped read via
-- team_id, the same write pool/scope runs use. artifacts carry no
-- private/org visibility (no visibility / creator_user_id columns), so the
-- policies are the team predicates from runs, swapped onto this table —
-- user_in_team for SELECT, user_can_write_team for INSERT/UPDATE/DELETE.

--
-- Name: artifacts artifacts_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY artifacts_delete ON public.artifacts FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)));


--
-- Name: artifacts artifacts_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY artifacts_insert ON public.artifacts FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)));


--
-- Name: artifacts artifacts_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY artifacts_select ON public.artifacts FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_in_team(team_id)));


--
-- Name: artifacts artifacts_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY artifacts_update ON public.artifacts FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)));


--
-- Name: run_memory; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.run_memory ENABLE ROW LEVEL SECURITY;

--
-- Name: run_memory run_memory_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY run_memory_all ON public.run_memory USING ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = run_memory.run_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = run_memory.run_id))));


--
-- Name: run_messages; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.run_messages ENABLE ROW LEVEL SECURITY;

--
-- Name: run_messages run_messages_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY run_messages_all ON public.run_messages USING ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = run_messages.run_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = run_messages.run_id))));


--
-- Name: run_worktrees; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.run_worktrees ENABLE ROW LEVEL SECURITY;

--
-- Name: run_worktrees run_worktrees_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY run_worktrees_all ON public.run_worktrees USING ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = run_worktrees.run_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = run_worktrees.run_id))));


--
-- Name: runs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.runs ENABLE ROW LEVEL SECURITY;

--
-- Name: runs runs_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY runs_delete ON public.runs FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_can_write_team(team_id)))));


--
-- Name: runs runs_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY runs_insert ON public.runs FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND ((visibility <> 'team'::text) OR tf.user_can_write_team(team_id)) AND ((visibility <> 'org'::text) OR tf.user_is_org_admin(org_id))));


--
-- Name: runs runs_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY runs_select ON public.runs FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_in_team(team_id)) OR (visibility = 'org'::text))));


--
-- Name: runs runs_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY runs_update ON public.runs FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_can_write_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id))))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_can_write_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id)))));


--
-- Name: sessions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.sessions ENABLE ROW LEVEL SECURITY;

--
-- Name: sessions sessions_modify; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY sessions_modify ON public.sessions USING ((user_id = tf.current_user_id())) WITH CHECK ((user_id = tf.current_user_id()));


--
-- Name: sessions sessions_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY sessions_select ON public.sessions FOR SELECT USING ((user_id = tf.current_user_id()));


--
-- Name: swipe_events; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.swipe_events ENABLE ROW LEVEL SECURITY;

--
-- Name: swipe_events swipe_events_modify; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY swipe_events_modify ON public.swipe_events USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()))) WITH CHECK (((org_id = tf.current_org_id()) AND (creator_user_id = tf.current_user_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: swipe_events swipe_events_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY swipe_events_select ON public.swipe_events FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id())));


--
-- Name: system_prompt_versions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.system_prompt_versions ENABLE ROW LEVEL SECURITY;

--
-- Name: system_prompt_versions system_prompt_versions_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY system_prompt_versions_select ON public.system_prompt_versions FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: task_events; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.task_events ENABLE ROW LEVEL SECURITY;

--
-- Name: task_events task_events_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY task_events_all ON public.task_events USING ((EXISTS ( SELECT 1
   FROM public.tasks t
  WHERE (t.id = task_events.task_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.tasks t
  WHERE (t.id = task_events.task_id))));


--
-- Name: task_teams; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.task_teams ENABLE ROW LEVEL SECURITY;

--
-- Name: task_teams task_teams_select; Type: POLICY; Schema: public; Owner: -
--

-- A user may read the visibility-set rows for any team they belong to.
-- Writes happen only on the system/admin path (router-created), which
-- bypasses RLS — no INSERT/UPDATE/DELETE policy is intentional.
CREATE POLICY task_teams_select ON public.task_teams FOR SELECT USING (tf.user_in_team(team_id));


--
-- Name: tasks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.tasks ENABLE ROW LEVEL SECURITY;

--
-- Name: tasks tasks_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tasks_delete ON public.tasks FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (((claimed_by_agent_id IS NULL) AND (claimed_by_user_id IS NULL) AND (EXISTS ( SELECT 1 FROM public.task_teams tt WHERE ((tt.task_id = tasks.id) AND tf.user_can_write_team(tt.team_id))))) OR (team_id IS NOT NULL AND tf.user_can_write_team(team_id)))))));


--
-- Name: tasks tasks_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tasks_insert ON public.tasks FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND ((visibility <> 'team'::text) OR (team_id IS NOT NULL AND tf.user_can_write_team(team_id))) AND ((visibility <> 'org'::text) OR tf.user_is_org_admin(org_id))));


--
-- Name: tasks tasks_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tasks_select ON public.tasks FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (((claimed_by_agent_id IS NULL) AND (claimed_by_user_id IS NULL) AND (EXISTS ( SELECT 1 FROM public.task_teams tt WHERE ((tt.task_id = tasks.id) AND tf.user_in_team(tt.team_id))))) OR (team_id IS NOT NULL AND tf.user_in_team(team_id)))) OR (visibility = 'org'::text))));


--
-- Name: tasks tasks_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tasks_update ON public.tasks FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (((claimed_by_agent_id IS NULL) AND (claimed_by_user_id IS NULL) AND (EXISTS ( SELECT 1 FROM public.task_teams tt WHERE ((tt.task_id = tasks.id) AND tf.user_can_write_team(tt.team_id))))) OR (team_id IS NOT NULL AND tf.user_can_write_team(team_id)))) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id))))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (((claimed_by_agent_id IS NULL) AND (claimed_by_user_id IS NULL) AND (EXISTS ( SELECT 1 FROM public.task_teams tt WHERE ((tt.task_id = tasks.id) AND tf.user_can_write_team(tt.team_id))))) OR (team_id IS NOT NULL AND tf.user_can_write_team(team_id)))) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id)))));


--
-- Name: team_agents; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.team_agents ENABLE ROW LEVEL SECURITY;

--
-- Name: team_agents team_agents_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_agents_delete ON public.team_agents FOR DELETE USING ((tf.team_in_current_org(team_id) AND tf.user_can_write_team(team_id)));


--
-- Name: team_agents team_agents_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_agents_insert ON public.team_agents FOR INSERT WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_can_write_team(team_id) AND (EXISTS ( SELECT 1
   FROM public.agents a
  WHERE ((a.id = team_agents.agent_id) AND (a.org_id = tf.current_org_id()))))));


--
-- Name: team_agents team_agents_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_agents_select ON public.team_agents FOR SELECT TO tf_app USING ((tf.team_in_current_org(team_id) AND tf.user_in_team(team_id)));


--
-- Name: team_agents team_agents_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_agents_update ON public.team_agents FOR UPDATE USING ((tf.team_in_current_org(team_id) AND tf.user_can_write_team(team_id))) WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_can_write_team(team_id) AND (EXISTS ( SELECT 1
   FROM public.agents a
  WHERE ((a.id = team_agents.agent_id) AND (a.org_id = tf.current_org_id()))))));


--
-- Name: team_settings; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.team_settings ENABLE ROW LEVEL SECURITY;

--
-- Name: team_settings team_settings_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_settings_delete ON public.team_settings FOR DELETE USING ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: team_settings team_settings_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_settings_insert ON public.team_settings FOR INSERT WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: team_settings team_settings_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_settings_select ON public.team_settings FOR SELECT USING ((tf.team_in_current_org(team_id) AND (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.team_id = team_settings.team_id) AND (m.user_id = tf.current_user_id()))))));


--
-- Name: team_settings team_settings_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_settings_update ON public.team_settings FOR UPDATE USING ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id))) WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id)));


--
-- Name: teams; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.teams ENABLE ROW LEVEL SECURITY;

--
-- Name: teams teams_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY teams_delete ON public.teams FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));


--
-- Name: teams teams_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY teams_insert ON public.teams FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));


--
-- Name: teams teams_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY teams_select ON public.teams FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: teams teams_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY teams_update ON public.teams FOR UPDATE USING (((org_id = tf.current_org_id()) AND (tf.user_is_team_admin(id) OR tf.user_is_org_admin(org_id)))) WITH CHECK (((org_id = tf.current_org_id()) AND (tf.user_is_team_admin(id) OR tf.user_is_org_admin(org_id))));


--
-- Name: user_github_identities; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.user_github_identities ENABLE ROW LEVEL SECURITY;

--
-- Name: user_github_identities user_github_identities_modify; Type: POLICY; Schema: public; Owner: -
--

-- A user can read/write only their own identity rows. No org_id leg: a
-- pre-org multi-mode signup (active_org_id not yet set) must still be able to
-- bind a PAT-derived / login-claim identity, mirroring users_modify. Any
-- future team-roster read goes through an explicit membership join, never a
-- blanket org read of this table.
CREATE POLICY user_github_identities_modify ON public.user_github_identities USING ((user_id = tf.current_user_id())) WITH CHECK ((user_id = tf.current_user_id()));

--
-- Name: user_github_identities user_github_identities_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY user_github_identities_select ON public.user_github_identities FOR SELECT USING ((user_id = tf.current_user_id()));


--
-- Name: user_jira_identities; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.user_jira_identities ENABLE ROW LEVEL SECURITY;

--
-- Name: user_jira_identities user_jira_identities_modify; Type: POLICY; Schema: public; Owner: -
--

-- Self-only read/write, mirroring user_github_identities_modify. No org_id
-- leg: a pre-org multi-mode signup must still be able to bind a PAT-derived
-- identity. Any future team-roster read goes through an explicit membership
-- join, never a blanket org read of this table.
CREATE POLICY user_jira_identities_modify ON public.user_jira_identities USING ((user_id = tf.current_user_id())) WITH CHECK ((user_id = tf.current_user_id()));

--
-- Name: user_jira_identities user_jira_identities_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY user_jira_identities_select ON public.user_jira_identities FOR SELECT USING ((user_id = tf.current_user_id()));


--
-- Name: user_settings; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.user_settings ENABLE ROW LEVEL SECURITY;

--
-- Name: user_settings user_settings_modify; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY user_settings_modify ON public.user_settings USING ((user_id = tf.current_user_id())) WITH CHECK ((user_id = tf.current_user_id()));


--
-- Name: user_settings user_settings_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY user_settings_select ON public.user_settings FOR SELECT USING ((user_id = tf.current_user_id()));


--
-- Name: users; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.users ENABLE ROW LEVEL SECURITY;

--
-- Name: users users_modify; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY users_modify ON public.users USING ((id = tf.current_user_id())) WITH CHECK ((id = tf.current_user_id()));


--
-- Name: users users_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY users_select ON public.users FOR SELECT USING (((id = tf.current_user_id()) OR (EXISTS ( SELECT 1
   FROM public.org_memberships om
  WHERE ((om.user_id = users.id) AND (om.org_id = tf.current_org_id()) AND tf.user_has_org_access(om.org_id))))));


--
-- Name: SCHEMA public; Type: ACL; Schema: -; Owner: -
--

GRANT USAGE ON SCHEMA public TO postgres;
GRANT USAGE ON SCHEMA public TO anon;
GRANT USAGE ON SCHEMA public TO authenticated;
GRANT USAGE ON SCHEMA public TO service_role;
GRANT USAGE ON SCHEMA public TO tf_app;


--
-- Name: SCHEMA tf; Type: ACL; Schema: -; Owner: -
--

GRANT USAGE ON SCHEMA tf TO tf_app;


--
-- Name: FUNCTION update_project_knowledge(p_id uuid, p_expected_version integer, p_content text, p_updated_by_run uuid); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.update_project_knowledge(p_id uuid, p_expected_version integer, p_content text, p_updated_by_run uuid) FROM PUBLIC;
-- supabase_admin's ALTER DEFAULT PRIVILEGES auto-grants public-schema
-- functions to anon/authenticated/service_role at CREATE time. Strip them —
-- only tf_app should call this OCC helper.
REVOKE ALL ON FUNCTION public.update_project_knowledge(p_id uuid, p_expected_version integer, p_content text, p_updated_by_run uuid) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.update_project_knowledge(p_id uuid, p_expected_version integer, p_content text, p_updated_by_run uuid) TO postgres;
GRANT ALL ON FUNCTION public.update_project_knowledge(p_id uuid, p_expected_version integer, p_content text, p_updated_by_run uuid) TO tf_app;


--
-- Name: FUNCTION vault_delete_org_secret(p_org_id uuid, p_key text); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.vault_delete_org_secret(p_org_id uuid, p_key text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_delete_org_secret(p_org_id uuid, p_key text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_delete_org_secret(p_org_id uuid, p_key text) TO postgres;
GRANT ALL ON FUNCTION public.vault_delete_org_secret(p_org_id uuid, p_key text) TO tf_app;


--
-- Name: FUNCTION vault_get_org_secret(p_org_id uuid, p_key text); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.vault_get_org_secret(p_org_id uuid, p_key text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_get_org_secret(p_org_id uuid, p_key text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_get_org_secret(p_org_id uuid, p_key text) TO postgres;
GRANT ALL ON FUNCTION public.vault_get_org_secret(p_org_id uuid, p_key text) TO tf_app;


--
-- Name: FUNCTION vault_get_org_secret_system(p_org_id uuid, p_key text); Type: ACL; Schema: public; Owner: -
--

-- System/admin pool ONLY. Deliberately NOT granted to tf_app: the app
-- pool must stay on the claims-checked vault_get_org_secret. The admin
-- pool connects as supabase_admin (superuser, owns this function) and
-- executes it regardless of grant; postgres is granted to mirror the
-- sibling vault_* ACLs. tf_app lacking EXECUTE here is the load-bearing
-- guardrail — pinned by the pgtest "tf_app denied" assertion.
REVOKE ALL ON FUNCTION public.vault_get_org_secret_system(p_org_id uuid, p_key text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_get_org_secret_system(p_org_id uuid, p_key text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_get_org_secret_system(p_org_id uuid, p_key text) TO postgres;


--
-- Name: FUNCTION vault_put_org_secret(p_org_id uuid, p_key text, p_secret text, p_description text); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.vault_put_org_secret(p_org_id uuid, p_key text, p_secret text, p_description text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_put_org_secret(p_org_id uuid, p_key text, p_secret text, p_description text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_put_org_secret(p_org_id uuid, p_key text, p_secret text, p_description text) TO postgres;
GRANT ALL ON FUNCTION public.vault_put_org_secret(p_org_id uuid, p_key text, p_secret text, p_description text) TO tf_app;


--
-- Name: FUNCTION current_org_id(); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.current_org_id() FROM PUBLIC;
GRANT ALL ON FUNCTION tf.current_org_id() TO tf_app;


--
-- Name: FUNCTION current_user_id(); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.current_user_id() FROM PUBLIC;
GRANT ALL ON FUNCTION tf.current_user_id() TO tf_app;


--
-- Name: FUNCTION team_in_current_org(target_team uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.team_in_current_org(target_team uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.team_in_current_org(target_team uuid) TO tf_app;


--
-- Name: FUNCTION user_has_org_access(target_org uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.user_has_org_access(target_org uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.user_has_org_access(target_org uuid) TO tf_app;


--
-- Name: FUNCTION org_tracked_repos(p_org_id uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.org_tracked_repos(p_org_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.org_tracked_repos(p_org_id uuid) TO tf_app;


--
-- Name: FUNCTION user_can_write_team(target_team uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.user_can_write_team(target_team uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.user_can_write_team(target_team uuid) TO tf_app;


--
-- Name: FUNCTION user_in_team(target_team uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.user_in_team(target_team uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.user_in_team(target_team uuid) TO tf_app;


--
-- Name: FUNCTION user_is_org_admin(target_org uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.user_is_org_admin(target_org uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.user_is_org_admin(target_org uuid) TO tf_app;


--
-- Name: FUNCTION user_is_org_admin_via_team(target_team uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.user_is_org_admin_via_team(target_team uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.user_is_org_admin_via_team(target_team uuid) TO tf_app;


--
-- Name: FUNCTION user_is_team_admin(target_team uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.user_is_team_admin(target_team uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.user_is_team_admin(target_team uuid) TO tf_app;


--
-- Name: FUNCTION user_owns_org(target_org uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.user_owns_org(target_org uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.user_owns_org(target_org uuid) TO tf_app;


--
-- Name: TABLE agents; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.agents TO postgres;
GRANT ALL ON TABLE public.agents TO anon;
GRANT ALL ON TABLE public.agents TO authenticated;
GRANT ALL ON TABLE public.agents TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.agents TO tf_app;


--
-- Name: TABLE curator_messages; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.curator_messages TO postgres;
GRANT ALL ON TABLE public.curator_messages TO anon;
GRANT ALL ON TABLE public.curator_messages TO authenticated;
GRANT ALL ON TABLE public.curator_messages TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.curator_messages TO tf_app;


--
-- Name: SEQUENCE curator_messages_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON SEQUENCE public.curator_messages_id_seq TO postgres;
GRANT ALL ON SEQUENCE public.curator_messages_id_seq TO anon;
GRANT ALL ON SEQUENCE public.curator_messages_id_seq TO authenticated;
GRANT ALL ON SEQUENCE public.curator_messages_id_seq TO service_role;
GRANT SELECT,USAGE ON SEQUENCE public.curator_messages_id_seq TO tf_app;


--
-- Name: TABLE curator_pending_context; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.curator_pending_context TO postgres;
GRANT ALL ON TABLE public.curator_pending_context TO anon;
GRANT ALL ON TABLE public.curator_pending_context TO authenticated;
GRANT ALL ON TABLE public.curator_pending_context TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.curator_pending_context TO tf_app;


--
-- Name: SEQUENCE curator_pending_context_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON SEQUENCE public.curator_pending_context_id_seq TO postgres;
GRANT ALL ON SEQUENCE public.curator_pending_context_id_seq TO anon;
GRANT ALL ON SEQUENCE public.curator_pending_context_id_seq TO authenticated;
GRANT ALL ON SEQUENCE public.curator_pending_context_id_seq TO service_role;
GRANT SELECT,USAGE ON SEQUENCE public.curator_pending_context_id_seq TO tf_app;


--
-- Name: TABLE curator_requests; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.curator_requests TO postgres;
GRANT ALL ON TABLE public.curator_requests TO anon;
GRANT ALL ON TABLE public.curator_requests TO authenticated;
GRANT ALL ON TABLE public.curator_requests TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.curator_requests TO tf_app;


--
-- Name: TABLE entities; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.entities TO postgres;
GRANT ALL ON TABLE public.entities TO anon;
GRANT ALL ON TABLE public.entities TO authenticated;
GRANT ALL ON TABLE public.entities TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.entities TO tf_app;


--
-- Name: TABLE entity_links; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.entity_links TO postgres;
GRANT ALL ON TABLE public.entity_links TO anon;
GRANT ALL ON TABLE public.entity_links TO authenticated;
GRANT ALL ON TABLE public.entity_links TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.entity_links TO tf_app;


--
-- Name: TABLE event_handlers; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.event_handlers TO postgres;
GRANT ALL ON TABLE public.event_handlers TO anon;
GRANT ALL ON TABLE public.event_handlers TO authenticated;
GRANT ALL ON TABLE public.event_handlers TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.event_handlers TO tf_app;


--
-- Name: TABLE events; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.events TO postgres;
GRANT ALL ON TABLE public.events TO anon;
GRANT ALL ON TABLE public.events TO authenticated;
GRANT ALL ON TABLE public.events TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.events TO tf_app;


--
-- Name: TABLE events_catalog; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.events_catalog TO postgres;
GRANT ALL ON TABLE public.events_catalog TO anon;
GRANT ALL ON TABLE public.events_catalog TO authenticated;
GRANT ALL ON TABLE public.events_catalog TO service_role;
GRANT SELECT ON TABLE public.events_catalog TO tf_app;


--
-- Name: TABLE jira_project_status_rules; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.jira_project_status_rules TO postgres;
GRANT ALL ON TABLE public.jira_project_status_rules TO anon;
GRANT ALL ON TABLE public.jira_project_status_rules TO authenticated;
GRANT ALL ON TABLE public.jira_project_status_rules TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.jira_project_status_rules TO tf_app;


--
-- Name: TABLE team_github_groups; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.team_github_groups TO postgres;
GRANT ALL ON TABLE public.team_github_groups TO anon;
GRANT ALL ON TABLE public.team_github_groups TO authenticated;
GRANT ALL ON TABLE public.team_github_groups TO service_role;
GRANT SELECT,INSERT,DELETE ON TABLE public.team_github_groups TO tf_app;


--
-- Name: TABLE team_github_repos; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.team_github_repos TO postgres;
GRANT ALL ON TABLE public.team_github_repos TO anon;
GRANT ALL ON TABLE public.team_github_repos TO authenticated;
GRANT ALL ON TABLE public.team_github_repos TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.team_github_repos TO tf_app;


--
-- Name: TABLE memberships; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.memberships TO postgres;
GRANT ALL ON TABLE public.memberships TO anon;
GRANT ALL ON TABLE public.memberships TO authenticated;
GRANT ALL ON TABLE public.memberships TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.memberships TO tf_app;


--
-- Name: TABLE org_memberships; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.org_memberships TO postgres;
GRANT ALL ON TABLE public.org_memberships TO anon;
GRANT ALL ON TABLE public.org_memberships TO authenticated;
GRANT ALL ON TABLE public.org_memberships TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.org_memberships TO tf_app;


--
-- Name: TABLE org_settings; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.org_settings TO postgres;
GRANT ALL ON TABLE public.org_settings TO anon;
GRANT ALL ON TABLE public.org_settings TO authenticated;
GRANT ALL ON TABLE public.org_settings TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.org_settings TO tf_app;


--
-- Name: TABLE orgs; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.orgs TO postgres;
GRANT ALL ON TABLE public.orgs TO anon;
GRANT ALL ON TABLE public.orgs TO authenticated;
GRANT ALL ON TABLE public.orgs TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.orgs TO tf_app;


--
-- Name: TABLE pending_firings; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.pending_firings TO postgres;
GRANT ALL ON TABLE public.pending_firings TO anon;
GRANT ALL ON TABLE public.pending_firings TO authenticated;
GRANT ALL ON TABLE public.pending_firings TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.pending_firings TO tf_app;


--
-- Name: SEQUENCE pending_firings_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON SEQUENCE public.pending_firings_id_seq TO postgres;
GRANT ALL ON SEQUENCE public.pending_firings_id_seq TO anon;
GRANT ALL ON SEQUENCE public.pending_firings_id_seq TO authenticated;
GRANT ALL ON SEQUENCE public.pending_firings_id_seq TO service_role;
GRANT SELECT,USAGE ON SEQUENCE public.pending_firings_id_seq TO tf_app;


--
-- Name: TABLE poller_state; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.poller_state TO postgres;
GRANT ALL ON TABLE public.poller_state TO anon;
GRANT ALL ON TABLE public.poller_state TO authenticated;
GRANT ALL ON TABLE public.poller_state TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.poller_state TO tf_app;


--
-- Name: TABLE project_knowledge; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.project_knowledge TO postgres;
GRANT ALL ON TABLE public.project_knowledge TO anon;
GRANT ALL ON TABLE public.project_knowledge TO authenticated;
GRANT ALL ON TABLE public.project_knowledge TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.project_knowledge TO tf_app;


--
-- Name: TABLE projects; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.projects TO postgres;
GRANT ALL ON TABLE public.projects TO anon;
GRANT ALL ON TABLE public.projects TO authenticated;
GRANT ALL ON TABLE public.projects TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.projects TO tf_app;


--
-- Name: TABLE prompts; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.prompts TO postgres;
GRANT ALL ON TABLE public.prompts TO anon;
GRANT ALL ON TABLE public.prompts TO authenticated;
GRANT ALL ON TABLE public.prompts TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.prompts TO tf_app;


--
-- Name: TABLE repo_profiles; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.repo_profiles TO postgres;
GRANT ALL ON TABLE public.repo_profiles TO anon;
GRANT ALL ON TABLE public.repo_profiles TO authenticated;
GRANT ALL ON TABLE public.repo_profiles TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.repo_profiles TO tf_app;


--
-- Name: TABLE system_llm_runs; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.system_llm_runs TO postgres;
GRANT ALL ON TABLE public.system_llm_runs TO anon;
GRANT ALL ON TABLE public.system_llm_runs TO authenticated;
GRANT ALL ON TABLE public.system_llm_runs TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.system_llm_runs TO tf_app;


--
-- Name: TABLE access_change_log; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.access_change_log TO postgres;
GRANT ALL ON TABLE public.access_change_log TO anon;
GRANT ALL ON TABLE public.access_change_log TO authenticated;
GRANT ALL ON TABLE public.access_change_log TO service_role;
-- Append-only: tf_app (the app pool) may read + insert but NOT delete/update.
-- An audit row must be immutable once written, so the app-pool role that serves
-- every in-org request is deliberately denied UPDATE/DELETE. The admin pool
-- (postgres) keeps GRANT ALL for the invite-accept insert + orgs ON DELETE
-- CASCADE; deliberate retention/redaction tooling is an EE concern (TFAC-449 D2).
GRANT SELECT,INSERT ON TABLE public.access_change_log TO tf_app;


--
-- Name: TABLE external_actions; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.external_actions TO postgres;
GRANT ALL ON TABLE public.external_actions TO anon;
GRANT ALL ON TABLE public.external_actions TO authenticated;
GRANT ALL ON TABLE public.external_actions TO service_role;
-- Append-only, exactly like access_change_log: tf_app (the app pool) may read +
-- insert but NOT delete/update — an audit row is immutable once written, so the
-- role serving every in-org request is denied UPDATE/DELETE. The admin pool
-- (postgres) keeps GRANT ALL for the system/event inserts + orgs ON DELETE
-- CASCADE; retention/redaction tooling is a later compliance concern (TFAC-483).
GRANT SELECT,INSERT ON TABLE public.external_actions TO tf_app;


--
-- Name: TABLE artifacts; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.artifacts TO postgres;
GRANT ALL ON TABLE public.artifacts TO anon;
GRANT ALL ON TABLE public.artifacts TO authenticated;
GRANT ALL ON TABLE public.artifacts TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.artifacts TO tf_app;


--
-- Name: TABLE run_memory; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.run_memory TO postgres;
GRANT ALL ON TABLE public.run_memory TO anon;
GRANT ALL ON TABLE public.run_memory TO authenticated;
GRANT ALL ON TABLE public.run_memory TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.run_memory TO tf_app;


--
-- Name: TABLE run_messages; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.run_messages TO postgres;
GRANT ALL ON TABLE public.run_messages TO anon;
GRANT ALL ON TABLE public.run_messages TO authenticated;
GRANT ALL ON TABLE public.run_messages TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.run_messages TO tf_app;


--
-- Name: SEQUENCE run_messages_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON SEQUENCE public.run_messages_id_seq TO postgres;
GRANT ALL ON SEQUENCE public.run_messages_id_seq TO anon;
GRANT ALL ON SEQUENCE public.run_messages_id_seq TO authenticated;
GRANT ALL ON SEQUENCE public.run_messages_id_seq TO service_role;
GRANT SELECT,USAGE ON SEQUENCE public.run_messages_id_seq TO tf_app;


--
-- Name: TABLE run_worktrees; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.run_worktrees TO postgres;
GRANT ALL ON TABLE public.run_worktrees TO anon;
GRANT ALL ON TABLE public.run_worktrees TO authenticated;
GRANT ALL ON TABLE public.run_worktrees TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.run_worktrees TO tf_app;


--
-- Name: TABLE runs; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.runs TO postgres;
GRANT ALL ON TABLE public.runs TO anon;
GRANT ALL ON TABLE public.runs TO authenticated;
GRANT ALL ON TABLE public.runs TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.runs TO tf_app;


--
-- Name: TABLE llm_spend; Type: ACL; Schema: public; Owner: -
--

-- Read-only view (TFAC-472): tf_app gets SELECT only. security_invoker='true'
-- means the base tables' RLS still applies under tf_app, so no view-level policy
-- is needed (and would be wrong — see the CREATE VIEW comment).
GRANT ALL ON TABLE public.llm_spend TO postgres;
GRANT ALL ON TABLE public.llm_spend TO anon;
GRANT ALL ON TABLE public.llm_spend TO authenticated;
GRANT ALL ON TABLE public.llm_spend TO service_role;
GRANT SELECT ON TABLE public.llm_spend TO tf_app;


--
-- Name: TABLE sessions; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.sessions TO postgres;
GRANT ALL ON TABLE public.sessions TO anon;
GRANT ALL ON TABLE public.sessions TO authenticated;
GRANT ALL ON TABLE public.sessions TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.sessions TO tf_app;


--
-- Name: TABLE swipe_events; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.swipe_events TO postgres;
GRANT ALL ON TABLE public.swipe_events TO anon;
GRANT ALL ON TABLE public.swipe_events TO authenticated;
GRANT ALL ON TABLE public.swipe_events TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.swipe_events TO tf_app;


--
-- Name: SEQUENCE swipe_events_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON SEQUENCE public.swipe_events_id_seq TO postgres;
GRANT ALL ON SEQUENCE public.swipe_events_id_seq TO anon;
GRANT ALL ON SEQUENCE public.swipe_events_id_seq TO authenticated;
GRANT ALL ON SEQUENCE public.swipe_events_id_seq TO service_role;
GRANT SELECT,USAGE ON SEQUENCE public.swipe_events_id_seq TO tf_app;


--
-- Name: TABLE system_prompt_versions; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.system_prompt_versions TO postgres;
GRANT ALL ON TABLE public.system_prompt_versions TO anon;
GRANT ALL ON TABLE public.system_prompt_versions TO authenticated;
GRANT ALL ON TABLE public.system_prompt_versions TO service_role;
GRANT SELECT ON TABLE public.system_prompt_versions TO tf_app;


--
-- Name: TABLE task_events; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.task_events TO postgres;
GRANT ALL ON TABLE public.task_events TO anon;
GRANT ALL ON TABLE public.task_events TO authenticated;
GRANT ALL ON TABLE public.task_events TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.task_events TO tf_app;


--
-- Name: TABLE tasks; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.tasks TO postgres;
GRANT ALL ON TABLE public.tasks TO anon;
GRANT ALL ON TABLE public.tasks TO authenticated;
GRANT ALL ON TABLE public.tasks TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.tasks TO tf_app;


--
-- Name: TABLE task_teams; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.task_teams TO postgres;
GRANT ALL ON TABLE public.task_teams TO anon;
GRANT ALL ON TABLE public.task_teams TO authenticated;
GRANT ALL ON TABLE public.task_teams TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.task_teams TO tf_app;


--
-- Name: TABLE team_agents; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.team_agents TO postgres;
GRANT ALL ON TABLE public.team_agents TO anon;
GRANT ALL ON TABLE public.team_agents TO authenticated;
GRANT ALL ON TABLE public.team_agents TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.team_agents TO tf_app;


--
-- Name: TABLE team_settings; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.team_settings TO postgres;
GRANT ALL ON TABLE public.team_settings TO anon;
GRANT ALL ON TABLE public.team_settings TO authenticated;
GRANT ALL ON TABLE public.team_settings TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.team_settings TO tf_app;


--
-- Name: TABLE teams; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.teams TO postgres;
GRANT ALL ON TABLE public.teams TO anon;
GRANT ALL ON TABLE public.teams TO authenticated;
GRANT ALL ON TABLE public.teams TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.teams TO tf_app;


--
-- Name: TABLE user_github_identities; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.user_github_identities TO postgres;
GRANT ALL ON TABLE public.user_github_identities TO anon;
GRANT ALL ON TABLE public.user_github_identities TO authenticated;
GRANT ALL ON TABLE public.user_github_identities TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.user_github_identities TO tf_app;


--
-- Name: TABLE user_jira_identities; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.user_jira_identities TO postgres;
GRANT ALL ON TABLE public.user_jira_identities TO anon;
GRANT ALL ON TABLE public.user_jira_identities TO authenticated;
GRANT ALL ON TABLE public.user_jira_identities TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.user_jira_identities TO tf_app;


--
-- Name: TABLE user_settings; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.user_settings TO postgres;
GRANT ALL ON TABLE public.user_settings TO anon;
GRANT ALL ON TABLE public.user_settings TO authenticated;
GRANT ALL ON TABLE public.user_settings TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.user_settings TO tf_app;


--
-- Name: TABLE users; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.users TO postgres;
GRANT ALL ON TABLE public.users TO anon;
GRANT ALL ON TABLE public.users TO authenticated;
GRANT ALL ON TABLE public.users TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.users TO tf_app;


--
-- Name: DEFAULT PRIVILEGES FOR SEQUENCES; Type: DEFAULT ACL; Schema: public; Owner: -
--

ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON SEQUENCES  TO postgres;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON SEQUENCES  TO anon;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON SEQUENCES  TO authenticated;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON SEQUENCES  TO service_role;


--
-- Name: DEFAULT PRIVILEGES FOR SEQUENCES; Type: DEFAULT ACL; Schema: public; Owner: -
--

ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON SEQUENCES  TO postgres;
ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON SEQUENCES  TO anon;
ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON SEQUENCES  TO authenticated;
ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON SEQUENCES  TO service_role;


--
-- Name: DEFAULT PRIVILEGES FOR FUNCTIONS; Type: DEFAULT ACL; Schema: public; Owner: -
--

ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON FUNCTIONS  TO postgres;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON FUNCTIONS  TO anon;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON FUNCTIONS  TO authenticated;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON FUNCTIONS  TO service_role;


--
-- Name: DEFAULT PRIVILEGES FOR FUNCTIONS; Type: DEFAULT ACL; Schema: public; Owner: -
--

ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON FUNCTIONS  TO postgres;
ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON FUNCTIONS  TO anon;
ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON FUNCTIONS  TO authenticated;
ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON FUNCTIONS  TO service_role;


--
-- Name: DEFAULT PRIVILEGES FOR TABLES; Type: DEFAULT ACL; Schema: public; Owner: -
--

ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON TABLES  TO postgres;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON TABLES  TO anon;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON TABLES  TO authenticated;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public GRANT ALL ON TABLES  TO service_role;


--
-- Name: DEFAULT PRIVILEGES FOR TABLES; Type: DEFAULT ACL; Schema: public; Owner: -
--

ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON TABLES  TO postgres;
ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON TABLES  TO anon;
ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON TABLES  TO authenticated;
ALTER DEFAULT PRIVILEGES FOR ROLE supabase_admin IN SCHEMA public GRANT ALL ON TABLES  TO service_role;


--
-- PostgreSQL database dump complete
--


-- events_catalog is seeded dynamically at runtime by db.SeedEventTypes
-- (called from db.Migrate after migrations apply), reconciled from
-- domain.AllEventTypes() — not by a static INSERT block here. See
-- internal/db/event_types.go.

--
-- Blueprints
--
-- A blueprint is the triggerable, team-scoped composition: an ordered list
-- of prompt steps (blueprint_steps), length >= 1. Everything an event
-- handler fires is a blueprint; a single prompt is just a 1-step blueprint.
-- Modeled on prompts (same team-scoping, same system_slug idempotency key,
-- same composite uniques so the trigger + step FKs can be same-team-guarded).
--
-- blueprint_steps is the ordered step list; blueprint_runs is the in-flight
-- instance for a multi-step blueprint (sharing one worktree, each step's
-- terminal runs.outcome driving advancement). Per-step runtime state stays
-- on runs (linked via runs.blueprint_run_id);
-- blueprint-wide abort/complete state lives on blueprint_runs.
--
-- Multi-tenant pattern matches the rest of the baseline: composite FKs
-- against (id, org_id) on every parent ref, RLS gated on
-- tf.current_org_id() with EXISTS guards against same-id cross-tenant
-- access (blueprints.id is text and can collide across orgs).
--

CREATE TABLE public.blueprints (
    id text DEFAULT (gen_random_uuid())::text NOT NULL,
    org_id uuid NOT NULL,
    creator_user_id uuid,
    team_id uuid NOT NULL,
    name text NOT NULL,
    source text DEFAULT 'user'::text NOT NULL,
    usage_count integer DEFAULT 0 NOT NULL,
    hidden boolean DEFAULT false NOT NULL,
    user_modified boolean DEFAULT false NOT NULL,
    system_slug text,
    -- deleted_at soft-deletes a blueprint (mirrors prompts). Deleting the
    -- prompt that solely constitutes a 1-step blueprint soft-deletes both;
    -- request-facing reads filter deleted_at IS NULL, ...System reads don't.
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    -- source is app-validated, not CHECK-constrained (source_check dropped in
    -- this baseline, both dialects). The system_has_no_creator pairing is
    -- the only source invariant the DB still enforces.
    CONSTRAINT blueprints_system_has_no_creator CHECK ((((source = 'system'::text) AND (creator_user_id IS NULL)) OR ((source <> 'system'::text) AND (creator_user_id IS NOT NULL))))
);

ALTER TABLE ONLY public.blueprints
    ADD CONSTRAINT blueprints_pkey PRIMARY KEY (org_id, id);
ALTER TABLE ONLY public.blueprints
    ADD CONSTRAINT blueprints_id_org_id_key UNIQUE (id, org_id);
ALTER TABLE ONLY public.blueprints
    ADD CONSTRAINT blueprints_id_team_id_key UNIQUE (id, team_id);
ALTER TABLE ONLY public.blueprints
    ADD CONSTRAINT blueprints_org_team_slug_key UNIQUE (org_id, team_id, system_slug);

ALTER TABLE ONLY public.blueprints
    ADD CONSTRAINT blueprints_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.blueprints
    ADD CONSTRAINT blueprints_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.blueprints
    ADD CONSTRAINT blueprints_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.blueprints FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();

ALTER TABLE public.blueprints ENABLE ROW LEVEL SECURITY;

CREATE POLICY blueprints_select ON public.blueprints FOR SELECT
  USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)
          AND ((creator_user_id = tf.current_user_id())
               OR (EXISTS (SELECT 1 FROM public.memberships m
                           WHERE ((m.user_id = tf.current_user_id()) AND (m.team_id = blueprints.team_id)))))));
CREATE POLICY blueprints_insert ON public.blueprints FOR INSERT
  WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND tf.user_can_write_team(team_id)));
CREATE POLICY blueprints_update ON public.blueprints FOR UPDATE
  USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)))
  WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)));
CREATE POLICY blueprints_delete ON public.blueprints FOR DELETE
  USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_can_write_team(team_id)));

GRANT ALL ON TABLE public.blueprints TO postgres;
GRANT ALL ON TABLE public.blueprints TO anon;
GRANT ALL ON TABLE public.blueprints TO authenticated;
GRANT ALL ON TABLE public.blueprints TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.blueprints TO tf_app;

-- Retargeted FKs deferred from the main section (blueprints didn't exist yet):
-- a trigger fires a blueprint its own team owns; a project's spec-authorship
-- blueprint resolves same-org.
ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_blueprint_id_org_id_fkey FOREIGN KEY (blueprint_id, org_id) REFERENCES public.blueprints(id, org_id) ON DELETE CASCADE;
ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_blueprint_id_team_id_fkey FOREIGN KEY (blueprint_id, team_id) REFERENCES public.blueprints(id, team_id) ON DELETE CASCADE;
ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_spec_authorship_blueprint_id_org_id_fkey FOREIGN KEY (spec_authorship_blueprint_id, org_id) REFERENCES public.blueprints(id, org_id) ON DELETE SET NULL;

CREATE TABLE public.blueprint_steps (
    org_id uuid NOT NULL,
    team_id uuid NOT NULL,
    blueprint_id text NOT NULL,
    step_index integer NOT NULL,
    step_prompt_id text NOT NULL,
    brief text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.blueprint_runs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    -- creator_user_id is nullable for trigger_type='event' runs
    -- (system-emitted by the router via the admin pool); manual
    -- runs carry the human delegator. The matching
    -- blueprint_runs_creator_matches_trigger_type CHECK below pairs the
    -- two so the seeder can't drift. Mirrors the runs table's
    -- runs_creator_matches_trigger_type pattern.
    creator_user_id uuid,
    blueprint_id text NOT NULL,
    task_id uuid NOT NULL,
    trigger_type text NOT NULL,
    trigger_id uuid,
    -- triggering_event_id is the event instance that auto-fired this blueprint
    -- run (NULL for manual). The blueprint_run is the firing unit, so the replay
    -- fence (blueprint_runs_event_trigger_fence below) lives here, relocated off
    -- runs. Step runs are not separately fenced (orchestrator-internal).
    triggering_event_id uuid,
    -- actor_agent_id is the bot that executes this blueprint run, resolved once
    -- at the delegation entry point and frozen here at mint (alongside
    -- creator_user_id / trigger_id — the "who/why" provenance axes). Every step
    -- run inherits it onto runs.actor_agent_id, so the execution attribution is
    -- stable across all steps and immune to a later task-claim change (a user
    -- takeover clears tasks.claimed_by_agent_id but does not rewrite who ran the
    -- bot's steps). Nullable: a run minted before the org agent bootstrapped, or
    -- whose agent row was later deleted (ON DELETE SET NULL), carries no actor.
    actor_agent_id uuid,
    status text DEFAULT 'running'::text NOT NULL,
    -- Durable sequencing for the queue-driven reactor: current_step_index is
    -- the 0-based step the blueprint is on, bumped as the reactor enqueues the
    -- next step, so a mid-flight blueprint resumes by re-enqueuing this step at
    -- boot rather than relying on a goroutine stack.
    current_step_index integer DEFAULT 0 NOT NULL,
    -- cancel_requested is the DB sequence-cancel signal: the claim skips queued
    -- steps of a cancel-requested blueprint and the reactor finalizes it
    -- 'cancelled' instead of advancing. The active-subprocess kill stays
    -- in-memory (s.cancels).
    cancel_requested boolean DEFAULT false NOT NULL,
    -- step_plan is the resolved step list frozen at mint: a JSON array of
    -- {step_index, prompt_id, prompt_name, prompt_body, source, allowed_tools,
    -- model, brief}, one entry per step. The dispatcher/reactor/resume sequence
    -- off this snapshot rather than re-reading blueprint_steps/prompts, so an
    -- in-flight run executes the plan it was minted with — edits to the
    -- blueprint (ReplaceSteps/SplitAt/MergeInto, a prompt-body edit, a future
    -- prompt-delete) are forward-only and can't mutate a running execution.
    -- Snapshots full prompt content, not just ids, so body edits can't leak in.
    -- Stored as text (JSON payload), mirroring the SQLite baseline.
    step_plan text NOT NULL,
    abort_reason text,
    aborted_at_step integer,
    worktree_path text NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT blueprint_runs_status_check CHECK ((status = ANY (ARRAY['running'::text, 'completed'::text, 'aborted'::text, 'failed'::text, 'cancelled'::text]))),
    CONSTRAINT blueprint_runs_creator_matches_trigger_type CHECK ((((trigger_type = 'manual'::text) AND (creator_user_id IS NOT NULL)) OR ((trigger_type = 'event'::text) AND (creator_user_id IS NULL))))
);

ALTER TABLE ONLY public.blueprint_steps
    ADD CONSTRAINT blueprint_steps_pkey PRIMARY KEY (org_id, blueprint_id, step_index);

-- Copy-only prompts: a prompt is a step in AT MOST ONE blueprint. step_prompt_id
-- holds globally-unique UUIDs; org_id is included to match the partition/RLS
-- convention of the other unique indexes. The handler pre-checks for a clean
-- 422; this constraint is the durable backstop (it also forbids a prompt at two
-- step_indexes within one blueprint, which is intended).
ALTER TABLE ONLY public.blueprint_steps
    ADD CONSTRAINT blueprint_steps_org_step_prompt_key UNIQUE (org_id, step_prompt_id);

ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_id_org_id_key UNIQUE (id, org_id);

CREATE INDEX idx_blueprint_steps_step_prompt ON public.blueprint_steps (step_prompt_id, org_id);
CREATE INDEX idx_blueprint_runs_task   ON public.blueprint_runs (task_id, org_id);
CREATE INDEX idx_blueprint_runs_status ON public.blueprint_runs (status) WHERE (status = 'running'::text);
CREATE INDEX idx_blueprint_runs_actor_agent ON public.blueprint_runs (actor_agent_id) WHERE (actor_agent_id IS NOT NULL);
CREATE INDEX idx_runs_blueprint        ON public.runs (blueprint_run_id, blueprint_step_index);
-- Claim index for the run queue: the dispatcher claims the globally-oldest
-- 'queued' run (FIFO by started_at, id) under FOR UPDATE SKIP LOCKED. Partial so
-- it only spans unclaimed work, mirroring idx_event_queue_pending.
CREATE INDEX idx_runs_queued ON public.runs (started_at, id) WHERE (status = 'queued'::text);
-- Replay fence (relocated from runs): one event firing one trigger materializes
-- at most one blueprint_run. Partial WHERE triggering_event_id IS NOT NULL so
-- manual blueprint runs (NULL) never participate.
CREATE UNIQUE INDEX blueprint_runs_event_trigger_fence ON public.blueprint_runs (triggering_event_id, trigger_id) WHERE (triggering_event_id IS NOT NULL);

ALTER TABLE ONLY public.blueprint_steps
    ADD CONSTRAINT blueprint_steps_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.blueprint_steps
    ADD CONSTRAINT blueprint_steps_blueprint_fkey FOREIGN KEY (blueprint_id, org_id) REFERENCES public.blueprints(id, org_id) ON DELETE CASCADE;
ALTER TABLE ONLY public.blueprint_steps
    ADD CONSTRAINT blueprint_steps_step_prompt_fkey  FOREIGN KEY (step_prompt_id,  org_id) REFERENCES public.prompts(id, org_id) ON DELETE RESTRICT;
-- Same-team guards: the blueprint resolves against blueprints(id, team_id)
-- and every step against prompts(id, team_id), so a blueprint can only step
-- through prompts its own team owns. The writer derives team_id from the
-- blueprint, making the blueprint FK a tautology and the step FK the real
-- cross-team refusal.
ALTER TABLE ONLY public.blueprint_steps
    ADD CONSTRAINT blueprint_steps_blueprint_team_fkey FOREIGN KEY (blueprint_id, team_id) REFERENCES public.blueprints(id, team_id) ON DELETE CASCADE;
ALTER TABLE ONLY public.blueprint_steps
    ADD CONSTRAINT blueprint_steps_step_prompt_team_fkey  FOREIGN KEY (step_prompt_id,  team_id) REFERENCES public.prompts(id, team_id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_org_id_fkey          FOREIGN KEY (org_id)          REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_blueprint_fkey       FOREIGN KEY (blueprint_id, org_id) REFERENCES public.blueprints(id, org_id);
ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_task_fkey            FOREIGN KEY (task_id, org_id)         REFERENCES public.tasks(id, org_id);
ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_trigger_fkey         FOREIGN KEY (trigger_id, org_id)      REFERENCES public.event_handlers(id, org_id);
-- Composite FK mirrors runs.triggering_event_id. NULL triggering_event_id
-- (manual blueprint runs) skips the check; org_id is pinned so a cross-org
-- event reference is impossible.
ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_triggering_event_id_org_id_fkey FOREIGN KEY (triggering_event_id, org_id) REFERENCES public.events(id, org_id);
-- Composite (id, org_id) FK like runs_actor_agent_fkey: the actor must belong to
-- the run's own org. ON DELETE SET NULL keeps the audit row when the agent is
-- deleted (the run still happened; the actor pointer just goes blank).
ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_actor_agent_fkey FOREIGN KEY (actor_agent_id, org_id) REFERENCES public.agents(id, org_id) ON DELETE SET NULL;

-- fired_run_id records the blueprint_run a firing produced (the firing unit is
-- the blueprint_run now), so it references blueprint_runs, not runs — the
-- spawner returns the blueprint_run id synchronously at fire time, before any
-- step run row exists. Must live after blueprint_runs is created.
ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_fired_run_id_org_id_fkey FOREIGN KEY (fired_run_id, org_id) REFERENCES public.blueprint_runs(id, org_id);

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_blueprint_run_fkey FOREIGN KEY (blueprint_run_id, org_id) REFERENCES public.blueprint_runs(id, org_id);

-- run_memory.blueprint_run_id is denormalized from the run and grouped per
-- blueprint run. Composite (blueprint_run_id, org_id) FK, matching the
-- tenant-isolation pattern every other cross-table reference here uses: a
-- cross-org blueprint_run reference is structurally impossible at the DB level
-- — defense in depth beyond RLS, whose WITH CHECK only validates org_id, not
-- which blueprint_run the row points at. Lives in this block (not the run_memory
-- FK section above) because blueprint_runs is created here. ON DELETE SET NULL
-- is scoped to blueprint_run_id alone (the PG 15 column-list form) so deleting a
-- blueprint run keeps the durable memory row with its org_id intact; a bare SET
-- NULL would try to null org_id too (NOT NULL) and fail the delete.
ALTER TABLE ONLY public.run_memory
    ADD CONSTRAINT run_memory_blueprint_run_id_org_id_fkey FOREIGN KEY (blueprint_run_id, org_id) REFERENCES public.blueprint_runs(id, org_id) ON DELETE SET NULL (blueprint_run_id);

ALTER TABLE public.blueprint_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.blueprint_runs  ENABLE ROW LEVEL SECURITY;

-- blueprint_steps inherits the blueprint's access — if the caller can't see
-- the parent blueprint, they can't see its step list. blueprints RLS already
-- applies creator + team-membership rules. The EXISTS subquery joins on
-- b.org_id = blueprint_steps.org_id because blueprints.id is text and can
-- collide across orgs.
CREATE POLICY blueprint_steps_all ON public.blueprint_steps FOR ALL
  USING      ((org_id = tf.current_org_id())
              AND (EXISTS (SELECT 1 FROM public.blueprints b
                           WHERE b.id = blueprint_steps.blueprint_id
                             AND b.org_id = blueprint_steps.org_id)))
  WITH CHECK ((org_id = tf.current_org_id())
              AND (EXISTS (SELECT 1 FROM public.blueprints b
                           WHERE b.id = blueprint_steps.blueprint_id
                             AND b.org_id = blueprint_steps.org_id)));

-- blueprint_runs are creator-scoped for manual runs and org-visible for
-- event-triggered runs. The blueprint_runs_creator_matches_trigger_type CHECK
-- pairs trigger_type with creator nullability: trigger_type='event' rows have
-- creator_user_id NULL (system-emitted via the admin pool by the router /
-- spawner). Per-command split mirrors the runs table.
CREATE POLICY blueprint_runs_select ON public.blueprint_runs FOR SELECT
  USING ((org_id = tf.current_org_id())
         AND tf.user_has_org_access(org_id)
         AND ((creator_user_id IS NULL)
              OR (creator_user_id = tf.current_user_id())));

CREATE POLICY blueprint_runs_insert ON public.blueprint_runs FOR INSERT
  WITH CHECK ((org_id = tf.current_org_id())
              AND tf.user_has_org_access(org_id)
              AND (creator_user_id = tf.current_user_id()));

CREATE POLICY blueprint_runs_update ON public.blueprint_runs FOR UPDATE
  USING ((org_id = tf.current_org_id())
         AND tf.user_has_org_access(org_id)
         AND ((creator_user_id IS NULL)
              OR (creator_user_id = tf.current_user_id())))
  WITH CHECK ((org_id = tf.current_org_id())
              AND tf.user_has_org_access(org_id)
              AND ((creator_user_id IS NULL)
                   OR (creator_user_id = tf.current_user_id())));

CREATE POLICY blueprint_runs_delete ON public.blueprint_runs FOR DELETE
  USING ((org_id = tf.current_org_id())
         AND tf.user_has_org_access(org_id)
         AND ((creator_user_id IS NULL)
              OR (creator_user_id = tf.current_user_id())));

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.blueprint_steps TO tf_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.blueprint_runs  TO tf_app;


--
-- Per-org GitHub App registration + installation tracking.
--
-- Per-org App is the v1 multi-mode default; orgs using the deployment-default
-- (hosted) App have NO org_github_apps row. The `_ref` columns hold Vault
-- secret-name pointers (the actual client_secret / PEM / webhook signing
-- secret are written via vault.create_secret in the manifest backend) so
-- app secrets never live in the relational schema.
--
-- Installations are 1:N from an org (and observed from GitHub's webhook
-- stream). They are NOT pointed back at org_github_apps: each org has at
-- most one App registered at any moment, so the org_id is sufficient to
-- resolve the App that produced the installation. When an org switches
-- Apps in the future, GitHub forces a fresh install ceremony and the
-- installation_ids change naturally.
--
-- App-pool writes on org_github_app_installations are blocked at the
-- policy layer: rows are mirrored from GitHub's webhook stream by the
-- webhook handler running on the admin pool. App-pool members can read
-- but never insert / update / delete.

CREATE TABLE public.org_github_apps (
    org_id uuid NOT NULL,
    app_id text NOT NULL,
    slug text NOT NULL,
    client_id text NOT NULL,
    client_secret_ref text NOT NULL,
    pem_ref text NOT NULL,
    webhook_secret_ref text NOT NULL,
    owner_type text DEFAULT 'user'::text NOT NULL,
    registered_at timestamp with time zone DEFAULT now() NOT NULL,
    registered_by_user_id uuid,
    active boolean DEFAULT true NOT NULL,
    -- Numeric GitHub user-account id of the App's bot ("<slug>[bot]"), fetched
    -- best-effort at registration (GET /users/<slug>[bot]). NOT the App id. NULL
    -- = unknown → plain "<slug>[bot]@..." commit email; when set, the resolver
    -- builds the numeric-id noreply form so bot commits link on github.com
    -- (TFAC-474).
    bot_user_id bigint
);

ALTER TABLE ONLY public.org_github_apps
    ADD CONSTRAINT org_github_apps_pkey PRIMARY KEY (org_id);

ALTER TABLE ONLY public.org_github_apps
    ADD CONSTRAINT org_github_apps_app_id_key UNIQUE (app_id);

ALTER TABLE ONLY public.org_github_apps
    ADD CONSTRAINT org_github_apps_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.org_github_apps
    ADD CONSTRAINT org_github_apps_registered_by_user_id_fkey FOREIGN KEY (registered_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;

ALTER TABLE public.org_github_apps ENABLE ROW LEVEL SECURITY;

-- Writes are admin-only (registering / rotating an App registration is
-- a sensitive workspace gesture). Reads open to any org member so
-- ResolveCredential() running on the app pool can see whether the org
-- has its own App on the read path.
CREATE POLICY org_github_apps_select ON public.org_github_apps FOR SELECT TO tf_app
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));

CREATE POLICY org_github_apps_insert ON public.org_github_apps FOR INSERT TO tf_app
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

CREATE POLICY org_github_apps_update ON public.org_github_apps FOR UPDATE TO tf_app
    USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)))
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

CREATE POLICY org_github_apps_delete ON public.org_github_apps FOR DELETE TO tf_app
    USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));


-- installation_id is unique only within a single GitHub host. Per-org
-- GHES bases mean two orgs can sit on independent GitHub instances whose
-- numeric installation IDs overlap, so the PK is composite (org_id,
-- installation_id): a webhook/backfill for one org can never collide with
-- or rewrite another org's row.
CREATE TABLE public.org_github_app_installations (
    installation_id text NOT NULL,
    org_id uuid NOT NULL,
    account_type text NOT NULL,
    account_login text NOT NULL,
    installed_at timestamp with time zone DEFAULT now() NOT NULL,
    removed_at timestamp with time zone,
    CONSTRAINT org_github_app_installations_account_type_check
        CHECK ((account_type = ANY (ARRAY['Organization'::text, 'User'::text])))
);

ALTER TABLE ONLY public.org_github_app_installations
    ADD CONSTRAINT org_github_app_installations_pkey PRIMARY KEY (org_id, installation_id);

ALTER TABLE ONLY public.org_github_app_installations
    ADD CONSTRAINT org_github_app_installations_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

-- Partial unique on active rows only: uninstall + reinstall cycles
-- stamp removed_at on the old row and insert a new row with a fresh
-- installation_id. The "at most one active install per account" guard
-- holds without overwriting historical rows (and without mutating the
-- installation_id PK on a reinstall).
CREATE UNIQUE INDEX org_github_app_installations_active_account_key
    ON public.org_github_app_installations (org_id, account_login)
    WHERE (removed_at IS NULL);

CREATE INDEX org_github_app_installations_org_idx
    ON public.org_github_app_installations (org_id);

ALTER TABLE public.org_github_app_installations ENABLE ROW LEVEL SECURITY;

-- Installation rows mirror GitHub-side state and are written exclusively
-- by the webhook handler (admin pool, system context). App-pool members
-- read but never write — any user gesture that needs to add or remove an
-- installation goes through the GitHub install / uninstall ceremony,
-- whose result we discover via webhook.
CREATE POLICY org_github_app_installations_select ON public.org_github_app_installations FOR SELECT TO tf_app
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));

CREATE POLICY org_github_app_installations_insert ON public.org_github_app_installations FOR INSERT TO tf_app
    WITH CHECK (false);

CREATE POLICY org_github_app_installations_update ON public.org_github_app_installations FOR UPDATE TO tf_app
    USING (false);

CREATE POLICY org_github_app_installations_delete ON public.org_github_app_installations FOR DELETE TO tf_app
    USING (false);

GRANT ALL ON TABLE public.org_github_apps TO postgres;
GRANT ALL ON TABLE public.org_github_apps TO anon;
GRANT ALL ON TABLE public.org_github_apps TO authenticated;
GRANT ALL ON TABLE public.org_github_apps TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.org_github_apps TO tf_app;

GRANT ALL ON TABLE public.org_github_app_installations TO postgres;
GRANT ALL ON TABLE public.org_github_app_installations TO anon;
GRANT ALL ON TABLE public.org_github_app_installations TO authenticated;
GRANT ALL ON TABLE public.org_github_app_installations TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.org_github_app_installations TO tf_app;


-- === Org default templates (SKY-381) ====================================
-- The org-level template a new team's prompts + handlers are copied from at
-- team creation. Org-scoped (no team_id): the template is the *source*, not a
-- team-owned set — BootstrapNewTeam materializes a per-team copy of it. Mirrors
-- the prompts / event_handlers content columns so the copy reproduces them
-- byte-for-byte. system_slug is NOT NULL and stable (the real shipped slug for
-- source='system' rows, a generated tmpl-<uuid> for admin-authored rows), so the
-- per-team copy dedupes on (org_id, team_id, system_slug) like the shipped seed.
-- All four CRUD verbs are org-admin-gated (tf.user_is_org_admin) — the editor is
-- an org-admin surface; the bootstrap seed + per-team materialize run on the
-- admin pool (BYPASSRLS). Self-contained block appended at the end of Up: every
-- FK target (orgs, teams, users, events_catalog) already exists.

CREATE TABLE public.org_template_prompts (
    id text DEFAULT (gen_random_uuid())::text NOT NULL,
    org_id uuid NOT NULL,
    system_slug text NOT NULL,
    name text NOT NULL,
    body text NOT NULL,
    source text DEFAULT 'user'::text NOT NULL,
    allowed_tools text DEFAULT ''::text NOT NULL,
    model text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
    -- source app-validated, not CHECK-constrained (source_check dropped in the
    -- this baseline, both dialects; mirrors prompts).
);

-- Template blueprints + their ordered steps. Org-scoped (no team_id) like the
-- other org_template_* tables — the template is the *source*; MaterializeIntoTeam
-- deep-copies a blueprint (header + steps' prompts) into the team's blueprints /
-- blueprint_steps. Mirrors blueprints / blueprint_steps minus the team scoping,
-- with org_id as the composite-FK partner in place of the team-table's team_id.
CREATE TABLE public.org_template_blueprints (
    id text DEFAULT (gen_random_uuid())::text NOT NULL,
    org_id uuid NOT NULL,
    system_slug text NOT NULL,
    name text NOT NULL,
    source text DEFAULT 'user'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
    -- source app-validated, not CHECK-constrained (source_check dropped in the
    -- this baseline, both dialects; mirrors blueprints).
);

CREATE TABLE public.org_template_blueprint_steps (
    org_id uuid NOT NULL,
    blueprint_id text NOT NULL,
    step_index integer NOT NULL,
    step_prompt_id text NOT NULL,
    brief text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.org_template_handlers (
    id text DEFAULT (gen_random_uuid())::text NOT NULL,
    org_id uuid NOT NULL,
    system_slug text NOT NULL,
    kind text NOT NULL,
    event_type text NOT NULL,
    scope_predicate_json jsonb,
    enabled boolean DEFAULT true NOT NULL,
    source text DEFAULT 'user'::text NOT NULL,
    name text,
    default_priority real,
    sort_order integer,
    blueprint_id text,
    breaker_threshold integer,
    min_autonomy_suitability real,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT org_template_handlers_kind_check CHECK ((kind = ANY (ARRAY['rule'::text, 'trigger'::text]))),
    -- source app-validated, not CHECK-constrained (source_check dropped in the
    -- this baseline, both dialects; mirrors event_handlers).
    CONSTRAINT org_template_handlers_rule_shape CHECK (((kind <> 'rule'::text) OR ((blueprint_id IS NULL) AND (breaker_threshold IS NULL) AND (min_autonomy_suitability IS NULL) AND (name IS NOT NULL) AND (default_priority IS NOT NULL) AND (sort_order IS NOT NULL)))),
    CONSTRAINT org_template_handlers_trigger_shape CHECK (((kind <> 'trigger'::text) OR ((blueprint_id IS NOT NULL) AND (breaker_threshold IS NOT NULL) AND (min_autonomy_suitability IS NOT NULL) AND (default_priority IS NULL) AND (sort_order IS NULL) AND (name IS NULL))))
);

ALTER TABLE ONLY public.org_template_prompts
    ADD CONSTRAINT org_template_prompts_pkey PRIMARY KEY (org_id, id);
ALTER TABLE ONLY public.org_template_prompts
    ADD CONSTRAINT org_template_prompts_id_org_id_key UNIQUE (id, org_id);
ALTER TABLE ONLY public.org_template_prompts
    ADD CONSTRAINT org_template_prompts_org_slug_key UNIQUE (org_id, system_slug);

ALTER TABLE ONLY public.org_template_blueprints
    ADD CONSTRAINT org_template_blueprints_pkey PRIMARY KEY (org_id, id);
ALTER TABLE ONLY public.org_template_blueprints
    ADD CONSTRAINT org_template_blueprints_id_org_id_key UNIQUE (id, org_id);
ALTER TABLE ONLY public.org_template_blueprints
    ADD CONSTRAINT org_template_blueprints_org_slug_key UNIQUE (org_id, system_slug);

ALTER TABLE ONLY public.org_template_blueprint_steps
    ADD CONSTRAINT org_template_blueprint_steps_pkey PRIMARY KEY (org_id, blueprint_id, step_index);

-- Copy-only prompts (template mirror): a template prompt is a step in at most
-- one template blueprint.
ALTER TABLE ONLY public.org_template_blueprint_steps
    ADD CONSTRAINT org_template_blueprint_steps_org_step_prompt_key UNIQUE (org_id, step_prompt_id);

ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_pkey PRIMARY KEY (org_id, id);
ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_org_slug_key UNIQUE (org_id, system_slug);

CREATE INDEX idx_org_template_handlers_kind ON public.org_template_handlers USING btree (org_id, kind);
CREATE INDEX idx_org_template_blueprint_steps_step_prompt ON public.org_template_blueprint_steps USING btree (step_prompt_id, org_id);

-- Mirror of the team-table backstop: a template trigger fires exactly one
-- template blueprint.
CREATE UNIQUE INDEX org_template_handlers_one_trigger_per_blueprint ON public.org_template_handlers USING btree (org_id, blueprint_id) WHERE (blueprint_id IS NOT NULL);

ALTER TABLE ONLY public.org_template_prompts
    ADD CONSTRAINT org_template_prompts_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.org_template_blueprints
    ADD CONSTRAINT org_template_blueprints_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

-- A template blueprint steps through template prompts in the same org; deleting
-- a template blueprint drops its steps (CASCADE), and a template prompt can't be
-- removed while a step references it (RESTRICT) — mirrors blueprint_steps.
ALTER TABLE ONLY public.org_template_blueprint_steps
    ADD CONSTRAINT org_template_blueprint_steps_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.org_template_blueprint_steps
    ADD CONSTRAINT org_template_blueprint_steps_blueprint_fkey FOREIGN KEY (blueprint_id, org_id) REFERENCES public.org_template_blueprints(id, org_id) ON DELETE CASCADE;
ALTER TABLE ONLY public.org_template_blueprint_steps
    ADD CONSTRAINT org_template_blueprint_steps_step_prompt_fkey FOREIGN KEY (step_prompt_id, org_id) REFERENCES public.org_template_prompts(id, org_id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_event_type_fkey FOREIGN KEY (event_type) REFERENCES public.events_catalog(id) ON DELETE RESTRICT;
-- A template trigger may only fire a template blueprint in the same org; deleting
-- a template blueprint cascades its triggers (mirrors the team-table CASCADE).
ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_blueprint_id_org_id_fkey FOREIGN KEY (blueprint_id, org_id) REFERENCES public.org_template_blueprints(id, org_id) ON DELETE CASCADE;

ALTER TABLE public.org_template_prompts ENABLE ROW LEVEL SECURITY;
CREATE POLICY org_template_prompts_all ON public.org_template_prompts
    USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)))
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

ALTER TABLE public.org_template_blueprints ENABLE ROW LEVEL SECURITY;
CREATE POLICY org_template_blueprints_all ON public.org_template_blueprints
    USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)))
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

ALTER TABLE public.org_template_blueprint_steps ENABLE ROW LEVEL SECURITY;
CREATE POLICY org_template_blueprint_steps_all ON public.org_template_blueprint_steps
    USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)))
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

ALTER TABLE public.org_template_handlers ENABLE ROW LEVEL SECURITY;
CREATE POLICY org_template_handlers_all ON public.org_template_handlers
    USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)))
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

GRANT ALL ON TABLE public.org_template_prompts TO postgres;
GRANT ALL ON TABLE public.org_template_prompts TO anon;
GRANT ALL ON TABLE public.org_template_prompts TO authenticated;
GRANT ALL ON TABLE public.org_template_prompts TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.org_template_prompts TO tf_app;

GRANT ALL ON TABLE public.org_template_blueprints TO postgres;
GRANT ALL ON TABLE public.org_template_blueprints TO anon;
GRANT ALL ON TABLE public.org_template_blueprints TO authenticated;
GRANT ALL ON TABLE public.org_template_blueprints TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.org_template_blueprints TO tf_app;

GRANT ALL ON TABLE public.org_template_blueprint_steps TO postgres;
GRANT ALL ON TABLE public.org_template_blueprint_steps TO anon;
GRANT ALL ON TABLE public.org_template_blueprint_steps TO authenticated;
GRANT ALL ON TABLE public.org_template_blueprint_steps TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.org_template_blueprint_steps TO tf_app;

GRANT ALL ON TABLE public.org_template_handlers TO postgres;
GRANT ALL ON TABLE public.org_template_handlers TO anon;
GRANT ALL ON TABLE public.org_template_handlers TO authenticated;
GRANT ALL ON TABLE public.org_template_handlers TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.org_template_handlers TO tf_app;


--
-- Durable router event queue. The in-memory bus drops events
-- for slow subscribers under burst; the router — which persists event
-- rows and creates tasks — drains this transactional-outbox queue
-- instead. The events audit row and a queue row are written atomically at
-- ingest (the store's Enqueue), and ClaimNext uses FOR UPDATE SKIP LOCKED
-- so multiple router workers can drain concurrently without claiming the
-- same row twice (groundwork for horizontal routing; running N is a
-- non-goal). Delivery is at-least-once, not exactly-once: a 'processing'
-- row left by a crash is reset to pending at boot and replayed, so the
-- router's processing is idempotent (the task dedup index makes a replay
-- a no-op). Admin-pool wired: the ingestor + drain worker are system
-- services, so the RLS policy below is defense-in-depth (admin bypasses
-- it) and org_id is bound in every statement.
--
CREATE TABLE public.event_queue (
    id           bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    org_id       uuid NOT NULL,
    event_id     uuid NOT NULL,
    entity_id    uuid,
    event_type   text NOT NULL,
    status       text NOT NULL DEFAULT 'pending',
    attempts     integer NOT NULL DEFAULT 0,
    last_error   text,
    enqueued_at  timestamp with time zone NOT NULL DEFAULT now(),
    claimed_at   timestamp with time zone,
    processed_at timestamp with time zone
);

ALTER TABLE ONLY public.event_queue
    ADD CONSTRAINT event_queue_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.event_queue
    ADD CONSTRAINT event_queue_event_id_org_id_fkey FOREIGN KEY (event_id, org_id) REFERENCES public.events(id, org_id) ON DELETE CASCADE;
ALTER TABLE ONLY public.event_queue
    ADD CONSTRAINT event_queue_entity_id_org_id_fkey FOREIGN KEY (entity_id, org_id) REFERENCES public.entities(id, org_id) ON DELETE CASCADE;

CREATE INDEX idx_event_queue_pending ON public.event_queue USING btree (id) WHERE (status = 'pending'::text);
CREATE INDEX idx_event_queue_entity ON public.event_queue USING btree (entity_id);
CREATE INDEX idx_event_queue_status_processed ON public.event_queue USING btree (status, processed_at);
CREATE UNIQUE INDEX idx_event_queue_event ON public.event_queue USING btree (event_id);

ALTER TABLE public.event_queue ENABLE ROW LEVEL SECURITY;
CREATE POLICY event_queue_all ON public.event_queue
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)))
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));

GRANT ALL ON TABLE public.event_queue TO postgres;
GRANT ALL ON TABLE public.event_queue TO anon;
GRANT ALL ON TABLE public.event_queue TO authenticated;
GRANT ALL ON TABLE public.event_queue TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.event_queue TO tf_app;


--
-- SKY-442: per-user secret scope. Mirrors the vault_*_org_secret quartet
-- (defined above), adding a p_user_id dimension and a user-scoped RLS
-- gate. Vault name convention: 'org/<org_id>/user/<user_id>/<key>'. The
-- claims-checked trio gates on BOTH p_org_id = tf.current_org_id() AND
-- p_user_id = tf.current_user_id() so a handler running as user A can
-- never read user B's token; the _system variant trusts explicit args
-- (admin pool only). Custodies the Jira "act as yourself" credential.
--

--
-- Name: vault_put_user_secret(uuid, uuid, text, text, text); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.vault_put_user_secret(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text DEFAULT NULL::text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/user/' || p_user_id::text || '/' || p_key;
  v_existing  UUID;
  -- vault.secrets.description is NOT NULL; coalesce NULL → '' so callers
  -- can pass NULL ergonomically.
  v_desc      TEXT := COALESCE(p_description, '');
BEGIN
  -- DEFINER + arbitrary p_org_id/p_user_id would let any tf_app caller
  -- read/write ANY user's secrets; gate on the JWT-claims org AND user
  -- so the wrapper only ever touches the active session's own credential.
  -- NULL p_org_id/p_user_id or NULL current_org_id()/current_user_id()
  -- would slip past IS DISTINCT FROM (both-NULL is "not distinct"). Refuse
  -- explicitly so a claims-less session can't sneak through.
  IF p_org_id IS NULL OR p_user_id IS NULL OR tf.current_org_id() IS NULL OR tf.current_user_id() IS NULL THEN
    RAISE EXCEPTION 'Vault access denied: missing org/user context (p_org_id, p_user_id, or request.jwt.claims is NULL)'
      USING ERRCODE = '42501';
  END IF;
  IF p_org_id <> tf.current_org_id() THEN
    RAISE EXCEPTION 'cross-org Vault access denied: p_org_id=% does not match request.jwt.claims.org_id', p_org_id
      USING ERRCODE = '42501';
  END IF;
  IF p_user_id <> tf.current_user_id() THEN
    RAISE EXCEPTION 'cross-user Vault access denied: p_user_id=% does not match request.jwt.claims.sub', p_user_id
      USING ERRCODE = '42501';
  END IF;
  SELECT id INTO v_existing FROM vault.decrypted_secrets WHERE name = v_full_name;
  IF v_existing IS NOT NULL THEN
    PERFORM vault.update_secret(v_existing, p_secret, v_full_name, v_desc);
    RETURN v_existing;
  END IF;
  RETURN vault.create_secret(p_secret, v_full_name, v_desc);
END;
$$;
-- +goose StatementEnd


--
-- Name: vault_get_user_secret(uuid, uuid, text); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.vault_get_user_secret(p_org_id uuid, p_user_id uuid, p_key text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/user/' || p_user_id::text || '/' || p_key;
  v_secret    TEXT;
BEGIN
  IF p_org_id IS NULL OR p_user_id IS NULL OR tf.current_org_id() IS NULL OR tf.current_user_id() IS NULL THEN
    RAISE EXCEPTION 'Vault access denied: missing org/user context (p_org_id, p_user_id, or request.jwt.claims is NULL)'
      USING ERRCODE = '42501';
  END IF;
  IF p_org_id <> tf.current_org_id() THEN
    RAISE EXCEPTION 'cross-org Vault access denied: p_org_id=% does not match request.jwt.claims.org_id', p_org_id
      USING ERRCODE = '42501';
  END IF;
  IF p_user_id <> tf.current_user_id() THEN
    RAISE EXCEPTION 'cross-user Vault access denied: p_user_id=% does not match request.jwt.claims.sub', p_user_id
      USING ERRCODE = '42501';
  END IF;
  SELECT decrypted_secret INTO v_secret
    FROM vault.decrypted_secrets
   WHERE name = v_full_name;
  RETURN v_secret;
END;
$$;
-- +goose StatementEnd


--
-- Name: vault_get_user_secret_system(uuid, uuid, text); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.vault_get_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/user/' || p_user_id::text || '/' || p_key;
  v_secret    TEXT;
BEGIN
  -- System/background read path (write-actor resolver acting as a user).
  -- No current_org_id()/current_user_id() check: p_org_id + p_user_id are
  -- trusted (the EXECUTE grant restricts this to the admin/system pool —
  -- tf_app has none, so a request handler can't reach it; those use the
  -- claims-checked vault_get_user_secret instead). Same name convention:
  -- 'org/<org_id>/user/<user_id>/<key>'. A NULL org/user is a caller bug,
  -- not a privilege failure — refuse explicitly rather than silently
  -- looking up a malformed name.
  IF p_org_id IS NULL OR p_user_id IS NULL THEN
    RAISE EXCEPTION 'system Vault access denied: p_org_id or p_user_id is NULL'
      USING ERRCODE = '22004';
  END IF;
  SELECT decrypted_secret INTO v_secret
    FROM vault.decrypted_secrets
   WHERE name = v_full_name;
  RETURN v_secret;
END;
$$;
-- +goose StatementEnd


--
-- Name: vault_delete_user_secret(uuid, uuid, text); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.vault_delete_user_secret(p_org_id uuid, p_user_id uuid, p_key text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/user/' || p_user_id::text || '/' || p_key;
  v_existing  UUID;
BEGIN
  IF p_org_id IS NULL OR p_user_id IS NULL OR tf.current_org_id() IS NULL OR tf.current_user_id() IS NULL THEN
    RAISE EXCEPTION 'Vault access denied: missing org/user context (p_org_id, p_user_id, or request.jwt.claims is NULL)'
      USING ERRCODE = '42501';
  END IF;
  IF p_org_id <> tf.current_org_id() THEN
    RAISE EXCEPTION 'cross-org Vault access denied: p_org_id=% does not match request.jwt.claims.org_id', p_org_id
      USING ERRCODE = '42501';
  END IF;
  IF p_user_id <> tf.current_user_id() THEN
    RAISE EXCEPTION 'cross-user Vault access denied: p_user_id=% does not match request.jwt.claims.sub', p_user_id
      USING ERRCODE = '42501';
  END IF;
  SELECT id INTO v_existing FROM vault.decrypted_secrets WHERE name = v_full_name;
  IF v_existing IS NULL THEN
    RETURN FALSE;
  END IF;
  DELETE FROM vault.secrets WHERE id = v_existing;
  RETURN TRUE;
END;
$$;
-- +goose StatementEnd


--
-- Name: FUNCTION vault_put_user_secret(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.vault_put_user_secret(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_put_user_secret(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_put_user_secret(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) TO postgres;
GRANT ALL ON FUNCTION public.vault_put_user_secret(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) TO tf_app;


--
-- Name: FUNCTION vault_get_user_secret(p_org_id uuid, p_user_id uuid, p_key text); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.vault_get_user_secret(p_org_id uuid, p_user_id uuid, p_key text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_get_user_secret(p_org_id uuid, p_user_id uuid, p_key text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_get_user_secret(p_org_id uuid, p_user_id uuid, p_key text) TO postgres;
GRANT ALL ON FUNCTION public.vault_get_user_secret(p_org_id uuid, p_user_id uuid, p_key text) TO tf_app;


--
-- Name: FUNCTION vault_get_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text); Type: ACL; Schema: public; Owner: -
--

-- System/admin pool ONLY. Deliberately NOT granted to tf_app: the app
-- pool must stay on the claims-checked vault_get_user_secret. The admin
-- pool connects as supabase_admin (superuser, owns this function) and
-- executes it regardless of grant; postgres is granted to mirror the
-- sibling vault_* ACLs. tf_app lacking EXECUTE here is the load-bearing
-- guardrail — pinned by the pgtest "tf_app denied" assertion. This is
-- the per-user mirror of vault_get_org_secret_system's grant matrix.
REVOKE ALL ON FUNCTION public.vault_get_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_get_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_get_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text) TO postgres;


--
-- Name: FUNCTION vault_delete_user_secret(p_org_id uuid, p_user_id uuid, p_key text); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.vault_delete_user_secret(p_org_id uuid, p_user_id uuid, p_key text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_delete_user_secret(p_org_id uuid, p_user_id uuid, p_key text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_delete_user_secret(p_org_id uuid, p_user_id uuid, p_key text) TO postgres;
GRANT ALL ON FUNCTION public.vault_delete_user_secret(p_org_id uuid, p_user_id uuid, p_key text) TO tf_app;


--
-- consolidated from 202606150001_org_jira_apps.sql
--

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


--
-- consolidated from 202606160001_jira_identity_cloud_source.sql
--

-- Widen user_jira_identities.source to admit 'cloud_api_token' — the provenance
-- marker the Cloud per-user API-token bind records (the paste counterpart of the
-- Data Center 'pat'). The value set stays closed (identity provenance is
-- security-relevant); this only adds the one Cloud paste method. Cloud OAuth
-- ('connect_oauth') was already allowed by the baseline.
ALTER TABLE public.user_jira_identities DROP CONSTRAINT user_jira_identities_source_check;
ALTER TABLE public.user_jira_identities ADD CONSTRAINT user_jira_identities_source_check
    CHECK ((source = ANY (ARRAY['pat'::text, 'connect_oauth'::text, 'scim'::text, 'cloud_api_token'::text])));


--
-- consolidated from 202606160002_vault_put_user_secret_system.sql
--

-- vault_put_user_secret_system — the write-side mirror of
-- vault_get_user_secret_system: persist a per-user secret WITHOUT a request JWT,
-- for system/background code acting as a user. The motivating caller is the
-- Cloud OAuth access-token minter — Atlassian rotates the refresh token on every
-- refresh, so the minter must write the new refresh token back into the user's
-- credential envelope while running on the claims-free admin pool (the
-- write-actor resolver holds no JWT). The claims-checked vault_put_user_secret
-- is unreachable there, exactly as the read path needs the _system door.
--
-- Same name convention as the rest of the per-user vault wrappers:
-- 'org/<org_id>/user/<user_id>/<key>'. No current_org_id()/current_user_id()
-- gate — p_org_id + p_user_id are trusted because the EXECUTE grant restricts
-- this to the admin/system pool (tf_app has none). A NULL org/user is a caller
-- bug, refused explicitly rather than building a malformed name.

-- +goose StatementBegin
CREATE FUNCTION public.vault_put_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text DEFAULT NULL::text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/user/' || p_user_id::text || '/' || p_key;
  v_existing  UUID;
  v_desc      TEXT := COALESCE(p_description, '');
BEGIN
  IF p_org_id IS NULL OR p_user_id IS NULL THEN
    RAISE EXCEPTION 'system Vault access denied: p_org_id or p_user_id is NULL'
      USING ERRCODE = '22004';
  END IF;
  SELECT id INTO v_existing FROM vault.decrypted_secrets WHERE name = v_full_name;
  IF v_existing IS NOT NULL THEN
    PERFORM vault.update_secret(v_existing, p_secret, v_full_name, v_desc);
    RETURN v_existing;
  END IF;
  RETURN vault.create_secret(p_secret, v_full_name, v_desc);
END;
$$;
-- +goose StatementEnd

-- System/admin pool ONLY. Deliberately NOT granted to tf_app: the app pool must
-- stay on the claims-checked vault_put_user_secret. Mirrors the
-- vault_get_user_secret_system grant matrix exactly.
REVOKE ALL ON FUNCTION public.vault_put_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_put_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_put_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) TO postgres;


--
-- consolidated from 202606170001_org_secrets_app_encrypted.sql
--

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
    -- Surrogate PK. The natural key (org_id, COALESCE(user_id, …), key) can't
    -- serve as one — it's an expression over a nullable column — so a surrogate
    -- id carries the PK that the rest of this schema, logical replication / CDC,
    -- and the Supabase tooling all expect. The unique index below is what
    -- actually dedups and what the UPSERTs target.
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
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
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- Schema-level invariants (defense-in-depth — the app is the only writer
    -- and always satisfies them). A secret must have a name; AES-256-GCM uses a
    -- 12-byte nonce and appends a 16-byte auth tag, so ciphertext is never
    -- shorter than the tag. Corruption or a manual mis-insert trips here at
    -- write time instead of surfacing as a decrypt failure much later.
    CONSTRAINT org_secrets_key_nonempty CHECK (key <> ''),
    CONSTRAINT org_secrets_nonce_len CHECK (octet_length(nonce) = 12),
    CONSTRAINT org_secrets_ciphertext_len CHECK (octet_length(ciphertext) >= 16)
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


--
-- consolidated from 202606170002_dashboard_backfill_marker.sql
--

-- dashboard_backfilled_at marks that the one-shot trailing-window dashboard
-- history backfill has run for this (user, host) GitHub identity (TFAC-396).
-- See the SQLite sibling migration for the full rationale. NULL = eligible; a
-- timestamp = done. Written by the claims-free backfill worker through the
-- admin pool (MarkDashboardBackfilledSystem), so it carries no RLS policy of
-- its own beyond the table's existing per-user gates.
ALTER TABLE public.user_github_identities ADD COLUMN dashboard_backfilled_at timestamp with time zone;


--
-- consolidated from 202606180001_permission_absent_autodeny.sql
--

-- Presence-gated fast auto-deny for unattended permission prompts (TFAC-392).
-- See the SQLite sibling migration for the full rationale. permission_absent_grace_ms
-- is the grace window (ms) an unattended prompt waits before denying with
-- "no operator available"; permission_absent_autodeny_enabled is the master toggle
-- (false = today's full-permTimeout() behavior, byte-for-byte). Both ship NOT NULL
-- with the same defaults as the SQLite tree and domain.DefaultTeamSettings().
ALTER TABLE public.team_settings ADD COLUMN permission_absent_grace_ms integer NOT NULL DEFAULT 15000;
ALTER TABLE public.team_settings ADD COLUMN permission_absent_autodeny_enabled boolean NOT NULL DEFAULT true;


--
-- consolidated from 202606180002_org_invites.sql
--

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

-- Tenant-scoped FK target for target_team_id below. teams.id is already the
-- PK, so (id, org_id) is trivially unique; declaring it lets org_invites FK
-- the *pair* and pin a target team to the invite's own org at the schema
-- level — the same (id, org_id)-unique + composite-FK pattern the rest of the
-- schema uses (agents, entities, runs, tasks, blueprint_runs, …). teams is
-- just the one parent that never needed it until now.
ALTER TABLE public.teams ADD CONSTRAINT teams_id_org_id_key UNIQUE (id, org_id);

CREATE TABLE public.org_invites (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         uuid NOT NULL REFERENCES public.orgs(id)  ON DELETE CASCADE,
    email          text NOT NULL,               -- stored lower-cased (app normalizes)
    role           public.org_role NOT NULL DEFAULT 'member',
    target_team_id uuid NULL,                   -- NULL = org-only; composite FK below pins it to org_id
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
    CONSTRAINT org_invites_role_not_owner CHECK (role <> 'owner'),
    -- Tenant-scoped FK: a non-NULL target_team_id MUST belong to this invite's
    -- own org. This is the load-bearing cross-org guard — the redeem/grant path
    -- runs on the admin pool (BYPASSRLS), so RLS can't protect it; only the
    -- integrity of this stored row can. MATCH SIMPLE means a NULL
    -- target_team_id skips the check (org-only invites). ON DELETE SET NULL
    -- (target_team_id) nulls just the team ref when a team is deleted, leaving
    -- the NOT NULL org_id intact (same pg15 column-list form blueprint_runs
    -- uses). The app layer validates this too, for a friendly 400 instead of a
    -- raw FK violation; this is the backstop that holds on every write path.
    CONSTRAINT org_invites_target_team_fkey
        FOREIGN KEY (target_team_id, org_id) REFERENCES public.teams (id, org_id)
        ON DELETE SET NULL (target_team_id)
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


--
-- consolidated from 202606180003_guard_org_owners_security_definer.sql
--

-- guard_org_owners is the global invariant "every org must retain ≥1 owner",
-- fired by AFTER UPDATE/DELETE statement triggers on org_memberships. The
-- baseline created it SECURITY INVOKER (the default), so its owner-existence
-- check ran under the mutating caller's RLS context — fine for an admin
-- removing/demoting someone (the admin still satisfies org_memberships_select
-- and sees every row), but BROKEN for a self-leave: the DELETE removes the
-- caller's own membership row, and org_memberships_select gates peer rows on
-- tf.user_has_org_access(), which now returns false for the just-departed
-- caller. The check then sees zero rows, concludes the org has no owner, and
-- raises a false 23514 — so a non-owner could never leave.
--
-- A global-invariant guard must evaluate the TRUE org state, not the caller's
-- RLS-filtered view. Make it SECURITY DEFINER (matching every other tf.*
-- helper) so the owner count is read past RLS. The body is otherwise the
-- baseline's verbatim; search_path stays pinned, so the definer rights are
-- safe. Behavior for admin-context mutations is unchanged (they already saw
-- the full set); this only repairs the self-delete case.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.guard_org_owners() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM affected ao
    WHERE NOT EXISTS (
      SELECT 1 FROM org_memberships
       WHERE org_id = ao.org_id AND role = 'owner'
    )
  ) THEN
    RAISE EXCEPTION 'each org must retain at least one owner role'
      USING ERRCODE = '23514';
  END IF;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd


--
-- consolidated from 202606200001_sso.sql
--

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
    -- "Require SSO" switch. When true, a login via a NON-SSO
    -- identity (GitHub) whose verified email is on one of this connection's
    -- VERIFIED domains is rejected unless the principal is break-glass. A
    -- separate axis from `enabled`: enabling makes SSO available, enforcing
    -- makes it mandatory. Enforcing a broken connection would lock everyone
    -- out, so the toggle is gated (handler-side) behind a proven-working
    -- connection — enabled + a verified domain + a passed Test — and the
    -- sso_break_glass set is the recovery path.
    enforced     boolean NOT NULL DEFAULT false,
    -- Stamped when a verify-before-enforce Test PASSES end-to-end:
    -- the durable "this connection has passed a Test" signal the enforcement
    -- toggle gates on. NULL = never passed.
    last_tested_at timestamptz NULL,
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
    -- A claim with an empty verification token is nonsensical — it would
    -- yield `_triagefactory-verification.<domain> ""` in the DNS-TXT
    -- challenge (TFAC-428). NOT NULL alone permits ''; mirror the domain
    -- non-empty guard.
    CONSTRAINT sso_domains_token_nonempty CHECK (token <> ''),
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

-- Index the FK's referencing column: Postgres doesn't auto-index it, and the
-- ON DELETE CASCADE from sso_connections must find a connection's domains
-- without a seq scan (the org_domain index leads with org_id, so it can't
-- serve a connection_id lookup). Also serves a future "domains for this
-- connection" read. Matches the baseline's index-the-FK-column pattern
-- (idx_tasks_entity, idx_runs_task, ...).
CREATE INDEX sso_domains_connection_idx ON public.sso_domains (connection_id);

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


-- sso_break_glass: the principals that may still authenticate via
-- their NON-SSO (GitHub) identity under SSO enforcement (sso_connections.
-- enforced) — the recovery path if the IdP breaks, so they can un-enforce.
-- Per (org, principal). The owner is the default, seeded when enforcement is
-- first enabled; the handler refuses to remove the last row while the
-- connection is enforced (the "can't enforce yourself into a no-recovery state"
-- guard, same shape as can't-remove-the-last-owner). FK to orgs + users (not
-- org_memberships) so a membership change never silently cascade-drops a
-- break-glass row out from under that guard; the handler validates org
-- membership at add time, and the login-time check is harmless for a non-member.
CREATE TABLE public.sso_break_glass (
    org_id     uuid NOT NULL REFERENCES public.orgs(id)  ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

-- Index the second PK column: the PK covers (org_id, user_id); this serves a
-- by-principal read (e.g. "which orgs is this principal break-glass for") and
-- the users-side ON DELETE CASCADE.
CREATE INDEX sso_break_glass_user_idx ON public.sso_break_glass (user_id);

ALTER TABLE public.sso_break_glass ENABLE ROW LEVEL SECURITY;

-- Management surface is org-admin-gated in the current org, mirroring
-- sso_connections / sso_domains. NOT expressible in RLS (deliberately absent):
-- the login-time IsBreakGlass read, which resolves a principal mid-login with
-- no membership claims for the target org — it runs on the admin pool
-- (BYPASSRLS) with the resolved (org_id, principal) as the authorization, like
-- sso_connections.GetByProviderID. The break-glass list's email join also runs
-- on the admin pool (user_identities is admin-pool-only) but stays org-scoped
-- in SQL behind the admin-gated handler.
CREATE POLICY sso_break_glass_select ON public.sso_break_glass FOR SELECT
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY sso_break_glass_insert ON public.sso_break_glass FOR INSERT
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY sso_break_glass_delete ON public.sso_break_glass FOR DELETE
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

REVOKE ALL ON public.sso_break_glass FROM PUBLIC;
REVOKE ALL ON public.sso_break_glass FROM anon, authenticated, service_role;
GRANT SELECT, INSERT, DELETE ON public.sso_break_glass TO tf_app;


--
-- Principal identity model. public.users is the PRINCIPAL (one row per human);
-- this table maps each GoTrue auth.users login identity to its principal. A
-- human can hold several identities (a GitHub login plus N SSO providers); on
-- login they resolve to one principal — an identity links to an existing
-- principal when its VERIFIED email matches that principal's, otherwise a new
-- principal is minted. auth_user_id is the bridge to GoTrue and the natural key
-- (one principal per login identity). provider_subject keeps the IdP's stable
-- subject / SAML NameID so a re-link need not trust email alone.
--
-- Resolution + linking run only on the admin pool at login (the actor has no
-- claims context yet), so this table is deliberately not granted to tf_app and
-- exposes no app-role policy; RLS is enabled as a denied-by-default backstop.
--

CREATE TABLE public.user_identities (
    auth_user_id uuid NOT NULL,
    user_id uuid NOT NULL,
    provider text NOT NULL,
    provider_subject text,
    email text,
    email_verified boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_identities_provider_check CHECK ((provider = ANY (ARRAY['github'::text, 'saml'::text])))
);

ALTER TABLE ONLY public.user_identities
    ADD CONSTRAINT user_identities_pkey PRIMARY KEY (auth_user_id);

ALTER TABLE ONLY public.user_identities
    ADD CONSTRAINT user_identities_auth_user_id_fkey FOREIGN KEY (auth_user_id) REFERENCES auth.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.user_identities
    ADD CONSTRAINT user_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

CREATE INDEX user_identities_user_id_idx ON public.user_identities USING btree (user_id);

-- The verified-email auto-link lookup at login: find the principal that already
-- owns this verified email so a second identity attaches to the same human.
CREATE INDEX user_identities_link_lookup_idx ON public.user_identities USING btree (lower(email)) WHERE email_verified;

CREATE TRIGGER user_identities_set_updated_at BEFORE UPDATE ON public.user_identities FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();

ALTER TABLE public.user_identities ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.user_identities FROM PUBLIC;
REVOKE ALL ON public.user_identities FROM anon, authenticated, service_role;


--
-- TFAC-444: guard_team_admins — each team must retain ≥1 admin.
--
-- The team-tier twin of guard_org_owners, fired by AFTER UPDATE/DELETE
-- statement triggers on memberships: a write that would leave a team with no
-- 'admin' role (demoting / removing / leaving the last admin) is rejected with
-- SQLSTATE 23514, which TeamsStore surfaces as db.ErrLastTeamAdminGuard and the
-- handler answers as a 409.
--
-- SECURITY DEFINER from the start (matching guard_org_owners' fixed form and
-- every other tf.* helper): a global-invariant guard must evaluate the TRUE
-- team state, not the mutating caller's RLS-filtered view. A team self-leave
-- DELETEs the caller's own membership row, and although memberships_select
-- still exposes peers via org access (a team-leave keeps org membership), the
-- definer rights make the owner-count read independent of the caller's row
-- visibility either way — the robust posture. search_path stays pinned, so the
-- definer rights are safe.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.guard_team_admins() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM affected a
    WHERE NOT EXISTS (
      SELECT 1 FROM memberships
       WHERE team_id = a.team_id AND role = 'admin'
    )
  ) THEN
    RAISE EXCEPTION 'each team must retain at least one admin role'
      USING ERRCODE = '23514';
  END IF;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER memberships_keep_admin_on_delete AFTER DELETE ON public.memberships REFERENCING OLD TABLE AS affected FOR EACH STATEMENT EXECUTE FUNCTION tf.guard_team_admins();

CREATE TRIGGER memberships_keep_admin_on_update AFTER UPDATE ON public.memberships REFERENCING OLD TABLE AS affected FOR EACH STATEMENT EXECUTE FUNCTION tf.guard_team_admins();


--
-- TFAC-76: auth_events — the SOC2 authentication audit log of record. Durable,
-- append-only capture of every authentication / session outcome: login,
-- logout, refresh failure, JWT-verify failure, SSO enforcement, break-glass.
-- The AUTHENTICATION sibling of access_change_log (the authorization-CHANGE
-- log) — together the two halves of the SOC2 access-controls surface (CC6.1
-- logical access, CC7.2 monitoring).
--
-- Uniform BEST-EFFORT writes: every row is recorded AFTER the auth action; a
-- write failure is logged and swallowed, never failing or rolling back the
-- action (the audit table is deliberately off the login critical path — the
-- ERROR log line is the accepted "known gap" guarantee, in lieu of
-- gaplessness). event_type is a free-text discriminator (domain.AuthEvent* —
-- extensible, NO CHECK). org_id is NULLABLE (a pre-auth failure has no org);
-- user_id is NULLABLE (a pre-identity failure has no principal); session_id is
-- FK ON DELETE SET NULL so an auth_events row OUTLIVES the session reaper (the
-- durable record must survive the 30-day session window). detail_json carries
-- the per-event payload ({"method":…,"sso":…} | {"reason":…} | {"count":…} |
-- {"domain":…}; org is the row's own org_id column, not duplicated into detail).
--
-- Admin-pool-only / system table: writes + reads never carry user claims, and
-- org_id is frequently NULL, so an org-scoped RLS policy can't gate it. Denied
-- to the app roles exactly like public.user_identities (the established
-- system-only-table pattern): RLS enabled with NO policy (deny-by-default to
-- non-BYPASSRLS roles) + REVOKE ALL from PUBLIC + the app roles. The
-- superuser/admin pool bypasses RLS and does all I/O. NO reaper — auth_events
-- is the durable record and is never auto-purged. See TFAC-76.
CREATE TABLE public.auth_events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid,
    user_id uuid,
    session_id uuid,
    event_type text NOT NULL,
    ip_address inet,
    user_agent text,
    detail_json jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.auth_events
    ADD CONSTRAINT auth_events_pkey PRIMARY KEY (id);

-- org_id / user_id FKs are plain (NO ACTION): an org/user deletion is blocked
-- while it still has auth_events, preserving the authentication trail. NULLABLE
-- because pre-auth / pre-identity failures reference neither.
ALTER TABLE ONLY public.auth_events
    ADD CONSTRAINT auth_events_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id);

ALTER TABLE ONLY public.auth_events
    ADD CONSTRAINT auth_events_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id);

-- ON DELETE SET NULL: reaping a session keeps its auth_events, nulling the link
-- — the audit row survives the session-retention window.
ALTER TABLE ONLY public.auth_events
    ADD CONSTRAINT auth_events_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.sessions(id) ON DELETE SET NULL;

-- Partial indexes skip the frequent NULL-org / NULL-user rows; the type index
-- backs alerting-style "all of event_type X over time" reads.
CREATE INDEX auth_events_org_ts ON public.auth_events USING btree (org_id, created_at DESC) WHERE (org_id IS NOT NULL);
CREATE INDEX auth_events_user_ts ON public.auth_events USING btree (user_id, created_at DESC) WHERE (user_id IS NOT NULL);
CREATE INDEX auth_events_type_ts ON public.auth_events USING btree (event_type, created_at DESC);

ALTER TABLE public.auth_events ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.auth_events FROM PUBLIC;
REVOKE ALL ON public.auth_events FROM anon, authenticated, service_role;


-- staged_agent_injections (TFAC-501): the durable, producer-agnostic "stage for next
-- resume" agent-injection queue — the generic terminal/parked half of the staged-injection
-- delivery seam TFAC-493 shipped live-only. Rolled into the baseline (not a
-- forward migration) because multi-mode / Postgres is net-new and unshipped; the
-- SQLite tree, which HAS shipped, carries the equivalent forward migration
-- 202606290001_staged_agent_injections.sql.
--
-- Run-scoped, modeled on run_messages: team-scoped RLS inherited via the runs FK,
-- (run_id, org_id) FK ON DELETE CASCADE so a purged run takes its undelivered
-- injections with it (the agent will never resume to read them), org_id bound as
-- defense in depth. The store reads/writes on the admin pool (claims-less
-- producer + consumer), but the policy + tf_app grants keep the run_messages
-- shape so a future request-scoped read needs no migration. body is the bare,
-- already-rendered injection line (the flush wraps + bundles); producer is a free-text
-- origin tag (domain.StagedInjectionProducer*, no CHECK).
CREATE TABLE public.staged_agent_injections (
    id         uuid DEFAULT gen_random_uuid() NOT NULL,
    run_id     uuid NOT NULL,
    org_id     uuid NOT NULL,
    producer   text NOT NULL,
    body       text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.staged_agent_injections
    ADD CONSTRAINT staged_agent_injections_pkey PRIMARY KEY (id);

-- The index covers the per-run claim's run_id lookup + the created_at sort,
-- mirroring idx_run_messages_run (run_id-leading, no org_id). org_id is applied
-- as a residual filter, NOT indexed: run_id is already selective and functionally
-- determines org_id, so leading with org_id buys nothing.
CREATE INDEX idx_staged_agent_injections_run ON public.staged_agent_injections USING btree (run_id, created_at);

ALTER TABLE ONLY public.staged_agent_injections
    ADD CONSTRAINT staged_agent_injections_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.staged_agent_injections
    ADD CONSTRAINT staged_agent_injections_run_id_org_id_fkey FOREIGN KEY (run_id, org_id) REFERENCES public.runs(id, org_id) ON DELETE CASCADE;

-- Team-scoped exactly like run_messages: visible iff the run is (the runs_select
-- RLS gates the EXISTS). The admin pool bypasses this; it's defense in depth for
-- any future app-pool read.
ALTER TABLE public.staged_agent_injections ENABLE ROW LEVEL SECURITY;

CREATE POLICY staged_agent_injections_all ON public.staged_agent_injections USING ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = staged_agent_injections.run_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = staged_agent_injections.run_id))));

GRANT ALL ON TABLE public.staged_agent_injections TO postgres;
GRANT ALL ON TABLE public.staged_agent_injections TO anon;
GRANT ALL ON TABLE public.staged_agent_injections TO authenticated;
GRANT ALL ON TABLE public.staged_agent_injections TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.staged_agent_injections TO tf_app;


-- Slack workspace connect: org_slack_workspaces — the (Slack workspace,
-- Slack app) <-> org bind. What TF actually holds is "an app installed in a
-- workspace", not a workspace in the abstract: team_id (Slack's ID for the
-- workspace, legacy naming, no relation to TF teams) and api_app_id (Slack's
-- ID for the app, present in every event delivery) together are the natural
-- key — PRIMARY KEY (workspace_id, api_app_id) rather than a surrogate id
-- plus a separate unique index.
--
-- Two invariants, both load-bearing for the Socket Mode boundary rule (a
-- later leaf: Socket Mode connections belong to the app, and Slack
-- load-balances an app's event envelopes across all its open sockets, so any
-- rule letting one app's stream span TF-org boundaries drops legitimate
-- events nondeterministically):
--
--   1. A workspace may host many apps (e.g. a prod TF org and an eval TF org
--      each running their own bot in the same workspace) — falls out of the
--      composite PK, no extra enforcement needed.
--   2. A Slack app belongs to exactly ONE TF org, across every workspace it's
--      installed in — NOT expressible as a SQL constraint on THIS table
--      (api_app_id is one half of the composite PK, not unique on its own;
--      two different workspace_id rows could otherwise carry the same
--      api_app_id under different orgs, and no constraint would notice). The
--      connect handler enforces it inside its transaction instead: it takes
--      a Postgres advisory lock keyed on api_app_id (ee/slack's
--      WorkspaceStore.LockApp) before checking whether any existing row
--      already carries that api_app_id under a different org. The lock is
--      the load-bearing part — without it, "check, then write" is a race:
--      two concurrent connects for the same api_app_id under different orgs
--      write different workspace_id rows, so they never collide on a unique
--      index, and each could observe "not bound yet" before either commits.
--      The advisory lock serializes every connect attempt for a given
--      api_app_id onto one transaction at a time, so the check a transaction
--      performs is guaranteed still true when it writes.
--
-- A second org's INSERT for an already-bound (workspace_id, api_app_id) pair
-- still hits the PK violation directly as a backstop — RLS never even sees
-- it (constraints are not subject to RLS) — so the handler maps the generic
-- unique-violation to a 409 WITHOUT naming the owning org: a workspace admin
-- may learn "already connected", never to whom. In practice the advisory-lock
-- protected check above fires first for any cross-org attempt; the PK only
-- matters for the same (workspace_id, api_app_id) pair racing itself, which
-- the lock already prevents from being a problem — it's defense in depth,
-- not the primary mechanism.
--
-- api_app_id is derived server-side at connect time (auth.test -> bot_id ->
-- bots.info -> app_id) — the admin never types it, same discipline as
-- workspace_id itself. See ee/slack's handleConnect.
--
-- transport is inferred at connect time from which credentials were
-- supplied (app-level token -> socket; signing secret -> events_api; both ->
-- the request must say which explicitly) and persisted here rather than
-- re-derived from the ref columns on every read. Exactly one of
-- signing_secret_ref/app_token_ref is expected to be NULL for the stored
-- transport, but that pairing is an application invariant enforced by
-- ee/slack, not a CHECK — a row may carry both refs (both credentials were
-- pasted) even though only one is the active transport.
--
-- bot_token_ref/signing_secret_ref/app_token_ref are org_secrets KEY NAMES
-- (see internal/integrations.SlackWorkspaceKeysFor), never the credential
-- values themselves — same discipline as org_github_apps/org_jira_apps. No
-- rotation machinery yet (TFAC-527, parked): re-submitting the connect form
-- upserts this row and overwrites the referenced secrets in place.
--
-- Postgres-only, same posture as sso_connections: local mode is N=1 with no
-- multi-workspace concept, so the SQLite store is a stub returning
-- ErrNotApplicableInLocal.

CREATE TABLE public.org_slack_workspaces (
    workspace_id text NOT NULL,
    api_app_id text NOT NULL,
    org_id uuid NOT NULL REFERENCES public.orgs(id) ON DELETE CASCADE,
    workspace_name text NOT NULL DEFAULT '',
    enterprise_id text,
    transport text NOT NULL CHECK (transport IN ('socket', 'events_api')),
    bot_user_id text NOT NULL DEFAULT '',
    bot_token_ref text NOT NULL,
    signing_secret_ref text,
    app_token_ref text,
    registered_by_user_id uuid REFERENCES public.users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, api_app_id)
);

-- Serves the per-org list/count reads (ListForOrg) and the FK's referencing
-- column, mirroring the index-the-FK-column convention used throughout.
CREATE INDEX org_slack_workspaces_org_idx ON public.org_slack_workspaces (org_id);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.org_slack_workspaces
    FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();

ALTER TABLE public.org_slack_workspaces ENABLE ROW LEVEL SECURITY;

-- SELECT is member-visible (mirrors org_jira_apps: any org member can see
-- what's connected, useful signal even for non-admins); writes are
-- org-admin-only (mirrors sso_connections/org_github_apps — connecting a
-- workspace is a sensitive workspace-wide credential gesture).
CREATE POLICY org_slack_workspaces_select ON public.org_slack_workspaces FOR SELECT TO tf_app
  USING ((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id));

CREATE POLICY org_slack_workspaces_insert ON public.org_slack_workspaces FOR INSERT TO tf_app
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY org_slack_workspaces_update ON public.org_slack_workspaces FOR UPDATE TO tf_app
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

CREATE POLICY org_slack_workspaces_delete ON public.org_slack_workspaces FOR DELETE TO tf_app
  USING ((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id));

-- supabase_admin's ALTER DEFAULT PRIVILEGES auto-grants public-schema tables
-- to anon/authenticated/service_role at CREATE time. Strip them so the table
-- is reachable only by tf_app (under RLS) and the admin pool (which owns it
-- as superuser and bypasses RLS for ListAllSystem) — same posture as
-- sso_connections/sso_domains.
REVOKE ALL ON public.org_slack_workspaces FROM PUBLIC;
REVOKE ALL ON public.org_slack_workspaces FROM anon, authenticated, service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.org_slack_workspaces TO tf_app;


-- Slack identity capture (TFAC-531): user_slack_identities auto-resolves the
-- human who @mentioned the bot to a TF user, so a later leave can grant that
-- user run-visibility (deep-link into the run) and audit rendering can
-- attribute the message. Capture infrastructure only — sender never affects
-- routing or ownership (the settled 3-axis model: visibility is
-- channel/team, acting identity is the bot, audit is per-message).
--
-- The mapping is the key, not the user. user_github_identities /
-- user_jira_identities are PK(user_id, host) because capture there is
-- self-initiated (a user binds their own PAT). Slack capture is inbound and
-- system-initiated (a mention arrives from an unknown sender), so the
-- natural key is (workspace_id, slack_user_id) with a NULLABLE user_id —
-- which doubles as the negative cache: an unmatched sender gets a NULL-user
-- row (source of last_attempt_at) so a repeat mention within the resolver's
-- retry TTL doesn't cost a users.info API call.
--
-- Resolution is a verified-email match against the auth-principal bridge
-- (public.user_identities), NOT a users.email column — none exists,
-- deliberately (see that table's doc above: email is per-login-identity and
-- multiplicity is load-bearing for account-linking). No email is stored
-- here either — the bridge already holds it; this table stores only the
-- Slack display name (audit rendering needs a name, not an email).
--
-- source records HOW the row was populated: 'auto_email' is this leaf's only
-- writer; 'oidc' is reserved for the future Sign-in-with-Slack follow-on (a
-- self-service bind).
--
-- Postgres-only, same posture as org_slack_workspaces: local mode is N=1
-- with no multi-workspace concept, so the SQLite store is a stub returning
-- ErrNotApplicableInLocal.
--
-- Deliberately keyed (workspace_id, slack_user_id) — NOT (workspace_id,
-- api_app_id, slack_user_id) — even though org_slack_workspaces itself is
-- keyed on the (workspace, app) composite bind. Identity is a fact about a
-- human in a workspace, not about an app: two apps installed in
-- the same workspace (e.g. a prod TF org and an eval TF org each running
-- their own bot there) see the same humans, so sharing the mapping rows
-- across both is correct and halves users.info traffic rather than
-- resolving the same sender twice.
--
-- No FK to org_slack_workspaces: workspace_id alone is no longer a
-- unique/PK column there (the composite PK is (workspace_id, api_app_id),
-- and a workspace deliberately CAN carry many rows), so Postgres has no
-- single-column key left to reference. Consequently disconnecting the last
-- app in a workspace no longer cascade-deletes that workspace's identity
-- rows; they go quietly stale (never looked up again for that workspace
-- unless some app reconnects there) rather than being cleaned up
-- automatically. That's an accepted tradeoff of the re-key, not a
-- regression in anything this leaf enforces — the rows carry no secret
-- material, and the reuse-across-orgs misattribution risk the old FK
-- guarded against is bounded by the same app-single-org invariant
-- org_slack_workspaces itself now enforces at the handler layer.
CREATE TABLE public.user_slack_identities (
    workspace_id text NOT NULL,
    slack_user_id text NOT NULL,
    user_id uuid REFERENCES public.users(id) ON DELETE CASCADE,
    slack_display_name text,
    source text NOT NULL DEFAULT 'auto_email',
    last_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, slack_user_id),
    CONSTRAINT user_slack_identities_source_check CHECK (source IN ('auto_email', 'oidc'))
);

-- Serves the future run-visibility grant's reverse lookup (which Slack
-- identities map to this TF user). Partial on NOT NULL: a NULL-user
-- negative-cache row is never looked up by user_id, and there are far more
-- of those than resolved rows in steady state.
CREATE INDEX user_slack_identities_user_idx ON public.user_slack_identities (user_id) WHERE user_id IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.user_slack_identities
    FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();

ALTER TABLE public.user_slack_identities ENABLE ROW LEVEL SECURITY;

-- Self-only read/write, mirroring user_github_identities_modify /
-- user_jira_identities_modify verbatim. Consequence (intentional): a
-- NULL-user negative-cache row matches no current_user_id(), so it is
-- invisible and unwritable on the app pool — correct, since negative-cache
-- rows are system-only. All resolution writes (UpsertResolvedSystem,
-- MarkAttemptSystem) go through the admin pool for the same reason: capture
-- is system-initiated, with no claims context. The future Sign-in-with-Slack
-- OIDC leaf can write its own self-row through this app-pool policy.
CREATE POLICY user_slack_identities_modify ON public.user_slack_identities USING ((user_id = tf.current_user_id())) WITH CHECK ((user_id = tf.current_user_id()));

CREATE POLICY user_slack_identities_select ON public.user_slack_identities FOR SELECT USING ((user_id = tf.current_user_id()));

-- supabase_admin's ALTER DEFAULT PRIVILEGES auto-grants public-schema tables
-- to anon/authenticated/service_role at CREATE time. Strip them so the table
-- is reachable only by tf_app (under RLS) and the admin pool (which bypasses
-- RLS for the System writes) — same posture as org_slack_workspaces.
REVOKE ALL ON public.user_slack_identities FROM PUBLIC;
REVOKE ALL ON public.user_slack_identities FROM anon, authenticated, service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.user_slack_identities TO tf_app;


-- Slack event dedup: slack_event_deliveries — the cross-transport delivery
-- dedup table. Slack retries deliveries
-- (webhook X-Slack-Retry-Num; Socket Mode redelivers unacked envelopes), and
-- the two transports (this leaf's Events API receiver, the next leaf's
-- Socket Mode client) can only share dedup state through storage — an
-- in-process map wouldn't survive a restart or span two transports.
--
-- Keyed (api_app_id, event_id), not workspace_id or org_id: delivery streams
-- are honestly per-app, not per-workspace — a Socket Mode connection belongs
-- to an app, and Slack load-balances an app's event envelopes across all of
-- that app's open sockets regardless of which workspace(s) it's installed
-- in. Keying dedup on workspace_id alone would let two different apps
-- sharing a workspace collide on the same event_id. The
-- pre-auth webhook receiver resolves org_id from org_slack_workspaces (the
-- (workspace_id, api_app_id) -> org bind), and the entity key
-- (domain.SlackSourceID) deliberately excludes workspace/app context too —
-- dedup mirrors that, staying keyed on the same natural identifier Slack
-- itself hands the receiver in every envelope.
--
-- Admin-pool-only / system table, same posture as auth_events / user_identities
-- (the established system-only-table pattern): RLS enabled with NO policy
-- (deny-by-default to non-BYPASSRLS roles) + REVOKE ALL from PUBLIC + the app
-- roles. The superuser/admin pool bypasses RLS and does all I/O
-- (ee/slack/store's Deliveries.MarkDeliveredSystem) — the receiver has no
-- request claims to gate an org-scoped policy against, and there is no
-- app-pool caller.
--
-- No index beyond the primary key: mention volume is low, and the
-- opportunistic prune (received_at < now() - 72h, piggybacked on every
-- insert) keeps the table small enough that a sequential scan is cheap.
CREATE TABLE public.slack_event_deliveries (
    api_app_id text NOT NULL,
    event_id text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (api_app_id, event_id)
);

ALTER TABLE public.slack_event_deliveries ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.slack_event_deliveries FROM PUBLIC;
REVOKE ALL ON public.slack_event_deliveries FROM anon, authenticated, service_role;


-- Slack channel registry (TFAC-541): slack_channels — org-wide "channels we
-- know of." Applies the entities pattern (durable "what exists" facts vs.
-- team "who cares" choices) to Slack channels: this table is the registry
-- (what exists), team_slack_channels below is the tracking (who cares).
-- Powers the discovery/claim UX — a mention in an untracked channel becomes
-- a visible "#eng-alerts — unclaimed" row instead of a silent drop — and
-- caches display names, since Slack event payloads carry only the channel
-- ID and resolving one costs an API call that needs a home rather than
-- being repeated on every render.
--
-- org_id is part of the PRIMARY KEY, not a plain column with a
-- channel_id-only unique index: channel IDs (C.../G...) are globally
-- unique in Slack's model and stable across Enterprise Grid / Slack
-- Connect shared channels (the same property domain.SlackSourceID leans
-- on), so the SAME channel can legitimately be seen by two different TF
-- orgs each running their own app in a shared/Connect channel (the
-- two-orgs/two-apps/one-workspace scenario TFAC-533 re-keyed
-- org_slack_workspaces for). Each org gets its own registry row for that
-- channel, never a shared one.
--
-- workspace_id is the workspace we last saw the channel through — context
-- for which bot token a name-resolution call would use, not a foreign key
-- (a channel can be seen through more than one workspace over an app's
-- lifetime, e.g. a Connect channel). name = '' means unresolved; render
-- the raw channel_id, never invent a display name.
--
-- Postgres-only, same posture as every other Slack table: local mode is
-- N=1 with no multi-workspace concept, so the SQLite store is a stub
-- returning ErrNotApplicableInLocal.
CREATE TABLE public.slack_channels (
    org_id uuid NOT NULL REFERENCES public.orgs(id) ON DELETE CASCADE,
    channel_id text NOT NULL,
    workspace_id text NOT NULL,
    name text NOT NULL DEFAULT '',
    name_resolved_at timestamptz,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_mention_at timestamptz,
    PRIMARY KEY (org_id, channel_id),
    CONSTRAINT sc_channel_populated CHECK (channel_id <> '')
);

ALTER TABLE public.slack_channels ENABLE ROW LEVEL SECURITY;

-- Member-read, system-write: any org member can see what channels TF knows
-- about, mirroring org_slack_workspaces_select's member-visible read. There
-- is NO app-pool insert/update/delete policy at all — every write is a
-- `...System` method on the admin pool (ingest sightings, name resolution,
-- the channels API's ensure — a sibling leaf).
CREATE POLICY slack_channels_select ON public.slack_channels FOR SELECT TO tf_app
  USING ((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id));

-- supabase_admin's ALTER DEFAULT PRIVILEGES auto-grants public-schema tables
-- to anon/authenticated/service_role at CREATE time. Strip them so the table
-- is reachable only by tf_app (SELECT-only, under RLS) and the admin pool
-- (which bypasses RLS for every System write) — same posture as the other
-- Slack tables, minus the write grants: there is no app-pool write path.
REVOKE ALL ON public.slack_channels FROM PUBLIC;
REVOKE ALL ON public.slack_channels FROM anon, authenticated, service_role;
GRANT SELECT ON public.slack_channels TO tf_app;


-- Slack channel tracking (TFAC-541): team_slack_channels — the team<->
-- channel bind, the tracking half of the registry/tracking split above.
-- The stage-1 scope gate for slack:mention routing (a sibling leaf) and the
-- source of a channel's primary owning team.
--
-- Deliberate deviation from team_github_repos (which carries no org_id —
-- org rides the teams FK): this table denormalizes org_id specifically so
-- the one-primary-per-channel invariant below can be a partial unique
-- index over (org_id, channel_id) rather than channel_id alone. Channel
-- IDs are global (see slack_channels above), so without the org column the
-- index would wrongly forbid two DIFFERENT orgs from each having their own
-- primary tracker on the same Connect channel. Resist "simplifying" this
-- column away — it is load-bearing for the invariant, not redundant with
-- the teams FK.
--
-- No FK to slack_channels: a team may track a channel TF has never seen a
-- mention in (the TF-first picker flow, a sibling leaf) — registry rows
-- are upserted opportunistically elsewhere and must never gate a tracking
-- write.
--
-- is_primary is never written from the app pool. RLS cannot gate a single
-- column, so this is purely a store-layer contract: the app-pool
-- ReplaceForTeam always inserts is_primary = false and leaves existing
-- rows' is_primary untouched; promotion, succession, and admin
-- reassignment happen ONLY through the admin-pool
-- ReconcilePrimariesSystem / SetPrimarySystem store methods.
CREATE TABLE public.team_slack_channels (
    org_id uuid NOT NULL REFERENCES public.orgs(id) ON DELETE CASCADE,
    team_id uuid NOT NULL REFERENCES public.teams(id) ON DELETE CASCADE,
    channel_id text NOT NULL,
    is_primary boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, channel_id),
    CONSTRAINT tsc_channel_populated CHECK (channel_id <> '')
);

-- The one-primary-per-channel invariant: at most one tracking row per
-- (org_id, channel_id) may have is_primary = true. See the table comment
-- for why org_id (not channel_id alone) is the partial index's scope.
CREATE UNIQUE INDEX team_slack_channels_one_primary
    ON public.team_slack_channels (org_id, channel_id) WHERE is_primary;

-- Serves the cross-team reads (PrimaryTeamForChannelSystem,
-- ListTrackersForOrgSystem, TracksChannelSystem, ReconcilePrimariesSystem)
-- that look up every tracker of a channel rather than one team's rows.
CREATE INDEX team_slack_channels_channel_idx
    ON public.team_slack_channels (org_id, channel_id);

ALTER TABLE public.team_slack_channels ENABLE ROW LEVEL SECURITY;

-- Mirrors team_github_repos_select exactly (see team_github_repos above),
-- with the org guard added since the column exists here: any team member
-- can see their team's tracked channels.
CREATE POLICY team_slack_channels_select ON public.team_slack_channels FOR SELECT TO tf_app
  USING ((org_id = tf.current_org_id()) AND tf.team_in_current_org(team_id) AND (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.team_id = team_slack_channels.team_id) AND (m.user_id = tf.current_user_id())))));

CREATE POLICY team_slack_channels_insert ON public.team_slack_channels FOR INSERT TO tf_app
  WITH CHECK ((org_id = tf.current_org_id()) AND tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id));

CREATE POLICY team_slack_channels_delete ON public.team_slack_channels FOR DELETE TO tf_app
  USING ((org_id = tf.current_org_id()) AND tf.team_in_current_org(team_id) AND tf.user_is_team_admin(team_id));

-- Deliberately no UPDATE policy/grant, unlike team_github_repos (which
-- carries one nobody uses either). This table's only mutable column is
-- is_primary, and is_primary is never app-pool-writable BY DESIGN (see the
-- table comment) — RLS cannot gate a single column, so a team-admin-scoped
-- UPDATE policy would be a live, unused path to flip it that a future bug
-- could walk through. The app pool only ever needs INSERT (new tracking
-- rows) and DELETE (untracking); every legitimate is_primary transition
-- goes through the admin-pool ReconcilePrimariesSystem / SetPrimarySystem.
REVOKE ALL ON public.team_slack_channels FROM PUBLIC;
REVOKE ALL ON public.team_slack_channels FROM anon, authenticated, service_role;
GRANT SELECT, INSERT, DELETE ON public.team_slack_channels TO tf_app;


-- === Marketplace V1 (TFAC-535) ===========================================
-- Within-org publish/copy for prompts + blueprints. Foundation schema only —
-- publish/browse/install flows land in TFAC-536/537/538. Copy-on-publish
-- throughout: a listing holds an immutable, self-contained snapshot
-- (marketplace_listing_versions.snapshot); nothing here FKs into
-- prompts/blueprints, and nothing a consumer copies FKs back to a listing.
-- That self-containment is deliberate cross-org insurance per the TFAC-92
-- epic's scoping decision 5. Self-contained block appended at the end of Up,
-- mirroring the org_template_* block: every FK target (orgs, teams, users,
-- events_catalog) already exists.
--
-- Multi-mode only (house pattern — see the Slack tables / org_invites
-- precedent): every marketplace table lives here, in the Postgres baseline,
-- only. There is no SQLite counterpart; the SQLite store wires a stub
-- (internal/db/sqlite/marketplace_store.go) returning ErrNotApplicableInLocal
-- from every method, matching internal/db/sqlite/invites.go. prompts and
-- blueprints stay untouched in both dialects — provenance rides on
-- marketplace_installs.root_object_id (listing → copy linkage), not a
-- column on the copy itself.
--
-- This is the first org-wide read path for prompt-shaped content: unlike
-- prompts/blueprints (SELECT stays creator-or-same-team), marketplace_listings
-- SELECT is every org member for published rows — the whole point of a
-- within-org marketplace. Plain uuid PK on marketplace_listings (no
-- composite org_id key): child tables FK directly to marketplace_listings(id)
-- and additionally carry a denormalized org_id column (house pattern, cf.
-- blueprint_steps.team_id) so their own RLS policies stay a cheap column
-- comparison instead of a join into the parent for the org check.
--
-- No denormalized counters: install_count / vote_count are COUNT(*) joins at
-- read time (org-scale N is small) — see idx_marketplace_installs_listing /
-- the votes PK below for the indexes that join backs. This avoids the RLS
-- trap where a consumer (who can't UPDATE another team's listing row) would
-- need a system-context write just to bump a counter — installs/votes are
-- plain INSERTs into tables the consumer is allowed to write.

CREATE TABLE public.marketplace_listings (
    id                uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id            uuid NOT NULL,
    -- scope/status/kind are app-validated open sets, NO CHECK constraints —
    -- same reasoning as prompts.source: 'global' scope and moderation
    -- statuses (e.g. 'pending_review') must be addable later with zero DDL.
    scope             text DEFAULT 'org'::text NOT NULL,        -- 'org' now; 'global' in cross-org phase
    kind              text NOT NULL,                             -- 'prompt' | 'blueprint'
    status            text DEFAULT 'published'::text NOT NULL,   -- 'published' | 'delisted'
    name              text NOT NULL,
    description       text DEFAULT ''::text NOT NULL,
    publisher_team_id uuid,             -- listing outlives its publishing team (ON DELETE SET NULL below)
    creator_user_id   uuid,             -- listing outlives its creator (ON DELETE SET NULL below)
    -- team-side blueprint/prompt id; NO FK (listing survives source
    -- deletion; used for republish lookup via GetActiveBySource).
    source_id         uuid,
    current_version   integer DEFAULT 1 NOT NULL,
    created_at        timestamp with time zone DEFAULT now() NOT NULL,
    updated_at        timestamp with time zone DEFAULT now() NOT NULL,
    delisted_at       timestamp with time zone
);

-- PK must land before the child CREATE TABLEs below: their inline
-- REFERENCES public.marketplace_listings(id) clauses need a unique
-- constraint on the target column to already exist when each CREATE TABLE
-- statement runs (goose replays this file top-to-bottom as a single
-- transaction, one statement at a time).
ALTER TABLE ONLY public.marketplace_listings
    ADD CONSTRAINT marketplace_listings_pkey PRIMARY KEY (id);

CREATE TABLE public.marketplace_listing_versions (
    listing_id      uuid NOT NULL REFERENCES public.marketplace_listings(id) ON DELETE CASCADE,
    org_id          uuid NOT NULL,      -- denormalized for RLS, house pattern (cf. blueprint_steps.team_id)
    version         integer NOT NULL,
    snapshot        jsonb NOT NULL,     -- domain.ListingSnapshot, immutable once written
    creator_user_id uuid,
    created_at      timestamp with time zone DEFAULT now() NOT NULL
);

-- Faceting: which event types this listing targets. Current/mutable browse
-- metadata (auto-suggested at publish time from the attached trigger's event
-- type, editable on republish) — distinct from the frozen
-- ListingSnapshot.EventTypes carried inside a specific version's snapshot.
CREATE TABLE public.marketplace_listing_events (
    listing_id uuid NOT NULL REFERENCES public.marketplace_listings(id) ON DELETE CASCADE,
    org_id     uuid NOT NULL,
    event_type text NOT NULL
);

-- One 'recommend' vote per user per listing (TFAC-92 scoping decision 2: no
-- 5-star, votes + installs only).
CREATE TABLE public.marketplace_votes (
    listing_id uuid NOT NULL REFERENCES public.marketplace_listings(id) ON DELETE CASCADE,
    org_id     uuid NOT NULL,
    user_id    uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

-- Audit + install counts + provenance + substrate for the run-derived stats
-- fast-follow (TFAC-540). Plain PK(id) like artifacts/system_llm_runs — no
-- child table needs a composite FK into this one. root_object_id (the
-- created copy — a blueprint id for kind=blueprint, a prompt id for
-- kind=prompt) is deliberately NOT an FK: install history must survive the
-- copy's later deletion, exactly like source_id survives the source's.
CREATE TABLE public.marketplace_installs (
    id             uuid DEFAULT gen_random_uuid() NOT NULL,
    listing_id     uuid NOT NULL REFERENCES public.marketplace_listings(id) ON DELETE CASCADE,
    org_id         uuid NOT NULL,
    version        integer NOT NULL,
    team_id        uuid NOT NULL,           -- installing team
    user_id        uuid,
    root_object_id uuid,
    created_at     timestamp with time zone DEFAULT now() NOT NULL
);

-- One active listing per source object: a team republishing the same
-- prompt/blueprint reuses its existing listing rather than minting a
-- duplicate (GetActiveBySource resolves this for the publish flow).
CREATE UNIQUE INDEX marketplace_listings_source_active ON public.marketplace_listings USING btree (org_id, source_id) WHERE ((status = 'published'::text) AND (source_id IS NOT NULL));

ALTER TABLE ONLY public.marketplace_listing_versions
    ADD CONSTRAINT marketplace_listing_versions_pkey PRIMARY KEY (listing_id, version);

ALTER TABLE ONLY public.marketplace_listing_events
    ADD CONSTRAINT marketplace_listing_events_pkey PRIMARY KEY (listing_id, event_type);

ALTER TABLE ONLY public.marketplace_votes
    ADD CONSTRAINT marketplace_votes_pkey PRIMARY KEY (listing_id, user_id);

ALTER TABLE ONLY public.marketplace_installs
    ADD CONSTRAINT marketplace_installs_pkey PRIMARY KEY (id);

CREATE INDEX idx_marketplace_installs_listing ON public.marketplace_installs USING btree (listing_id, org_id);

ALTER TABLE ONLY public.marketplace_listings
    ADD CONSTRAINT marketplace_listings_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_listings
    ADD CONSTRAINT marketplace_listings_publisher_team_id_fkey FOREIGN KEY (publisher_team_id) REFERENCES public.teams(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.marketplace_listings
    ADD CONSTRAINT marketplace_listings_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.marketplace_listing_versions
    ADD CONSTRAINT marketplace_listing_versions_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.marketplace_listing_events
    ADD CONSTRAINT marketplace_listing_events_event_type_fkey FOREIGN KEY (event_type) REFERENCES public.events_catalog(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.marketplace_votes
    ADD CONSTRAINT marketplace_votes_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.marketplace_installs
    ADD CONSTRAINT marketplace_installs_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_installs
    ADD CONSTRAINT marketplace_installs_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.marketplace_listings FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();

ALTER TABLE public.marketplace_listings ENABLE ROW LEVEL SECURITY;

-- Every org member browses published listings; the publisher team also sees
-- its own delisted ones (to relist/manage).
CREATE POLICY marketplace_listings_select ON public.marketplace_listings FOR SELECT
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)
            AND ((status = 'published'::text) OR tf.user_can_write_team(publisher_team_id))));
CREATE POLICY marketplace_listings_insert ON public.marketplace_listings FOR INSERT
    WITH CHECK (((org_id = tf.current_org_id()) AND (creator_user_id = tf.current_user_id()) AND tf.user_can_write_team(publisher_team_id)));
CREATE POLICY marketplace_listings_update ON public.marketplace_listings FOR UPDATE
    USING (((org_id = tf.current_org_id()) AND tf.user_can_write_team(publisher_team_id)))
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_can_write_team(publisher_team_id)));
CREATE POLICY marketplace_listings_delete ON public.marketplace_listings FOR DELETE
    USING (((org_id = tf.current_org_id()) AND tf.user_can_write_team(publisher_team_id)));

ALTER TABLE public.marketplace_listing_versions ENABLE ROW LEVEL SECURITY;

-- SELECT mirrors listings (org member + published-or-publisher, via EXISTS on
-- the parent listing since this child table carries no status of its own);
-- INSERT gated by publisher-team write via the same EXISTS.
CREATE POLICY marketplace_listing_versions_select ON public.marketplace_listing_versions FOR SELECT
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)
            AND (EXISTS ( SELECT 1 FROM public.marketplace_listings l
                          WHERE (l.id = marketplace_listing_versions.listing_id) AND (l.org_id = marketplace_listing_versions.org_id)
                            AND ((l.status = 'published'::text) OR tf.user_can_write_team(l.publisher_team_id))))));
CREATE POLICY marketplace_listing_versions_insert ON public.marketplace_listing_versions FOR INSERT
    WITH CHECK (((org_id = tf.current_org_id())
                 AND (EXISTS ( SELECT 1 FROM public.marketplace_listings l
                              WHERE (l.id = marketplace_listing_versions.listing_id) AND (l.org_id = marketplace_listing_versions.org_id)
                                AND tf.user_can_write_team(l.publisher_team_id)))));

ALTER TABLE public.marketplace_listing_events ENABLE ROW LEVEL SECURITY;

CREATE POLICY marketplace_listing_events_select ON public.marketplace_listing_events FOR SELECT
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)
            AND (EXISTS ( SELECT 1 FROM public.marketplace_listings l
                          WHERE (l.id = marketplace_listing_events.listing_id) AND (l.org_id = marketplace_listing_events.org_id)
                            AND ((l.status = 'published'::text) OR tf.user_can_write_team(l.publisher_team_id))))));
CREATE POLICY marketplace_listing_events_insert ON public.marketplace_listing_events FOR INSERT
    WITH CHECK (((org_id = tf.current_org_id())
                 AND (EXISTS ( SELECT 1 FROM public.marketplace_listings l
                              WHERE (l.id = marketplace_listing_events.listing_id) AND (l.org_id = marketplace_listing_events.org_id)
                                AND tf.user_can_write_team(l.publisher_team_id)))));
-- DELETE mirrors INSERT: PublishVersion replaces the facet set under the
-- same publisher-team write gate.
CREATE POLICY marketplace_listing_events_delete ON public.marketplace_listing_events FOR DELETE
    USING (((org_id = tf.current_org_id())
            AND (EXISTS ( SELECT 1 FROM public.marketplace_listings l
                         WHERE (l.id = marketplace_listing_events.listing_id) AND (l.org_id = marketplace_listing_events.org_id)
                           AND tf.user_can_write_team(l.publisher_team_id)))));

ALTER TABLE public.marketplace_votes ENABLE ROW LEVEL SECURITY;

CREATE POLICY marketplace_votes_select ON public.marketplace_votes FOR SELECT
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));
CREATE POLICY marketplace_votes_insert ON public.marketplace_votes FOR INSERT
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (user_id = tf.current_user_id())));
CREATE POLICY marketplace_votes_delete ON public.marketplace_votes FOR DELETE
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (user_id = tf.current_user_id())));

ALTER TABLE public.marketplace_installs ENABLE ROW LEVEL SECURITY;

CREATE POLICY marketplace_installs_select ON public.marketplace_installs FOR SELECT
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));
-- Write role on the *installing* team, not the publishing team.
CREATE POLICY marketplace_installs_insert ON public.marketplace_installs FOR INSERT
    WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_can_write_team(team_id)));

GRANT ALL ON TABLE public.marketplace_listings TO postgres;
GRANT ALL ON TABLE public.marketplace_listings TO anon;
GRANT ALL ON TABLE public.marketplace_listings TO authenticated;
GRANT ALL ON TABLE public.marketplace_listings TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.marketplace_listings TO tf_app;

GRANT ALL ON TABLE public.marketplace_listing_versions TO postgres;
GRANT ALL ON TABLE public.marketplace_listing_versions TO anon;
GRANT ALL ON TABLE public.marketplace_listing_versions TO authenticated;
GRANT ALL ON TABLE public.marketplace_listing_versions TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.marketplace_listing_versions TO tf_app;

GRANT ALL ON TABLE public.marketplace_listing_events TO postgres;
GRANT ALL ON TABLE public.marketplace_listing_events TO anon;
GRANT ALL ON TABLE public.marketplace_listing_events TO authenticated;
GRANT ALL ON TABLE public.marketplace_listing_events TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.marketplace_listing_events TO tf_app;

GRANT ALL ON TABLE public.marketplace_votes TO postgres;
GRANT ALL ON TABLE public.marketplace_votes TO anon;
GRANT ALL ON TABLE public.marketplace_votes TO authenticated;
GRANT ALL ON TABLE public.marketplace_votes TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.marketplace_votes TO tf_app;

GRANT ALL ON TABLE public.marketplace_installs TO postgres;
GRANT ALL ON TABLE public.marketplace_installs TO anon;
GRANT ALL ON TABLE public.marketplace_installs TO authenticated;
GRANT ALL ON TABLE public.marketplace_installs TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.marketplace_installs TO tf_app;

-- === Run-derived listing stats (TFAC-540 fast-follow) ====================
-- Denormalized, system-computed aggregates across every copy of a listing —
-- the objective social-proof metric votes/installs can't fake: how much real
-- work listings' copies have actually done, and how well. Copy linkage is
-- marketplace_installs.root_object_id (TFAC-535: no provenance columns on
-- prompts/blueprints), joined to runs.prompt_id for kind=prompt listings and
-- blueprint_runs.blueprint_id for kind=blueprint listings.
--
-- Written exclusively by MarketplaceStore.RecomputeStatsSystem (admin pool,
-- bypasses RLS) — a cross-team, cross-run aggregate can't be computed at
-- request/read time without an RLS-crossing query, which the CLAUDE.md
-- multi-mode read-scoping standing rule forbids. Browse/detail reads join
-- this table like any other listing column, under ordinary RLS. No
-- INSERT/UPDATE/DELETE policy is declared below: only the admin pool ever
-- writes here (same shape as system_prompt_versions / events_catalog).
--
-- root_object_id persists on marketplace_installs after a copy is deleted
-- (TFAC-535), so total_runs counts historical runs from deleted copies too —
-- teams_using does not, since it additionally requires the copy to still
-- exist (prompts.deleted_at / blueprints.deleted_at IS NULL). This asymmetry
-- is deliberate: "how much work has this listing's lineage done, ever" vs.
-- "how many teams are using it right now."
CREATE TABLE public.marketplace_listing_stats (
    listing_id   uuid NOT NULL REFERENCES public.marketplace_listings(id) ON DELETE CASCADE,
    org_id       uuid NOT NULL,   -- denormalized for RLS, house pattern (cf. marketplace_listing_versions.org_id)
    teams_using  integer NOT NULL DEFAULT 0,
    total_runs   integer NOT NULL DEFAULT 0,
    -- NULL until total_runs > 0 — no wrong fallbacks (never a fake "0%
    -- success" for a listing with no run history yet).
    success_rate double precision,
    last_run_at  timestamp with time zone,
    computed_at  timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.marketplace_listing_stats
    ADD CONSTRAINT marketplace_listing_stats_pkey PRIMARY KEY (listing_id);

ALTER TABLE public.marketplace_listing_stats ENABLE ROW LEVEL SECURITY;

-- SELECT-only, mirroring marketplace_listings' org-member visibility (any
-- org member reads any listing's stats — the row itself carries no
-- publisher-team distinction to gate on; browse/detail already scope which
-- listings are visible before this table is ever joined).
CREATE POLICY marketplace_listing_stats_select ON public.marketplace_listing_stats FOR SELECT
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));

GRANT ALL ON TABLE public.marketplace_listing_stats TO postgres;
GRANT ALL ON TABLE public.marketplace_listing_stats TO anon;
GRANT ALL ON TABLE public.marketplace_listing_stats TO authenticated;
GRANT ALL ON TABLE public.marketplace_listing_stats TO service_role;
GRANT SELECT ON TABLE public.marketplace_listing_stats TO tf_app;


-- +goose Down
SELECT 'down not supported';
