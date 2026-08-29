-- +goose Up
-- Consolidated Postgres baseline (2026-05-13).
--
-- This file is mechanically regenerated from `pg_dump --schema-only -n public
-- -n tf` of all 14 prior Postgres migrations applied to a fresh supabase
-- testcontainer. It collapses the prior multi-milestone
-- migration history into a single fresh-install baseline.
--
-- Brick policy: pre-baseline Postgres installs are refused at boot. (No such
-- installs exist in the wild today — multi-mode hasn't shipped — but the
-- contract is kept consistent with the SQLite baseline so this stays the
-- canonical Postgres entry point.)
--
-- Editing policy: multi mode has not shipped, so there is no Postgres install
-- anywhere to migrate and this baseline is MUTABLE. Schema changes are folded
-- into it in place rather than amended by a follow-up file — the data model is
-- still churning heavily, and one readable document beats a baseline plus a
-- stack of diffs that never gets re-collapsed. (The SQLite twin is the
-- opposite: local mode ships, so it takes forward migrations.) When multi mode
-- ships this file freezes and further changes go in NEW NNN-numbered migration
-- files in this directory. Down is a no-op.
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
--      SQLite leaves them bare TEXT (or NO-ACTION on conversations).
--      Deliberate: local mode is N=1 and the sentinel user is never deleted.
--      NARROW scope — this does NOT cover identity/ownership user_id PKs
--      (user_settings, user_github/jira_identities, memberships,
--      org_memberships), which carry the users(id) FK in BOTH dialects.
--   4. Tables that exist in one dialect only. instance_config is SQLite-only
--      (host port lives in container env under multi mode). sessions is
--      Postgres-only (multi-mode auth).
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
-- schemas. TF drops the vault extension below (secrets are app-encrypted,
-- not Vault-backed) and never creates pgsodium. gen_random_uuid() lives in
-- pg_catalog on PG 13+; no extension dependency required.

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

-- Least-privilege executor role. NOLOGIN like tf_app (the executor's
-- admin-pool DSN authenticates directly as tf_system — LOGIN is added
-- below, not on the role itself, mirroring how authenticator switches
-- into tf_app via SET LOCAL ROLE). BYPASSRLS reproduces today's `*System`
-- semantics (superuser RLS bypass) exactly; the enumerated GRANTs at the
-- end of this file — not per-table policies — are the actual security
-- boundary; bypass only matters on tables this role can reach at all.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tf_system') THEN
    CREATE ROLE tf_system NOLOGIN NOINHERIT BYPASSRLS;
  END IF;
END
$$;
-- +goose StatementEnd

-- The image pre-creates supabase_vault (and preloads pgsodium) before this
-- migration ever runs; TF doesn't use either, so drop the callable SQL
-- surface immediately. shared_preload_libraries still loads the underlying
-- C library into the postmaster (image-managed, outside this migration's
-- control), but no vault.* function remains reachable afterward.
DROP EXTENSION IF EXISTS supabase_vault CASCADE;

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

-- SECURITY DEFINER because the owner count must be read past RLS: on a
-- self-leave the caller's own membership row is already gone, so an INVOKER
-- read sees zero owners and raises a false 23514.
-- +goose StatementBegin
CREATE FUNCTION tf.guard_org_owners() RETURNS trigger
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
-- Name: blueprint_run_is_running(uuid, uuid); Type: FUNCTION; Schema: tf; Owner: -
--

-- The resume CAS (ConversationStore.MarkQueuedForResume) refuses to
-- un-terminal a `completed` conversation whose blueprint is still running:
-- that row's terminal is the reactor's input, moments from an advance or a
-- finalize. The check has to be IN the CAS statement — a caller reading the
-- blueprint separately races the very window the guard exists to close.
--
-- That statement runs on the app pool, and blueprint_runs_select is
-- creator-scoped for manual runs: a teammate who may legitimately update a
-- team-visible conversation cannot see its manual blueprint_runs row. A plain
-- correlated subquery would find nothing for that teammate and the guard would
-- fail OPEN — the one direction that reopens the failure. So this reads one
-- blueprint's running-ness past the creator scope — a within-org, non-security
-- boundary — while preserving
-- the ORG boundary: with request claims it requires p_org_id to equal the
-- caller's org; with no claims (admin-pool / system / test) the guard is
-- skipped. The pinned search_path blocks definer hijacking.
--
-- One bit, about a blueprint the caller's own conversation already names. A
-- NULL id (a conversation with no blueprint) and a row that does not exist
-- both answer false, so the CAS's NOT reads as "nothing says stop".
-- +goose StatementBegin
CREATE FUNCTION tf.blueprint_run_is_running(p_id uuid, p_org_id uuid) RETURNS boolean
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
  IF p_id IS NULL THEN
    RETURN false;
  END IF;
  IF tf.current_org_id() IS NOT NULL AND p_org_id <> tf.current_org_id() THEN
    RAISE EXCEPTION 'blueprint_run_is_running: requested org % does not match caller org %', p_org_id, tf.current_org_id();
  END IF;
  RETURN EXISTS (
    SELECT 1 FROM blueprint_runs br
    WHERE br.id = p_id AND br.org_id = p_org_id AND br.status = 'running'
  );
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
-- team-scoped write policy keyed on user_can_write_team (tasks, conversations,
-- prompts, blueprints, event_handlers, team_agents) — plus the
-- RequireTeamWrite / RequireTaskWrite handler gates that call it — reject the
-- write at the row level. This is the DB backstop that covers the task-scoped
-- mutations (swipe / snooze / requeue / advance) whose team is derived from the
-- task, not the URL. The team-settings family gates on user_is_team_admin
-- instead, so those handlers add the explicit authz.VerifyTeamNotArchived gate.
-- Archive itself stamps deleted_at via teams_update (org-admin) and reaps
-- conversations on the admin pool (BYPASSRLS), so neither path is blocked by
-- this filter.

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
    github_org_email text,
    jira_service_account_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);




--
-- Name: system_llm_runs; Type: TABLE; Schema: public; Owner: -
--

