-- +goose Up
-- Consolidated Postgres baseline for v1.11.0 (2026-05-13).
--
-- This file is mechanically regenerated from `pg_dump --schema-only -n public
-- -n tf` of all 14 prior Postgres migrations applied to a fresh supabase
-- testcontainer. It collapses the SKY-247 D3 + SKY-246 D2 + SKY-249 D6/D7/D9
-- migration history into a single fresh-install baseline.
--
-- Brick policy: pre-v1.11.0 Postgres installs are refused at boot. (No such
-- installs exist in the wild today — multi-mode hasn't shipped — but the
-- contract is kept consistent with the SQLite baseline so this stays the
-- canonical Postgres entry point.)
--
-- Future Postgres schema changes go in NEW NNN-numbered migration files in
-- this directory. NEVER edit this baseline. Down is a no-op.
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
    status text DEFAULT 'queued'::text NOT NULL,
    user_input text NOT NULL,
    error_msg text,
    cost_usd real DEFAULT 0 NOT NULL,
    duration_ms integer DEFAULT 0 NOT NULL,
    num_turns integer DEFAULT 0 NOT NULL,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
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
    system_slug text,
    name text,
    default_priority real,
    sort_order integer,
    prompt_id text,
    breaker_threshold integer,
    min_autonomy_suitability real,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT event_handlers_kind_check CHECK ((kind = ANY (ARRAY['rule'::text, 'trigger'::text]))),
    CONSTRAINT event_handlers_rule_shape CHECK (((kind <> 'rule'::text) OR ((prompt_id IS NULL) AND (breaker_threshold IS NULL) AND (min_autonomy_suitability IS NULL) AND (name IS NOT NULL) AND (default_priority IS NOT NULL) AND (sort_order IS NOT NULL)))),
    CONSTRAINT event_handlers_source_check CHECK ((source = ANY (ARRAY['system'::text, 'user'::text]))),
    CONSTRAINT event_handlers_system_has_no_creator CHECK ((((source = 'system'::text) AND (creator_user_id IS NULL)) OR ((source = 'user'::text) AND (creator_user_id IS NOT NULL)))),
    CONSTRAINT event_handlers_trigger_shape CHECK (((kind <> 'trigger'::text) OR ((prompt_id IS NOT NULL) AND (breaker_threshold IS NOT NULL) AND (min_autonomy_suitability IS NOT NULL) AND (default_priority IS NULL) AND (sort_order IS NULL) AND (name IS NULL))))
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
    -- Max Claude tier the org permits teams/users to pick. NULL means no
    -- cap (any tier allowed). The inheritance helper (sibling ticket) reads
    -- this to clamp team_settings.default_model and per-prompt overrides.
    max_llm_model_tier text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT org_settings_github_clone_protocol_check CHECK ((github_clone_protocol = ANY (ARRAY['https'::text, 'ssh'::text]))),
    CONSTRAINT org_settings_max_llm_model_tier_check CHECK ((max_llm_model_tier IS NULL OR max_llm_model_tier = ANY (ARRAY['haiku'::text, 'sonnet'::text, 'opus'::text])))
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
    sso_provider_id uuid,
    sso_enforced boolean DEFAULT false NOT NULL,
    -- SKY-345: distinguishes a user's auto-provisioned personal org
    -- (created on first signup under personal-org-on-signup policy)
    -- from explicit enterprise orgs. Future UI keys off this — "you
    -- can't delete your personal org", billing surfaces, picker
    -- filters. No UNIQUE constraint on (owner_user_id, is_personal):
    -- the data model permits a user to own a personal org and also
    -- create additional orgs. Race-safety on first-signup provisioning
    -- lives in the auth callback's pg_advisory_xact_lock + inside-tx
    -- zero-membership check, not at the schema level.
    is_personal boolean DEFAULT false NOT NULL,
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
-- Name: pending_prs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.pending_prs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    run_id uuid NOT NULL,
    owner text NOT NULL,
    repo text NOT NULL,
    head_branch text NOT NULL,
    head_sha text NOT NULL,
    base_branch text NOT NULL,
    title text NOT NULL,
    body text,
    original_title text,
    original_body text,
    locked boolean DEFAULT false NOT NULL,
    draft boolean DEFAULT false NOT NULL,
    submitted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: pending_review_comments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.pending_review_comments (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    review_id uuid NOT NULL,
    path text NOT NULL,
    line integer NOT NULL,
    start_line integer,
    body text NOT NULL,
    original_body text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    severity text DEFAULT ''::text NOT NULL
);