-- One row per agentproc.Run made by a headless system job (the scorer and
-- repo-profiler each run a Haiku call every poll cycle). Captures the
-- cost + token breakdown the subprocess already
-- computed so org spend reconciles with the Anthropic bill and a "system
-- overhead" line exists alongside conversation / claim spend rows.
-- Org-level, no team_id by design: scorer batches mix teams, and
-- repositories/entities carry no team. System-written (admin pool); the
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
    completed_at timestamp with time zone DEFAULT now() NOT NULL,
    -- trace_id (TFAC-579) is the agentproc invocation's Claude Code
    -- session id — the only value genuinely unique per Run call (the
    -- caller-supplied agentproc TraceID is a per-job-type label like
    -- "scorer-batch", reused across every invocation, so it can't serve as
    -- a key). Backs the idempotency constraint below: an accidental
    -- duplicate Record() for the same invocation (retry, double-call) is a
    -- no-op instead of double-counting spend. NULL when the subprocess
    -- never got far enough to mint a session.
    trace_id text
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
-- conversation_id is the producing conversation (the agent's, or the drafter's
-- for an approval, FK ON DELETE SET NULL so it outlives a conversation purge);
-- actor_user_id is the human authorizer/initiator (NULL for an autonomous
-- system write). dedup_key is the
-- natural per-action key — a branch push (the one true double-capture case: the
-- pre-push hook AND the git-proxy backstop both observe it) carries a
-- deterministic key so the twin collapses under ON CONFLICT DO NOTHING; every
-- other action gets a unique key, so DO NOTHING only ever collapses the twin.
-- One amendment to "immutable": the record of the act — target, action,
-- detail_json, credential, occurred_at, url as captured — is frozen, while
-- current_url, the POINTER to where the object lives now, is maintained (a
-- repository rename fills it; reads serve COALESCE(current_url, url)). It is
-- the only column an UPDATE may touch.
CREATE TABLE public.external_actions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    team_id uuid,
    provider text NOT NULL,
    action text NOT NULL,
    target text NOT NULL,
    external_id text,
    url text,
    current_url text,
    from_state text,
    to_state text,
    conversation_id uuid,
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
    owning_team_id uuid,
    last_polled_at timestamp with time zone,
    closed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    -- poll_seq (TFAC-579) backs a CAS on the tracker's snapshot write:
    -- UpdateSnapshotSystem was a blind UPDATE, so a straggler ex-leader
    -- (a poll cycle still in flight after a lease handoff) could overwrite
    -- a newer snapshot the current leader already wrote, losing a
    -- transition. Leadership (P1) is the primary guarantee; this CAS makes
    -- a late write from a stale reader a harmless no-op (poll_seq no
    -- longer matches) instead of a silent regression. Bumped by 1 on every
    -- successful snapshot write.
    poll_seq bigint DEFAULT 0 NOT NULL
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
    -- user_modified / deleted_at mirror prompts and blueprints: the
    -- shipped-content sync's "never clobber a user edit" signal, and a
    -- soft-delete that keeps (org_id, team_id, system_slug) occupied so sync
    -- never resurrects a deleted shipped row. No multi-mode deployment exists
    -- yet, so this lands in the baseline directly (the SQLite side gets a
    -- forward migration instead).
    user_modified boolean DEFAULT false NOT NULL,
    deleted_at timestamp with time zone,
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
    -- Members are jsonb arrays of {"id","name"} status refs, and each
    -- canonical is one such object. The id is the identity: a Jira
    -- workflow references the status entity, so an id survives a rename
    -- and a name does not — which is what keeps the discovery JQL and
    -- the claim/complete transitions matching. The name is the display
    -- snapshot taken when the rule was saved.
    pickup_members jsonb DEFAULT '[]'::jsonb NOT NULL,
    in_progress_members jsonb DEFAULT '[]'::jsonb NOT NULL,
    in_progress_canonical jsonb,
    -- in_review is the OPTIONAL fourth rule: the status a team names for work
    -- awaiting human review. No Jira status is ever read back into TF's
    -- in_review board column (that column is a fact about a run, not about a
    -- ticket), and these members reach neither the discovery JQL nor the stock
    -- deck — so an empty rule is a complete configuration, and a status may
    -- legitimately appear in both in_progress_members and in_review_members.
    in_review_members jsonb DEFAULT '[]'::jsonb NOT NULL,
    in_review_canonical jsonb,
    done_members jsonb DEFAULT '[]'::jsonb NOT NULL,
    done_canonical jsonb,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    -- Mirror of the SQLite CHECKs: a persisted row is complete-or-empty
    -- per rule. The row is the team's commitment to WATCH the project;
    -- whether it is ARMED (pickup plus the in-progress and done rules) is
    -- a separate question it may answer "not yet" to, so an unmapped
    -- project is storable and mapping it is the step after watching it.
    -- What stays bound is members-and-canonical on the three write-target
    -- rules — the canonical is the status TF transitions a ticket INTO, so
    -- members without one is a rule that cannot be executed. Pickup has no
    -- canonical column, so empty members is the whole of "unset" and it
    -- carries no constraint at all. The HTTP handler is the user-facing
    -- gate; these are defense-in-depth against any other write path
    -- (admin UI in multi mode, direct SQL, restore). "canonical is in
    -- members" stays in the HTTP validator because PG CHECK can't have
    -- subqueries.
    CONSTRAINT jpsr_in_progress_complete_or_empty CHECK (
        (jsonb_array_length(in_progress_members) = 0 AND in_progress_canonical IS NULL)
        OR (jsonb_array_length(in_progress_members) > 0 AND in_progress_canonical IS NOT NULL)
    ),
    CONSTRAINT jpsr_in_review_complete_or_empty CHECK (
        (jsonb_array_length(in_review_members) = 0 AND in_review_canonical IS NULL)
        OR (jsonb_array_length(in_review_members) > 0 AND in_review_canonical IS NOT NULL)
    ),
    CONSTRAINT jpsr_done_complete_or_empty CHECK (
        (jsonb_array_length(done_members) = 0 AND done_canonical IS NULL)
        OR (jsonb_array_length(done_members) > 0 AND done_canonical IS NOT NULL)
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

-- One row per (team, repository): a team declaring "I track this repo."
-- The GitHub *tracking-scope* twin of jira_project_status_rules and the
-- source of truth for which repos a team cares about. NOT the same as
-- team_github_groups above (CODEOWNERS review-routing teams) — this is
-- the tracking selection. No org_id column: org scope rides the teams FK,
-- mirroring jira_project_status_rules. Local mode (N=1) tracks every
-- configured repo on the single default team, so the router's team↔repo gate
-- never drops anything there.
--
-- The repository is referenced by the registry row's id, not by its slug. A
-- rename therefore moves one column on one row in `repositories` and every
-- tracking row here still resolves, untouched — which is the property the
-- previous (team_id, owner, repo) key could not have, since the slug is a
-- display handle GitHub lets you change.
--
-- The direction of the two lifetimes is the reason for the ON DELETE below.
-- A tracking row is a statement ABOUT a repository, so it cannot outlive the
-- registry row (CASCADE). The converse does not hold: untracking deletes a row
-- HERE and leaves the registry row standing, because a worktree ledger entry
-- or a task may still name that repository. Tracking is
-- forward-only in both directions.
--
-- org_id is denormalized from the team so both references can be COMPOSITE
-- foreign keys — see the constraints below. It is not a scoping convenience:
-- it is what makes "the team and the repository belong to the same org" a
-- thing the database checks rather than a thing each writer remembers. A
-- cross-tenant row is invisible to every org-scoped read, so the failure it
-- produces is a save that reports success and tracks nothing.
CREATE TABLE public.team_github_repos (
    team_id uuid NOT NULL,
    repository_id uuid NOT NULL,
    org_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
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
    -- github_base_url, github_poll_interval, jira_base_url and jira_poll_interval
    -- used to live here and have moved onto org_event_sources (base_url,
    -- poll_interval), keyed (org_id, kind) instead of one column pair per
    -- source — see that table's comment. github_clone_protocol stays: it is a
    -- GitHub-only concept, not a uniform per-source setting.
    --
    -- Defaults to https where the SQLite twin defaults to ssh, and the
    -- divergence is the point rather than drift: Postgres means multi mode,
    -- where an SSH clone has nothing to authenticate with — a GitHub App
    -- installation token is an HTTPS bearer credential, and the runtime
    -- container carries no key, agent or known_hosts. Provisioning names only
    -- org_id and takes every other column from its DEFAULT, so this literal is
    -- the value every multi org is actually born with; seeding ssh would put a
    -- value in the column that the settings PATCH beside it refuses to accept,
    -- safe to read only through domain.EffectiveCloneProtocol. The CHECK below
    -- still admits both — the column's vocabulary is the product's, not this
    -- deployment's.
    github_clone_protocol text DEFAULT 'https'::text NOT NULL,
    -- org_secrets key refs (not raw secrets) for Anthropic / Bedrock credentials.
    -- NULL means "use deployment default" on hosted SaaS or "not configured
    -- yet" on self-host. The SecretStore API resolves the ref to a live
    -- token at request time; rotation replaces the secret behind the ref
    -- without touching this row.
    anthropic_api_key_ref text,
    bedrock_credentials_ref text,
    -- Where the org's Claude credentials come from, stated rather than inferred
    -- from the two refs above being empty. 'system' is the host's credentials
    -- (TF supplies the subprocess nothing and the Claude Code SDK resolves auth
    -- from the inherited environment); 'byok' is the org's own bound material,
    -- whichever provider serves the model. The refs stay the record of WHICH
    -- providers are bound; this column answers what it means when none are —
    -- deliberately on the host's credentials, or not set up.
    --
    -- App-validated, not CHECK-constrained, the background_jobs_model
    -- precedent: the SQLite tree adds this column with ALTER TABLE, where
    -- widening a CHECK later means a full table rebuild, and a constraint that
    -- exists on one dialect only is worse than one on neither.
    --
    -- DEFAULT 'byok', and here that is not a default so much as the only value:
    -- a hosted deployment has no host credentials to lend — the operator's
    -- environment would be shared by every tenant, and credential resolution
    -- refuses an org with nothing bound outright — so the settings PATCH
    -- refuses 'system' in multi mode and domain.EffectiveLLMAuthMethod resolves
    -- to 'byok' whatever the column says. The SQLite tree defaults to 'system'
    -- instead (202608260003_org_llm_auth_method.sql): local mode is
    -- single-user and zero-configuration, so a fresh install arrives already on
    -- the host's credentials and is never asked to pick. Rolled into the
    -- baseline (not a forward migration) because multi-mode / Postgres is
    -- net-new and unshipped, so there are no existing rows to backfill.
    llm_auth_method text DEFAULT 'byok'::text NOT NULL,
    -- The org's enable-set: the model keys its teams may pick from, as a JSON
    -- array. This is the selection-time control — its whole effect is a smaller
    -- picker — and it says which models may be picked without claiming any is
    -- better than another, which is the claim a ranked ceiling has to make and
    -- the catalog declines to.
    --
    -- The keys are native wire ids, this dialect's own execution vocabulary, as
    -- default_model and background_jobs_model on these tables already are: a set
    -- names models the deployment will be asked to run, and a harness alias
    -- stored here would go on the bifrost wire unresolved.
    --
    -- NULL is the absent value and means the org has expressed no preference,
    -- which resolves to every model this deployment offers: a model a later
    -- release adds is enabled for that org the day it ships, while a stored set
    -- stays frozen at what it names. The RESOLVED set is never stored — "chose
    -- nothing" and "chose everything" are different facts about what happens
    -- next, and collapsing them at the column is what would make the first
    -- impossible to spell.
    --
    -- JSON text rather than a child table: nothing semi-joins through
    -- enablement, the catalog is code so there is no FK target, and the set is
    -- read whole at one choke point. App-validated against the build's catalog,
    -- not CHECK-constrained: the accepted set changes with the binary, not with
    -- the schema, and a CHECK would have to be migrated on both dialects for
    -- every model TF names.
    enabled_models text,
    -- The model the two headless system jobs — scorer, repo profiler —
    -- run on. One knob for both: they are the same kind of
    -- work (short, toolless, no transcript) bought from the same budget, so a
    -- per-job column would be two ways to answer one question.
    --
    -- A modelcatalog key, validated app-side against the catalog and against the
    -- providers the org has connected — not CHECK-constrained, the
    -- enabled_models precedent above: the accepted set is the build's own,
    -- which changes with the binary rather than with the schema.
    --
    -- NOT NULL with an EMPTY default, and the empty string is the load-bearing
    -- part: it means the org has not picked yet, and until it does those jobs
    -- skip their cycles. TF ships no fallback model, so a default here would be
    -- a model nobody chose being bought with somebody's money. A fresh org is
    -- therefore forced through the setup pick. The SQLite tree defaults to
    -- 'claude-haiku-4-5-20251001' instead (202608260001_org_background_jobs_model.sql):
    -- local mode is single-user and zero-configuration, with no setup step to
    -- make someone choose. Rolled into the baseline (not a forward migration)
    -- because multi-mode / Postgres is net-new and unshipped, so there are no
    -- existing rows to seed.
    background_jobs_model text DEFAULT ''::text NOT NULL,
    -- Org-wide daily LLM spend cap (TFAC-477). NULL = no cap; the app layer also
    -- treats 0 as "no cap". When the org's spend for the current UTC calendar day
    -- (summed across every category — autonomous + manual + system
    -- overhead) is at or above this value, the delegation choke point
    -- (Spawner.Delegate) refuses all new agent runs, a runaway-spend fuse. Mirrors
    -- the nullable enabled_models shape above; in-flight runs are unaffected
    -- and the read fails open on error so a transient failure can't wedge delegation.
    max_daily_cost_usd double precision,
    -- Per-org cap on how many runs the org may have executing concurrently
    -- across the whole executor fleet. NULL = unlimited (the app/claim also
    -- treats <= 0 as unlimited, mirroring max_daily_cost_usd's "0 = no cap").
    -- Enforced in ClaimNextRun: a queued run is invisible to claims while its
    -- org's active-run count (status='running') is at or
    -- above this value, so one tenant's burst can't monopolize the fleet's
    -- slots — the admission ceiling the daily *spend* cap can't express. Read
    -- live in the claim statement, so lowering it takes effect on the next
    -- claims with no restart; in-flight runs are never interrupted (the cap
    -- gates new claims only). Local mode / SQLite is N=1 and ignores it.
    max_concurrent_runs integer,
    -- Ship-dark org toggle for the within-org prompt marketplace (TFAC-535 /
    -- TFAC-92 scoping decision 4). Default false: rendered grayed out with
    -- "coming soon" in org settings until the TFAC-539 launch flip; UI +
    -- enforcement of this column are that ticket's concern, not this one's.
    marketplace_enabled boolean DEFAULT false NOT NULL,
    -- Which credential system this org's GitHub access belongs to: 'pat' or
    -- 'byo_app' today, 'managed_app' (the deployment's own shared App) reserved.
    --
    -- Stated rather than inferred, because "no org_github_apps row" cannot mean
    -- one thing: that table holds one row per org keyed on a UNIQUE app_id, so
    -- an org riding a deployment-level shared App has no row either, and every
    -- site reading a missing row as PAT would hand such an org a credential it
    -- does not have. The class names WHICH SYSTEM the org is in;
    -- org_github_apps.active names WHICH CREDENTIAL IS LIVE — orthogonal, and
    -- visibly so during the staged window of a PAT→App switch, where the class
    -- is already byo_app while the PAT is still the live credential.
    --
    -- App-validated, not CHECK-constrained, matching enabled_models above:
    -- admitting a new class must cost no DDL on either backend.
    --
    -- Not owned by the settings writer. OrgsStore.UpdateSettings deliberately
    -- omits this column from both halves of its upsert so a bulk settings save
    -- can never reset it; the credential transitions write it via
    -- SetGitHubCredentialClass inside their own transaction.
    github_credential_class text DEFAULT 'pat'::text NOT NULL,
    -- Optimistic-concurrency token for the settings save. That save is a
    -- read-modify-write over the whole row, so two admins editing different
    -- sections of the same page would otherwise overwrite each other silently —
    -- the later save carries the earlier's fields as its client last read them,
    -- and the first admin's change is undone with a 200 and nothing to notice.
    -- The read hands the client this token, the write requires it, and a write
    -- against a stale token is refused (409) so the loser refetches and
    -- re-applies. There is no server-side merge.
    --
    -- Owned by the settings writer alone: OrgsStore.UpdateSettings bumps it,
    -- SetGitHubCredentialClass — the surgical single-column write the
    -- credential transitions use — deliberately does not, so a credential
    -- transition never invalidates an admin's in-flight settings edit.
    --
    -- SCOPE: this token covers org_settings alone, not org_event_sources. A
    -- settings-page save (PATCH /api/orgs/{org_id}/settings) still writes that
    -- table's base_url/poll_interval columns for github and jira inside the
    -- SAME transaction as this row's guarded write, so a stale token still
    -- refuses the whole save with nothing written — ordinary transaction
    -- atomicity gives that for free, with no second token to invent. What the
    -- token does NOT cover is the admin-only per-source surface (PATCH
    -- /api/orgs/{org_id}/sources/{kind}, the org-admin disable switch): that
    -- route's writes are an unguarded, last-writer-wins upsert
    -- per (org_id, kind) row, the same concurrency shape `disabled` already
    -- shipped with. Extending this token across a second table keyed by a
    -- runtime-registry column (kind) would mean fabricating a whole-table
    -- version for a table whose whole point is that absence is meaningful and
    -- rows come and go per source — the smaller, more honest answer is to leave
    -- it scoped here and accept that the per-source route races the way
    -- `disabled` already does.
    version integer DEFAULT 1 NOT NULL,
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
    -- claimed_at stamps a claiming pop (status -> 'draining'); cleared on
    -- release. The drain sweeper requeues 'draining' rows whose claim is
    -- stale -- the crash recovery for a drainer that died mid-drain.
    claimed_at timestamp with time zone,
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
    -- deleted_at soft-deletes a prompt: the row + its conversations stay
    -- (conversations.prompt_id has no ON DELETE action, so a hard DELETE on a
    -- prompt with conversation history would error); request-facing reads
    -- filter deleted_at IS NULL, the ...System reads keep resolving it for
    -- in-flight conversations + past-conversation timelines. Load-bearing once
    -- every new prompt is auto-wrapped as a 1-step blueprint (the step FK is
    -- RESTRICT), so hard-delete is impossible.
    deleted_at timestamp with time zone,
    -- source is app-validated, not CHECK-constrained (source_check dropped in
    -- this baseline, both dialects). The system_has_no_creator pairing is
    -- the only source invariant the DB still enforces.
    CONSTRAINT prompts_system_has_no_creator CHECK ((((source = 'system'::text) AND (creator_user_id IS NULL)) OR ((source <> 'system'::text) AND (creator_user_id IS NOT NULL))))
);


--
-- Name: repositories; Type: TABLE; Schema: public; Owner: -
--

-- The registry of the repositories TF works with, and the target every
-- repository reference in this schema now points at: team_github_repos and
-- conversation_worktrees carry this row's id. The
-- slug survives only where it is genuinely something else's natural key
-- (entities.source_id is a pull request's, "owner/repo#18") or where a human
-- reads it (an API payload, a CLI argument, a directory name).
--
-- source + external_id are the half of a repository's identity a rename does
-- not move. Without them a renamed repository is indistinguishable, under
-- polling, from a 404 plus a brand-new one.
--
-- The row is durable. It is created when a repository is first tracked and it
-- is NOT deleted when the last team untracks it, because a worktree ledger
-- entry or a task may still name the repository. "Which
-- repositories does TF poll and profile" is a question about tracking, and
-- team_github_repos is what answers it.
--
-- source is app-validated, not CHECK-constrained — the convention this
-- baseline states for the other source columns (prompts.source above), so a
-- second provider costs a value rather than a schema change. external_id is
-- deliberately not named github_repo_id for the same reason:
-- (source, external_id) is the shape entities (source, source_id) already
-- uses. It is nullable and NULL is a supported state rather than a gap to
-- backfill — a row without an id behaves exactly as it does today,
-- everywhere — and it fills in from GitHub payloads TF already fetches, never
-- from a fetch added to obtain it.
CREATE TABLE public.repositories (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    owner text NOT NULL,
    repo text NOT NULL,
    source text DEFAULT 'github'::text NOT NULL,
    external_id text,
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

-- artifacts (TFAC-455): the single durable, conversation-attributed,
-- polymorphic record of everything a conversation produces in an external
-- system (a pushed branch, a draft/open PR, a draft/submitted review, a
-- Jira/Linear issue, a comment). One row per external object; provider +
-- kind discriminate the shape. All capture writers UPSERT on
-- (org_id, dedup_key) so the same logical object is one row. team_id is
-- denormalized from the conversation so reads scope by team exactly like
-- conversations; conversation_id is nullable (ON DELETE SET NULL) so a row
-- survives a conversation purge for audit. state is per-kind lifecycle
-- (domain consts, no CHECK — extensible).
CREATE TABLE public.artifacts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    conversation_id uuid,
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
-- Name: conversation_memory; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.conversation_memory (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    entity_id uuid NOT NULL,
    blueprint_run_id uuid,
    agent_content text,
    human_content text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: conversation_memory_entities; Type: TABLE; Schema: public; Owner: -
--

-- TFAC-622: lets one conversation_memory row reach every entity the run actually
-- touched (a Jira ticket + the PR it opens), not just the denormalized
-- primary entity_id above. Keyed on conversation_id rather than the conversation_memory row
-- id — touches are recorded mid-run, before the conversation_memory row exists (it's
-- written at termination); conversation_memory's UNIQUE(conversation_id) makes the join
-- through conversation_id sound once that row lands. role is free text (house
-- style: external_actions.action; see domain.MemoryRole*), no CHECK.
CREATE TABLE public.conversation_memory_entities (
    org_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    entity_id uuid NOT NULL,
    role text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: messages; Type: TABLE; Schema: public; Owner: -
--

-- messages is the transcript canon: one row per neutral (OpenAI-shaped)
-- API message, owned by exactly one conversation. The columns ARE the
-- storage format; assembly is a pure function of these rows and nothing
-- else.
CREATE TABLE public.messages (
    id bigint NOT NULL,
    org_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    -- user_id stamps the whole turn: the requesting user owns their user
    -- row AND the assistant/tool rows produced in response. NULL =
    -- system-triggered (an event-fired delegation's rows, injections
    -- minted by the system).
    user_id uuid,
    -- claim_id attributes a row to the executor engagement that produced
    -- it — per-engagement token accounting and turn reconstruction both
    -- hang off it. NULL for rows produced outside any claim (a queued
    -- user message, a pending injection). Never read by assembly.
    claim_id uuid,
    -- role is app-validated, not CHECK-constrained: 'assistant' | 'tool' |
    -- 'user'. A blank subtype is a normal row for its role; only rows that
    -- deviate from normal role behavior carry one. System-minted 'user'
    -- subtypes: 'injection:compaction-request', 'injection:compaction-result',
    -- 'injection:steer', 'injection:system-note', 'injection:context',
    -- 'injection:nudge', 'injection:wrap-up', 'stop-note'.
    role text NOT NULL,
    content text,
    subtype text DEFAULT ''::text,
    tool_calls jsonb,
    tool_call_id text,
    is_error boolean DEFAULT false NOT NULL,
    metadata jsonb,
    model text,
    input_tokens integer,
    output_tokens integer,
    cache_read_tokens integer,
    cache_creation_tokens integer,
    -- cost_usd is dollars settled at this row: NULL = not a settlement
    -- row, 0 = genuinely free. The runtime stamps an invocation's reported
    -- total as ONE lump on the invocation's last recorded row at terminal
    -- time (no proration — proration without a pricing table would be
    -- confidently wrong), so SUM over any window is the spend in that
    -- window. messages is the money/token ledger; conversations and claims
    -- carry no dollar or token caches.
    cost_usd real,
    -- stop_reason is why the provider stopped generating THIS turn, in its
    -- own spelling ('end_turn' / 'max_tokens' / 'tool_use' / 'refusal' / …).
    -- Assistant rows only; NULL = not an assistant row, or a stream that
    -- reported none. Both runtimes populate it — the SDK parser from the
    -- assistant event, the native loop from the completion's finish reason.
    -- Observability, never an assembly input.
    stop_reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    -- reasoning is an array of reasoning-detail objects mirroring bifrost's
    -- ChatReasoningDetails shape ({index, type, text?, signature?, data?}) —
    -- the provider's replay reference for a prior turn's chain-of-thought.
    -- Reference-only: the API validates each block's signature, not their
    -- ordering, so this is never reconstructed or reordered, only replayed
    -- verbatim. NULL = no reasoning on this message.
    reasoning jsonb,
    -- content_blocks is an array of neutral content-block objects (text /
    -- image / file). NULL means the flat `content` column is the whole
    -- story. Applies to every role — a tool row uses this for an image
    -- result the flat text column can't carry.
    content_blocks jsonb,
    -- delivered=false marks a durable pending input (a steer, follow-up,
    -- or staged injection) not yet folded into any context assembly.
    -- Ordering among pending rows is insertion order.
    -- Two different mechanisms carry exactly-once consumption, and which
    -- one applies is the consumer's, not this column's: the pending-input
    -- flush claims its rows in a single UPDATE … RETURNING predicated on
    -- delivered=false, so it is safe even against a racing claimant; the
    -- native loop's drain names the ids it already assembled and is a
    -- plain UPDATE behind the claim fence, resting on the
    -- at-most-one-active-claim invariant rather than on the statement.
    delivered boolean DEFAULT true NOT NULL,
    -- window_state is app-validated: 'active' (default) | 'elided'
    -- (renders as a deterministic stub, content + is_error retained) |
    -- 'inactive' (superseded by compaction, never assembled). Flips are
    -- policy-gated and sticky by construction — row state, no watermark.
    window_state text DEFAULT 'active'::text NOT NULL,
    -- seq is the assembly-order override: the effective sort key is
    -- COALESCE(seq, id). NULL for every normally-appended row; only a
    -- synthetic insertion (a compaction result) sets a fractional value.
    seq double precision,
    -- duration_ms is the wall-clock milliseconds of the work THIS row
    -- represents — assistant: request issued → message complete (reasoning
    -- included, which is what makes a "thought for Ns" readout derivable);
    -- tool: dispatch → result; any other role: NULL. Timing lives ON the row
    -- because the pairwise alternative cannot be made correct: a streaming
    -- append has no predecessor, a paged read is missing one, compaction
    -- inserts at a fractional seq so "the previous row" stops meaning "the
    -- previous event", and an elided partner is gone. NULL is "not measured",
    -- never "took no time" — a measured sub-millisecond step writes 0.
    --
    -- Human time is never in here: a permission-gated call parks the runtime
    -- until someone answers, and the runtime holds its marks still across
    -- that window, so a gated call reads as the seconds it ran rather than
    -- the minutes it waited to be allowed to run.
    duration_ms integer
);


--
-- Name: messages_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.messages_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: messages_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.messages_id_seq OWNED BY public.messages.id;


--
-- Name: conversation_worktrees; Type: TABLE; Schema: public; Owner: -
--

-- One row per (conversation, repository, ref): a worktree a run materialized.
--
-- repository_id was a slug despite the column's old name. It references the
-- registry row now, so a rename moves nothing here. The agent-facing surface is
-- unchanged — `tfac exec workspace add owner/repo` still takes a slug from
-- argv, and the store resolves it.
--
-- No ON DELETE action on the repository FK, so deleting a registry row a run
-- still names is refused rather than followed. A ledger entry records that a
-- checkout happened and outlives the tracking decision that created it;
-- cascading would leave the ledger with a hole in it, which is exactly the rot
-- the reference was made checkable to prevent.
CREATE TABLE public.conversation_worktrees (
    conversation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    repository_id uuid NOT NULL,
    path text NOT NULL,
    ref text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: conversations; Type: TABLE; Schema: public; Owner: -
--

-- conversations is the durable agent-context table: one row per transcript
-- the system can rebuild for a model, regardless of surface. A delegated
-- task run, a future interactive session, and a future
-- subagent are all conversations. Per-engagement execution state (which
-- executor drove it, when, under which sealed credentials, at what reported
-- duration/turn telemetry) lives on claims (see the multi-mode machinery
-- section); the transcript — and with it the money/token ledger (cost_usd +
-- per-row tokens, summed at read time) — is the messages table. No
-- accounting cache lives here.
CREATE TABLE public.conversations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    -- type names the owning surface: 'delegation' (task runs — every
    -- blueprint step today), 'interactive' (reserved: the taskless "pick
    -- repos and type" surface), or a namespaced 'subagent:<kind>'
    -- ('subagent:clone', 'subagent:explore', … — never the bare word).
    -- App-validated, no value CHECK (house pattern, mirrors origin/source).
    type text DEFAULT 'delegation'::text NOT NULL,
    creator_user_id uuid,
    -- team_id is nullable: delegation conversations always carry their
    -- team (pinned via the visibility CHECK below).
    team_id uuid,
    visibility text DEFAULT 'team'::text NOT NULL,
    -- task_id / prompt_id are NULLABLE so a conversation need not descend
    -- from a task or a saved prompt (interactive/subagent rows).
    -- conversations_origin_requires_parents pins them for origin
    -- 'blueprint' (every delegation conversation today).
    task_id uuid,
    prompt_id text,
    trigger_id uuid,
    trigger_type text DEFAULT 'manual'::text NOT NULL,
    -- origin discriminates blueprint-step delegation from other creation
    -- modes (future 'interactive'); app-validated (no value
    -- CHECK). See the SQLite twin.
    origin text DEFAULT 'blueprint'::text NOT NULL,
    -- runtime picks the executing engine: 'sdk' (the Claude Code SDK
    -- subprocess — everything today) or 'native' (the executor-side agent
    -- loop hydrating straight from messages). App-validated. A one-way
    -- ratchet per conversation: once any engagement runs native, the SDK
    -- can never continue this transcript (it cannot ingest message rows;
    -- only the native loop assembles from them). An sdk conversation may
    -- carry reasoning/content_blocks on its messages for display, but they
    -- are never read back for replay — sdk_session_id is truth for resume.
    runtime text DEFAULT 'sdk'::text NOT NULL,
    -- status is OUTCOME-OR-NOTHING: 'open' (a deliberate park), a terminal
    -- ('completed' — the agent concluded — or 'failed' — the infrastructure
    -- died), or NULL. Stopping a run without concluding it is a park, not a
    -- third terminal: cancellation is spelled at the task layer
    -- (return-to-queue, drag-to-done) and the blueprint layer
    -- (cancel_requested / blueprint_runs.status='cancelled').
    -- NULL is not "unknown" — it is the mid-flight state: either an active
    -- claim is driving the conversation right now, or its last claim
    -- released without an outcome and nobody has picked it up yet. "Queued"
    -- and "running" are DERIVED (from the claim table plus the
    -- needs-driving predicate) and never stored. Engagement sub-state
    -- (fetching / cloning / agent_starting / awaiting_credentials) lives on
    -- the live claim's phase, coalesced over this column on display reads —
    -- a retry or re-claim never rewrites the conversation row. Mint writes
    -- nothing here; parks write 'open'; terminals write their terminal;
    -- nothing else touches it.
    status text,
    model text,
    -- sdk_session_id is the SDK-runtime resume handle. Meaningless
    -- (and left NULL) under runtime='native', where the message rows are
    -- the resume state.
    sdk_session_id text,
    worktree_path text,
    result_summary text,
    outcome text,
    outcome_reason text,
    -- failure_kind is the machine-readable discriminator for WHY status
    -- reached 'failed' — the domain.RunFailureKind vocabulary (memory_limit,
    -- crash, executor_lost, …). App-validated; NULL = no classification.
    failure_kind text,
    -- park_reason is WHY the conversation was parked `open` — the closed
    -- domain.ParkReason vocabulary (idle, user_cancelled, blueprint_terminal,
    -- …). App-validated; NULL = never parked, or resumed since. The MODEL's
    -- stop reason is a per-turn fact and lives on messages.stop_reason.
    --
    -- Neither list above is exhaustive, deliberately: the Go set is the
    -- authority and a comment is the one mirror no test can pin. Copying the
    -- members here makes a second source that silently falls behind — which
    -- is not hypothetical, it is what happened to failure_kind, which listed
    -- four of its six values until this line was written.
    park_reason text,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    -- parked_at is stamped when a delegation conversation enters the `open`
    -- parked state and cleared on resume, so the snapshot-retention sweep
    -- keys an open conversation off its last park.
    parked_at timestamp with time zone,
    -- archived_at retires a conversation from its surface's "current" view
    -- without deleting history.
    -- NULL = live. An archived conversation is never claimed again.
    archived_at timestamp with time zone,
    actor_agent_id uuid,
    -- parent_conversation_id links a subagent conversation to the
    -- conversation that spawned it. Scoping anchors (e.g. task_id) are
    -- denormalized from the parent at mint so RLS never recurses.
    parent_conversation_id uuid,
    blueprint_run_id uuid,
    blueprint_step_index integer,
    triggering_event_id uuid,
    -- queued_at is when the conversation entered the claim queue, stamped
    -- once at enqueue and never bumped again (there is no requeue write left
    -- to bump it on — releasing a claim is the requeue). Display-only: the
    -- scheduler orders by started_at. claimed_at lives on the claim row, so
    -- claim.claimed_at − queued_at is the first queue dwell.
    queued_at timestamp with time zone,
    -- preferred_executor_id is the placement affinity stamp: the
    -- capacity-weighted rendezvous winner for this conversation's
    -- (org, repo) key, computed at enqueue, cleared on requeue. Purely
    -- advisory queue-time state, so it lives here, not on a claim (which
    -- doesn't exist yet while queued). Deliberately NO FK to instances.
    preferred_executor_id text,
    CONSTRAINT conversations_creator_matches_trigger_type CHECK ((((trigger_type = 'manual'::text) AND (creator_user_id IS NOT NULL)) OR ((trigger_type = 'event'::text) AND (creator_user_id IS NULL)))),
    CONSTRAINT conversations_team_visibility_requires_team CHECK (((visibility <> 'team'::text) OR (team_id IS NOT NULL))),
    CONSTRAINT conversations_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'team'::text, 'org'::text]))),
    -- A 'blueprint'-origin conversation carries its full parentage
    -- (blueprint_run + task + prompt).
    CONSTRAINT conversations_origin_requires_parents CHECK (((origin = 'blueprint'::text AND blueprint_run_id IS NOT NULL AND task_id IS NOT NULL AND prompt_id IS NOT NULL) OR (origin <> 'blueprint'::text)))
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
    -- The post-scoring re-derive, owed as a row rather than promised by a
    -- live process. The event-time trigger pass skips every deferred
    -- (min_autonomy_suitability > 0) trigger on the promise that the
    -- re-derive fires it once a score exists; the re-derive runs as a
    -- post-commit callback, so a crash between the scores committing and
    -- the callback finishing left the task 'scored' — never revisited by a
    -- future cycle — with those triggers never evaluated.
    --
    -- Set in the same statement that writes the scores, so the debt and the
    -- scores are never separable; cleared only by a re-derive pass that
    -- actually evaluated the task. The scorer drains the owed set at cycle
    -- start, the crash backstop behind the callback's fast path. Exactly
    -- two writers, and nothing else may touch it — a task may legitimately
    -- be pending for a re-score while still owing a re-derive from the last
    -- one.
    rederive_owed boolean DEFAULT false NOT NULL,
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
    -- team owns the model used for scoring + agent runs (drawn from the
    -- enable-sets on this row and on org_settings) and the master toggle for
    -- auto-delegation. auto_delegate_enabled defaults TRUE: a team whose
    -- trigger is enabled means the run to fire, and shipped triggers are off
    -- until opted in, so a second gate defaulting off would swallow it. Teams
    -- wanting review-before-run turn it off.
    --
    -- default_model is a concrete model id (a modelcatalog key), never a
    -- vendor tier word — it is dispatched verbatim and priced by that same
    -- string. NOT NULL with a literal DEFAULT because a partial upsert
    -- (SetDailyCostCapSystem) materializes the row without naming the column,
    -- so what this defaults to is what that team runs on; kept equal to
    -- domain.DefaultModel.
    default_model text DEFAULT 'claude-sonnet-5'::text NOT NULL,
    auto_delegate_enabled boolean DEFAULT true NOT NULL,
    -- Claude Code SDK permission posture. Multi-mode SDK runs always use auto;
    -- local mode honors this team setting, and native runs ignore it.
    auto_mode_enabled boolean DEFAULT true NOT NULL,
    -- Per-team daily LLM spend cap (TFAC-482, EE/governance-gated). NULL = no cap;
    -- the app layer also treats 0 as "no cap". When the team's spend for the
    -- current UTC calendar day (summed over its team_id rows ONLY — system
    -- overhead carries a NULL team_id and never counts toward a
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
    -- Per-team review-posting posture: how a delegated agent's
    -- finalized review reaches GitHub. 'identity' (the default) decides from the
    -- acting credential — an App installation posts as a bot and goes direct, a
    -- borrowed PAT posts as the lending user and stages for approval, and an
    -- indeterminate identity stages. 'draft' always stages, 'auto' always
    -- submits, 'auto_unless_blocking' stages only a consequential review
    -- (REQUEST_CHANGES, or any BLOCKER-severity inline comment). NOT NULL with a
    -- literal DEFAULT so partial upserts (e.g. SetDailyCostCapSystem) materialize
    -- it without the writer naming the column; the app coalesces an empty write
    -- to the default and validates the value set app-side (no CHECK — the
    -- enabled_models precedent). Rolled into the baseline (not a forward
    -- migration) because multi-mode / Postgres is net-new and unshipped; the
    -- SQLite tree carries 202607260001_team_review_posture.sql.
    review_posture text DEFAULT 'identity'::text NOT NULL,
    -- Per-team base-branch push policy: whether a delegated agent may push to a
    -- repo's base / default branch (main, master, the profile's default branch,
    -- the configured base branch). 'never' (the default) refuses every such
    -- push, 'manual_only' permits it on a human-dispatched run and refuses it on
    -- an event-triggered one, 'always' permits it. A safety guard against a
    -- mistaken agent, enforced here at the per-run git proxy's ref gate (local
    -- mode enforces the same policy at the pre-push hook). NOT NULL with a
    -- literal DEFAULT so partial upserts (e.g. SetDailyCostCapSystem)
    -- materialize it without the writer naming the column; the app coalesces an
    -- empty write to the default and validates the value set app-side (no CHECK
    -- — the enabled_models precedent). Rolled into the baseline (not a
    -- forward migration) because multi-mode / Postgres is net-new and unshipped;
    -- the SQLite tree carries 202607270001_team_base_branch_push_policy.sql.
    base_branch_push_policy text DEFAULT 'never'::text NOT NULL,
    -- Which of the models its org enables this team may pick from, as a JSON
    -- array of catalog keys — same encoding and same absent-value discipline as
    -- org_settings.enabled_models. NULL inherits the org's effective set whole.
    --
    -- A subset of that set at every save (the write refuses a superset) and
    -- narrowed to it again at every read, because the org may shrink its own set
    -- afterwards and nothing rewrites a team row when it does.
    --
    -- Model-granular, and that grain is what makes it the only selection-time
    -- control a team needs: restricting a team from a provider is unchecking
    -- that path's models, so no coarser provider column stands beside this one
    -- to disagree with it.
    -- Rolled into the baseline (not a forward migration) because multi-mode /
    -- Postgres is net-new and unshipped; the SQLite tree, which HAS shipped,
    -- carries 202608270001_model_enable_sets.sql.
    enabled_models text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    -- Grace window (ms) an unattended permission prompt waits before denying
    -- with "no operator available"; the boolean is the master toggle, false =
    -- wait out the full permission timeout instead.
    permission_absent_grace_ms integer DEFAULT 15000 NOT NULL,
    permission_absent_autodeny_enabled boolean DEFAULT true NOT NULL
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
    -- durable work (tasks, conversations, memory) is never hard-deleted.
    deleted_at timestamp with time zone,
    -- shipped_defaults_backfilled_at is the durable per-team marker that the
    -- one-time grandfather backfill preceding the boot-time shipped-defaults
    -- sync has run for this team: before the first sync stamps user_modified on
    -- any system-slugged blueprint whose step structure already diverges from
    -- shipped, then sets this timestamp. NULL = backfill still owed; a timestamp
    -- = done, so the clobber-guard runs at most once per team.
    shipped_defaults_backfilled_at timestamp with time zone
);


--
-- Name: user_settings; Type: TABLE; Schema: public; Owner: -
--

-- Per-user prefs (theme, notification destinations, swipe sensitivity,
-- onboarding state are the shapes still to come). The ai_model +
-- ai_auto_delegate_enabled columns that used to live here moved to
-- team_settings — the team owns the AI behavior policy, users don't override
-- in v1.
CREATE TABLE public.user_settings (
    user_id uuid NOT NULL,
    -- When this user last opened the Overview, which the page's away line
    -- ("you were last here at 18:40 yesterday") is anchored to and its counts
    -- are measured from. Written explicitly by the page rather than stamped on
    -- read: a read that mutates is a read nobody can repeat, and the caller
    -- that knows a HUMAN looked is the page, not the request log — a rail-count
    -- refetch on an idle open tab keeps any last-action timestamp perpetually
    -- fresh while nobody is there.
    --
    -- NULL is load-bearing: it means never opened, which the page renders as
    -- its own thing (an anchor at midnight, said out loud) rather than as a
    -- very old visit.
    --
    -- Keyed by user alone, like the row it sits on, so a multi-org user carries
    -- one marker across their orgs. Accepted: the value is a display anchor
    -- read at minute resolution, and per-org markers would be a second table to
    -- make one sentence per org marginally more accurate.
    overview_seen_at timestamp with time zone,
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

-- Host-scoped GitHub identity bindings. Replaces the single
-- users.github_username column: one human can hold a different login on each
-- GitHub host (github.com for one org, a corp GHES for another), so the
-- natural key is (user_id, github_base_url), not a lone column. For the first
-- self-deploy (one org, one host) this is exactly one row per user.
--
-- source records HOW the binding was captured ('pat' | 'connect_oauth' |
-- 'scim' | 'login_claim') — load-bearing for the "verified against the
-- org's host, never typed-unverified" integrity rule. verified_at timestamps
-- the last authenticated /user confirmation against the host — the hook for
-- future drift re-checks. It is deliberately nullable: NULL is a meaningful
-- state, "login known but not yet host-verified" — the shape a future SCIM
-- sync (source='scim') would write when it learns a login from the directory
-- without an authenticated round-trip to the host. Today's writers (pat,
-- login_claim) always stamp it. An absent row is a durable, supported state
-- (the NULL-degrades-gracefully contract).
-- github_user_id is the account's numeric GitHub id, stored as text like every
-- other GitHub-issued id in this schema. login is renameable; this is not, so
-- the pair records who the user is as well as what they are currently called.
-- Nullable: a row bound before this was captured keeps working off the login
-- alone and fills the id in on the next capture.
CREATE TABLE public.user_github_identities (
    user_id uuid NOT NULL,
    github_base_url text NOT NULL,
    login text NOT NULL,
    github_user_id text,
    email text,
    source text NOT NULL,
    verified_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    -- NULL = the one-shot trailing-window dashboard history backfill is still
    -- owed for this (user, host) identity; a timestamp = done.
    dashboard_backfilled_at timestamp with time zone,
    CONSTRAINT user_github_identities_source_check CHECK ((source = ANY (ARRAY['pat'::text, 'connect_oauth'::text, 'scim'::text, 'login_claim'::text])))
);


--
-- Name: user_jira_identities; Type: TABLE; Schema: public; Owner: -
--

-- Host-scoped Jira identity bindings. The Jira sibling of
-- user_github_identities: it replaces the single-valued users.jira_account_id
-- / users.jira_display_name columns. One human can hold a different Jira
-- account on each Jira site (a Cloud site for one org, a Server/DC host for
-- another), so the natural key is (user_id, jira_base_url), not a lone column.
-- For the first self-deploy (one org, one host) this is exactly one row per
-- user.
--
-- The access layer already keys per-(user, host): the per-user Jira PAT is
-- custodied as "jira_token/<host>". This table makes IDENTITY
-- symmetric with that access — both on the same canonical host — so a second
-- Jira site can't overwrite the first's identity the way the single column did.
--
-- account_id is the Atlassian StableID (Cloud accountId, else Server/DC key);
-- display_name is the Jira-side display name. source records HOW the binding
-- was captured ('pat' | 'connect_oauth' | 'scim') — 'pat' is the only writer
-- today (DC paste-a-PAT); Cloud OAuth and SCIM are later tickets. verified_at
-- timestamps the last authenticated /myself confirmation (nullable for a
-- future SCIM directory sync; today's pat writer always stamps it). An absent
-- row is a durable, supported state (the NULL-degrades-gracefully contract).
CREATE TABLE public.user_jira_identities (
    user_id uuid NOT NULL,
    jira_base_url text NOT NULL,
    account_id text NOT NULL,
    display_name text,
    source text NOT NULL,
    verified_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_jira_identities_source_check CHECK ((source = ANY (ARRAY['pat'::text, 'connect_oauth'::text, 'scim'::text, 'cloud_api_token'::text])))
);






--
-- Name: pending_firings id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_firings ALTER COLUMN id SET DEFAULT nextval('public.pending_firings_id_seq'::regclass);


--
-- Name: messages id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.messages ALTER COLUMN id SET DEFAULT nextval('public.messages_id_seq'::regclass);


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
-- collide.
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
    ADD CONSTRAINT team_github_repos_pkey PRIMARY KEY (team_id, repository_id);


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
-- Name: prompts prompts_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_id_org_id_key UNIQUE (id, org_id);


--
-- Name: prompts prompts_id_team_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

-- Parent key for the same-team composite FK (blueprint_steps.step_prompt_id)
-- so a blueprint step can only bind a prompt its own team owns.
ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_id_team_id_key UNIQUE (id, team_id);


--
-- Name: prompts prompts_org_team_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

-- Per-team re-seed idempotency key: one copy of each shipped prompt per team
-- (same system_slug, distinct team_id); NULLs distinct so user prompts never
-- collide.
ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_org_team_slug_key UNIQUE (org_id, team_id, system_slug);


--
-- Name: prompts prompts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompts
    ADD CONSTRAINT prompts_pkey PRIMARY KEY (org_id, id);


--
-- Name: repositories repositories_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.repositories
    ADD CONSTRAINT repositories_pkey PRIMARY KEY (id);


--
-- Name: repositories repositories_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

-- Redundant on its own — id alone is already unique — and present so that
-- team_github_repos can reference (repository_id, org_id) as a composite
-- foreign key. Postgres requires a unique constraint over exactly the
-- referenced columns; this is that requirement, not a second identity.
ALTER TABLE ONLY public.repositories
    ADD CONSTRAINT repositories_id_org_id_key UNIQUE (id, org_id);


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
-- Name: conversation_memory conversation_memory_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_memory
    ADD CONSTRAINT conversation_memory_pkey PRIMARY KEY (id);


--
-- Name: conversation_memory conversation_memory_conversation_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_memory
    ADD CONSTRAINT conversation_memory_conversation_id_key UNIQUE (conversation_id);


--
-- Name: conversation_memory_entities conversation_memory_entities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_memory_entities
    ADD CONSTRAINT conversation_memory_entities_pkey PRIMARY KEY (conversation_id, entity_id);


--
-- Name: messages messages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_pkey PRIMARY KEY (id);


--
-- Name: conversation_worktrees conversation_worktrees_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_worktrees
    ADD CONSTRAINT conversation_worktrees_pkey PRIMARY KEY (conversation_id, repository_id, ref);


--
-- Name: conversations conversations_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_id_org_id_key UNIQUE (id, org_id);


--
-- Name: conversations conversations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_pkey PRIMARY KEY (id);


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
-- Name: teams teams_id_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

-- Same rationale as repositories_id_org_id_key: a composite-FK target, not a
-- second identity for a team. Two tables reference it — team_github_repos for
-- the tracking row's team half, org_invites for an invite's target team — and
-- it is declared here so it precedes both.
--
-- This is the schema's established shape for "these two rows must belong to
-- the same tenant": agents, entities, conversations, tasks and blueprint_runs
-- all pair an (id, org_id) unique constraint with a composite foreign key.
ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_id_org_id_key UNIQUE (id, org_id);


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
-- Name: idx_entities_closed_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entities_closed_at ON public.entities USING btree (closed_at) WHERE (closed_at IS NOT NULL);


--
-- Name: idx_entities_github_author; Type: INDEX; Schema: public; Owner: -
--
-- Serves the dashboard PR list's author predicate. That read used to select
-- every GitHub entity in the org and drop the non-matches in Go, so its cost
-- scaled with the org's whole PR history rather than with the caller's own
-- PRs. The index expression and the partial predicate both match the query's.

CREATE INDEX idx_entities_github_author ON public.entities USING btree (org_id, ((snapshot_json ->> 'author'::text)), last_polled_at DESC) WHERE ((source = 'github'::text) AND (snapshot_json IS NOT NULL));


--
-- Name: idx_entities_org_source_polled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entities_org_source_polled ON public.entities USING btree (org_id, source, last_polled_at);


--
-- Name: idx_entities_org_state; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_entities_org_state ON public.entities USING btree (org_id, state);


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
-- deleted_at IS NULL so a soft-deleted trigger frees its blueprint slot
-- instead of permanently blocking a replacement.

CREATE UNIQUE INDEX event_handlers_one_trigger_per_blueprint ON public.event_handlers USING btree (org_id, blueprint_id) WHERE (blueprint_id IS NOT NULL AND deleted_at IS NULL);


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

CREATE UNIQUE INDEX idx_pending_firings_dedup ON public.pending_firings USING btree (task_id, trigger_id) WHERE (status = ANY (ARRAY['pending'::text, 'draining'::text]));


--
-- Name: idx_pending_firings_entity_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_pending_firings_entity_pending ON public.pending_firings USING btree (entity_id, queued_at) WHERE (status = 'pending'::text);


--
-- Name: idx_repositories_org_owner_repo; Type: INDEX; Schema: public; Owner: -
--

-- The repository natural key. It includes source because two providers may
-- issue the same owner/repo spelling and those are different repositories, and
-- it folds case because GitHub identifiers are case-insensitive, so "one
-- repository" has always meant one row per folded slug. A key that did not fold
-- would leave that invariant to the writers, and a writer is exactly what
-- fails: an INSERT guarded by a case-insensitive NOT EXISTS over a
-- case-sensitive index admits two rows whenever two transactions race, because
-- neither sees the other's uncommitted row and their keys then differ. Here the
-- second writer conflicts instead — blocking on the first while it is in
-- flight — so the create path needs no lock of its own.
--
-- An expression key has to be an index rather than a UNIQUE constraint; the
-- store infers it by expression list in ON CONFLICT.
CREATE UNIQUE INDEX repositories_identity ON public.repositories USING btree (org_id, source, lower(owner), lower(repo));


--
-- Name: idx_repositories_org_owner_repo; Type: INDEX; Schema: public; Owner: -
--

-- Kept alongside the folded key: it serves the ordered org-wide list
-- (WHERE org_id ORDER BY owner, repo), which the folded index cannot.
CREATE INDEX idx_repositories_org_owner_repo ON public.repositories USING btree (org_id, owner, repo);


--
-- Name: idx_system_llm_runs_org_started; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_system_llm_runs_org_started ON public.system_llm_runs USING btree (org_id, started_at DESC);


--
-- Name: idx_system_llm_runs_org_job_started; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_system_llm_runs_org_job_started ON public.system_llm_runs USING btree (org_id, job, started_at DESC);


--
-- Name: idx_system_llm_runs_trace_id; Type: INDEX; Schema: public; Owner: -
--

-- Idempotency key (TFAC-579): a duplicate Record() call for the same
-- agentproc invocation (retry, double-call) is a no-op via ON CONFLICT
-- rather than double-counting spend. Partial on trace_id IS NOT NULL so
-- the rare invocation that never minted a session (subprocess died before
-- the init event) doesn't collide with every other such row.
CREATE UNIQUE INDEX idx_system_llm_runs_trace_id ON public.system_llm_runs USING btree (trace_id) WHERE (trace_id IS NOT NULL);


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
-- Name: idx_external_actions_conversation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_external_actions_conversation ON public.external_actions USING btree (conversation_id);


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
-- Name: idx_artifacts_conversation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_artifacts_conversation ON public.artifacts USING btree (conversation_id);


--
-- Name: idx_conversation_memory_entity_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_conversation_memory_entity_created ON public.conversation_memory USING btree (entity_id, created_at);


--
-- Name: idx_conversation_memory_entity_blueprint; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_conversation_memory_entity_blueprint ON public.conversation_memory USING btree (entity_id, blueprint_run_id);


--
-- Name: idx_conversation_memory_conversation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_conversation_memory_conversation ON public.conversation_memory USING btree (conversation_id);


--
-- Name: idx_conversation_memory_entities_entity; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_conversation_memory_entities_entity ON public.conversation_memory_entities USING btree (org_id, entity_id);


--
-- Name: idx_messages_conversation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_messages_conversation ON public.messages USING btree (conversation_id);


--
-- Name: idx_messages_conversation_seq; Type: INDEX; Schema: public; Owner: -
--

-- Assembly order: the effective sort key is COALESCE(seq, id), so this
-- expression index is what a native loop's ListForAssembly actually walks —
-- idx_messages_conversation above stays for the plain id-order transcript reads
-- (UI, spend sums) that never touch seq.
CREATE INDEX idx_messages_conversation_seq ON public.messages (conversation_id, (COALESCE(seq, (id)::double precision)));

-- The claim-trigger scan ("conversations with undelivered input") + the
-- pending-flush read.
CREATE INDEX idx_messages_undelivered ON public.messages USING btree (conversation_id) WHERE (delivered = false);
-- Per-engagement token accounting (SUM over one claim's rows).
CREATE INDEX idx_messages_claim ON public.messages USING btree (claim_id) WHERE (claim_id IS NOT NULL);
-- Per-user message reads; partial because most rows are agent-side (NULL user).
CREATE INDEX idx_messages_user ON public.messages USING btree (user_id) WHERE (user_id IS NOT NULL);


--
-- Name: idx_conversation_worktrees_conversation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_conversation_worktrees_conversation ON public.conversation_worktrees USING btree (conversation_id);


--
-- Name: idx_conversations_org_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_conversations_org_status ON public.conversations USING btree (org_id, status);


--
-- Name: idx_conversations_prompt_started; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_conversations_prompt_started ON public.conversations USING btree (prompt_id, started_at DESC);


--
-- Name: idx_conversations_task; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_conversations_task ON public.conversations USING btree (task_id);


--
-- Name: idx_conversations_trigger; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_conversations_trigger ON public.conversations USING btree (trigger_id);


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
-- Name: idx_tasks_rederive_owed; Type: INDEX; Schema: public; Owner: -
--

-- Partial: the scorer reads this set once per cycle per org and it is empty
-- in every crash-free cycle, so the index spans only the rare owed rows
-- rather than the whole board. created_at trails org_id because the drain
-- reads the set oldest-first — without it the predicate is served from the
-- index and the ordering still costs a sort.
CREATE INDEX idx_tasks_rederive_owed ON public.tasks USING btree (org_id, created_at) WHERE rederive_owed;


--
-- Name: conversations_actor_agent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX conversations_actor_agent_idx ON public.conversations USING btree (actor_agent_id) WHERE (actor_agent_id IS NOT NULL);

CREATE INDEX idx_conversations_parent ON public.conversations USING btree (parent_conversation_id) WHERE (parent_conversation_id IS NOT NULL);


--
-- Name: conversations_event_trigger_fence; Type: INDEX; Schema: public; Owner: -
--
-- Fired-fence: one event firing one trigger materializes at most
-- one conversation. Partial WHERE triggering_event_id IS NOT NULL so manual
-- and blueprint-step conversations (NULL) never participate — multiple
-- manual conversations on one task stay allowed, and two distinct event
-- instances still fire independently.

CREATE UNIQUE INDEX conversations_event_trigger_fence ON public.conversations USING btree (triggering_event_id, trigger_id) WHERE (triggering_event_id IS NOT NULL);


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
-- Name: team_github_repos_repository_idx; Type: INDEX; Schema: public; Owner: -
--

-- The reverse lookup, "which teams track this repository", which is the
-- direction every tracked-set semi-join reads: the factory belt
-- (factoryGitHubRepoTrackedExists), the repo list's team scoping
-- (repoProfileTrackedByViewerTeams), and the per-repo gates
-- (TracksRepoViewerScoped / TracksRepoViewerAdminScoped). The primary key
-- leads with team_id and cannot serve them — none of them pins a team. Those
-- semi-joins reach a repository by id now, so this replaces the functional
-- (lower(owner), lower(repo), team_id) index the slug columns needed: the
-- case-folding moved to the registry's own identity index, where one
-- repository is one row however it is spelled.
CREATE INDEX team_github_repos_repository_idx ON public.team_github_repos USING btree (repository_id, team_id);


--
-- Name: user_github_identities_login_lookup_idx; Type: INDEX; Schema: public; Owner: -
--

-- Serves the reverse (host, login) → user lookup the routing subscribers run.
-- lower(login) because GitHub logins are case-insensitive while the writers
-- persist them as captured, so a verbatim compare silently misses on
-- capitalisation — the same reason user_identities_link_lookup_idx keys on
-- lower(email). github_base_url leads: identity is host-scoped, so every
-- lookup pins it.
CREATE INDEX user_github_identities_login_lookup_idx ON public.user_github_identities USING btree (github_base_url, lower(login));


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
-- Name: prompts set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.prompts FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


--
-- Name: repositories set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER set_updated_at BEFORE UPDATE ON public.repositories FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();


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
-- Name: entities entities_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entities
    ADD CONSTRAINT entities_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


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

-- Both references carry org_id, so a tracking row cannot straddle two tenants:
-- the team half and the repository half must agree on the org, or neither
-- parent row exists to reference. This replaces a pair of single-column keys
-- that checked existence and said nothing about tenancy.
--
-- No ON UPDATE clause on either: moving a team or a repository between orgs
-- would drag its tracking rows across the tenant boundary, so refusing (the
-- default) is the wanted behaviour. Nothing writes those columns.
ALTER TABLE ONLY public.team_github_repos
    ADD CONSTRAINT team_github_repos_team_id_fkey FOREIGN KEY (team_id, org_id) REFERENCES public.teams(id, org_id) ON DELETE CASCADE;


--
-- Name: team_github_repos team_github_repos_repository_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_github_repos
    ADD CONSTRAINT team_github_repos_repository_id_fkey FOREIGN KEY (repository_id, org_id) REFERENCES public.repositories(id, org_id) ON DELETE CASCADE;


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
-- Name: repositories repositories_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.repositories
    ADD CONSTRAINT repositories_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


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

-- org_id CASCADE (drop an org's log with the org); conversation_id SET NULL
-- (the action outlives a purged conversation for the audit trail, like
-- artifacts). team_id and
-- actor_user_id are deliberately FK-free so the audit row outlives the team/user
-- it references — an audit log must outlive its subjects (same rule as
-- access_change_log).
ALTER TABLE ONLY public.external_actions
    ADD CONSTRAINT external_actions_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: external_actions external_actions_conversation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_actions
    ADD CONSTRAINT external_actions_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE SET NULL;


--
-- Name: artifacts artifacts_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.artifacts
    ADD CONSTRAINT artifacts_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: artifacts artifacts_conversation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

-- Single-column ref to conversations(id) with ON DELETE SET NULL: the
-- artifact outlives a purged conversation for the audit ledger. A composite
-- (conversation_id, org_id) ref like the other conversation children can't
-- SET NULL here because org_id is NOT NULL — and the artifact's own
-- org_id_fkey already pins the org.
ALTER TABLE ONLY public.artifacts
    ADD CONSTRAINT artifacts_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE SET NULL;


--
-- Name: artifacts artifacts_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.artifacts
    ADD CONSTRAINT artifacts_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;


--
-- Name: conversation_memory conversation_memory_entity_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_memory
    ADD CONSTRAINT conversation_memory_entity_id_org_id_fkey FOREIGN KEY (entity_id, org_id) REFERENCES public.entities(id, org_id);


--
-- Name: conversation_memory conversation_memory_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_memory
    ADD CONSTRAINT conversation_memory_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: conversation_memory conversation_memory_conversation_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_memory
    ADD CONSTRAINT conversation_memory_conversation_id_org_id_fkey FOREIGN KEY (conversation_id, org_id) REFERENCES public.conversations(id, org_id) ON DELETE CASCADE;


--
-- Name: conversation_memory_entities conversation_memory_entities_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_memory_entities
    ADD CONSTRAINT conversation_memory_entities_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: conversation_memory_entities conversation_memory_entities_entity_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_memory_entities
    ADD CONSTRAINT conversation_memory_entities_entity_id_org_id_fkey FOREIGN KEY (entity_id, org_id) REFERENCES public.entities(id, org_id) ON DELETE CASCADE;


--
-- Name: conversation_memory_entities conversation_memory_entities_conversation_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_memory_entities
    ADD CONSTRAINT conversation_memory_entities_conversation_id_org_id_fkey FOREIGN KEY (conversation_id, org_id) REFERENCES public.conversations(id, org_id) ON DELETE CASCADE;


--
-- Name: messages messages_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: messages messages_conversation_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_conversation_id_org_id_fkey FOREIGN KEY (conversation_id, org_id) REFERENCES public.conversations(id, org_id) ON DELETE CASCADE;


--
-- Name: conversation_worktrees conversation_worktrees_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_worktrees
    ADD CONSTRAINT conversation_worktrees_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;


--
-- Name: conversation_worktrees conversation_worktrees_repository_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_worktrees
    ADD CONSTRAINT conversation_worktrees_repository_id_fkey FOREIGN KEY (repository_id) REFERENCES public.repositories(id);


--
-- Name: conversation_worktrees conversation_worktrees_conversation_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversation_worktrees
    ADD CONSTRAINT conversation_worktrees_conversation_id_org_id_fkey FOREIGN KEY (conversation_id, org_id) REFERENCES public.conversations(id, org_id) ON DELETE CASCADE;


--
-- Name: conversations conversations_actor_agent_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_actor_agent_fkey FOREIGN KEY (actor_agent_id, org_id) REFERENCES public.agents(id, org_id) ON DELETE SET NULL;


--
-- Name: conversations conversations_creator_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_creator_user_id_fkey FOREIGN KEY (creator_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: conversations conversations_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_parent_id_org_id_fkey FOREIGN KEY (parent_conversation_id, org_id) REFERENCES public.conversations(id, org_id) ON DELETE CASCADE;


--
-- Name: conversations conversations_prompt_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_prompt_id_org_id_fkey FOREIGN KEY (prompt_id, org_id) REFERENCES public.prompts(id, org_id);


--
-- Name: conversations conversations_task_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_task_id_org_id_fkey FOREIGN KEY (task_id, org_id) REFERENCES public.tasks(id, org_id);


--
-- Name: conversations conversations_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;


--
-- Name: conversations conversations_trigger_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_trigger_id_org_id_fkey FOREIGN KEY (trigger_id, org_id) REFERENCES public.event_handlers(id, org_id);


--
-- Name: conversations conversations_triggering_event_id_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--
-- Composite FK mirrors pending_firings.triggering_event_id. NULL
-- triggering_event_id (manual / blueprint-step conversations) skips the
-- check under MATCH SIMPLE, so only event-fired conversations are tied to a
-- real event row.

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_triggering_event_id_org_id_fkey FOREIGN KEY (triggering_event_id, org_id) REFERENCES public.events(id, org_id);


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
-- Name: repositories; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.repositories ENABLE ROW LEVEL SECURITY;

--
-- Name: repositories repositories_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY repositories_all ON public.repositories USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));


--
-- Name: system_llm_runs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.system_llm_runs ENABLE ROW LEVEL SECURITY;

--
-- Name: system_llm_runs system_llm_runs_all; Type: POLICY; Schema: public; Owner: -
--

-- Mirrors repositories_all: org-scoped read/write under the app pool.
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

-- artifacts mirror the conversations team-visibility branch: team-scoped read
-- via team_id, the same write pool/scope conversations use. artifacts carry no
-- private/org visibility (no visibility / creator_user_id columns), so the
-- policies are the team predicates from conversations, swapped onto this
-- table — user_in_team for SELECT, user_can_write_team for
-- INSERT/UPDATE/DELETE.

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
-- Name: conversation_memory; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.conversation_memory ENABLE ROW LEVEL SECURITY;

--
-- Name: conversation_memory conversation_memory_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY conversation_memory_all ON public.conversation_memory USING ((EXISTS ( SELECT 1
   FROM public.conversations r
  WHERE (r.id = conversation_memory.conversation_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.conversations r
  WHERE (r.id = conversation_memory.conversation_id))));


--
-- Name: conversation_memory_entities; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.conversation_memory_entities ENABLE ROW LEVEL SECURITY;

--
-- Name: conversation_memory_entities conversation_memory_entities_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY conversation_memory_entities_all ON public.conversation_memory_entities USING ((EXISTS ( SELECT 1
   FROM public.conversations r
  WHERE (r.id = conversation_memory_entities.conversation_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.conversations r
  WHERE (r.id = conversation_memory_entities.conversation_id))));


--
-- Name: messages; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.messages ENABLE ROW LEVEL SECURITY;

--
-- Name: messages messages_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY messages_all ON public.messages USING ((EXISTS ( SELECT 1
   FROM public.conversations r
  WHERE (r.id = messages.conversation_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.conversations r
  WHERE (r.id = messages.conversation_id))));


--
-- Name: conversation_worktrees; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.conversation_worktrees ENABLE ROW LEVEL SECURITY;

--
-- Name: conversation_worktrees conversation_worktrees_all; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY conversation_worktrees_all ON public.conversation_worktrees USING ((EXISTS ( SELECT 1
   FROM public.conversations r
  WHERE (r.id = conversation_worktrees.conversation_id)))) WITH CHECK ((EXISTS ( SELECT 1
   FROM public.conversations r
  WHERE (r.id = conversation_worktrees.conversation_id))));


--
-- Name: conversations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.conversations ENABLE ROW LEVEL SECURITY;

--
-- Name: conversations conversations_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY conversations_delete ON public.conversations FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_can_write_team(team_id)))));


--
-- Name: conversations conversations_insert; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY conversations_insert ON public.conversations FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (creator_user_id = tf.current_user_id()) AND ((visibility <> 'team'::text) OR tf.user_can_write_team(team_id)) AND ((visibility <> 'org'::text) OR tf.user_is_org_admin(org_id))));


--
-- Name: conversations conversations_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY conversations_select ON public.conversations FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_in_team(team_id)) OR (visibility = 'org'::text))));


--
-- Name: conversations conversations_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY conversations_update ON public.conversations FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_can_write_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id))))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id) AND (((visibility = 'private'::text) AND (creator_user_id = tf.current_user_id())) OR ((visibility = 'team'::text) AND tf.user_can_write_team(team_id)) OR ((visibility = 'org'::text) AND tf.user_is_org_admin(org_id)))));


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
-- Name: FUNCTION blueprint_run_is_running(p_id uuid, p_org_id uuid); Type: ACL; Schema: tf; Owner: -
--

REVOKE ALL ON FUNCTION tf.blueprint_run_is_running(p_id uuid, p_org_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION tf.blueprint_run_is_running(p_id uuid, p_org_id uuid) TO tf_app;


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
-- Name: TABLE prompts; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.prompts TO postgres;
GRANT ALL ON TABLE public.prompts TO anon;
GRANT ALL ON TABLE public.prompts TO authenticated;
GRANT ALL ON TABLE public.prompts TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.prompts TO tf_app;


--
-- Name: TABLE repositories; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.repositories TO postgres;
GRANT ALL ON TABLE public.repositories TO anon;
GRANT ALL ON TABLE public.repositories TO authenticated;
GRANT ALL ON TABLE public.repositories TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.repositories TO tf_app;


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
-- Name: TABLE conversation_memory; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.conversation_memory TO postgres;
GRANT ALL ON TABLE public.conversation_memory TO anon;
GRANT ALL ON TABLE public.conversation_memory TO authenticated;
GRANT ALL ON TABLE public.conversation_memory TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.conversation_memory TO tf_app;


--
-- Name: TABLE conversation_memory_entities; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.conversation_memory_entities TO postgres;
GRANT ALL ON TABLE public.conversation_memory_entities TO anon;
GRANT ALL ON TABLE public.conversation_memory_entities TO authenticated;
GRANT ALL ON TABLE public.conversation_memory_entities TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.conversation_memory_entities TO tf_app;


--
-- Name: TABLE messages; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.messages TO postgres;
GRANT ALL ON TABLE public.messages TO anon;
GRANT ALL ON TABLE public.messages TO authenticated;
GRANT ALL ON TABLE public.messages TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.messages TO tf_app;


--
-- Name: SEQUENCE messages_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON SEQUENCE public.messages_id_seq TO postgres;
GRANT ALL ON SEQUENCE public.messages_id_seq TO anon;
GRANT ALL ON SEQUENCE public.messages_id_seq TO authenticated;
GRANT ALL ON SEQUENCE public.messages_id_seq TO service_role;
GRANT SELECT,USAGE ON SEQUENCE public.messages_id_seq TO tf_app;


--
-- Name: TABLE conversation_worktrees; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.conversation_worktrees TO postgres;
GRANT ALL ON TABLE public.conversation_worktrees TO anon;
GRANT ALL ON TABLE public.conversation_worktrees TO authenticated;
GRANT ALL ON TABLE public.conversation_worktrees TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.conversation_worktrees TO tf_app;


--
-- Name: TABLE conversations; Type: ACL; Schema: public; Owner: -
--

GRANT ALL ON TABLE public.conversations TO postgres;
GRANT ALL ON TABLE public.conversations TO anon;
GRANT ALL ON TABLE public.conversations TO authenticated;
GRANT ALL ON TABLE public.conversations TO service_role;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.conversations TO tf_app;


--
-- Name: TABLE llm_spend; Type: ACL; Schema: public; Owner: -
--



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
-- terminal conversations.outcome driving advancement). Per-step runtime
-- state stays on conversations (linked via conversations.blueprint_run_id);
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
-- a trigger fires a blueprint its own team owns.
ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_blueprint_id_org_id_fkey FOREIGN KEY (blueprint_id, org_id) REFERENCES public.blueprints(id, org_id) ON DELETE CASCADE;
ALTER TABLE ONLY public.event_handlers
    ADD CONSTRAINT event_handlers_blueprint_id_team_id_fkey FOREIGN KEY (blueprint_id, team_id) REFERENCES public.blueprints(id, team_id) ON DELETE CASCADE;

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
    -- two so the seeder can't drift. Mirrors the conversations table's
    -- conversations_creator_matches_trigger_type pattern.
    creator_user_id uuid,
    blueprint_id text NOT NULL,
    task_id uuid NOT NULL,
    trigger_type text NOT NULL,
    trigger_id uuid,
    -- triggering_event_id is the event instance that auto-fired this blueprint
    -- run (NULL for manual). The blueprint_run is the firing unit, so the
    -- replay fence (blueprint_runs_event_trigger_fence below) lives here —
    -- step conversations are not separately fenced (orchestrator-internal).
    triggering_event_id uuid,
    -- actor_agent_id is the bot that executes this blueprint run, resolved once
    -- at the delegation entry point and frozen here at mint (alongside
    -- creator_user_id / trigger_id — the "who/why" provenance axes). Every
    -- step conversation inherits it onto conversations.actor_agent_id, so the
    -- execution attribution is stable across all steps and immune to a later
    -- task-claim change (a user
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
CREATE INDEX idx_conversations_blueprint        ON public.conversations (blueprint_run_id, blueprint_step_index);
-- Team-scoped conversation reads: a team-marked surface narrows the list to
-- its own team, and both the page and the count beside it carry status /
-- attention predicates that are DERIVED — correlated subqueries over claims,
-- messages, artifacts and conversation_permissions — so they cost per
-- CANDIDATE row and no index can serve them directly. Putting team_id in the
-- access path is what keeps that per-row work off the rest of the org's
-- conversations. idx_conversations_org_status does not substitute: it covers
-- the stored status column, which those predicates never read. org_id leads to
-- match every WHERE in this store and the RLS policy beside them; the sort
-- (task_id, started_at DESC, id) leads elsewhere, so no sort key trails here.
CREATE INDEX idx_conversations_org_team         ON public.conversations (org_id, team_id);
-- Claim index for the run queue: the dispatcher claims the globally-oldest
-- eligible conversation (FIFO by started_at, id) under FOR UPDATE SKIP LOCKED.
-- Partial on the NULL arm of the needs-driving predicate — the mid-flight set,
-- which is where every fresh mint and every released-without-outcome claim
-- lands — so it spans only work that could need driving, mirroring
-- idx_event_queue_pending. The predicate's other arm (a parked 'open'
-- conversation woken by input) is served by idx_messages_undelivered.
CREATE INDEX idx_conversations_needs_driving ON public.conversations (started_at, id) WHERE (status IS NULL);
-- Placement tier-1 claim index: the two-tier claim's hot path is an executor
-- pulling its OWN preferred queued runs (preferred_executor_id = me), ordered
-- by started_at, id. This partial index makes that an indexed equality — the
-- point of stamping the rendezvous winner at enqueue instead of re-evaluating
-- the hash per claim — and lets the ORDER BY (preferred = me) DESC, started_at,
-- id resolve a tier-1 row without scanning the whole queued set. Same partial
-- WHERE status IS NULL as idx_conversations_needs_driving, so it too spans only
-- mid-flight work. Global-oldest (placement disabled) still uses
-- idx_conversations_needs_driving.
CREATE INDEX idx_conversations_queued_preferred ON public.conversations (preferred_executor_id, started_at, id) WHERE (status IS NULL);
-- Per-org fairness/cap: the claim computes each org's active-engagement count
-- once per statement to both filter orgs at their max_concurrent_runs cap and
-- order claimable rows fewest-active first. "Active" is now an unreleased
-- claim, so that GROUP BY reads claims directly and is served by
-- idx_claims_one_active (partial on released_at IS NULL, bounded by fleet
-- capacity) — there is no conversation-side status to index for it any more.
-- idx_conversations_org_status still covers the org-scoped outcome reads.
-- Replay fence: one event firing one trigger materializes
-- at most one blueprint_run. Partial WHERE triggering_event_id IS NOT NULL so
-- manual blueprint runs (NULL) never participate.
CREATE UNIQUE INDEX blueprint_runs_event_trigger_fence ON public.blueprint_runs (triggering_event_id, trigger_id) WHERE (triggering_event_id IS NOT NULL);
-- One active auto (trigger_type='event') run per task — the DB-enforced twin
-- of the router's in-process gate (HasActiveAutoRunForTaskSystem), which is
-- check-then-act, so two processes (or two goroutines racing a leader-failover
-- overlap) could both pass the check and each mint an active auto run on the
-- same task via different triggers.
--
-- The task is the unit, not the entity. A task is one situation that needs
-- attention — that is exactly what its (entity_id, event_type, dedup_key)
-- dedup index means — so two tasks on one pull request may each have an agent
-- in flight. Keying this on the entity also made a parked run load-bearing: a
-- conversation-level stop parks its conversation and leaves the blueprint
-- 'running' deliberately, so one stopped run held the only slot for the whole
-- entity and silently halted every future automated firing on it.
--
-- Still scoped to trigger_type='event'. A second manual run on one task is
-- deliberately allowed today; widening this to cover manual delegations
-- requires re-delegation to first replace its predecessor.
--
-- A violation here is a DEFERRAL, not the event/trigger fence's clean skip:
-- CreateRunIfNotFiredSystem translates it to db.ErrTaskBusyActiveAutoRun so
-- the caller queues the intent instead of dropping it as a replay.
CREATE UNIQUE INDEX blueprint_runs_one_active_auto_run_per_task ON public.blueprint_runs (org_id, task_id) WHERE (trigger_type = 'event'::text AND status = 'running'::text);

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
-- Composite FK mirrors conversations.triggering_event_id. NULL
-- triggering_event_id (manual blueprint runs) skips the check; org_id is
-- pinned so a cross-org event reference is impossible.
ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_triggering_event_id_org_id_fkey FOREIGN KEY (triggering_event_id, org_id) REFERENCES public.events(id, org_id);
-- Composite (id, org_id) FK like conversations_actor_agent_fkey: the actor must belong to
-- the run's own org. ON DELETE SET NULL keeps the audit row when the agent is
-- deleted (the run still happened; the actor pointer just goes blank).
ALTER TABLE ONLY public.blueprint_runs
    ADD CONSTRAINT blueprint_runs_actor_agent_fkey FOREIGN KEY (actor_agent_id, org_id) REFERENCES public.agents(id, org_id) ON DELETE SET NULL;