--
-- Name: pending_reviews; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.pending_reviews (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    pr_number integer NOT NULL,
    owner text NOT NULL,
    repo text NOT NULL,
    commit_sha text NOT NULL,
    diff_lines text,
    run_id uuid,
    review_body text,
    review_event text,
    original_review_body text,
    original_review_event text,
    diff_hunks text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


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
-- Name: preferences; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.preferences (
    user_id uuid NOT NULL,
    summary_md text,
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
    spec_authorship_prompt_id text,
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
    kind text DEFAULT 'leaf'::text NOT NULL,
    usage_count integer DEFAULT 0 NOT NULL,
    hidden boolean DEFAULT false NOT NULL,
    user_modified boolean DEFAULT false NOT NULL,
    allowed_tools text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    model text DEFAULT ''::text NOT NULL,
    system_slug text,
    CONSTRAINT prompts_source_check CHECK ((source = ANY (ARRAY['system'::text, 'user'::text, 'imported'::text]))),
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
-- Name: run_artifacts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.run_artifacts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    run_id uuid NOT NULL,
    kind text NOT NULL,
    url text,
    title text,
    metadata_json jsonb,
    is_primary boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: run_memory; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.run_memory (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    run_id uuid NOT NULL,
    entity_id uuid NOT NULL,
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
    feature_branch text NOT NULL,
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
    task_id uuid NOT NULL,
    prompt_id text NOT NULL,
    trigger_id uuid,
    trigger_type text DEFAULT 'manual'::text NOT NULL,
    status text DEFAULT 'cloning'::text NOT NULL,
    model text,
    session_id text,
    worktree_path text,
    result_summary text,
    stop_reason text,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    duration_ms integer,
    num_turns integer,
    total_cost_usd real,
    actor_agent_id uuid,
    chain_run_id uuid,
    chain_step_index integer,
    CONSTRAINT runs_creator_matches_trigger_type CHECK ((((trigger_type = 'manual'::text) AND (creator_user_id IS NOT NULL)) OR ((trigger_type = 'event'::text) AND (creator_user_id IS NULL)))),
    CONSTRAINT runs_team_visibility_requires_team CHECK (((visibility <> 'team'::text) OR (team_id IS NOT NULL))),
    CONSTRAINT runs_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'team'::text, 'org'::text])))
);


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
    team_id uuid NOT NULL,
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
    CONSTRAINT tasks_team_visibility_requires_team CHECK (((visibility <> 'team'::text) OR (team_id IS NOT NULL))),
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
    updated_at timestamp with time zone DEFAULT now() NOT NULL
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
    id uuid NOT NULL,
    display_name text,
    avatar_url text,
    timezone text DEFAULT 'UTC'::text NOT NULL,
    default_org_id uuid,
    last_acting_team_id uuid,
    jira_account_id text,
    jira_display_name text,
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
-- the last authenticated /user (or SCIM / login-claim) confirmation against
-- the host — the hook for future drift re-checks. An absent row is a durable,
-- supported state (the NULL-degrades-gracefully contract from SKY-264).
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
-- Name: pending_prs pending_prs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_prs
    ADD CONSTRAINT pending_prs_pkey PRIMARY KEY (id);


--
-- Name: pending_prs pending_prs_run_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_prs
    ADD CONSTRAINT pending_prs_run_id_key UNIQUE (run_id);


--
-- Name: pending_review_comments pending_review_comments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_review_comments
    ADD CONSTRAINT pending_review_comments_pkey PRIMARY KEY (id);


--
-- Name: pending_reviews pending_reviews_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_reviews
    ADD CONSTRAINT pending_reviews_id_org_id_key UNIQUE (id, org_id);


--
-- Name: pending_reviews pending_reviews_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_reviews
    ADD CONSTRAINT pending_reviews_pkey PRIMARY KEY (id);


--
-- Name: poller_state poller_state_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.poller_state
    ADD CONSTRAINT poller_state_pkey PRIMARY KEY (org_id, source, source_id);


--
-- Name: preferences preferences_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.preferences
    ADD CONSTRAINT preferences_pkey PRIMARY KEY (user_id);


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

-- Parent key for the same-team composite FKs (event_handlers.prompt_id,
-- prompt_chain_steps.chain_prompt_id / step_prompt_id) so a trigger / chain
-- step can only bind a prompt its own team owns (SKY-380).
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
-- Name: run_artifacts run_artifacts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_artifacts
    ADD CONSTRAINT run_artifacts_pkey PRIMARY KEY (id);


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
    ADD CONSTRAINT run_worktrees_pkey PRIMARY KEY (run_id, repo_id);


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
-- Name: idx_event_handlers_prompt; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_event_handlers_prompt ON public.event_handlers USING btree (org_id, prompt_id) WHERE (prompt_id IS NOT NULL);


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
-- Name: idx_pending_prs_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_pending_prs_run ON public.pending_prs USING btree (run_id);


--
-- Name: idx_pending_review_comments_review_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_pending_review_comments_review_id ON public.pending_review_comments USING btree (review_id);


--
-- Name: idx_repo_profiles_org_owner_repo; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_repo_profiles_org_owner_repo ON public.repo_profiles USING btree (org_id, owner, repo);


--
-- Name: idx_run_artifacts_kind_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_run_artifacts_kind_created ON public.run_artifacts USING btree (kind, created_at DESC);


--
-- Name: idx_run_artifacts_primary_per_run; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_run_artifacts_primary_per_run ON public.run_artifacts USING btree (run_id) WHERE (is_primary = true);


--
-- Name: idx_run_artifacts_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_run_artifacts_run ON public.run_artifacts USING btree (run_id);


--
-- Name: idx_run_memory_entity_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_run_memory_entity_created ON public.run_memory USING btree (entity_id, created_at);


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
-- Name: preferences set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.preferences FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


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


--
-- Name: event_handlers event_handlers_prompt_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_prompt_id_org_id_fkey FOREIGN KEY (prompt_id, org_id) REFERENCES public.prompts(id, org_id) ON DELETE CASCADE;


--
-- Name: event_handlers event_handlers_prompt_id_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

-- Same-team guard (SKY-380): a trigger may only bind a prompt its own team
-- owns. (prompt_id, team_id) -> prompts(id, team_id) refuses a handler whose
-- team_id doesn't match the referenced prompt's team. NULL prompt_id (rules)
-- skips the FK. CASCADE mirrors the org FK so a prompt delete still removes
-- its triggers.
ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_prompt_id_team_id_fkey FOREIGN KEY (prompt_id, team_id) REFERENCES public.prompts(id, team_id) ON DELETE CASCADE;


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
-- Name: pending_firings pending_firings_fired_run_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_fired_run_id_org_id_fkey FOREIGN KEY (fired_run_id, org_id) REFERENCES public.runs(id, org_id);


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
-- Name: pending_prs pending_prs_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_prs
    ADD CONSTRAINT pending_prs_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: pending_prs pending_prs_run_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_prs
    ADD CONSTRAINT pending_prs_run_id_org_id_fkey FOREIGN KEY (run_id, org_id) REFERENCES public.runs(id, org_id) ON DELETE CASCADE;


--
-- Name: pending_review_comments pending_review_comments_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_review_comments
    ADD CONSTRAINT pending_review_comments_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: pending_review_comments pending_review_comments_review_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_review_comments
    ADD CONSTRAINT pending_review_comments_review_id_org_id_fkey FOREIGN KEY (review_id, org_id) REFERENCES public.pending_reviews(id, org_id) ON DELETE CASCADE;


--
-- Name: pending_reviews pending_reviews_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_reviews
    ADD CONSTRAINT pending_reviews_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: pending_reviews pending_reviews_run_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_reviews
    ADD CONSTRAINT pending_reviews_run_id_org_id_fkey FOREIGN KEY (run_id, org_id) REFERENCES public.runs(id, org_id) ON DELETE SET NULL;


--
-- Name: poller_state poller_state_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.poller_state
    ADD CONSTRAINT poller_state_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: preferences preferences_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.preferences
    ADD CONSTRAINT preferences_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


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


--
-- Name: projects projects_spec_authorship_prompt_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_spec_authorship_prompt_id_org_id_fkey FOREIGN KEY (spec_authorship_prompt_id, org_id) REFERENCES public.prompts(id, org_id) ON DELETE SET NULL;


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
-- Name: run_artifacts run_artifacts_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_artifacts
    ADD CONSTRAINT run_artifacts_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: run_artifacts run_artifacts_run_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_artifacts
    ADD CONSTRAINT run_artifacts_run_id_org_id_fkey FOREIGN KEY (run_id, org_id) REFERENCES public.runs(id, org_id) ON DELETE CASCADE;


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
-- Name: users users_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_id_fkey FOREIGN KEY (id) REFERENCES auth.users(id) ON DELETE CASCADE;


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

CREATE POLICY event_handlers_delete ON public.event_handlers FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_in_team(team_id)));


--
-- Name: event_handlers event_handlers_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY event_handlers_insert ON public.event_handlers FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND tf.user_in_team(team_id)));


--
-- Name: event_handlers event_handlers_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY event_handlers_select ON public.event_handlers FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND ((creator_user_id = tf.current_user_id()) OR (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.user_id = tf.current_user_id()) AND (m.team_id = event_handlers.team_id)))))));


--
-- Name: event_handlers event_handlers_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY event_handlers_update ON public.event_handlers FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_in_team(team_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_in_team(team_id)));


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
-- Name: pending_prs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.pending_prs ENABLE ROW LEVEL SECURITY;

--
-- Name: pending_prs pending_prs_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY pending_prs_all ON public.pending_prs USING ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = pending_prs.run_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = pending_prs.run_id))));


--
-- Name: pending_review_comments; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.pending_review_comments ENABLE ROW LEVEL SECURITY;

--
-- Name: pending_review_comments pending_review_comments_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY pending_review_comments_all ON public.pending_review_comments USING ((EXISTS ( SELECT 1
   FROM public.pending_reviews pr
  WHERE (pr.id = pending_review_comments.review_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.pending_reviews pr
  WHERE (pr.id = pending_review_comments.review_id))));


--
-- Name: pending_reviews; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.pending_reviews ENABLE ROW LEVEL SECURITY;

--
-- Name: pending_reviews pending_reviews_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY pending_reviews_all ON public.pending_reviews USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND ((run_id IS NULL) OR (EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = pending_reviews.run_id)))))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND ((run_id IS NULL) OR (EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = pending_reviews.run_id))))));