-- fired_run_id records the blueprint_run a firing produced (the firing unit
-- is the blueprint_run), so it references blueprint_runs — the spawner
-- returns the blueprint_run id synchronously at fire time, before any step
-- conversation row exists. Must live after blueprint_runs is created.
ALTER TABLE ONLY public.pending_firings
    ADD CONSTRAINT pending_firings_fired_run_id_org_id_fkey FOREIGN KEY (fired_run_id, org_id) REFERENCES public.blueprint_runs(id, org_id);

ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_blueprint_run_fkey FOREIGN KEY (blueprint_run_id, org_id) REFERENCES public.blueprint_runs(id, org_id);

-- conversation_memory.blueprint_run_id is denormalized from the run and grouped per
-- blueprint run. Composite (blueprint_run_id, org_id) FK, matching the
-- tenant-isolation pattern every other cross-table reference here uses: a
-- cross-org blueprint_run reference is structurally impossible at the DB level
-- — defense in depth beyond RLS, whose WITH CHECK only validates org_id, not
-- which blueprint_run the row points at. Lives in this block (not the conversation_memory
-- FK section above) because blueprint_runs is created here. ON DELETE SET NULL
-- is scoped to blueprint_run_id alone (the PG 15 column-list form) so deleting a
-- blueprint run keeps the durable memory row with its org_id intact; a bare SET
-- NULL would try to null org_id too (NOT NULL) and fail the delete.
ALTER TABLE ONLY public.conversation_memory
    ADD CONSTRAINT conversation_memory_blueprint_run_id_org_id_fkey FOREIGN KEY (blueprint_run_id, org_id) REFERENCES public.blueprint_runs(id, org_id) ON DELETE SET NULL (blueprint_run_id);

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
-- spawner). Per-command split mirrors the conversations table.
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
-- (hosted) App have NO org_github_apps row. The `_ref` columns hold
-- org_secrets key pointers (the actual client_secret / PEM / webhook signing
-- secret are written via SecretStore in the manifest backend) so
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
-- or rewrite another org's row. github_host below records WHICH GitHub each
-- row came from, so that assumption is checkable from the row instead of
-- through a join to the owning org's settings.
-- account_id is GitHub's numeric id for the installed-on account, stored as
-- text like every other GitHub-issued id in this schema (entities.source_id,
-- installation_id above). It is the half of the account's identity a rename
-- does not change, so credential resolution matches on it and account_login
-- is left to be what it is good at: the handle the UI renders. Nullable —
-- a row mirrored before this was captured carries NULL and resolves by login
-- exactly as it did; the next reconcile or webhook that names the account
-- fills it in.
CREATE TABLE public.org_github_app_installations (
    installation_id text NOT NULL,
    org_id uuid NOT NULL,
    account_type text NOT NULL,
    account_id text,
    account_login text NOT NULL,
    -- The GitHub deployment this installation lives on, normalized the way
    -- every other GitHub host key in this schema is (EffectiveGitHubHost, what
    -- user_github_identities.github_base_url stores): trailing slashes
    -- trimmed, an unconfigured base URL resolving to https://github.com, and
    -- any path component KEPT — a bare-origin derivation would strip a GHES
    -- mount path and key a different host than the identity rows for that same
    -- GitHub. The DEFAULT is that rule rather than a placeholder: an org that
    -- configured no base URL is on github.com. The CHECK keeps the column from
    -- degrading into the empty string, which is not a host anything can be
    -- compared against.
    github_host text DEFAULT 'https://github.com'::text NOT NULL,
    installed_at timestamp with time zone DEFAULT now() NOT NULL,
    removed_at timestamp with time zone,
    -- Suspension. An account owner can suspend an installation without
    -- uninstalling it: the grant stays, every token minted from it is refused.
    -- An installation is suspended iff suspended_at is non-NULL; suspended_by
    -- is the suspending GitHub login (display provenance, never a join key).
    -- Both are written by the installation webhook (suspend / unsuspend) and
    -- by the /app/installations reconcile, which converges a deployment that
    -- missed a delivery. Retained when the installation is later removed; a
    -- re-install clears them alongside removed_at.
    suspended_at timestamp with time zone,
    suspended_by text,
    -- Whether the grant is every repository on the account or an enumerated
    -- selection. GitHub reports it on the installation object
    -- (GET /app/installations and every installation webhook payload).
    --
    -- It is what says whether drift is even POSSIBLE for a given installation.
    -- Without it the org page cannot distinguish "nothing is drifting" from
    -- "drift is impossible here", and those need different copy: an 'all'
    -- installation reaches everything the account owns, so a tracked repository
    -- on that account can never sit outside the grant.
    --
    -- NULL means "not learned yet" — a row mirrored before the first reconcile
    -- — never "no selection". The CHECK admits NULL for exactly that reason and
    -- refuses anything outside GitHub's two values, so a typo cannot reach the
    -- column and be read later as a third state.
    repository_selection text,
    CONSTRAINT org_github_app_installations_account_type_check
        CHECK ((account_type = ANY (ARRAY['Organization'::text, 'User'::text]))),
    CONSTRAINT org_github_app_installations_github_host_check
        CHECK ((github_host <> ''::text)),
    CONSTRAINT org_github_app_installations_repository_selection_check
        CHECK ((repository_selection IS NULL
                OR repository_selection = ANY (ARRAY['all'::text, 'selected'::text])))
);