--
-- Name: poller_state; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.poller_state ENABLE ROW LEVEL SECURITY;

--
-- Name: poller_state poller_state_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY poller_state_all ON public.poller_state USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: preferences; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.preferences ENABLE ROW LEVEL SECURITY;

--
-- Name: preferences preferences_modify; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY preferences_modify ON public.preferences USING ((user_id = tf.current_user_id())) WITH CHECK ((user_id = tf.current_user_id()));


--
-- Name: preferences preferences_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY preferences_select ON public.preferences FOR SELECT USING ((user_id = tf.current_user_id()));


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

CREATE POLICY projects_delete ON public.projects FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (team_id IS NOT NULL) AND tf.user_in_team(team_id)))));


--
-- Name: projects projects_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY projects_insert ON public.projects FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND ((visibility <> 'team'::text) OR ((team_id IS NOT NULL) AND tf.user_in_team(team_id))) AND ((visibility <> 'org'::text) OR tf.user_is_org_admin(org_id))));


--
-- Name: projects projects_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY projects_select ON public.projects FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND ((creator_user_id = tf.current_user_id()) OR ((visibility = 'team'::text) AND (team_id IS NOT NULL) AND (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.user_id = tf.current_user_id()) AND (m.team_id = projects.team_id))))) OR (visibility = 'org'::text))));


--
-- Name: projects projects_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY projects_update ON public.projects FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (team_id IS NOT NULL) AND tf.user_in_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id))))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (team_id IS NOT NULL) AND tf.user_in_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id)))));


--
-- Name: prompts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.prompts ENABLE ROW LEVEL SECURITY;

--
-- Name: prompts prompts_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY prompts_delete ON public.prompts FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_in_team(team_id)));


--
-- Name: prompts prompts_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY prompts_insert ON public.prompts FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND tf.user_in_team(team_id)));


--
-- Name: prompts prompts_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY prompts_select ON public.prompts FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND ((creator_user_id = tf.current_user_id()) OR (EXISTS ( SELECT 1
   FROM public.memberships m
  WHERE ((m.user_id = tf.current_user_id()) AND (m.team_id = prompts.team_id)))))));


--
-- Name: prompts prompts_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY prompts_update ON public.prompts FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_in_team(team_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND tf.user_in_team(team_id)));


--
-- Name: repo_profiles; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.repo_profiles ENABLE ROW LEVEL SECURITY;

--
-- Name: repo_profiles repo_profiles_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY repo_profiles_all ON public.repo_profiles USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: run_artifacts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.run_artifacts ENABLE ROW LEVEL SECURITY;

--
-- Name: run_artifacts run_artifacts_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY run_artifacts_all ON public.run_artifacts USING ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = run_artifacts.run_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.runs r
  WHERE (r.id = run_artifacts.run_id))));


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

CREATE POLICY runs_delete ON public.runs FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_in_team(team_id)))));


--
-- Name: runs runs_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY runs_insert ON public.runs FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND ((visibility <> 'team'::text) OR tf.user_in_team(team_id)) AND ((visibility <> 'org'::text) OR tf.user_is_org_admin(org_id))));


--
-- Name: runs runs_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY runs_select ON public.runs FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_in_team(team_id)) OR (visibility = 'org'::text))));


--
-- Name: runs runs_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY runs_update ON public.runs FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_in_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id))))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_in_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id)))));


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

CREATE POLICY tasks_delete ON public.tasks FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (((claimed_by_agent_id IS NULL) AND (claimed_by_user_id IS NULL) AND (EXISTS ( SELECT 1 FROM public.task_teams tt WHERE ((tt.task_id = tasks.id) AND tf.user_in_team(tt.team_id))))) OR tf.user_in_team(team_id))))));


--
-- Name: tasks tasks_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tasks_insert ON public.tasks FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND ((visibility <> 'team'::text) OR tf.user_in_team(team_id)) AND ((visibility <> 'org'::text) OR tf.user_is_org_admin(org_id))));


--
-- Name: tasks tasks_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tasks_select ON public.tasks FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (((claimed_by_agent_id IS NULL) AND (claimed_by_user_id IS NULL) AND (EXISTS ( SELECT 1 FROM public.task_teams tt WHERE ((tt.task_id = tasks.id) AND tf.user_in_team(tt.team_id))))) OR tf.user_in_team(team_id))) OR (visibility = 'org'::text))));