-- What github_host deliberately does NOT get is a
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


-- The reachable-repo cache: one mirror of "which repositories can this org
-- reach", populated by BOTH credential classes and read by every consumer —
-- the repository picker, the team-repos write gate, and the two App-tier drift
-- findings below.
--
-- The framing is the one the App-only grant mirror was built on and it is
-- unchanged: the reachable set is a CACHE of an external fact, rebuilt in full
-- per refresh, with no durable identity, surviving nothing. `repositories` is a
-- REGISTRY of TF entities that worktrees, entities, clone state and a hand-set
-- base_branch hang off. A reachable row is deliberately not a registry row —
-- the cache contains repositories nobody tracks, and that is the entire content
-- of the reach-without-purpose finding.
--
-- # Why the cache is not a column on repositories
--
-- The App-tier relationship really is 1:N — a repository belongs to one GitHub
-- account, an account has at most one installation of a given App, and a
-- workspace has exactly one App — so a nullable granted_installation_id on the
-- repository row would be relationally correct. It is still wrong, for four
-- reasons:
--
--  1. It changes what a repository row means. That table is the repositories TF
--     works with; the cache deliberately contains repositories nobody tracks,
--     which is the entire content of reach without purpose. Merging them makes
--     an "all repositories" installation on a 3,000-repo org mint 3,000 rows
--     that the profiler and every context loader then iterate.
--  2. Two lifecycles, two owners, contradictory answers to "exists".
--     Repository identity is TF-owned and durable — worktrees, entities, clone
--     state and a hand-set base_branch hang off it. Reachability is
--     GitHub-owned and ephemeral: revoked from a selective grant it must
--     vanish, while the repository row must survive because a task may
--     reference it.
--  3. The never-empty invariant gets harder. As its own table that invariant is
--     a scoped transaction over rows only the refresh writes. As a column it
--     is a wide UPDATE nulling a field across a table five subsystems write.
--  4. It re-introduces the provider-specific column the repository-identity
--     work designed out: an installation id is a GitHub App concept, and GitLab
--     has none.
--
-- # What both tiers being here buys
--
-- Two consumers were asking GitHub the same question live, differently, per
-- credential class:
--
--   * the picker (POST /api/github/repos/list) proxied GET /user/repos on a PAT
--     org and GET /installation/repositories on an App org. Because it was a
--     proxy list it could not report a total — and it could not for two
--     DIFFERENT reasons per tier, which is the tell that the shape was wrong.
--     /user/repos returns a bare array with no count at all;
--     /installation/repositories does return total_count, but one
--     installation's, and the picker serves the union across all of them.
--   * the team-repos write gate re-asked, and kept its own in-process,
--     per-(org, user) map to avoid asking twice — a cache that died on restart
--     and, in multi mode, lived on whichever pod happened to serve the picker
--     rather than the one that later served the write.
--
-- A table answers both, identically on both tiers, and can COUNT(*).
--
-- The two App-tier findings this table was first built for are unchanged, and
-- are still derivable from nothing else TF stores:
--
--   * reach without purpose — the App can reach a repository no team tracks,
--     which means TF holds write access to code nobody asked it to touch;
--   * scope drift — a team tracks a repository outside the grant, which means
--     that repository is silently unpollable.
--
-- Both are security findings rather than cosmetics, and both read only the
-- 'byo_app' rows: they are statements about an App's grant, and a PAT's reach
-- is not a grant TF holds.
CREATE TABLE public.reachable_repositories (
    org_id uuid NOT NULL,
    -- credential_class: which credential system observed this reach. It is part
    -- of the key rather than a derived attribute because an org that switches
    -- tiers has, for a moment, both answers on file, and serving the union of
    -- them would be serving a reach the org no longer has. Every read filters on
    -- the org's CURRENT class, so the other tier's rows are inert until their
    -- next refresh replaces them.
    credential_class text NOT NULL
        CONSTRAINT reachable_repositories_class_check
        CHECK (credential_class IN ('pat', 'byo_app')),
    -- installation_id / host: the credential INSTANCE the reach was observed
    -- through — the scope one refresh replaces atomically.
    --
    -- The App tier keeps the installation FK it always had, which is also how it
    -- keeps inheriting host scoping (an installation id is unique per GitHub
    -- deployment, and the installation row records which deployment that is) and
    -- how an uninstall still takes the mirror with it.
    --
    -- The PAT tier has no installation row to hang off, so it carries the host
    -- directly. That is not bookkeeping: an org that repoints GitHubBaseURL is
    -- looking at a different deployment's repositories, and a mirror that did
    -- not record which one it read would answer the new host with the old host's
    -- set.
    installation_id text,
    host text,
    -- source: the provider that issued this repository, carried so the join to
    -- the registry is written against the same key the registry is keyed under
    -- (org_id, source, lower(owner), lower(repo)) rather than against a literal.
    -- Not part of this table's own key: every row here is reached through a
    -- GitHub credential, so 'github' is the only value it can hold.
    source text DEFAULT 'github'::text NOT NULL,
    owner text NOT NULL,
    repo text NOT NULL,
    -- external_id: the provider's own repository id. It is what lets a cache
    -- entry and a registry row be matched by id even when their slugs
    -- momentarily disagree mid-rename. Nullable, and NULL is a supported state
    -- rather than a gap to backfill: a row without an id is matched by slug
    -- exactly as it would be anyway.
    external_id text,
    -- The picker's display fields, mirrored because the picker is now served
    -- from here rather than proxied. Both enumerations return whole repository
    -- objects, so recording them costs nothing beyond the width of the row, and
    -- the alternative — a slug-only cache plus a live fetch to decorate it —
    -- would put the upstream call back on the read this table exists to take it
    -- off. Empty string rather than NULL throughout: these are display strings
    -- with no meaningful absent state, and '' renders the same as a missing
    -- field without every reader having to say so.
    description text DEFAULT ''::text NOT NULL,
    language text DEFAULT ''::text NOT NULL,
    html_url text DEFAULT ''::text NOT NULL,
    -- pushed_at: GitHub's own timestamp string, stored verbatim as text. It is
    -- passed through to the client and never compared, sorted on, or arithmetic
    -- done to here — parsing it into a real timestamp would be claiming a
    -- precision this column does not need and inviting a NULL for the repos that
    -- have never been pushed to.
    pushed_at text DEFAULT ''::text NOT NULL,
    private boolean DEFAULT false NOT NULL,
    -- observed_at: when the refresh that wrote this row ran. Unlike the App-only
    -- mirror this replaces, it IS a staleness gate here as well as display
    -- provenance: the TTL that bounds how often a 3,000-repo account is
    -- re-enumerated reads it, and so does the write gate deciding whether to
    -- trust the cache or probe upstream. One refresh replaces a whole scope
    -- atomically, so every row in one scope carries the same instant.
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    -- Exactly one scope column per class, and it is the database that says so.
    -- The two are alternatives, never both and never neither: a row with both
    -- would have two answers to "what does one refresh replace", and a row with
    -- neither could not be replaced at all.
    CONSTRAINT reachable_repositories_scope_check
        CHECK ((credential_class = 'byo_app' AND installation_id IS NOT NULL AND host IS NULL)
            OR (credential_class = 'pat'     AND installation_id IS NULL     AND host IS NOT NULL))
);

ALTER TABLE ONLY public.reachable_repositories
    ADD CONSTRAINT reachable_repositories_installation_fkey
    FOREIGN KEY (org_id, installation_id)
    REFERENCES public.org_github_app_installations (org_id, installation_id) ON DELETE CASCADE;

-- The natural key, per class, folded. Folded for the reason repositories_identity
-- states: GitHub identifiers are case-insensitive, so a case-sensitive index
-- behind a case-insensitive guard admits duplicates whenever two writers race —
-- neither sees the other's uncommitted row and their keys then differ. The
-- database has to be the thing that refuses.
--
-- Two partial indexes rather than one over coalesce(installation_id, host): the
-- scope columns are genuinely different columns with different nullability, and
-- a coalesced key would silently collide an installation whose id equals another
-- org's host string.
CREATE UNIQUE INDEX reachable_repositories_app_identity
    ON public.reachable_repositories USING btree (org_id, installation_id, lower(owner), lower(repo))
    WHERE credential_class = 'byo_app';

CREATE UNIQUE INDEX reachable_repositories_pat_identity
    ON public.reachable_repositories USING btree (org_id, host, lower(owner), lower(repo))
    WHERE credential_class = 'pat';

-- The registry join, and the drift queries that run over it. Deliberately the
-- same shape as repositories_identity — (org_id, source, lower(owner),
-- lower(repo)) — so the two sides of "is this reachable repository tracked?" are
-- keyed identically and either can drive the join.
CREATE INDEX reachable_repositories_registry_join
    ON public.reachable_repositories USING btree (org_id, source, lower(owner), lower(repo));

-- The picker's own read: one org's current class, ordered by folded slug. The
-- order is part of the index because offset paging over an unordered set drops
-- and repeats rows, and the folded slug is a total order (the per-class unique
-- index above proves no two rows tie on it).
CREATE INDEX reachable_repositories_picker
    ON public.reachable_repositories USING btree (org_id, credential_class, lower(owner), lower(repo));

ALTER TABLE public.reachable_repositories ENABLE ROW LEVEL SECURITY;

-- Same posture as the installation rows the App tier's half hangs off: the cache
-- is GitHub-side state written exclusively by the refresh on the admin pool, so
-- app-pool members read it and never write it. There is no user gesture that
-- adds or removes a reachable entry — that happens on GitHub, and TF discovers
-- it. The picker's force-refresh control is process control over the refresh
-- pass, not a write to this table.
CREATE POLICY reachable_repositories_select ON public.reachable_repositories FOR SELECT TO tf_app
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));

CREATE POLICY reachable_repositories_insert ON public.reachable_repositories FOR INSERT TO tf_app
    WITH CHECK (false);

CREATE POLICY reachable_repositories_update ON public.reachable_repositories FOR UPDATE TO tf_app
    USING (false);

CREATE POLICY reachable_repositories_delete ON public.reachable_repositories FOR DELETE TO tf_app
    USING (false);

GRANT ALL ON TABLE public.reachable_repositories TO postgres;
GRANT ALL ON TABLE public.reachable_repositories TO anon;
GRANT ALL ON TABLE public.reachable_repositories TO authenticated;
GRANT ALL ON TABLE public.reachable_repositories TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.reachable_repositories TO tf_app;


-- The refreshes themselves, one row per scope. Its whole job is to make "we have
-- not looked yet" representable, because the repository rows cannot: a scope that
-- genuinely reaches NOTHING writes zero of them, so row count alone reads a
-- legitimately empty grant as an un-run refresh — the picker would say
-- "discovering repositories…" forever and kick a refresh on every open, which is
-- exactly the unbounded enumeration the TTL exists to prevent.
--
-- No FK, deliberately. The scope column holds an installation id for one class
-- and a host for the other, so there is no single parent to point at; the two
-- writers that retire an installation (the store's soft-removal, and the
-- standalone clear) delete the matching row in the same transaction as the
-- repository rows.
--
-- What it does NOT try to represent is a scope that has never been refreshed at
-- all — a freshly-installed installation has no row here, so it does not drag the
-- org's staleness back. That case is covered by the forced refresh every
-- credential change fires, which is the mechanism for reach moving without a
-- timestamp moving, and it is why nothing here has to enumerate the scopes it
-- expects to see.
--
-- Same RLS posture as the repository rows it accompanies: app-pool members read
-- it, every app-pool write is denied, because a refresh is not a user gesture.
CREATE TABLE public.reachable_scopes (
    org_id uuid NOT NULL,
    credential_class text NOT NULL
        CONSTRAINT reachable_scopes_class_check
        CHECK (credential_class IN ('pat', 'byo_app')),
    -- scope: the credential instance one refresh replaces — the installation id
    -- for byo_app, the host for pat. Recorded as opaque text: this table records
    -- THAT a scope was refreshed and when, and never joins on it.
    scope text NOT NULL,
    refreshed_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.reachable_scopes
    ADD CONSTRAINT reachable_scopes_pkey PRIMARY KEY (org_id, credential_class, scope);

ALTER TABLE public.reachable_scopes ENABLE ROW LEVEL SECURITY;

CREATE POLICY reachable_scopes_select ON public.reachable_scopes FOR SELECT TO tf_app
    USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));

CREATE POLICY reachable_scopes_insert ON public.reachable_scopes FOR INSERT TO tf_app
    WITH CHECK (false);

CREATE POLICY reachable_scopes_update ON public.reachable_scopes FOR UPDATE TO tf_app
    USING (false);

CREATE POLICY reachable_scopes_delete ON public.reachable_scopes FOR DELETE TO tf_app
    USING (false);

GRANT ALL ON TABLE public.reachable_scopes TO postgres;
GRANT ALL ON TABLE public.reachable_scopes TO anon;
GRANT ALL ON TABLE public.reachable_scopes TO authenticated;
GRANT ALL ON TABLE public.reachable_scopes TO service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.reachable_scopes TO tf_app;


-- GitHub webhook delivery dedup: github_webhook_deliveries. The receiver read
-- X-GitHub-Delivery only to decorate the published event's metadata — never
-- persisted, never checked — so a redelivered copy re-ran the installation
-- upsert and re-published to the bus. GitHub does not auto-retry a failed
-- delivery, but it does redeliver on demand (the Redeliver button; POST
-- /app/hook/deliveries/{delivery_id}/attempts, 30-day window), which is exactly
-- the recovery path an operator reaches for after an outage. A redelivery is
-- ordinary traffic, not an anomaly.
--
-- Keyed (installation_id, delivery_id), NOT (org_id, delivery_id). The
-- org-keyed form cannot be written before the tenant is known, and "before the
-- tenant is known" is precisely the shape a shared-App receiver has — one
-- static URL, one deployment secret, routing installation.id -> org_id and
-- dropping an unmapped delivery before any side effect. The installation-keyed
-- form costs nothing today and does not have to be re-keyed later. A delivery
-- carrying no installation object (github_app_authorization, for one) keys on
-- the delivery id alone and stores '' in this column: the empty string rather
-- than NULL, because a NULL in a primary key defeats the uniqueness the table
-- exists for.
--
-- Retention is 72h, pruned opportunistically on every insert — long past the
-- burst of redeliveries that follows an outage, and deliberately short of
-- GitHub's 30-day redelivery window. A copy redelivered a week later re-applies;
-- that is an operator deliberately replaying one delivery, not the automatic
-- retry this table absorbs.
--
-- Admin-pool-only / system table, same posture as slack_event_deliveries /
-- poll_readiness: RLS enabled with NO policy (deny-by-default to non-BYPASSRLS
-- roles) + REVOKE ALL from PUBLIC and the app roles. The receiver is pre-auth —
-- it holds a trusted org_id from the URL path and no request claims for a
-- policy to gate on — and there is no app-pool caller.
CREATE TABLE public.github_webhook_deliveries (
    installation_id text NOT NULL,
    delivery_id     text NOT NULL,
    received_at     timestamp with time zone NOT NULL DEFAULT now()
);

ALTER TABLE ONLY public.github_webhook_deliveries
    ADD CONSTRAINT github_webhook_deliveries_pkey PRIMARY KEY (installation_id, delivery_id);

-- The prune is the only query that doesn't go through the primary key. Unlike
-- the Slack dedup table, this one sees every push / check_run / comment across
-- every tracked repo, so a sequential scan per delivery is not a safe assumption
-- to ship; with this index the usual "nothing to prune" case is a range probe
-- that matches nothing.
CREATE INDEX github_webhook_deliveries_received_at_idx
    ON public.github_webhook_deliveries (received_at);