--
-- Name: tasks tasks_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tasks_update ON public.tasks FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (((claimed_by_agent_id IS NULL) AND (claimed_by_user_id IS NULL) AND (EXISTS ( SELECT 1 FROM public.task_teams tt WHERE ((tt.task_id = tasks.id) AND tf.user_in_team(tt.team_id))))) OR tf.user_in_team(team_id))) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id))))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND (((claimed_by_agent_id IS NULL) AND (claimed_by_user_id IS NULL) AND (EXISTS ( SELECT 1 FROM public.task_teams tt WHERE ((tt.task_id = tasks.id) AND tf.user_in_team(tt.team_id))))) OR tf.user_in_team(team_id))) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id)))));


--
-- Name: team_agents; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.team_agents ENABLE ROW LEVEL SECURITY;

--
-- Name: team_agents team_agents_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_agents_delete ON public.team_agents FOR DELETE USING ((tf.team_in_current_org(team_id) AND tf.user_in_team(team_id)));


--
-- Name: team_agents team_agents_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_agents_insert ON public.team_agents FOR INSERT WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_in_team(team_id) AND (EXISTS ( SELECT 1
   FROM public.agents a
  WHERE ((a.id = team_agents.agent_id) AND (a.org_id = tf.current_org_id()))))));


--
-- Name: team_agents team_agents_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_agents_select ON public.team_agents FOR SELECT TO tf_app USING ((tf.team_in_current_org(team_id) AND tf.user_in_team(team_id)));