ALTER TABLE public.github_webhook_deliveries ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.github_webhook_deliveries FROM PUBLIC;
REVOKE ALL ON public.github_webhook_deliveries FROM anon, authenticated, service_role;


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
    processed_at timestamp with time zone,
    -- executor_id / boot_epoch (TFAC-578) are claims.executor_id/boot_epoch's
    -- twins: stamped atomically at claim (pending -> processing) so
    -- ResetProcessing can self-sweep only its own instance's orphaned rows
    -- (executor_id = self AND boot_epoch < the current boot's epoch), never
    -- a live sibling's claimed-but-still-processing row.
    executor_id  text,
    boot_epoch   bigint,
    -- traceparent carries the W3C trace context of whoever enqueued this
    -- row, so the routing of an event ties back to the poll cycle that
    -- emitted it. The queue row is a message envelope — the one place in
    -- this schema a trace id belongs (the Kafka-header / Sidekiq-payload
    -- pattern), and its retention already matches a trace backend's, since
    -- done rows are pruned after a week. Domain tables (events, tasks,
    -- conversations) deliberately carry none: they outlive traces by years,
    -- so such a column would mostly point at spans that no longer exist,
    -- and domain ids as span attributes give the reverse join for free.
    --
    -- NULL is the normal state, not a degraded one: a row enqueued with
    -- tracing off, from an untraced path, or by a process with no exporter
    -- configured has no context to carry, and routes identically — it just
    -- gets no link. The consumer LINKS to this context rather than
    -- descending from it: one poll cycle emits N events routed later, and
    -- possibly on another pod, so parenting would build a single sprawling
    -- multi-pod trace instead of N navigable ones.
    traceparent  text
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


-- Per-org Atlassian OAuth (3LO) app registration — the (client_id,
-- client_secret) of the Atlassian app the per-user "Connect Jira"
-- authorize/token flow runs against. Mirrors org_github_apps but far simpler:
-- an Atlassian OAuth app has no installations, no PEM, and no webhook secret,
-- only the OAuth client credentials.
--
-- This row is the per-org OVERRIDE in credential precedence: an org with no
-- row uses the deployment first-party app (read from operator config). The
-- client_secret_ref column holds an org_secrets key pointer (the actual
-- client_secret is written via SecretStore) so the app secret never
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
-- Secrets are encrypted app-side (AES-256-GCM) rather than delegated to
-- Postgres: a Vault-style design keeps its root key in the Postgres
-- container filesystem, not the pg-data volume, so a routine container
-- recreate would regenerate the key and permanently lose every stored
-- secret. The TF binary instead encrypts org/user integration secrets
-- with TF_SECRET_ENCRYPTION_KEY (a .env key, same model as
-- TF_SESSION_ENCRYPTION_KEY) and stores opaque ciphertext in this normal
-- RLS-gated table. A DB dump then yields only AES ciphertext, and there
-- is no Postgres-side key to lose.

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
    -- share one table, discriminated by user_id.
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

-- Two permissive policies, scoped to tf_app. Permissive policies combine
-- with OR, so the effective gate is:
--   (org-scope row AND org matches) OR (own user-scope row AND org matches)
-- The admin pool (supabase_admin) bypasses RLS entirely and is the only
-- path to the *System reads/writes — it trusts the explicit org_id /
-- user_id args.

-- Org-scope rows (user_id IS NULL): gate on org only.
CREATE POLICY org_secrets_org ON public.org_secrets
    FOR ALL
    TO tf_app
    USING (user_id IS NULL AND org_id = tf.current_org_id())
    WITH CHECK (user_id IS NULL AND org_id = tf.current_org_id());

-- Per-user rows: gate on org AND user. This is the load-bearing
-- cross-user boundary — a session acting as user A cannot see user B's
-- secret.
CREATE POLICY org_secrets_user ON public.org_secrets
    FOR ALL
    TO tf_app
    USING (user_id = tf.current_user_id() AND org_id = tf.current_org_id())
    WITH CHECK (user_id = tf.current_user_id() AND org_id = tf.current_org_id());

-- supabase_admin's ALTER DEFAULT PRIVILEGES auto-grants public-schema
-- tables to anon/authenticated/service_role at CREATE time. Strip them: a
-- secrets table must be reachable only by tf_app, even if RLS were ever
-- misconfigured. supabase_admin (admin pool) owns the table as superuser
-- and bypasses RLS for the *System methods.
REVOKE ALL ON public.org_secrets FROM PUBLIC;
REVOKE ALL ON public.org_secrets FROM anon, authenticated, service_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.org_secrets TO tf_app;


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

-- The tenant-scoped FK target for target_team_id below is teams_id_org_id_key,
-- declared with the teams table's other constraints because team_github_repos
-- references it too and does so earlier in this file.

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
-- the rest of the schema uses (agents, entities, conversations, tasks,
-- blueprint_runs, teams). RLS gates the org_id *column* on write, but only
-- this FK keeps a domain's connection_id in the same org as its org_id.
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
-- (idx_tasks_entity, idx_conversations_task, ...).
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


-- seat_claims: the per-seat license ledger — one row per (billing period, user)
-- = one committed seat held for that period. This is the ATOMIC seat-claim that
-- makes per-seat enforcement race-free: the login critical edge counts distinct
-- claimants for the period AND inserts this row in ONE serialized transaction (a
-- per-period advisory lock), so concurrent first-logins can never both read an
-- under-cap count and both slip past the maxSeats cap. The PK (period, user_id)
-- is the idempotency + uniqueness key — a returning claimant's insert is a no-op,
-- and count(*) for a period is the billable distinct-user total.
--
-- Deployment-wide (no org_id): a per-seat cap is a deployment property (self-host
-- licensing is auto-all) and a human is one seat regardless of org membership.
-- Admin-pool-only system table, same posture as auth_events: RLS deny-by-default,
-- REVOKEd from the app roles; the superuser/admin pool — which owns the JWT-less
-- login-edge writes — bypasses RLS and does all I/O. SQLite is N=1 with no GoTrue
-- login, so the table is parity-only there.
CREATE TABLE public.seat_claims (
    period     text NOT NULL,
    user_id    uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

-- PK doubles as the per-period count index (leftmost-prefix scan on period).
ALTER TABLE ONLY public.seat_claims
    ADD CONSTRAINT seat_claims_pkey PRIMARY KEY (period, user_id);

-- ON DELETE CASCADE: a deleted user frees their held seats. Unlike the
-- auth_events audit trail (NO ACTION, preserved), this is a transient ledger.
ALTER TABLE ONLY public.seat_claims
    ADD CONSTRAINT seat_claims_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

ALTER TABLE public.seat_claims ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.seat_claims FROM PUBLIC;
REVOKE ALL ON public.seat_claims FROM anon, authenticated, service_role;




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
-- The stage-1 scope gate for slack:message routing (a sibling leaf) and the
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
-- epic's scoping decision 5. Self-contained block appended at the end of Up:
-- every FK target (orgs, teams, users, events_catalog) already exists.
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
-- prompts/blueprints), joined to conversations.prompt_id for kind=prompt
-- listings and blueprint_runs.blueprint_id for kind=blueprint listings.
--
-- Written exclusively by MarketplaceStore.RecomputeStatsSystem (admin pool,
-- bypasses RLS) — a cross-team, cross-run aggregate can't be computed at
-- request/read time without an RLS-crossing query, which the CLAUDE.md
-- multi-mode read-scoping standing rule forbids. Browse/detail reads join
-- this table like any other listing column, under ordinary RLS. No
-- INSERT/UPDATE/DELETE policy is declared below: only the admin pool ever
-- writes here (same shape as events_catalog).
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


-- instances: the fleet membership registry every TF process registers
-- into at boot and refreshes via periodic heartbeat. Role-neutral name on
-- purpose — every TF process registers (control pods too, for
-- deployment-wide visibility: versions, lease holder, health), not just
-- executors; "executor" stays the *role* name everywhere else
-- (claims.executor_id keeps its shipped meaning: the id of the instance
-- driving a claimed engagement).
--
-- id is text, not uuid: it is minted OUTSIDE Postgres, by a file under
-- <TF_STATE_ROOT>/instance-id (internal/instance) read/created once per
-- state root and re-read on every boot — identity travels with the state
-- root/volume, not the host or the process. Register is the atomic
-- boot-registration upsert (INSERT ... ON CONFLICT (id) DO UPDATE SET
-- boot_epoch = instances.boot_epoch + 1 ...) so a restart with the same id
-- keeps the id and bumps the epoch monotonically regardless of volume
-- snapshot/restore/clone weirdness.
--
-- max_runs / active_runs / mem_total_mb / mem_available_mb / dispatch_gated
-- move capacity + admission state that used to be process-local onto this
-- heartbeat row. Meaningful only for executor-capable roles; NULL on
-- pure-control rows.
--
-- Admin-pool-only / system table, same posture as auth_events /
-- system_llm_runs: no org_id (a fleet member isn't tenant data), so an
-- org-scoped RLS policy can't gate it and there's nothing for the app pool
-- to read here anyway. RLS enabled with NO policy (deny-by-default to
-- non-BYPASSRLS roles) + REVOKE ALL from PUBLIC + the app roles; the
-- superuser/admin pool bypasses RLS and does all I/O.
CREATE TABLE public.instances (
    id                 text NOT NULL,
    role               text NOT NULL,
    version            text NOT NULL,
    boot_epoch         bigint NOT NULL,
    started_at         timestamp with time zone NOT NULL,
    last_heartbeat_at  timestamp with time zone NOT NULL,
    draining           boolean NOT NULL DEFAULT false,
    max_runs           integer,
    active_runs        integer,
    mem_total_mb       integer,
    mem_available_mb   integer,
    dispatch_gated     boolean,
    labels             jsonb,
    -- pubkey (TFAC-614): this boot's ephemeral X25519 public key (base64),
    -- minted in-memory at process start and never persisted — a restart
    -- mints a fresh one. Written ONLY by Register's initial INSERT and its
    -- ON CONFLICT re-stamp (every restart gets a new key alongside the new
    -- boot_epoch); the heartbeat never touches it, same non-write rule as
    -- draining/labels, but for the opposite reason — those survive a
    -- restart on purpose, this one must NOT (an old-epoch key sealing a
    -- bundle nobody can decrypt anymore is exactly the crash window
    -- TFAC-614's epoch check exists to catch). NULL for a control/all row
    -- that never provisions or claims conversations.
    pubkey             text
);

ALTER TABLE ONLY public.instances
    ADD CONSTRAINT instances_pkey PRIMARY KEY (id);

ALTER TABLE public.instances ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.instances FROM PUBLIC;
REVOKE ALL ON public.instances FROM anon, authenticated, service_role;

-- leases (TFAC-583): the leader-election primitive for the horizontal-
-- scaling control plane. One row today — name='background-brain' — but the
-- schema is deliberately name-keyed (not a singleton) so per-org brain
-- sharding (P4) is just more rows, not a migration.
--
-- term is the fencing token: +1 on every acquisition, never on a renewal.
-- Acquisition is `UPDATE ... SET holder_id=me, term=term+1, acquired_at=now(),
-- renewed_at=now() WHERE renewed_at < now() - TTL`; renewal is `UPDATE ...
-- SET renewed_at=now() WHERE name=$1 AND holder_id=me AND term=$myterm` — a
-- renewal matching 0 rows means the lease was taken by someone else, and the
-- holder demotes immediately. All cross-node comparisons here run in DB time
-- (renewed_at < now() - TTL); a holder's own self-demotion decision runs on
-- its OWN monotonic clock instead (internal/lease), never compared against
-- this table's timestamps.
--
-- Bootstrap race: the row may not exist on first boot. Callers INSERT ...
-- ON CONFLICT (name) DO NOTHING first (seeding holder_id='', renewed_at far
-- in the past so it's immediately acquirable), then run the normal
-- acquisition UPDATE — two pods racing the insert converge on exactly one
-- holder via the UPDATE's row lock, never a duplicate row.
--
-- Admin-pool-only / system table, same posture as instances: no org_id (a
-- lease isn't tenant data), RLS enabled with NO policy (deny-by-default to
-- non-BYPASSRLS roles) + REVOKE ALL from PUBLIC + the app roles. No
-- SQLite counterpart — local mode (and TF_ROLE=all) never elects, the
-- single process always self-holds (internal/lease's Static branch), so
-- there is zero lease I/O to back with a table there.
CREATE TABLE public.leases (
    name         text NOT NULL,
    holder_id    text NOT NULL,
    term         bigint NOT NULL,
    acquired_at  timestamp with time zone NOT NULL,
    renewed_at   timestamp with time zone NOT NULL
);

ALTER TABLE ONLY public.leases
    ADD CONSTRAINT leases_pkey PRIMARY KEY (name);

ALTER TABLE public.leases ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.leases FROM PUBLIC;
REVOKE ALL ON public.leases FROM anon, authenticated, service_role;

-- poll_readiness (TFAC-583): the org-scoped, DB-backed replacement for what
-- used to be two in-memory, leader-coupled Server fields — the "config took
-- effect" one-shot announce toast and the /api/jira/stock readiness gate
-- (internal/app's old `announcer` type + server.go's jiraPollMu/
-- jiraRestartedAt/jiraLastPollAt). Both were process-local state written by
-- whichever pod ran the poller; under the control/standby split that pod is
-- not necessarily the one serving a given /api/jira/stock request, so the
-- state has to live somewhere every control pod's API can read — this table.
--
-- One row per (org_id, source) — source is 'github' or 'jira'. restarted_at
-- is stamped when a config change restarts that source's poller (clearing
-- readiness until a fresh completion lands); last_poll_at is stamped on
-- every completed cycle. Ready = last_poll_at IS NOT NULL AND (restarted_at
-- IS NULL OR last_poll_at > restarted_at) — see
-- internal/db/postgres/poll_readiness.go. announce_pending is the one-shot
-- toast flag, consumed atomically (UPDATE ... WHERE announce_pending
-- RETURNING) so a poll completion observed by two racing pods can't double-
-- fire the toast.
--
-- App-pool readable: a control pod's /api/jira/stock handler reads this for
-- an orgID it already resolved+authorized via session claims (not a
-- browsable RLS surface), so the store methods take an explicit orgID and
-- run on the admin pool like the instances table, rather than adding a new
-- per-row RLS policy for what is operational state, not tenant content.
CREATE TABLE public.poll_readiness (
    org_id            text NOT NULL,
    source            text NOT NULL,
    restarted_at      timestamp with time zone,
    last_poll_at      timestamp with time zone,
    announce_pending  boolean NOT NULL DEFAULT false
);

ALTER TABLE ONLY public.poll_readiness
    ADD CONSTRAINT poll_readiness_pkey PRIMARY KEY (org_id, source);

ALTER TABLE public.poll_readiness ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.poll_readiness FROM PUBLIC;
REVOKE ALL ON public.poll_readiness FROM anon, authenticated, service_role;


-- org_event_sources: declared per-(org, source) POLICY — today, whether an org
-- admin has turned a source off: it is not polled, produces no events, mints no
-- tasks, and resolves no credential for an agent. The credential itself is left
-- bound, which is what makes this reversible where unbinding it is not.
-- Whether a given source may be turned off at all is a vocabulary question the
-- application answers (internal/eventsource.Disableable), not a constraint
-- here: kind is FK-free on purpose, and a row naming a source that has no off
-- switch is read as inert rather than obeyed.
--
-- Its own table, not a column on org_settings, for two reasons. The source
-- vocabulary is a RUNTIME REGISTRY (internal/eventsource.Register lets a source
-- outside core declare itself), so a column per source would make every new
-- source a DDL change in both dialects and would leave a source core holds no
-- symbol for with nowhere to record the fact. And org_settings is a whole-row
-- read-modify-write behind a `version` optimistic-concurrency token, whose
-- consequence this schema already records at github_credential_class: a
-- surgical single-column write from a different surface either fights that
-- token or mints a second bypass of it.
--
-- Not poll_readiness either, though that table is keyed (org_id, source) too:
-- its rows are OBSERVED RUNTIME STATE, written by the poll tracker and pruned
-- when an org stops polling. This is declared admin policy, and a prune of
-- bookkeeping that silently re-enabled a source is a bad failure mode to design
-- in.
--
-- `disabled` is an explicit column rather than presence-is-off. A bare
-- disabled-sources table would be tighter for this one fact, but the row was
-- expected to grow — base_url and poll_interval below are that growth — and
-- the moment a row exists for a configuration reason "row present" stops
-- meaning "off". With more than one fact on the row, an ABSENT row reads
-- honestly as "no per-source overrides recorded" — which is what every
-- deployment has until an admin opts in, so the derivation stays byte-identical
-- to a build without this table.
--
-- kind carries no foreign key on purpose: the vocabulary is a registry, not a
-- table, so a row naming a source this build does not carry is inert (nothing
-- resolves it) rather than invalid. disabled_by carries none either — the
-- who-did-what trail lives in access_changes, and a departed admin's deletion
-- must not quietly rewrite the org's policy.
--
-- base_url / poll_interval are the four org_settings columns
-- (github_base_url, github_poll_interval, jira_base_url, jira_poll_interval)
-- collapsed onto one column pair per uniform setting, keyed by kind instead of
-- by a column-name prefix. Both are NULLABLE, and NULL means "no override
-- recorded" here too — never "use a default". Not every kind may carry a
-- non-null base_url: it names where a credential is sent, so it is only
-- meaningful for a source with a self-host story (internal/eventsource's
-- registration declares this; GitHub Enterprise Server and Jira Data Center
-- have one, Slack does not and the write path refuses a value for it). Not
-- every kind may carry a meaningful poll_interval either — a push source has
-- no cadence. These two columns are owned by OrgsStore (the settings writer);
-- disabled / disabled_at / disabled_by above stay owned by
-- OrgEventSourceStore's SetDisabled — same row, disjoint columns, same split
-- org_settings already has between UpdateSettings and SetGitHubCredentialClass.
CREATE TABLE public.org_event_sources (
    org_id        uuid NOT NULL,
    kind          text NOT NULL,
    disabled      boolean DEFAULT false NOT NULL,
    disabled_at   timestamp with time zone,
    disabled_by   uuid,
    base_url      text,
    poll_interval interval
);

ALTER TABLE ONLY public.org_event_sources
    ADD CONSTRAINT org_event_sources_pkey PRIMARY KEY (org_id, kind);

ALTER TABLE ONLY public.org_event_sources
    ADD CONSTRAINT org_event_sources_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

ALTER TABLE public.org_event_sources ENABLE ROW LEVEL SECURITY;

-- Member SELECT, admin write — the same split as org_settings, and here RLS
-- really can express the write gate (a source belongs to no team, so the
-- org-wide-mutation rule reduces to plain org admin). The handler's
-- RequireOrgAdmin is still the enforcement point callers see; this is the
-- backstop underneath it. A member must be able to READ it, because that is
-- what renders why their own event handler is sitting inert.
CREATE POLICY org_event_sources_select ON public.org_event_sources FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));
CREATE POLICY org_event_sources_insert ON public.org_event_sources FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));
CREATE POLICY org_event_sources_update ON public.org_event_sources FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));
CREATE POLICY org_event_sources_delete ON public.org_event_sources FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

REVOKE ALL ON public.org_event_sources FROM PUBLIC;
REVOKE ALL ON public.org_event_sources FROM anon, authenticated, service_role;

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.org_event_sources TO tf_app;


-- model_availability: per (org, provider, model) evidence that the org's
-- credentials can actually invoke that model, established by spending one
-- minimal real request on it.
--
-- A probe rather than a control-plane lookup, because no control-plane API
-- answers the question uniformly. Anthropic's /v1/models reflects the key's
-- access; Bedrock's ListFoundationModels reports what the REGION carries, not
-- what the account was granted, and returns base model ids rather than the
-- inference-profile ids that are the only invocable spelling for the current
-- Claude models. Its per-model entitlement call needs its own IAM action on a
-- different service endpoint and answers a weaker question. A probe needs no
-- permission the run does not already need — if the probe fails, the run would
-- have failed too — and it tests the exact id that will be sent on the wire.
--
-- Two stored states and a meaningful absence:
--
--   green   a probe succeeded. Terminal for automatic transitions: only a
--           manual re-test writes the row again, and a re-test that is refused
--           moves it to red.
--   red     the provider ANSWERED and refused (401/403, AccessDeniedException,
--           model-not-found). Cleared only by a later successful test.
--   absent  never probed, or every attempt so far was inconclusive — a 5xx, a
--           timeout, a dial failure. Those write NOTHING, so one bad provider
--           minute can never hide a model that works.
--
-- There is no TTL column and nothing sweeps this table. A green row is a fact
-- about a request that really succeeded, and re-checking would spend money to
-- learn something a dispatch failure already reports loudly. Every transition
-- is caused by a user gesture — connecting a credential, or pressing "test
-- connection" — which is also why there is no writer without request claims,
-- and why this table needs no admin-pool arm.
--
-- provider is part of the primary key rather than derived from model_key: what
-- the row establishes is that a CREDENTIAL PATH works, the eager sweep
-- addresses rows by provider, and a row must still say which credential it was
-- tested against after the build stops offering the key.
--
-- model_key carries no foreign key, for the same reason org_event_sources.kind
-- carries none: the catalog is a compiled-in file, not a table, so a row for a
-- key this build no longer offers is inert (nothing iterates to it) rather
-- than invalid. Its length is unconstrained beyond the CHECK: a Bedrock
-- inference-profile id is already 40+ characters and an ARN is longer.
CREATE TABLE public.model_availability (
    org_id     uuid NOT NULL,
    provider   text NOT NULL,
    model_key  text NOT NULL,
    state      text NOT NULL,
    checked_at timestamp with time zone NOT NULL,
    detail     text DEFAULT ''::text NOT NULL
);

ALTER TABLE ONLY public.model_availability
    ADD CONSTRAINT model_availability_pkey PRIMARY KEY (org_id, provider, model_key);

ALTER TABLE ONLY public.model_availability
    ADD CONSTRAINT model_availability_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;

-- The stored vocabulary is exactly two values. An inconclusive probe has no
-- spelling here on purpose — it writes no row — so a third value arriving
-- would mean some caller invented one, and the CHECK is what stops that from
-- reaching a picker as an unrenderable badge.
ALTER TABLE ONLY public.model_availability
    ADD CONSTRAINT model_availability_state_check CHECK ((state = ANY (ARRAY['green'::text, 'red'::text])));

ALTER TABLE public.model_availability ENABLE ROW LEVEL SECURITY;

-- Member SELECT, admin write. The read is member-level because its only
-- consumer is the model catalog read, which is itself member-level: a team
-- admin who cannot read org settings still has to see why a model in their
-- picker is badged unavailable. The write is admin because it SPENDS THE ORG'S
-- MONEY — every row here is the receipt for a real, billed request — and
-- because it is written from the same settings surface that binds the
-- credential. A model is in no team's tracked set, so the org-wide-mutation
-- rule reduces to plain org admin and RLS can express the whole gate; the
-- handler's RequireOrgAdmin is still the enforcement point callers see.
CREATE POLICY model_availability_select ON public.model_availability FOR SELECT USING (((org_id = tf.current_org_id()) AND tf.user_has_org_access(org_id)));
CREATE POLICY model_availability_insert ON public.model_availability FOR INSERT WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));
CREATE POLICY model_availability_update ON public.model_availability FOR UPDATE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id))) WITH CHECK (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));
CREATE POLICY model_availability_delete ON public.model_availability FOR DELETE USING (((org_id = tf.current_org_id()) AND tf.user_is_org_admin(org_id)));

REVOKE ALL ON public.model_availability FROM PUBLIC;
REVOKE ALL ON public.model_availability FROM anon, authenticated, service_role;

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.model_availability TO tf_app;