--
-- Name: team_agents team_agents_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY team_agents_update ON public.team_agents FOR UPDATE USING ((tf.team_in_current_org(team_id) AND tf.user_in_team(team_id))) WITH CHECK ((tf.team_in_current_org(team_id) AND tf.user_in_team(team_id) AND (EXISTS ( SELECT 1
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

CREATE POLICY teams_update ON public.teams FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));


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
-- Name: TABLE pending_prs; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.pending_prs TO postgres;
GRANT ALL ON TABLE public.pending_prs TO anon;
GRANT ALL ON TABLE public.pending_prs TO authenticated;
GRANT ALL ON TABLE public.pending_prs TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.pending_prs TO tf_app;


--
-- Name: TABLE pending_review_comments; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.pending_review_comments TO postgres;
GRANT ALL ON TABLE public.pending_review_comments TO anon;
GRANT ALL ON TABLE public.pending_review_comments TO authenticated;
GRANT ALL ON TABLE public.pending_review_comments TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.pending_review_comments TO tf_app;


--
-- Name: TABLE pending_reviews; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.pending_reviews TO postgres;
GRANT ALL ON TABLE public.pending_reviews TO anon;
GRANT ALL ON TABLE public.pending_reviews TO authenticated;
GRANT ALL ON TABLE public.pending_reviews TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.pending_reviews TO tf_app;


--
-- Name: TABLE poller_state; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.poller_state TO postgres;
GRANT ALL ON TABLE public.poller_state TO anon;
GRANT ALL ON TABLE public.poller_state TO authenticated;
GRANT ALL ON TABLE public.poller_state TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.poller_state TO tf_app;


--
-- Name: TABLE preferences; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.preferences TO postgres;
GRANT ALL ON TABLE public.preferences TO anon;
GRANT ALL ON TABLE public.preferences TO authenticated;
GRANT ALL ON TABLE public.preferences TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.preferences TO tf_app;


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
-- Name: TABLE run_artifacts; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.run_artifacts TO postgres;
GRANT ALL ON TABLE public.run_artifacts TO anon;
GRANT ALL ON TABLE public.run_artifacts TO authenticated;
GRANT ALL ON TABLE public.run_artifacts TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.run_artifacts TO tf_app;


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



-- events_catalog seed (system-managed event type registry).
INSERT INTO events_catalog (id, source, category, label, description) VALUES
  ('github:pr:review_changes_requested', 'github', 'pr', 'Changes Requested',  'A reviewer requested changes on a PR'),
  ('github:pr:review_approved',          'github', 'pr', 'Review: Approved',   'A reviewer approved a PR'),
  ('github:pr:review_commented',         'github', 'pr', 'Review: Commented',  'A reviewer left non-blocking comments on a PR'),
  ('github:pr:review_dismissed',         'github', 'pr', 'Review: Dismissed',  'A reviewer dismissed their previous review on a PR'),
  ('github:pr:review_requested',         'github', 'pr', 'Review Requested',   'Someone requested your review on a PR'),
  ('github:pr:review_submitted',         'github', 'pr', 'Review Submitted',   'I reviewed someone else''s PR (inverse of review_*)'),
  ('github:pr:review_request_removed',   'github', 'pr', 'Review Request Removed', 'Your review request was removed from a PR (review completed or request rescinded)'),
  ('github:pr:ci_check_failed',          'github', 'pr', 'CI Check Failed',    'A CI check failed on a PR'),
  ('github:pr:ci_check_passed',          'github', 'pr', 'CI Check Passed',    'A CI check passed on a PR'),
  ('github:pr:label_added',              'github', 'pr', 'Label Added',        'A label was added to a PR'),
  ('github:pr:label_removed',            'github', 'pr', 'Label Removed',      'A label was removed from a PR'),
  ('github:pr:new_commits',              'github', 'pr', 'New Commits',        'A tracked PR has new commits since the last poll'),
  ('github:pr:conflicts',                'github', 'pr', 'Merge Conflicts',    'A PR has merge conflicts'),
  ('github:pr:ready_for_review',         'github', 'pr', 'Ready for Review',   'A draft PR was marked ready for review'),
  ('github:pr:mentioned',                'github', 'pr', 'Mentioned',          'You were @mentioned in a PR'),
  ('github:pr:opened',                   'github', 'pr', 'PR Opened',          'A pull request was opened'),
  ('github:pr:merged',                   'github', 'pr', 'PR Merged',          'A pull request was merged'),
  ('github:pr:closed',                   'github', 'pr', 'PR Closed',          'A pull request was closed without merging'),
  ('jira:issue:assigned',                'jira',   'issue', 'Issue Assigned',  'Issue was assigned to you'),
  ('jira:issue:available',               'jira',   'issue', 'Issue Available', 'New unassigned issue in pickup queue'),
  ('jira:issue:status_changed',          'jira',   'issue', 'Status Changed',  'Issue status changed (uses dedup_key=new_status)'),
  ('jira:issue:priority_changed',        'jira',   'issue', 'Priority Changed','Issue priority was changed (uses dedup_key=new_priority)'),
  ('jira:issue:commented',               'jira',   'issue', 'New Comment',     'A new comment was added to an issue'),
  ('jira:issue:completed',               'jira',   'issue', 'Issue Completed', 'Issue was marked as done'),
  ('jira:issue:became_atomic',           'jira',   'issue', 'Issue Became Atomic', 'Last open subtask closed — parent is now an atomic work unit'),
  ('system:poll:completed',              'system', 'poll', 'Poll Complete',    'A poller finished a cycle'),
  ('system:scoring:completed',           'system', 'scoring', 'Scoring Complete', 'AI scoring finished for a task'),
  ('system:delegation:completed',        'system', 'delegation', 'Delegation Complete', 'Agent delegation run completed'),
  ('system:delegation:failed',           'system', 'delegation', 'Delegation Failed',   'Agent delegation run failed'),
  ('system:prompt:auto_suspended',       'system', 'delegation', 'Prompt Auto-suspended', 'Per-(entity, prompt) breaker tripped after repeated failures'),
  ('system:task:delegation_blocked_by_subtasks', 'system', 'delegation', 'Delegation Blocked: Subtasks', 'Auto-delegation skipped because parent has open subtasks');


--
-- Prompt chains
--
-- Linear sequences of prompt steps that share one worktree. Each step
-- runs as a fresh Claude session in the same worktree; adjacent steps
-- communicate via a handoff file and record proceed/abort verdicts on
-- run_artifacts(kind='chain:verdict'). Per-step runtime state stays on
-- runs (linked via runs.chain_run_id); chain-wide abort/complete state
-- lives on chain_runs.
--
-- Multi-tenant pattern matches the rest of the baseline: composite FKs
-- against (id, org_id) on every parent ref, RLS gated on
-- tf.current_org_id() with EXISTS guards against same-id cross-tenant
-- access (prompts.id is text and can collide across orgs).
--

CREATE TABLE public.prompt_chain_steps (
    org_id uuid NOT NULL,
    team_id uuid NOT NULL,
    chain_prompt_id text NOT NULL,
    step_index integer NOT NULL,
    step_prompt_id text NOT NULL,
    brief text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.chain_runs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    -- creator_user_id is nullable for trigger_type='event' chains
    -- (system-emitted by the router via the admin pool); manual
    -- chains carry the human delegator. The matching
    -- chain_runs_creator_matches_trigger_type CHECK below pairs the
    -- two so the seeder can't drift. Mirrors the runs table's
    -- runs_creator_matches_trigger_type pattern.
    creator_user_id uuid,
    chain_prompt_id text NOT NULL,
    task_id uuid NOT NULL,
    trigger_type text NOT NULL,
    trigger_id uuid,
    status text DEFAULT 'running'::text NOT NULL,
    abort_reason text,
    aborted_at_step integer,
    worktree_path text NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT chain_runs_status_check CHECK ((status = ANY (ARRAY['running'::text, 'completed'::text, 'aborted'::text, 'failed'::text, 'cancelled'::text]))),
    CONSTRAINT chain_runs_creator_matches_trigger_type CHECK ((((trigger_type = 'manual'::text) AND (creator_user_id IS NOT NULL)) OR ((trigger_type = 'event'::text) AND (creator_user_id IS NULL))))
);

ALTER TABLE ONLY public.prompt_chain_steps
    ADD CONSTRAINT prompt_chain_steps_pkey PRIMARY KEY (org_id, chain_prompt_id, step_index);

ALTER TABLE ONLY public.chain_runs
    ADD CONSTRAINT chain_runs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.chain_runs
    ADD CONSTRAINT chain_runs_id_org_id_key UNIQUE (id, org_id);

CREATE INDEX idx_prompt_chain_steps_step_prompt ON public.prompt_chain_steps (step_prompt_id, org_id);
CREATE INDEX idx_chain_runs_task   ON public.chain_runs (task_id, org_id);
CREATE INDEX idx_chain_runs_status ON public.chain_runs (status) WHERE (status = 'running'::text);
CREATE INDEX idx_runs_chain        ON public.runs (chain_run_id, chain_step_index) WHERE (chain_run_id IS NOT NULL);

ALTER TABLE ONLY public.prompt_chain_steps
    ADD CONSTRAINT prompt_chain_steps_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.prompt_chain_steps
    ADD CONSTRAINT prompt_chain_steps_chain_prompt_fkey FOREIGN KEY (chain_prompt_id, org_id) REFERENCES public.prompts(id, org_id) ON DELETE CASCADE;
ALTER TABLE ONLY public.prompt_chain_steps
    ADD CONSTRAINT prompt_chain_steps_step_prompt_fkey  FOREIGN KEY (step_prompt_id,  org_id) REFERENCES public.prompts(id, org_id) ON DELETE RESTRICT;
-- Same-team guards (SKY-380): both the chain prompt and every step resolve
-- against prompts(id, team_id), so a chain can only step through prompts its
-- own team owns. The writer derives team_id from the chain prompt, making the
-- chain FK a tautology and the step FK the real cross-team refusal.
ALTER TABLE ONLY public.prompt_chain_steps
    ADD CONSTRAINT prompt_chain_steps_chain_prompt_team_fkey FOREIGN KEY (chain_prompt_id, team_id) REFERENCES public.prompts(id, team_id) ON DELETE CASCADE;
ALTER TABLE ONLY public.prompt_chain_steps
    ADD CONSTRAINT prompt_chain_steps_step_prompt_team_fkey  FOREIGN KEY (step_prompt_id,  team_id) REFERENCES public.prompts(id, team_id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.chain_runs
    ADD CONSTRAINT chain_runs_org_id_fkey          FOREIGN KEY (org_id)          REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.chain_runs
    ADD CONSTRAINT chain_runs_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.chain_runs
    ADD CONSTRAINT chain_runs_chain_prompt_fkey    FOREIGN KEY (chain_prompt_id, org_id) REFERENCES public.prompts(id, org_id);
ALTER TABLE ONLY public.chain_runs
    ADD CONSTRAINT chain_runs_task_fkey            FOREIGN KEY (task_id, org_id)         REFERENCES public.tasks(id, org_id);
ALTER TABLE ONLY public.chain_runs
    ADD CONSTRAINT chain_runs_trigger_fkey         FOREIGN KEY (trigger_id, org_id)      REFERENCES public.event_handlers(id, org_id);

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_chain_run_fkey FOREIGN KEY (chain_run_id, org_id) REFERENCES public.chain_runs(id, org_id);

ALTER TABLE public.prompt_chain_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.chain_runs         ENABLE ROW LEVEL SECURITY;

-- prompt_chain_steps inherits the chain prompt's access — if the
-- caller can't see the parent prompt, they can't see its step list.
-- prompts RLS already applies creator + team-membership rules.
-- The EXISTS subquery joins on p.org_id = prompt_chain_steps.org_id
-- because prompts.id is text and can collide across orgs.
CREATE POLICY prompt_chain_steps_all ON public.prompt_chain_steps FOR ALL
  USING      ((org_id = tf.current_org_id())
              AND (EXISTS (SELECT 1 FROM public.prompts p
                           WHERE p.id = prompt_chain_steps.chain_prompt_id
                             AND p.org_id = prompt_chain_steps.org_id)))
  WITH CHECK ((org_id = tf.current_org_id())
              AND (EXISTS (SELECT 1 FROM public.prompts p
                           WHERE p.id = prompt_chain_steps.chain_prompt_id
                             AND p.org_id = prompt_chain_steps.org_id)));

-- chain_runs are creator-scoped for manual chains and org-visible
-- for event-triggered chains. The chain_runs_creator_matches_trigger_type
-- CHECK pairs trigger_type with creator nullability: trigger_type='event'
-- rows have creator_user_id NULL (system-emitted via the admin pool by
-- the router / spawner). Those event rows would otherwise be unreadable
-- through the app pool, because the manual-only `creator_user_id =
-- tf.current_user_id()` predicate is unsatisfiable when the column is
-- NULL — leaving the chain detail handlers, GetRun, GetRunForRun, and
-- CancelChain unable to see system-emitted chains for any request user.
--
-- Policies are split per command so INSERT keeps the strict
-- creator-equals-caller WITH CHECK (the app pool must never be able to
-- write trigger_type='event' rows directly — that would let any org
-- member impersonate the router by inserting NULL-creator rows and
-- erase the audit boundary between system-emitted and human-delegated
-- chains). SELECT, UPDATE, DELETE widen to "creator IS NULL OR creator
-- = caller" so request handlers can read + cancel auto-fired chains.
-- Mirrors the runs table's per-command policy split.
CREATE POLICY chain_runs_select ON public.chain_runs FOR SELECT
  USING ((org_id = tf.current_org_id())
         AND tf.user_has_org_access(org_id)
         AND ((creator_user_id IS NULL)
              OR (creator_user_id = tf.current_user_id())));

CREATE POLICY chain_runs_insert ON public.chain_runs FOR INSERT
  WITH CHECK ((org_id = tf.current_org_id())
              AND tf.user_has_org_access(org_id)
              AND (creator_user_id = tf.current_user_id()));

CREATE POLICY chain_runs_update ON public.chain_runs FOR UPDATE
  USING ((org_id = tf.current_org_id())
         AND tf.user_has_org_access(org_id)
         AND ((creator_user_id IS NULL)
              OR (creator_user_id = tf.current_user_id())))
  WITH CHECK ((org_id = tf.current_org_id())
              AND tf.user_has_org_access(org_id)
              AND ((creator_user_id IS NULL)
                   OR (creator_user_id = tf.current_user_id())));

CREATE POLICY chain_runs_delete ON public.chain_runs FOR DELETE
  USING ((org_id = tf.current_org_id())
         AND tf.user_has_org_access(org_id)
         AND ((creator_user_id IS NULL)
              OR (creator_user_id = tf.current_user_id())));

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.prompt_chain_steps TO tf_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.chain_runs         TO tf_app;


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
    registered_at timestamp with time zone DEFAULT now() NOT NULL,
    registered_by_user_id uuid,
    active boolean DEFAULT true NOT NULL
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
    kind text DEFAULT 'leaf'::text NOT NULL,
    allowed_tools text DEFAULT ''::text NOT NULL,
    model text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT org_template_prompts_source_check CHECK ((source = ANY (ARRAY['system'::text, 'user'::text])))
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
    prompt_id text,
    breaker_threshold integer,
    min_autonomy_suitability real,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT org_template_handlers_kind_check CHECK ((kind = ANY (ARRAY['rule'::text, 'trigger'::text]))),
    CONSTRAINT org_template_handlers_source_check CHECK ((source = ANY (ARRAY['system'::text, 'user'::text]))),
    CONSTRAINT org_template_handlers_rule_shape CHECK (((kind <> 'rule'::text) OR ((prompt_id IS NULL) AND (breaker_threshold IS NULL) AND (min_autonomy_suitability IS NULL) AND (name IS NOT NULL) AND (default_priority IS NOT NULL) AND (sort_order IS NOT NULL)))),
    CONSTRAINT org_template_handlers_trigger_shape CHECK (((kind <> 'trigger'::text) OR ((prompt_id IS NOT NULL) AND (breaker_threshold IS NOT NULL) AND (min_autonomy_suitability IS NOT NULL) AND (default_priority IS NULL) AND (sort_order IS NULL) AND (name IS NULL))))
);

ALTER TABLE ONLY public.org_template_prompts
    ADD CONSTRAINT org_template_prompts_pkey PRIMARY KEY (org_id, id);
ALTER TABLE ONLY public.org_template_prompts
    ADD CONSTRAINT org_template_prompts_id_org_id_key UNIQUE (id, org_id);
ALTER TABLE ONLY public.org_template_prompts
    ADD CONSTRAINT org_template_prompts_org_slug_key UNIQUE (org_id, system_slug);

ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_pkey PRIMARY KEY (org_id, id);
ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_org_slug_key UNIQUE (org_id, system_slug);

CREATE INDEX idx_org_template_handlers_kind ON public.org_template_handlers USING btree (org_id, kind);

ALTER TABLE ONLY public.org_template_prompts
    ADD CONSTRAINT org_template_prompts_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_event_type_fkey FOREIGN KEY (event_type) REFERENCES public.events_catalog(id) ON DELETE RESTRICT;
-- A template trigger may only bind a template prompt in the same org; deleting
-- a template prompt cascades its triggers (mirrors the team-table CASCADE).
ALTER TABLE ONLY public.org_template_handlers
    ADD CONSTRAINT org_template_handlers_prompt_id_org_id_fkey FOREIGN KEY (prompt_id, org_id) REFERENCES public.org_template_prompts(id, org_id) ON DELETE CASCADE;

ALTER TABLE public.org_template_prompts ENABLE ROW LEVEL SECURITY;
CREATE POLICY org_template_prompts_all ON public.org_template_prompts
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

GRANT ALL ON TABLE public.org_template_handlers TO postgres;
GRANT ALL ON TABLE public.org_template_handlers TO anon;
GRANT ALL ON TABLE public.org_template_handlers TO authenticated;
GRANT ALL ON TABLE public.org_template_handlers TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.org_template_handlers TO tf_app;


-- +goose Down
SELECT 'down not supported';