-- placement_overrides (TFAC-587, spec §6.1): human intent that wins over the
-- computed capacity-weighted rendezvous order for a single placement key — a
-- manual pin, or a hot-key replica count, and nothing else (drain lives on
-- the executor's own instances row). Checked before the hash; expected to
-- stay nearly empty, because the rendezvous hash is the default map and
-- needs no rows at all. This is the "table for the exceptions" half of the
-- design's "hash for the map, table for the exceptions" — the mutable state
-- deliberately confined to what an operator explicitly types, never a
-- rebalancer-written routing table.
--
-- Keyed (org_id, key_kind, key_value): key_kind is 'repo' (a delegation key,
-- key_value "owner/repo"). org_id is a plain text column (not a uuid FK) matching the rendezvous
-- key's own org fold and the instances-family convention — a pin is
-- operational intent, not tenant content. pinned_instance_id / replicas are
-- the two intents; a pin wins when both are set. Neither is FK-validated
-- (a pin to a not-yet-registered or since-retired instance is legal and
-- self-heals via the claim's aging tier, exactly like the rendezvous stamp).
--
-- Admin-pool-only / system table, same posture as instances / poll_readiness:
-- the explainer endpoint reads it for an already-authorized, operator-gated
-- orgID, not as a browsable RLS surface. RLS enabled with NO policy
-- (deny-by-default to non-BYPASSRLS roles) + REVOKE ALL from PUBLIC + the app
-- roles; the admin pool does all I/O with org_id bound by argument.
CREATE TABLE public.placement_overrides (
    org_id             text NOT NULL,
    key_kind           text NOT NULL,
    key_value          text NOT NULL,
    pinned_instance_id text,
    replicas           integer NOT NULL DEFAULT 0,
    updated_at         timestamp with time zone NOT NULL DEFAULT now()
);

ALTER TABLE ONLY public.placement_overrides
    ADD CONSTRAINT placement_overrides_pkey PRIMARY KEY (org_id, key_kind, key_value);

ALTER TABLE public.placement_overrides ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.placement_overrides FROM PUBLIC;
REVOKE ALL ON public.placement_overrides FROM anon, authenticated, service_role;


-- instance_stats: 1-minute telemetry samples, one row per pod per minute,
-- written by the sampler alongside the registry heartbeat and read by the
-- fleet dashboard's timeseries + overview surfaces. The registry row
-- (instances) carries the *current* capacity/admission snapshot; this table
-- carries its *history* plus the fields too transient for the 4s heartbeat to
-- be a useful record of (per-sample claim rate, spawn latency, OOM kills).
--
-- Every metric column is nullable on purpose: a control pod has no runs to
-- report (active_runs/queued_visible/claims/spawn_p50_ms stay NULL), and a
-- non-Linux or unconfined host can't report cpu/load/oom — a partial sample is
-- still a useful row, so the sampler writes whatever it could read rather than
-- dropping the sample. mem_available_mb comes from hostmem (cgroup-aware
-- instance truth), never a host-wide /proc/meminfo read.
--
-- Admin-pool-only / system table, same posture as instances: no org_id (a
-- fleet member's telemetry isn't tenant data). RLS enabled with NO policy
-- (deny-by-default to non-BYPASSRLS roles) + REVOKE ALL from PUBLIC + the app
-- roles; the admin pool does all I/O. A ~30d retention reaper trims it.
CREATE TABLE public.instance_stats (
    instance_id       text NOT NULL,
    at                timestamp with time zone NOT NULL,
    active_runs       integer,
    queued_visible    integer,
    mem_available_mb  integer,
    cpu_pct           real,
    load1             real,
    claims            integer,
    spawn_p50_ms      integer,
    oom_kills         integer
);

ALTER TABLE ONLY public.instance_stats
    ADD CONSTRAINT instance_stats_pkey PRIMARY KEY (instance_id, at);

-- The reaper deletes by age and the fleet-wide timeseries scans a recent
-- window across all instances, both keyed on `at` alone (the PK's leading
-- instance_id can't serve either) — so a dedicated `at` index earns its keep.
CREATE INDEX instance_stats_at_idx ON public.instance_stats USING btree (at);

ALTER TABLE public.instance_stats ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.instance_stats FROM PUBLIC;
REVOKE ALL ON public.instance_stats FROM anon, authenticated, service_role;


-- sandbox_stats: the per-sandbox resource series — one row per live jail per
-- sampler tick, appended by the executor's existing instance-stat sampler and
-- read by the per-conversation usage-over-time surface.
--
-- This is the SHAPE of an engagement's consumption while it runs, which the
-- claim's end-state actuals (claims.peak_mem_mb / claims.cpu_usec, read once at
-- teardown) cannot give. The two disagree slightly by design: a periodic
-- sampler misses the sub-minute spikes the kernel high-watermark catches. The
-- series is shape, the teardown snapshot is truth, and they are never
-- reconciled numerically.
--
-- cpu_usec_cum is CUMULATIVE, not a rate: a consumer derives rate from the
-- difference between two samples, so a dropped tick self-heals into a
-- wider-but-correct interval instead of leaving a gap that reads as idle.
--
-- Both metric columns are nullable for the same reason instance_stats' are: a
-- partial sample is still a useful row, so the sampler writes whatever it
-- could read. A tick that read NOTHING for a jail (torn down between the
-- registry snapshot and the file read) writes no row at all.
--
-- claim_id is a bare uuid with NO foreign key to claims, matching the
-- instance_stats family convention: the write is a best-effort batch on a
-- telemetry path that must never be rejected by referential integrity, and
-- the table is bounded by its own age reaper — same ~30d retention, same
-- hourly cadence as instance_stats. One family, one policy.
--
-- Admin-pool-only / system table, same posture as instance_stats: no org_id
-- (executor telemetry isn't tenant content). RLS enabled with NO policy
-- (deny-by-default to non-BYPASSRLS roles) + REVOKE ALL from PUBLIC + the app
-- roles; the admin pool does all I/O.
CREATE TABLE public.sandbox_stats (
    claim_id        uuid NOT NULL,
    at              timestamp with time zone NOT NULL,
    mem_current_mb  integer,
    cpu_usec_cum    bigint
);

-- The PK is also the per-claim series index (the read is "one claim, ordered
-- by at"), and it makes a retried tick an idempotent no-op via ON CONFLICT DO
-- NOTHING.
ALTER TABLE ONLY public.sandbox_stats
    ADD CONSTRAINT sandbox_stats_pkey PRIMARY KEY (claim_id, at);

-- The reaper deletes by age alone, which the PK's leading claim_id can't
-- serve — so a dedicated `at` index earns its keep, same as instance_stats.
CREATE INDEX sandbox_stats_at_idx ON public.sandbox_stats USING btree (at);

ALTER TABLE public.sandbox_stats ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.sandbox_stats FROM PUBLIC;
REVOKE ALL ON public.sandbox_stats FROM anon, authenticated, service_role;


-- operators: the deployment-operator identity — the CLI-managed flag that
-- gates the fleet console (fleet administration is deployment-scoped, so an
-- operator is org-less: it is not a member role). Managed only through
-- `triagefactory operator add|remove|list <email>`; the bootstrap trust
-- boundary is shell access to the deployment, the same boundary as jwk-init.
--
-- Keyed on the lower-cased email rather than a user id on purpose: the email is
-- what the CLI names, it is present on every verified session (claims.Email
-- from GoTrue), and keying on it lets an operator be authorized before their
-- first login (no user row exists yet) and survive user-row churn. The gate is
-- a plain lookup of the request's verified email against this table.
--
-- Admin-pool-only / system table, same posture as instances: no org_id (an
-- operator is deployment config, not tenant data). RLS enabled with NO policy +
-- REVOKE ALL from PUBLIC + the app roles; the admin pool does all I/O.
CREATE TABLE public.operators (
    email     text NOT NULL,
    added_at  timestamp with time zone NOT NULL DEFAULT now(),
    -- added_by: the OS user that ran the CLI, best-effort provenance for the
    -- `operator list` read-out. Not an authorization input.
    added_by  text
);

ALTER TABLE ONLY public.operators
    ADD CONSTRAINT operators_pkey PRIMARY KEY (email);

ALTER TABLE public.operators ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.operators FROM PUBLIC;
REVOKE ALL ON public.operators FROM anon, authenticated, service_role;


-- ws_outbox: overflow storage for the tf_ws backplane (TFAC-584, spec
-- §5.1/§5). websocket.Hub.Broadcast publishes an envelope on tf_ws for
-- every control pod to fan into its own local sockets; NOTIFY caps a
-- payload at ~8 KB and agent-message frames regularly exceed the
-- configured inline threshold (default 6 KB, TF_WS_INLINE_MAX_B), so an
-- oversized event lands here instead and the NOTIFY carries only a row
-- ref. Row insert + NOTIFY happen in the same transaction (see
-- internal/wsbackplane), so a reader can always fetch what the ref
-- points to unless the tx rolled back after NOTIFY was already
-- buffered — that race, and a TTL-reaped row, both resolve by dropping
-- the event with a debug log, consistent with tf_ws's lossy-by-contract
-- rule (a dropped frame costs a UI blip; the frontend already refetches
-- on focus/poll). TTL 60 s (TF_WS_OUTBOX_TTL_SECONDS); any pod's slow
-- timer may run the reaper (`DELETE WHERE created_at < now() - TTL` is
-- concurrency-safe), so this is deliberately NOT leader-gated — reaping
-- is too trivial to route through the brain lease.
--
-- Admin-pool-only / system table, same posture as instances: no org_id
-- (an in-flight envelope isn't tenant data with an access-control
-- question — the payload itself already carries whatever org scoping
-- the original event had). RLS enabled with NO policy (deny-by-default
-- to non-BYPASSRLS roles) + REVOKE ALL from PUBLIC + the app roles.
CREATE TABLE public.ws_outbox (
    id          bigserial NOT NULL,
    payload     jsonb NOT NULL,
    created_at  timestamp with time zone NOT NULL DEFAULT now()
);

ALTER TABLE ONLY public.ws_outbox
    ADD CONSTRAINT ws_outbox_pkey PRIMARY KEY (id);

CREATE INDEX ws_outbox_created_at_idx ON public.ws_outbox USING btree (created_at);

ALTER TABLE public.ws_outbox ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.ws_outbox FROM PUBLIC;
REVOKE ALL ON public.ws_outbox FROM anon, authenticated, service_role;


-- ws_presence: cross-pod presence for the unattended-prompt fast-deny and
-- the fleet dashboard's live-viewer count (TFAC-584, spec §5.1). Hub.PresentFor
-- only ever saw this pod's own local sockets' viewing/visible state — a
-- reviewer's tab connected to pod B is invisible to a permission check
-- running on pod A's executor. Every control pod upserts one row per
-- (user_id, conn_id) on a ~15 s heartbeat (TF_WS_PRESENCE_HEARTBEAT_SECONDS)
-- mirroring its Hub's locally-connected, identified sockets (see
-- websocket.Hub.Snapshot); conn_id is qualified with the owning pod's
-- instance id by the writer (internal/wsbackplane) so two pods' connections
-- can never collide on this key even though the id itself is only unique
-- within one process. A row is "live" iff last_seen is within the last 45 s
-- (TF_WS_PRESENCE_TTL_SECONDS) — checked at read time, not enforced by a DB
-- constraint; no delete on disconnect, so a dead pod's (or closed
-- connection's) rows simply age out of every live-read query's filter
-- without needing an explicit cleanup step for READ correctness. That's
-- necessary but not sufficient for STORAGE bounding, though: conn_id is
-- minted from a per-boot monotonic counter (websocket.Hub.connSeq) that's
-- never reused, so every reconnect (tab refresh, network blip, a
-- RevalidateSessions kick) mints a brand-new row nothing will ever
-- overwrite. A periodic reaper (RunPresenceReaper, mirroring ws_outbox's
-- own) deletes rows past a separate, longer TTL (TF_WS_PRESENCE_ROW_TTL_SECONDS,
-- default well above the live window) purely for that storage bound — it
-- has zero effect on PresentFor's answer, which the live-window filter
-- alone already determines.
--
-- Admin-pool-only / system table, same posture as instances/ws_outbox: not
-- genuinely tenant business data (org_id here is a read-time filter on
-- otherwise-plumbing rows, not something a tenant's own RLS-scoped session
-- should read directly), so RLS is enabled with NO policy + REVOKE ALL.
CREATE TABLE public.ws_presence (
    user_id     text NOT NULL,
    conn_id     text NOT NULL,
    org_id      text NOT NULL,
    instance_id text NOT NULL,
    viewing     text NOT NULL DEFAULT ''::text,
    visible     boolean NOT NULL DEFAULT false,
    last_seen   timestamp with time zone NOT NULL DEFAULT now()
);

ALTER TABLE ONLY public.ws_presence
    ADD CONSTRAINT ws_presence_pkey PRIMARY KEY (user_id, conn_id);

CREATE INDEX ws_presence_org_last_seen_idx ON public.ws_presence USING btree (org_id, last_seen);

ALTER TABLE public.ws_presence ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.ws_presence FROM PUBLIC;
REVOKE ALL ON public.ws_presence FROM anon, authenticated, service_role;


-- conversation_signals (TFAC-585): the cross-pod conversation-control outbox
-- — a control pod's
-- delivery of cancel/interrupt/steer/permission/inject to the executor that
-- owns the conversation's live process (claims.executor_id), for the case
-- where the local process registry (Spawner.procs) misses. See
-- docs/for-agents/specs/horizontal-scaling/README.md §5.2 for the full design
-- (RunController's intended second implementation).
--
-- Postgres-only, deliberately no SQLite twin: local mode is always its own
-- run's owner (TF_ROLE=all is one process), so no code path may ever write
-- a signal there — internal/db/sqlite/conversation_signals.go is a stub returning
-- ErrNotApplicableInLocal, mirroring MarketplaceStore/InvitesStore.
--
-- Admin-pool-only / system table, same posture as instances / auth_events:
-- conversation_signals is pure system-to-system coordination, never read under a
-- user's RLS context, so RLS is enabled with NO policy (deny-by-default) +
-- REVOKE ALL from the app roles — the superuser/admin pool does all I/O.
--
-- `kind` carries the fifth value 'inject' because TFAC-594's additive-event
-- injection is a routed operation wearing a queued API: its live path
-- (getProc(runID)) is a process-local map hit the router (brain) can't
-- reach for a run executing on a remote executor. Under conversation_signals, the
-- owner delivers into its live process; the staged-injection fallback
-- (an undelivered messages row) remains for genuinely parked conversations.
--
-- `payload` carries steer text / the permission decision / the injection
-- body — riding the outbox row, not the NOTIFY payload, so there is no
-- 8 KB size concern. `target` is the executor id (internal/instance,
-- text — see instances.id above) the signal is delivered to; no FK, same
-- as claims.executor_id/event_queue.executor_id (a tombstoned/GC'd instance
-- leaves a signal to expire harmlessly, never cascades).
--
-- Acked rows are purged after 24h (TF_CONVERSATION_SIGNAL_PURGE_AFTER,
-- audit convenience window); unacked rows simply expire harmlessly if the
-- owner never returns — the fleet reaper (TFAC-586) owns a dead executor's
-- conversations, not this table.
CREATE TABLE public.conversation_signals (
    id         bigint GENERATED BY DEFAULT AS IDENTITY,
    org_id     uuid NOT NULL,
    conversation_id     uuid NOT NULL,
    kind       text NOT NULL,
    payload    jsonb,
    target     text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    acked_at   timestamp with time zone,
    ack_result text,
    CONSTRAINT conversation_signals_kind_check CHECK ((kind = ANY (ARRAY['cancel'::text, 'interrupt'::text, 'steer'::text, 'permission'::text, 'inject'::text])))
);

ALTER TABLE ONLY public.conversation_signals
    ADD CONSTRAINT conversation_signals_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.conversation_signals
    ADD CONSTRAINT conversation_signals_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.conversation_signals
    ADD CONSTRAINT conversation_signals_conversation_id_org_id_fkey FOREIGN KEY (conversation_id, org_id) REFERENCES public.conversations(id, org_id) ON DELETE CASCADE;

-- The owner's apply-loop scan: unacked rows targeting me, oldest id first —
-- signals apply per conversation in id order, and ascending id across the
-- whole target is a superset ordering that trivially preserves
-- per-conversation order too (§5.2).
CREATE INDEX idx_conversation_signals_target_unacked ON public.conversation_signals USING btree (target, id) WHERE (acked_at IS NULL);
-- The 24h purge sweep.
CREATE INDEX idx_conversation_signals_acked ON public.conversation_signals USING btree (acked_at) WHERE (acked_at IS NOT NULL);

ALTER TABLE public.conversation_signals ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.conversation_signals FROM PUBLIC;
REVOKE ALL ON public.conversation_signals FROM anon, authenticated, service_role;




-- claims: one row per executor engagement with a conversation — the claim
-- machinery that used to live as columns on runs (claimed_at / attempts /
-- executor_id / boot_epoch / cred_pubkey). A conversation is claimed (a row
-- is minted here), driven
-- for a while, and released; a retry or a resumed park is simply the next
-- claim. At most one active (released_at IS NULL) claim per conversation,
-- enforced below. Rows are immutable identity + terminal telemetry: the
-- executor/boot pair never changes after mint.
--
-- Both dialects (the local dispatcher claims its own work through the same
-- path); the credential columns are multi-mode-only in practice and simply
-- stay NULL locally.
CREATE TABLE public.claims (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    executor_id text NOT NULL,
    -- boot_epoch pairs with executor_id (an instance's persistent id
    -- survives restarts); the boot self-sweep only releases claims where
    -- executor_id = self AND boot_epoch < the current boot's epoch.
    boot_epoch bigint NOT NULL,
    -- cred_pubkey is the per-engagement credential sidecar's X25519 public
    -- key (base64), published when the executor parks the engagement in
    -- phase='awaiting_credentials' and read by the brain at seal time.
    cred_pubkey text,
    -- phase is the setup/parked sub-state of a LIVE engagement: 'fetching'
    -- | 'cloning' | 'agent_starting' | 'awaiting_credentials'. NULL = the
    -- agent process is live (or the claim is released — a released claim's
    -- phase is inert history). Display reads coalesce this over the
    -- conversation's stored status.
    phase text,
    claimed_at timestamp with time zone DEFAULT now() NOT NULL,
    -- released_at NULL = this claim is live (its executor owns the
    -- conversation's process). Stamped exactly once, on release.
    released_at timestamp with time zone,
    -- outcome is app-validated: how the engagement ended — 'completed' |
    -- 'failed' | 'cancelled' | 'requeued' | 'parked' | 'reaped'. NULL while
    -- live.
    outcome text,
    error text,
    -- Engagement telemetry the runtime reports per invocation and nothing
    -- can derive from messages. Dollars and tokens deliberately do NOT
    -- live here — messages is the ledger.
    duration_ms integer,
    num_turns integer,
    -- Measured sandbox cost: what this engagement's jail actually consumed,
    -- read from its cgroup (memory.peak, cpu.stat's usage_usec) at teardown,
    -- before the group is removed. Ground truth for margin analysis and
    -- capacity planning, and unbackfillable — the cgroup is gone the instant
    -- the run ends.
    --
    -- Distinct from the pre-allocated envelope (mem_budget_mb stays an
    -- author-set profile config item; these are what the run then really
    -- used) and from instance_stats, which samples the whole host and so
    -- attributes a concurrent workload's memory to whatever run was live.
    --
    -- NULL means "not measured", never "measured zero": local mode has no
    -- sandbox at all, a pre-5.19 kernel has no memory.peak (cpu_usec lands,
    -- peak_mem_mb stays NULL), and a crashed teardown records neither. The
    -- recorded CPU deliberately includes the gVisor sentry's systrap
    -- overhead — it is TF's cost view of the run, not a billed quantity.
    peak_mem_mb integer,
    cpu_usec bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.claims
    ADD CONSTRAINT claims_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.claims
    ADD CONSTRAINT claims_id_org_unique UNIQUE (id, org_id);
ALTER TABLE ONLY public.claims
    ADD CONSTRAINT claims_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.claims
    ADD CONSTRAINT claims_conversation_id_org_id_fkey FOREIGN KEY (conversation_id, org_id) REFERENCES public.conversations(id, org_id) ON DELETE CASCADE;

-- The single-active-claim invariant: a conversation has at most one live
-- executor engagement at any moment.
CREATE UNIQUE INDEX idx_claims_one_active ON public.claims USING btree (conversation_id) WHERE (released_at IS NULL);
CREATE INDEX idx_claims_conversation ON public.claims USING btree (conversation_id, claimed_at);
-- The credential provisioner's backstop sweep scans live claims parked in
-- phase='awaiting_credentials'; live claims in any phase are few, so the
-- partial index stays tiny.
CREATE INDEX idx_claims_active_phase ON public.claims USING btree (phase) WHERE (released_at IS NULL AND phase IS NOT NULL);

-- App-pool visibility composes through the conversation (the runstation
-- shows which executor drove an engagement); every write is system-side.
ALTER TABLE public.claims ENABLE ROW LEVEL SECURITY;

CREATE POLICY claims_select ON public.claims FOR SELECT USING ((EXISTS ( SELECT 1
   FROM public.conversations c
  WHERE (c.id = claims.conversation_id))));

GRANT ALL ON TABLE public.claims TO postgres;
GRANT ALL ON TABLE public.claims TO anon;
GRANT ALL ON TABLE public.claims TO authenticated;
GRANT ALL ON TABLE public.claims TO service_role;
GRANT SELECT ON TABLE public.claims TO tf_app;
GRANT SELECT, INSERT, UPDATE ON TABLE public.claims TO tf_system;

-- messages.claim_id attaches here (declared late: claims_id_org_unique must
-- exist first). ON DELETE SET NULL so a conversation cascade (conversation →
-- claims and conversation → messages independently) never orders wrong.
ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_claim_id_fkey FOREIGN KEY (claim_id) REFERENCES public.claims(id) ON DELETE SET NULL;


-- claim_credentials: the sealed per-claim credential bundle channel — one
-- sealed bundle per engagement. An executor claims a conversation and parks
-- the claim in phase='awaiting_credentials'; the brain resolves the
-- engagement's
-- LLM/GitHub/Jira credentials, seals them (credseal, an X25519 sealed box)
-- to the claim's published cred_pubkey, and writes exactly one row here.
-- One row per claim (PK claim_id), replaced wholesale on every write —
-- never merged — so a refresh (re-minted git tokens for a long engagement)
-- simply overwrites the prior bundle, and a re-claim after a crash is a NEW
-- claim with its own fresh seal.
--
-- boot_epoch is the claiming executor's boot epoch at seal time, carried in
-- cleartext alongside the sealed blob so the executor can compare it
-- against its own current epoch BEFORE attempting to unseal — never even
-- attempt a bundle sealed for an earlier boot.
--
-- include_tools is the claim's cleartext tools manifest: the event-source
-- kinds available to the org at seal time (eventsource.AvailableKindsSystem),
-- stamped by the brain on the same pass that seals the bundle. The executor
-- composes the run's <tools> prompt section from it, because it cannot
-- answer the question itself — its secret store is disabled and an
-- event-triggered conversation has no user identity to claim as. Source-kind
-- names only, never secret material, so it travels in cleartext like
-- executor_id/boot_epoch. NULL = the writer stamped no answer (resolve
-- failure, reader falls back); '[]' = a real answer of "nothing available".
--
-- Admin-pool only, no RLS policy (deny-by-default), no app-pool grant at
-- all: credential-bearing sealed ciphertext, never a request-handler
-- surface. Rows cascade with their claim.
CREATE TABLE public.claim_credentials (
    claim_id      uuid NOT NULL,
    org_id        uuid NOT NULL,
    executor_id   text NOT NULL,
    boot_epoch    bigint NOT NULL,
    sealed        bytea NOT NULL,
    include_tools jsonb,
    created_at    timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.claim_credentials
    ADD CONSTRAINT claim_credentials_pkey PRIMARY KEY (claim_id);

ALTER TABLE ONLY public.claim_credentials
    ADD CONSTRAINT claim_credentials_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.claim_credentials
    ADD CONSTRAINT claim_credentials_claim_id_org_id_fkey FOREIGN KEY (claim_id, org_id) REFERENCES public.claims(id, org_id) ON DELETE CASCADE;

ALTER TABLE public.claim_credentials ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.claim_credentials FROM PUBLIC;
REVOKE ALL ON public.claim_credentials FROM anon, authenticated, service_role;

-- The executor's awaiting-credentials wait polls for the bundle the brain
-- sealed to its published pubkey (SELECT), and best-effort deletes the row
-- on engagement teardown so sealed material doesn't linger (DELETE). No
-- INSERT — only the brain provisions, on its own superuser admin pool.
GRANT SELECT, DELETE ON TABLE public.claim_credentials TO tf_system;


-- conversation_permissions: the durable record of every tool-approval prompt
-- a conversation raised, and how it was answered.
--
-- Before this table a pending approval existed in exactly two places — a map
-- on one process and one fire-once websocket frame — so a page refresh, a
-- second tab, or a cold board load could never learn that a healthy, parked
-- agent was waiting on a human. That is a reachability failure, not a
-- durability one: the state was valid and live, it simply had no address.
-- This gives it one, and the socket frame demotes to a hint that something
-- changed (the standing rule everywhere else in the app).
--
-- It is also the only place an approval is recorded at all. An allow used to
-- leave no trace whatsoever — the tool just ran — and a deny survived only as
-- prose inside a tool_result body, where "a human denied this" and "nobody was
-- watching" are indistinguishable without string-matching agent-facing English
-- that is deliberately free to change. reason as an enum column is what fixes
-- that; it is the point of the exercise.
--
-- Multi mints runtime='native', which has no permission gate, and sandboxed
-- SDK runs auto-approve — so nothing writes here in a multi deployment today.
-- The table exists in this dialect because the store is one dual-dialect
-- contract with one conformance suite, the same reason sandbox_stats' SQLite
-- arm does.
CREATE TABLE public.conversation_permissions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    -- claim_id is the engagement that asked. Pending is derived against this:
    -- a row whose claim is no longer the conversation's active claim is a
    -- question asked by a process that no longer exists, about a workspace
    -- that may since have been restored, so it is not pending however the row
    -- itself reads.
    claim_id uuid,
    -- message_id is the assistant row carrying the gated tool_use block. Stays
    -- NULL on the SDK path: the wrapper's canUseTool fires from inside the
    -- SDK's tool dispatch, so the prompt is raised before the assistant row it
    -- belongs to has been streamed, let alone inserted.
    message_id bigint,
    -- tool_call_id is the SDK's toolUseID — the same id as
    -- messages.tool_calls[].id and the answering row's tool_call_id, so a
    -- decision ties back to the exact call it authorized.
    tool_call_id text NOT NULL,
    tool_name text NOT NULL,
    input_json text,
    -- title is the SDK's pre-rendered prompt sentence, absent when it rendered
    -- none — a consumer must still be able to reconstruct a headline from
    -- tool_name + input.
    title text,
    -- state is app-validated: 'pending' | 'allowed' | 'denied' | 'expired'.
    state text NOT NULL,
    -- reason is app-validated: 'user' | 'timeout' | 'absent' | 'closing' |
    -- 'claim_lost'.
    reason text,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    decided_by uuid,
    decided_at timestamp with time zone,
    -- waited_ms is how long the prompt stood open: milliseconds from
    -- requested_at to decided_at, stamped once at resolution. NULL while
    -- pending.
    --
    -- Stored rather than derived for the reason messages' own per-row
    -- duration is: subtracting the two timestamps at read time is a
    -- cross-clock subtraction (requested_at can come from the DB's now()
    -- default, decided_at from the app), so a consumer doing the arithmetic
    -- gets an answer that is wrong by whatever the two clocks disagree by.
    -- The write anchors it to the row's own stored requested_at, so the
    -- number is internally consistent whatever answered and wherever it ran.
    --
    -- Meaningful on every terminal state, not just an approval: "a human
    -- allowed it after 4s" and "it sat unattended for the full window" are
    -- the same measurement, and reading them together is how the timeout and
    -- absent-grace windows get tuned against what humans actually do.
    waited_ms integer
);

ALTER TABLE ONLY public.conversation_permissions
    ADD CONSTRAINT conversation_permissions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.conversation_permissions
    ADD CONSTRAINT conversation_permissions_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.orgs(id) ON DELETE CASCADE;
-- No ON DELETE CASCADE from conversations, deliberately: an audit record whose
-- whole value is outliving the work must not be destroyed by cleaning up the
-- work. Conversations are archived (archived_at), not hard-deleted, in normal
-- operation, so the practical cost is that a hard delete of a conversation
-- carrying permission history is refused — the correct trade for this table.
ALTER TABLE ONLY public.conversation_permissions
    ADD CONSTRAINT conversation_permissions_conversation_id_org_id_fkey FOREIGN KEY (conversation_id, org_id) REFERENCES public.conversations(id, org_id);
ALTER TABLE ONLY public.conversation_permissions
    ADD CONSTRAINT conversation_permissions_claim_id_fkey FOREIGN KEY (claim_id) REFERENCES public.claims(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.conversation_permissions
    ADD CONSTRAINT conversation_permissions_message_id_fkey FOREIGN KEY (message_id) REFERENCES public.messages(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.conversation_permissions
    ADD CONSTRAINT conversation_permissions_decided_by_fkey FOREIGN KEY (decided_by) REFERENCES public.users(id) ON DELETE SET NULL;

-- One row per gated call, which is also what makes the writer's insert safe to
-- reach twice.
CREATE UNIQUE INDEX idx_conv_perms_call ON public.conversation_permissions USING btree (conversation_id, tool_call_id);
-- The read this table exists for: "what is this conversation waiting on".
CREATE INDEX idx_conv_perms_pending ON public.conversation_permissions USING btree (conversation_id) WHERE (state = 'pending');

-- App-pool visibility composes through the conversation, mirroring claims —
-- conversation-scoped, visible through the conversation, written system-side.
ALTER TABLE public.conversation_permissions ENABLE ROW LEVEL SECURITY;

CREATE POLICY conversation_permissions_select ON public.conversation_permissions FOR SELECT USING ((EXISTS ( SELECT 1
   FROM public.conversations c
  WHERE (c.id = conversation_permissions.conversation_id))));

GRANT ALL ON TABLE public.conversation_permissions TO postgres;
GRANT ALL ON TABLE public.conversation_permissions TO anon;
GRANT ALL ON TABLE public.conversation_permissions TO authenticated;
GRANT ALL ON TABLE public.conversation_permissions TO service_role;
GRANT SELECT ON TABLE public.conversation_permissions TO tf_app;
GRANT SELECT, INSERT, UPDATE ON TABLE public.conversation_permissions TO tf_system;

-- workspace_snapshots: the durable lifecycle of one snapshot key's workspace
-- blob — pending, written, or failed — and the engagement that owns the write.
--
-- The blob store answers "is there a blob" and nothing else, so its silence is
-- four different situations wearing one face: a persist still in flight, a
-- persist that failed, a conversation that never got far enough to have a
-- workspace, and a snapshot the retention reaper collected. A resume that
-- cannot tell them apart has to treat every one of them as gone forever. This
-- row is what separates them: no row is the last two (nothing was ever owed),
-- 'pending' names a write in flight and, through writer_claim_id, the
-- engagement to ask about it, and 'failed' is the durable answer that stops a
-- waiter waiting.
--
-- writer_claim_id is also the stale-write guard. A cross-pod stop parks the
-- conversation and releases the claim immediately while the old executor's
-- teardown snapshots afterwards, so an older engagement's Put can land after a
-- successor has already written its own newer blob. A writer re-reads this
-- column just before uploading and skips the upload when the key has moved on;
-- the completion CAS keys on it too, so a displaced writer cannot close out a
-- write it no longer owns. Keying on "does a successor claim exist" instead
-- would be wrong — a successor on another executor may be waiting for exactly
-- this blob.
--
-- The key is the snapshot key: (org_id, blueprint_run_id), because a
-- blueprint's steps share one workspace tree and one blob. No claims FK on
-- writer_claim_id: the column identifies a writer, and a claim row's fate must
-- not decide whether the blob's state is knowable.
CREATE TABLE public.workspace_snapshots (
    org_id uuid NOT NULL,
    blueprint_run_id uuid NOT NULL,
    state text NOT NULL,
    writer_claim_id uuid NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT workspace_snapshots_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'written'::text, 'failed'::text])))
);

ALTER TABLE ONLY public.workspace_snapshots
    ADD CONSTRAINT workspace_snapshots_pkey PRIMARY KEY (org_id, blueprint_run_id);

ALTER TABLE ONLY public.workspace_snapshots
    ADD CONSTRAINT workspace_snapshots_blueprint_run_id_org_id_fkey FOREIGN KEY (blueprint_run_id, org_id) REFERENCES public.blueprint_runs(id, org_id) ON DELETE CASCADE;

-- Admin-pool only, same posture as claim_credentials: every caller is an
-- executor-side teardown goroutine or a system sweep, and no request handler
-- reads it — so RLS is on with no policy at all and the app roles hold no
-- grant.
ALTER TABLE public.workspace_snapshots ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.workspace_snapshots FROM PUBLIC;
REVOKE ALL ON public.workspace_snapshots FROM anon, authenticated, service_role;

-- The executor writes the whole lifecycle (INSERT/UPDATE on begin and finish),
-- reads it back for the pre-Put supersede check (SELECT), and drops it
-- alongside the blob at terminal cleanup and in the retention reaper (DELETE).
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.workspace_snapshots TO tf_system;


--
-- Name: llm_spend; Type: VIEW; Schema: public; Owner: -
--

-- Unified spend view: one row-per-spend shape UNION-ing the messages
-- ledger (delegated conversations) and headless system jobs onto a single
-- category axis. Placed here (not with the tables) because the historical
-- tail position survives the table-order shuffle — every dependency
-- already exists by here.
--
-- The agent arm reads messages directly: a row participates when it
-- carries tokens (an assistant row) or settled dollars (any cost-stamped
-- row — the runtime's one-lump terminal stamp), attributed through the
-- owning conversation's team/creator/actor/trigger snapshot. category:
-- delegation splits on trigger_type (manual → per-user, anything else →
-- autonomous); system jobs are 'system_overhead' with the job carried as
-- subtype. source_id is the message id (system arm: the job row id), cast
-- to text so the UNION's column type is uniform.
--
-- security_invoker = true is LOAD-BEARING and mandatory (PG 15+): the base
-- tables' RLS (messages-through-conversation, conversations' visibility
-- arms, system_llm_runs org) applies under the querying app-pool identity.
CREATE VIEW public.llm_spend WITH (security_invoker='true') AS
 SELECT 'run'::text AS source,
    (m.id)::text AS source_id,
    m.org_id,
    c.team_id,
        CASE
            WHEN (c.trigger_type = 'manual'::text) THEN 'manual'::text
            ELSE 'autonomous'::text
        END AS category,
    NULL::text AS subtype,
    c.creator_user_id,
    c.actor_agent_id,
    c.trigger_id,
    m.model,
    COALESCE(m.cost_usd, (0)::real) AS total_cost_usd,
    COALESCE(m.input_tokens, 0) AS input_tokens,
    COALESCE(m.output_tokens, 0) AS output_tokens,
    COALESCE(m.cache_read_tokens, 0) AS cache_read_tokens,
    COALESCE(m.cache_creation_tokens, 0) AS cache_creation_tokens,
    m.created_at AS occurred_at
   FROM (public.messages m
     JOIN public.conversations c ON (((c.id = m.conversation_id) AND (c.org_id = m.org_id))))
  WHERE ((c.type = 'delegation'::text)
     AND ((m.role = 'assistant'::text) OR (m.cost_usd IS NOT NULL)))
UNION ALL

 SELECT 'system'::text AS source,
    (system_llm_runs.id)::text AS source_id,
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

GRANT ALL ON TABLE public.llm_spend TO postgres;
GRANT ALL ON TABLE public.llm_spend TO anon;
GRANT ALL ON TABLE public.llm_spend TO authenticated;
GRANT ALL ON TABLE public.llm_spend TO service_role;
GRANT SELECT ON TABLE public.llm_spend TO tf_app;




-- tf_system: least-privilege executor role. Login mechanics mirror
-- authenticator — the postgres-postinit-system sidecar ALTERs this role
-- to LOGIN with TF_SYSTEM_PASSWORD once this migration has applied; see
-- docker-compose.yml. No DDL grant of any kind, ever. This is an
-- enumerated allowlist of exactly the tables the executor's dispatcher,
-- cross-pod signal apply loop, heartbeat, and telemetry sampler touch —
-- verified against code and pinned by two pgtest suites, both of which run as
-- this role rather than as a superuser: the executor-surface walk
-- (internal/db/pgtest/tf_system_test.go) and the role-bound store conformance
-- runs (internal/db/pgtest/tf_system_stores_test.go). Any future migration
-- that adds a table to the executor's code path must add that table's
-- tf_system grant in the SAME migration; those suites are the enforcement
-- backstop, and a store whose conformance suite is not run role-bound has no
-- backstop at all.

GRANT USAGE ON SCHEMA public, tf TO tf_system;

-- Conversation lifecycle: claim CAS / requeue / complete / resume+executor
-- stamps (SELECT, UPDATE) and the dispatcher's own queue inserts — both the
-- initial blueprint-step enqueue and every subsequent step (SELECT,
-- INSERT). No DELETE — conversations are never removed, only
-- status-terminated.
GRANT SELECT, INSERT, UPDATE ON TABLE public.conversations TO tf_system;
GRANT SELECT, INSERT, UPDATE ON TABLE public.messages TO tf_system;
GRANT USAGE, SELECT ON SEQUENCE public.messages_id_seq TO tf_system;
GRANT SELECT, INSERT, UPDATE ON TABLE public.artifacts TO tf_system;
GRANT SELECT, INSERT, DELETE ON TABLE public.conversation_worktrees TO tf_system;
-- Agent memory (TaskMemory.UpsertAgentMemorySystem / GetMemoriesForEntitySystem).
GRANT SELECT, INSERT, UPDATE ON TABLE public.conversation_memory TO tf_system;
-- Multi-entity memory join (TaskMemory.RecordEntityTouchSystem / the
-- GetMemoriesForEntitySystem read-path join). UPDATE backs the
-- role-precedence upgrade (touched -> produced -> primary).
GRANT SELECT, INSERT, UPDATE ON TABLE public.conversation_memory_entities TO tf_system;
-- External-credential write audit log (RecordExternalWrite / the Jira mirror).
GRANT SELECT, INSERT ON TABLE public.external_actions TO tf_system;
-- Touched-entity resolution.
GRANT SELECT, INSERT, UPDATE ON TABLE public.entities TO tf_system;
GRANT SELECT ON TABLE public.entity_links TO tf_system;
-- Tasks.RecordEventSystem ('injected' etc.) + the dispatcher's own status
-- flips (SetStatusSystem, CloseSystem on run completion).
GRANT SELECT, INSERT ON TABLE public.task_events TO tf_system;
GRANT SELECT, UPDATE ON TABLE public.tasks TO tf_system;
-- The inject signal's gone-compensation enqueues a pending_firing from the
-- executor's cross-pod apply loop. Claim/drain/requeue is router-side.
GRANT SELECT, INSERT ON TABLE public.pending_firings TO tf_system;
GRANT USAGE, SELECT ON SEQUENCE public.pending_firings_id_seq TO tf_system;
-- conversation_signals (TFAC-585): SELECT for the owning executor's apply-loop scan,
-- UPDATE for ack. Insert (cross-pod dispatch) and the purge reaper are
-- brain-gated — control-side only.
GRANT SELECT, UPDATE ON TABLE public.conversation_signals TO tf_system;
-- ws_outbox (TFAC-584): INSERT to publish a websocket event over the
-- backplane, plus SELECT + DELETE for the TTL reaper, which deliberately
-- runs on every backplane-wired pod, executors included.
GRANT SELECT, INSERT, DELETE ON TABLE public.ws_outbox TO tf_system;
GRANT USAGE, SELECT ON SEQUENCE public.ws_outbox_id_seq TO tf_system;
-- ws_presence (TFAC-584): SELECT for the PresentFor fast-deny fallback and
-- the presence reaper's DELETE. Executors never INSERT presence — the
-- heartbeat that upserts it runs only on HTTP-serving pods.
GRANT SELECT, DELETE ON TABLE public.ws_presence TO tf_system;
-- Fleet registry: this instance's own register + heartbeat + drain flag.
GRANT SELECT, INSERT, UPDATE ON TABLE public.instances TO tf_system;
-- instance_stats: the per-pod sampler INSERTs one row a minute, and the
-- retention reaper DELETEs the tail — both run on every pod, executors
-- included. SELECT for the dashboard read (a control pod's admin pool, but
-- granted here too so the posture matches instances regardless of which admin
-- role a pod's pool binds).
GRANT SELECT, INSERT, DELETE ON TABLE public.instance_stats TO tf_system;
-- sandbox_stats: the same sampler tick that writes instance_stats appends one
-- row per live jail, and the same retention reaper DELETEs the tail. Unlike
-- its twin, this one only ever has work to do where the jails are — the
-- executor, which is also the only pod whose admin pool binds this role — so
-- there is no other pod whose success could stand in for it. SELECT for the
-- per-conversation usage read, granted here for the instance_stats reason:
-- the posture matches regardless of which admin role a pod's pool binds.
GRANT SELECT, INSERT, DELETE ON TABLE public.sandbox_stats TO tf_system;
-- operators: the deployment-operator identity. The fleet gate + the operator
-- CLI read/write it via the admin pool; granted to tf_system so the operator
-- CLI works regardless of which admin role its connection binds.
GRANT SELECT, INSERT, DELETE ON TABLE public.operators TO tf_system;
-- placement_overrides (TFAC-587): SELECT-only. The dispatcher's post-step
-- reactor enqueues the next blueprint step on the executor, and the
-- placement stamp it computes reads any override for the conversation's
-- placement key. Writes
-- (pin / replica count) are an operator action on a control pod, never the
-- executor's path.
GRANT SELECT ON TABLE public.placement_overrides TO tf_system;

-- Blueprint run-instance sequencing: the dispatcher's post-step reactor
-- advances current_step_index, flips terminal status, and applies
-- cross-pod cancel requests. The blueprint DEFINITION tables
-- (blueprints/blueprint_steps) are read-only — a running blueprint_run
-- executes off its own frozen step_plan snapshot, never a live re-read.
GRANT SELECT, UPDATE ON TABLE public.blueprint_runs TO tf_system;
GRANT SELECT ON TABLE public.blueprints TO tf_system;
GRANT SELECT ON TABLE public.blueprint_steps TO tf_system;
-- Prompts.IncrementUsageSystem bumps a usage counter after resolving the
-- step prompt.
GRANT SELECT, UPDATE ON TABLE public.prompts TO tf_system;
GRANT SELECT ON TABLE public.agents TO tf_system;

-- Read-only reference data the dispatcher resolves per run.
GRANT SELECT ON TABLE public.orgs TO tf_system;
GRANT SELECT ON TABLE public.org_settings TO tf_system;
-- OrgsStore.GetSettingsSystem composes github_base_url / github_poll_interval
-- / jira_base_url / jira_poll_interval from here now (moved off org_settings
-- above), so the github/jira resolvers' per-run host + credential lookups
-- need the same read tf_system already had on org_settings for those fields.
GRANT SELECT ON TABLE public.org_event_sources TO tf_system;
GRANT SELECT ON TABLE public.teams TO tf_system;
GRANT SELECT ON TABLE public.team_settings TO tf_system;
GRANT SELECT ON TABLE public.users TO tf_system;
-- Users.GetGitHubLoginSystem (the manual-run co-author trailer) reads this,
-- not the users table itself.
GRANT SELECT ON TABLE public.user_github_identities TO tf_system;
GRANT SELECT ON TABLE public.team_github_repos TO tf_system;
-- repositories: UpdateCloneStatusSystem is local-mode-only in the current
-- call graph, granted ahead of the multi-mode bare-clone-cache path it's
-- designed for.
GRANT SELECT, UPDATE ON TABLE public.repositories TO tf_system;
GRANT SELECT ON TABLE public.org_github_apps TO tf_system;
GRANT SELECT ON TABLE public.org_github_app_installations TO tf_system;
GRANT SELECT ON TABLE public.events TO tf_system;
GRANT SELECT ON TABLE public.jira_project_status_rules TO tf_system;
-- llm_spend is security_invoker: reading it needs SELECT on its own base
-- tables too (conversations, claims, system_llm_runs), not just the view.
GRANT SELECT ON TABLE public.llm_spend TO tf_system;
GRANT SELECT ON TABLE public.system_llm_runs TO tf_system;
-- Slack provider policy ops (ee/slack/exec_provider_ops.go): the orchestrator
-- serves the sidecar's relayed `exec slack` calls against these, resolving an
-- authorization decision or a workspace IDENTITY, never a bot token (that rides
-- the sealed bundle). All read-only — the executor authorizes and selects, it
-- never mutates Slack config: team_slack_channels gates whether the
-- conversation's team tracks the channel (TracksChannelSystem),
-- slack_channels maps a channel to its
-- workspace (Channels.GetSystem), org_slack_workspaces picks which connected app
-- identity to act as (Workspaces.GetByWorkspaceAppSystem / ListAllSystem).
GRANT SELECT ON TABLE public.team_slack_channels TO tf_system;
GRANT SELECT ON TABLE public.slack_channels TO tf_system;
GRANT SELECT ON TABLE public.org_slack_workspaces TO tf_system;
-- The executor's schema-compatibility assert (internal/db/migrations.go)
-- reads this and nothing else; no DDL, ever.
GRANT SELECT ON TABLE public.goose_db_version TO tf_system;

-- +goose Down
SELECT 'down not supported';
