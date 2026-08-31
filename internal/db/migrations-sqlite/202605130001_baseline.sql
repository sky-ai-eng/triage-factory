-- +goose Up
-- Consolidated baseline (2026-05-13). Captures the cumulative
-- post-state of the schema chain that ran from the original D1 baseline
-- through every post-baseline migration up to and including 202605120012.
--
-- This baseline is a hard reset: any database that does not have *this*
-- migration applied (via goose_db_version) is refused at boot. There
-- is no upgrade path from a pre-baseline install — operators wipe
-- ~/.triagefactory/ and reinstall. The brick check lives in
-- internal/db/migrations.go.
--
-- Fresh installs only. No `IF NOT EXISTS` defensive guards on the
-- schema DDL — goose's version tracker prevents re-runs. The seed
-- inserts at the bottom use `INSERT OR IGNORE` because the application
-- also writes to those tables; that's idempotent seeding, not legacy
-- compat.
--
-- Future schema changes go in NEW NNN-numbered files alongside this
-- one — never edit this baseline. Down migrations are not supported
-- (see internal/db/migrations.go); the trailing Down block is a
-- deliberate no-op.
--
-- === Deliberate cross-dialect divergence (do NOT "fix" in a later migration) ======
-- This baseline and its Postgres twin
-- (internal/db/migrations-postgres/202605130001_pg_baseline.sql) differ in
-- the following ways ON PURPOSE. A mechanical (table,col,target,on_delete)
-- or CHECK diff will flag each as a "gap" — it is not. Later migration
-- authors must not converge these:
--
--   1. Type representation. Postgres uses native enum TYPES
--      (membership_role, org_role) plus uuid / timestamptz / jsonb /
--      interval / array column types; SQLite uses TEXT / INTEGER plus
--      CHECKs (and TEXT-encoded durations, JSON-in-TEXT). Same domain,
--      different mechanism. A CHECK-only diff falsely reports PG "missing"
--      the role CHECKs.
--   2. RLS keys. Postgres carries composite (id, org_id) FKs and (id, org_id)
--      uniques so row-level security can pin tenant scope; SQLite uses
--      single-column FKs (one connection, no RLS). Every *_org_id composite
--      FK / unique on the PG side is this category, not a divergence.
--   3. Attribution refs (creator_user_id and the like — the "who authored
--      this" columns). Postgres FK-constrains these with ON DELETE CASCADE;
--      SQLite leaves them bare TEXT (or NO-ACTION on runs). Deliberate: local
--      mode is N=1 and the sentinel user is never deleted, so there is no
--      referential hazard to guard. Do not add FKs to these attribution
--      columns here. NARROW scope — this does NOT cover identity/ownership
--      user_id PKs (preferences, user_settings, user_github/jira_identities,
--      memberships, org_memberships), which carry the users(id) FK in BOTH
--      dialects: there the row *is* a user, and deleting the user should take
--      it with them.
--   4. Tables that exist in one dialect only. instance_config is SQLite-only
--      (host port lives in container env under multi mode). sessions and
--      project_knowledge are Postgres-only (multi-mode auth + shared KB).
--   5. PG-only RLS apparatus. Row-level-security policies, the tf.* helper
--      functions, the auth.users FK, and admin-only columns (orgs.owner_user_id,
--      teams.created_by_user_id, users.default_org_id, sessions.active_org_id)
--      have no SQLite analog — local mode has a single trusted connection.
--   6. blueprint_runs.status. Postgres enforces the value set with a CHECK
--      (cheap to ALTER pre-tenant); SQLite leaves it app-validated (a
--      full-table rebuild is the only way to widen a SQLite CHECK, so the
--      headroom is the absence of the CHECK). Both accept the same values;
--      the app is the source of truth.

-- === Prompts =============================================================
CREATE TABLE prompts (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    body            TEXT NOT NULL,
    -- source is app-validated, not CHECK-constrained: today's writers use
    -- 'system' | 'user' | 'imported', but new provenance values (marketplace
    -- copies, future import kinds) must be addable without a SQLite table
    -- rebuild. The structural pairing below (a non-system row needs a creator)
    -- is the only invariant the DB still enforces on source.
    source          TEXT NOT NULL DEFAULT 'user',
    usage_count     INTEGER DEFAULT 0,
    hidden          BOOLEAN DEFAULT 0,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    user_modified   INTEGER NOT NULL DEFAULT 0,
    allowed_tools   TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    -- team_id is NOT NULL: every prompt is team-owned. The
    -- LocalDefaultTeamID sentinel is the default so local-mode inserts and
    -- the single-team N=1 case never thread it explicitly.
    team_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000010',
    -- creator_user_id is nullable: source='system' rows have NULL (no human author),
    -- source='user' rows must have a value. Enforced by prompts_system_has_no_creator.
    creator_user_id TEXT,
    -- system_slug is the stable shipped identifier ("system-ci-fix", …) for
    -- source='system' rows; NULL for user/imported prompts. The id is a
    -- random UUID per team copy, so re-seed idempotency and slug→id lookups
    -- key on (org_id, team_id, system_slug) instead of the id.
    system_slug     TEXT,
    -- deleted_at soft-deletes a prompt. The row + its runs stay as the
    -- durable audit trail (runs.prompt_id is RESTRICT, so a hard DELETE on a
    -- prompt with run history would 500); request-facing reads (List/Get/
    -- pickers/CountStepReferences) filter deleted_at IS NULL, while the
    -- ...System reads keep resolving it so in-flight runs + past-run timelines
    -- still render the prompt's name/body. Auto-wrapping every new prompt as a
    -- 1-step blueprint makes hard-delete impossible (the step FK is RESTRICT),
    -- so soft-delete is load-bearing, not a nicety.
    deleted_at      DATETIME,
    -- There is no visibility column: every prompt is team-owned (team_id NOT
    -- NULL) and visible to its team. A per-user 'private' tier was never
    -- reachable and org-wide sharing is the marketplace's copy-on-publish
    -- concern, so a discriminator column would only ever hold one value.
    -- Uses source<>'system' (not source='user') because prompts.source
    -- has three valid values (system|user|imported) — both non-system
    -- variants require a creator. event_handlers, whose enum is just
    -- system|user, uses a tighter source='user' form.
    CONSTRAINT prompts_system_has_no_creator CHECK (
        (source = 'system' AND creator_user_id IS NULL)
        OR (source <> 'system' AND creator_user_id IS NOT NULL)
    ),
    -- Per-team re-seed idempotency key: each team gets exactly one copy of
    -- each shipped prompt (same system_slug, distinct team_id). NULLs are
    -- distinct in both dialects, so user prompts (system_slug NULL) never
    -- collide with each other.
    CONSTRAINT prompts_org_team_slug_unique UNIQUE (org_id, team_id, system_slug)
);

CREATE TABLE system_prompt_versions (
    prompt_id     TEXT PRIMARY KEY REFERENCES prompts(id) ON DELETE CASCADE,
    content_hash  TEXT NOT NULL,
    applied_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- === Blueprints ==========================================================
-- A blueprint is the triggerable, team-scoped composition: an ordered list
-- of prompt steps (blueprint_steps), length >= 1. Everything an event
-- handler fires is a blueprint; a single prompt is just a 1-step blueprint.
-- Modeled on prompts (same team-scoping, same system_slug idempotency key,
-- same composite uniques so the trigger + step FKs can be same-team-guarded).
CREATE TABLE blueprints (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    -- source is app-validated, not CHECK-constrained (mirrors prompts): new
    -- provenance values must be addable without a SQLite table rebuild. The
    -- *_system_has_no_creator pairing below is the only source invariant the
    -- DB still enforces.
    source          TEXT NOT NULL DEFAULT 'user',
    usage_count     INTEGER DEFAULT 0,
    hidden          BOOLEAN DEFAULT 0,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    user_modified   INTEGER NOT NULL DEFAULT 0,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    -- team_id is NOT NULL: every blueprint is team-owned (mirrors prompts).
    team_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000010',
    -- creator_user_id is nullable: source='system' rows have NULL (no human
    -- author); other sources must have a value. Enforced by the CHECK below.
    creator_user_id TEXT,
    -- system_slug is the stable shipped identifier ("system-ci-fix", …) for
    -- source='system' rows; NULL for user/imported blueprints. Re-seed
    -- idempotency + slug→id lookups key on (org_id, team_id, system_slug).
    system_slug     TEXT,
    -- deleted_at soft-deletes a blueprint (mirrors prompts). Auto-wrapping
    -- makes every new prompt's blueprint un-hard-deletable (its step FK is
    -- RESTRICT, and a blueprint a trigger fired has run history), so deleting
    -- the prompt that solely constitutes a 1-step blueprint soft-deletes both;
    -- request-facing reads filter deleted_at IS NULL, ...System reads don't.
    deleted_at      DATETIME,
    CONSTRAINT blueprints_system_has_no_creator CHECK (
        (source = 'system' AND creator_user_id IS NULL)
        OR (source <> 'system' AND creator_user_id IS NOT NULL)
    ),
    CONSTRAINT blueprints_org_team_slug_unique UNIQUE (org_id, team_id, system_slug)
);

-- === Event catalog =======================================================
CREATE TABLE events_catalog (
    id           TEXT PRIMARY KEY,
    source       TEXT NOT NULL,
    category     TEXT NOT NULL,
    label        TEXT NOT NULL,
    description  TEXT NOT NULL
);

-- === Preferences / settings ==============================================
-- Per-user, keyed on user_id (matches the Postgres baseline so local=multi at
-- N=1). The summary_md is the user's personalized dashboard brief. In local
-- mode the single row hangs off the sentinel local user. ON DELETE CASCADE so
-- a removed user takes their preferences with them.
CREATE TABLE preferences (
    user_id     TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    summary_md  TEXT,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Host-level config: SQLite-only. PG has no analog because in multi mode
-- the port comes from container env.
-- Single-row table (CHECK id=1) so config.Load/Save can upsert
-- without a WHERE-search.
CREATE TABLE instance_config (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    server_port INTEGER NOT NULL DEFAULT 3000,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- === Tenancy (orgs / teams / users) ======================================
CREATE TABLE orgs (
    id         TEXT PRIMARY KEY,
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE teams (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug       TEXT NOT NULL,
    name       TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (org_id, slug)
);

CREATE TABLE users (
    id                TEXT PRIMARY KEY,
    display_name      TEXT,
    avatar_url        TEXT,
    timezone          TEXT NOT NULL DEFAULT 'UTC',
    default_org_id    TEXT,
    -- last_acting_team_id is the user's sticky default team — the team the
    -- per-page filter and the write-time picker seed to when ≥2 teams are
    -- visible. Soft reference (no FK), mirroring default_org_id above: a
    -- deleted team leaves a dangling id that the acting-team resolver
    -- simply ignores (membership re-validation drops it), so no cascade
    -- is needed and the column stays declaration-order-independent.
    last_acting_team_id TEXT,
    -- Per-user Jira identity (account_id + display_name) is NOT a column:
    -- it moved to the host-scoped user_jira_identities sibling below,
    -- mirroring how the GitHub login moved to
    -- user_github_identities. A single human can hold a different Jira
    -- account on each host, so the identity is keyed (user_id, host),
    -- never a lone column.
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- user_github_identities — host-scoped GitHub identity bindings.
-- Replaces the single users.github_username column: one human can hold a
-- different login on each GitHub host (github.com for one org, a corp GHES
-- for another), so the natural key is (user_id, github_base_url), not a lone
-- column. For the first self-deploy (one org, one host) this is exactly one
-- row per user — the column behaviour, with a future-proof key.
--
-- source records HOW the binding was captured ('pat' | 'connect_oauth' |
-- 'scim' | 'login_claim') — load-bearing for the "verified against the
-- org's host, never typed-unverified" integrity rule and for trusting /
-- distrusting a row later. verified_at timestamps the last authenticated
-- /user confirmation against the host — the hook for future drift re-checks
-- (rename / left-the-org). It is deliberately nullable: NULL is a meaningful
-- state, "login known but not yet host-verified" — the shape a future SCIM
-- sync (source='scim') would write when it learns a login from the directory
-- without an authenticated round-trip to the host. Today's writers (pat,
-- login_claim) always stamp it. An absent row is a durable, supported state:
-- the NULL-degrades-gracefully contract carries over unchanged.
CREATE TABLE user_github_identities (
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    github_base_url TEXT NOT NULL,
    login           TEXT NOT NULL,
    source          TEXT NOT NULL
                        CHECK (source IN ('pat', 'connect_oauth', 'scim', 'login_claim')),
    verified_at     TIMESTAMP,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, github_base_url)
);

-- user_jira_identities — host-scoped Jira identity bindings.
-- The Jira sibling of user_github_identities: it replaces the single-valued
-- users.jira_account_id / users.jira_display_name columns. One human can hold
-- a different Jira account on each Jira site (a Cloud site for one org, a
-- Server/DC host for another), so the natural key is (user_id, jira_base_url),
-- not a lone column. For the first self-deploy (one org, one host) this is
-- exactly one row per user — the column behaviour, with a future-proof key.
--
-- The access layer already keys per-(user, host): the per-user Jira PAT is
-- custodied as "jira_token/<host>". This table makes IDENTITY
-- symmetric with that access — both keyed on the same canonical host — so a
-- second Jira site can't overwrite the first's identity the way the single
-- column did.
--
-- account_id is the Atlassian StableID (Cloud accountId, else Server/DC key —
-- auth.JiraUser.StableID()); display_name is the Jira-side display name. Both
-- are captured together from the /myself validation at PAT-bind time. source
-- records HOW the binding was captured ('pat' | 'connect_oauth' | 'scim') —
-- 'pat' is the only writer today (DC paste-a-PAT); Cloud OAuth and SCIM are
-- later tickets. The source CHECK is KEPT (here and on user_github_identities)
-- on purpose: identity provenance is security-relevant and the value set is
-- closed. The asymmetry — GitHub allows 'login_claim', Jira does not — is also
-- deliberate: a GitHub login can be minted from a delegated-run PR's claimed
-- author, but a Jira account is never inferred from a login claim. verified_at
-- timestamps the last authenticated /myself
-- confirmation against the host (nullable for a future SCIM directory sync
-- that learns an account without a round-trip; today's pat writer always
-- stamps it). An absent row is a durable, supported state: the
-- NULL-degrades-gracefully contract carries over unchanged.
CREATE TABLE user_jira_identities (
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    jira_base_url TEXT NOT NULL,
    account_id    TEXT NOT NULL,
    display_name  TEXT,
    source        TEXT NOT NULL
                      CHECK (source IN ('pat', 'connect_oauth', 'scim')),
    verified_at   TIMESTAMP,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, jira_base_url)
);

CREATE TABLE org_memberships (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_id     TEXT NOT NULL REFERENCES orgs(id)  ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'member'
                  CHECK (role IN ('owner', 'admin', 'member')),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, org_id)
);

CREATE TABLE memberships (
    user_id    TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    team_id    TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'member'
                  CHECK (role IN ('admin', 'member', 'viewer')),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, team_id)
);

-- === Tenant-scoped settings ==============================================
-- Shapes mirror the Postgres baseline so local=multi at N=1. Each table
-- holds at most one row per tenant; first config.Save() upserts the row.
-- Intervals stored as TEXT in Go time.Duration string form (e.g. "5m0s")
-- — there is no SQLite interval type, and storing as TEXT keeps the
-- parse logic in the application layer the same in both backends.
CREATE TABLE org_settings (
    org_id                  TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    github_base_url         TEXT,
    github_poll_interval    TEXT NOT NULL DEFAULT '5m0s',
    -- HTTPS is the default because it is the only protocol that can carry
    -- the org's own GitHub credential: a PAT and an App installation token are
    -- both HTTPS bearer credentials, so an SSH clone authenticates as whoever
    -- owns the operator's key instead, and the per-run credential proxy — with
    -- its ref policy and push recording — has nothing to attach.
    github_clone_protocol   TEXT NOT NULL DEFAULT 'https'
                                CHECK (github_clone_protocol IN ('https', 'ssh')),
    jira_base_url           TEXT,
    jira_poll_interval      TEXT NOT NULL DEFAULT '5m0s',
    -- Vault refs (not raw secrets) for Anthropic / Bedrock credentials.
    -- NULL means "use the deployment default" or "not configured yet" on a
    -- self-host. Self-host single-tenant deployments typically leave both NULL
    -- and supply ANTHROPIC_API_KEY via env to the spawner.
    anthropic_api_key_ref   TEXT,
    bedrock_credentials_ref TEXT,
    -- Max model tier the org permits teams/users to pick. NULL means no cap
    -- (any tier allowed). The inheritance helper reads this to clamp
    -- team_settings.default_model and per-prompt overrides.
    --
    -- App-validated, not CHECK-constrained: the column is an opaque,
    -- provider-agnostic model/capability identifier. The app knows
    -- 'haiku' | 'sonnet' | 'opus' today (validated in the settings handler),
    -- but dropping the CHECK lets OpenAI / future model families be added with
    -- zero DDL. A richer provider/model split (separate provider + tier
    -- columns) stays additive — new columns, additive — so the column is
    -- not renamed and the app layer is unchanged.
    max_llm_model_tier      TEXT,
    updated_at              TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE team_settings (
    team_id                       TEXT PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    -- JSON array of project keys. Stored as JSON text since SQLite has no array type.
    jira_projects                 TEXT NOT NULL DEFAULT '[]',
    -- AI knobs ship NOT NULL with defaults matching config.Default() so
    -- Load can scan directly into int and a freshly-inserted row never
    -- needs a "use the app default" decoding step. The numbers are
    -- counts (events seen) — not durations — kept aligned with the
    -- Go-side defaults in config.Default().
    ai_reprioritize_threshold     INTEGER NOT NULL DEFAULT 5,
    ai_preference_update_interval INTEGER NOT NULL DEFAULT 20,
    -- Team-scope AI behavior policy. Moved off user_settings: in v1 the
    -- team owns the model + auto-delegate toggle, users do not override.
    -- default_model is the Claude tier used for scoring + agent runs
    -- (clamped by org_settings.max_llm_model_tier when set).
    -- auto_delegate_enabled defaulted FALSE in this shipped (frozen)
    -- baseline; migration 202607160002 rebuilds the table to default it
    -- TRUE, matching the Postgres baseline and domain.DefaultTeamSettings.
    default_model                 TEXT NOT NULL DEFAULT 'sonnet',
    auto_delegate_enabled         INTEGER NOT NULL DEFAULT 0,
    updated_at                    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Reserved for future per-user prefs (theme, notification destinations,
-- swipe sensitivity, onboarding state). The AI model + auto-delegate
-- toggle that used to live here moved to team_settings — the team owns
-- the policy, users don't override in v1.
CREATE TABLE user_settings (
    user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- One row per (team, jira_project). Team-keyed (not org-keyed) so
-- different teams within an org can give the same Jira project
-- different pickup/in_progress/done semantics — same domain shape as
-- team_settings.jira_projects, which is also team-level. Local mode
-- treats all projects uniformly: config.Save() writes one row per
-- jira_projects entry with identical values for the LocalDefaultTeam.
CREATE TABLE jira_project_status_rules (
    team_id               TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    project_key           TEXT NOT NULL,
    pickup_members        TEXT NOT NULL DEFAULT '[]',
    in_progress_members   TEXT NOT NULL DEFAULT '[]',
    in_progress_canonical TEXT,
    done_members          TEXT NOT NULL DEFAULT '[]',
    done_canonical        TEXT,
    updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, project_key),
    -- A row is the team's commitment to track this Jira project. The
    -- HTTP handler rejects partial saves, and these CHECKs are the
    -- belt-and-suspenders guarantee that any persisted row carries a
    -- non-empty pickup set + members + canonical for both write-target
    -- rules. The "canonical is in members" check is enforced at the
    -- HTTP layer (subqueries-in-CHECK aren't portable to PG) — a row
    -- that bypasses validation with a stale canonical would surface
    -- as a TransitionTo failure at runtime, visible rather than silent.
    CONSTRAINT jpsr_pickup_populated CHECK (
        pickup_members <> '' AND pickup_members <> '[]'
    ),
    CONSTRAINT jpsr_in_progress_populated CHECK (
        in_progress_members <> '' AND in_progress_members <> '[]'
        AND in_progress_canonical IS NOT NULL AND in_progress_canonical <> ''
    ),
    CONSTRAINT jpsr_done_populated CHECK (
        done_members <> '' AND done_members <> '[]'
        AND done_canonical IS NOT NULL AND done_canonical <> ''
    )
);

-- One row per (team, github_org_login, github_team_slug). The GitHub
-- twin of jira_project_status_rules: a team declaring "route this
-- GitHub team's review requests to me." Dumb string labels for routing
-- only — no membership resolution, no nested-team traversal, no sync of
-- GitHub's team graph. Fully-qualified with the org login so
-- @acme/frontend and @beta/frontend don't collide. Many GitHub teams
-- can sit under one TF team (the primary "funnel" direction); the
-- reverse (one GitHub team under many TF teams, a shared backlog) stays
-- permitted. Rows are pure key tuples, so edits are replace-sets, never
-- in-place updates. Local mode (N=1) self-maps the single team.
CREATE TABLE team_github_groups (
    team_id          TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    github_org_login TEXT NOT NULL,
    github_team_slug TEXT NOT NULL,
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, github_org_login, github_team_slug),
    CONSTRAINT tgg_org_login_populated CHECK (github_org_login <> ''),
    CONSTRAINT tgg_team_slug_populated CHECK (github_team_slug <> '')
);

-- One row per (team, owner, repo): a team declaring "I track this repo."
-- The GitHub *tracking-scope* twin of jira_project_status_rules and the
-- source of truth for which repos a team cares about. NOT to be confused
-- with team_github_groups above, which maps CODEOWNERS review-routing
-- teams — this table is the tracking selection.
--
-- repo_profiles is the org-shared UNION of every team's rows here — a
-- derived poll/profile/ETag cache, reconciled on every write to this
-- table and never user-written directly anymore. No org_id column: org
-- scope rides the teams FK, mirroring jira_project_status_rules. Local
-- mode (N=1) tracks every configured repo on the single default team, so
-- the router's team↔repo gate never drops anything there.
CREATE TABLE team_github_repos (
    team_id    TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    owner      TEXT NOT NULL,
    repo       TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, owner, repo),
    CONSTRAINT tgr_owner_populated CHECK (owner <> ''),
    CONSTRAINT tgr_repo_populated CHECK (repo <> '')
);

-- === Agents ==============================================================
CREATE TABLE agents (
    id                            TEXT PRIMARY KEY,
    org_id                        TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    display_name                  TEXT NOT NULL DEFAULT 'Triage Factory Bot',
    default_model                 TEXT,
    default_autonomy_suitability  REAL,
    github_pat_user_id            TEXT REFERENCES users(id) ON DELETE SET NULL,
    jira_service_account_id       TEXT,
    created_at                    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (org_id)
);
CREATE UNIQUE INDEX agents_id_org_unique ON agents (id, org_id);

-- === Per-org GitHub App registration =====================================
-- Mirrors public.org_github_apps + public.org_github_app_installations in
-- the Postgres baseline (RLS-free; SQLite has one connection). Per-org
-- App registration is the v1 multi-mode default — local mode never
-- writes here (the credential resolver falls through to tier 3, PAT-
-- borrow via agents.github_pat_user_id), but the tables exist so the
-- cross-backend conformance tests can exercise the same schema.
CREATE TABLE org_github_apps (
    org_id                 TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    app_id                 TEXT NOT NULL UNIQUE,
    slug                   TEXT NOT NULL,
    client_id              TEXT NOT NULL,
    client_secret_ref      TEXT NOT NULL,
    pem_ref                TEXT NOT NULL,
    webhook_secret_ref     TEXT NOT NULL,
    -- 'user' (personal account) or 'org' (organization). Captured from the
    -- signed registration state at callback time so the App account type
    -- survives a reload; the picker default is 'user'.
    owner_type             TEXT NOT NULL DEFAULT 'user',
    registered_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    registered_by_user_id  TEXT REFERENCES users(id) ON DELETE SET NULL,
    active                 INTEGER NOT NULL DEFAULT 1
);

-- installation_id is unique only within a single GitHub host. Per-org
-- GHES bases mean two orgs can sit on independent GitHub instances whose
-- numeric installation IDs overlap, so the PK is composite
-- (org_id, installation_id): a webhook/backfill for one org can never
-- collide with or rewrite another org's row.
CREATE TABLE org_github_app_installations (
    installation_id  TEXT NOT NULL,
    org_id           TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    account_type     TEXT NOT NULL CHECK (account_type IN ('Organization','User')),
    account_login    TEXT NOT NULL,
    installed_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    removed_at       TIMESTAMP,
    PRIMARY KEY (org_id, installation_id)
);
-- Partial unique on active rows only: uninstall + reinstall cycles
-- stamp removed_at on the old row and insert a new row with a fresh
-- installation_id, preserving history without mutating the PK.
CREATE UNIQUE INDEX org_github_app_installations_active_account_key
    ON org_github_app_installations (org_id, account_login)
    WHERE removed_at IS NULL;
CREATE INDEX org_github_app_installations_org_idx
    ON org_github_app_installations (org_id);

CREATE TABLE team_agents (
    team_id                        TEXT NOT NULL REFERENCES teams(id)  ON DELETE CASCADE,
    agent_id                       TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    enabled                        INTEGER NOT NULL DEFAULT 1,
    per_team_model                 TEXT,
    per_team_autonomy_suitability  REAL,
    added_at                       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, agent_id)
);
CREATE INDEX idx_team_agents_agent ON team_agents(agent_id);

-- === Projects + entities =================================================
CREATE TABLE projects (
    id                        TEXT PRIMARY KEY,
    name                      TEXT NOT NULL,
    description               TEXT NOT NULL DEFAULT '',
    curator_session_id        TEXT,
    pinned_repos              TEXT NOT NULL DEFAULT '[]',
    created_at                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    jira_project_key          TEXT,
    linear_project_key        TEXT,
    spec_authorship_blueprint_id TEXT REFERENCES blueprints(id) ON DELETE SET NULL,
    org_id                    TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    team_id                   TEXT,
    creator_user_id           TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100',
    visibility                TEXT NOT NULL DEFAULT 'team'
        CHECK (visibility IN ('private','team','org')),
    CONSTRAINT projects_team_visibility_requires_team CHECK (
        visibility <> 'team' OR team_id IS NOT NULL
    )
);

CREATE TABLE entities (
    id                       TEXT PRIMARY KEY,
    source                   TEXT NOT NULL,
    source_id                TEXT NOT NULL,
    kind                     TEXT NOT NULL,
    title                    TEXT,
    url                      TEXT,
    snapshot_json            TEXT,
    description              TEXT NOT NULL DEFAULT '',
    state                    TEXT NOT NULL DEFAULT 'active',
    created_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_polled_at           DATETIME,
    closed_at                DATETIME,
    project_id               TEXT REFERENCES projects(id) ON DELETE SET NULL,
    -- owning_team_id is the explicit-override tier of the author-centric
    -- owning-team ladder: NULL = derive (project → prior task → author
    -- identity); a set value pins the entity to a team (transfer op /
    -- TF-origin PR stamp). ON DELETE SET NULL so deleting a team drops the
    -- override rather than orphaning the row.
    owning_team_id           TEXT REFERENCES teams(id) ON DELETE SET NULL,
    classified_at            DATETIME,
    classification_rationale TEXT,
    org_id                   TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    UNIQUE(source, source_id)
);
CREATE INDEX idx_entities_state         ON entities(state);
CREATE INDEX idx_entities_source_polled ON entities(source, last_polled_at);
CREATE INDEX idx_entities_closed_at     ON entities(closed_at) WHERE closed_at IS NOT NULL;
CREATE INDEX idx_entities_project_id    ON entities(project_id) WHERE project_id IS NOT NULL;

CREATE TABLE entity_links (
    from_entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_entity_id   TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    origin         TEXT NOT NULL,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    PRIMARY KEY (from_entity_id, to_entity_id, kind)
);
CREATE INDEX idx_entity_links_from_kind ON entity_links(from_entity_id, kind);
CREATE INDEX idx_entity_links_to_kind   ON entity_links(to_entity_id, kind);

-- === Events ==============================================================
CREATE TABLE events (
    id            TEXT PRIMARY KEY,
    entity_id     TEXT REFERENCES entities(id),
    event_type    TEXT NOT NULL REFERENCES events_catalog(id),
    dedup_key     TEXT NOT NULL DEFAULT '',
    metadata_json TEXT,
    occurred_at   DATETIME,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);
CREATE INDEX idx_events_entity_created  ON events(entity_id, created_at DESC);
CREATE INDEX idx_events_type_created    ON events(event_type, created_at DESC);
CREATE INDEX idx_events_entity_occurred ON events(entity_id, occurred_at DESC);
CREATE INDEX idx_events_type_entity     ON events(event_type, entity_id) WHERE entity_id IS NOT NULL;

-- === Event handlers (rules + triggers) ====================================
CREATE TABLE event_handlers (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    creator_user_id TEXT,
    -- team_id is NOT NULL: every handler is team-owned (shipped
    -- rows are per-team copies, so the old org-visible / team_id-NULL tier is
    -- gone). The LocalDefaultTeamID sentinel is the DEFAULT so local-mode
    -- inserts and direct INSERTs that omit it land on the sole team. There
    -- is no visibility column — team_id is the sole scoping signal.
    team_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000010',

    kind            TEXT NOT NULL CHECK (kind IN ('rule','trigger')),

    event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,
    enabled              BOOLEAN NOT NULL DEFAULT 1,
    -- source is app-validated, not CHECK-constrained (mirrors prompts /
    -- blueprints): 'system' | 'user' today, but new provenance values must be
    -- addable without a SQLite table rebuild. The creator pairing below is the
    -- only source invariant the DB still enforces.
    source               TEXT NOT NULL DEFAULT 'user',
    -- system_slug is the stable shipped identifier ("system-rule-ci-check-failed",
    -- "system-trigger-ci-fix", …) for source='system' rows; NULL for user
    -- handlers. The id is a random UUID per team copy, so re-seed idempotency
    -- keys on (org_id, team_id, system_slug) instead of the id.
    system_slug          TEXT,

    -- Rule-only fields. NULL for triggers.
    name             TEXT,
    default_priority REAL,
    sort_order       INTEGER,

    -- Trigger-only fields. NULL for rules.
    blueprint_id             TEXT,
    breaker_threshold        INTEGER,
    min_autonomy_suitability REAL,

    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,

    FOREIGN KEY (blueprint_id, org_id) REFERENCES blueprints (id, org_id) ON DELETE CASCADE,
    -- Same-team guard: a trigger may only fire a blueprint its own team owns.
    -- (blueprint_id, team_id) → blueprints(id, team_id) refuses a handler
    -- whose team_id doesn't match the referenced blueprint's team. NULL
    -- blueprint_id (rules) skips the FK. CASCADE mirrors the org FK so a
    -- blueprint delete still removes its triggers.
    FOREIGN KEY (blueprint_id, team_id) REFERENCES blueprints (id, team_id) ON DELETE CASCADE,

    CHECK (kind <> 'rule' OR (
        blueprint_id IS NULL
        AND breaker_threshold IS NULL
        AND min_autonomy_suitability IS NULL
        AND name IS NOT NULL
        AND default_priority IS NOT NULL
        AND sort_order IS NOT NULL
    )),
    CHECK (kind <> 'trigger' OR (
        blueprint_id IS NOT NULL
        AND breaker_threshold IS NOT NULL
        AND min_autonomy_suitability IS NOT NULL
        AND default_priority IS NULL
        AND sort_order IS NULL
        AND name IS NULL
    )),
    -- Harmonized to the tolerant source<>'system' form (was source='user')
    -- so it survives the source CHECK drop above: any non-system provenance
    -- (a future 'imported'/marketplace handler) still requires a creator,
    -- matching the prompts / blueprints pairing.
    CONSTRAINT event_handlers_system_has_no_creator CHECK (
        (source = 'system' AND creator_user_id IS NULL)
        OR (source <> 'system' AND creator_user_id IS NOT NULL)
    ),
    -- Per-team re-seed idempotency key: each team gets one copy of each
    -- shipped handler (same system_slug, distinct team_id). NULLs distinct,
    -- so user handlers (system_slug NULL) never collide.
    CONSTRAINT event_handlers_org_team_slug_unique UNIQUE (org_id, team_id, system_slug)
);
CREATE UNIQUE INDEX event_handlers_id_org_unique          ON event_handlers (id, org_id);
CREATE INDEX        idx_event_handlers_event_type_enabled ON event_handlers(org_id, event_type) WHERE enabled = 1;
CREATE INDEX        idx_event_handlers_kind               ON event_handlers(org_id, kind);
CREATE INDEX        idx_event_handlers_blueprint          ON event_handlers(org_id, blueprint_id) WHERE blueprint_id IS NOT NULL;
-- A blueprint is fired by exactly one event: at most one trigger may reference
-- a given blueprint. blueprint_id IS NOT NULL already implies kind='trigger'
-- (the rule-shape CHECK pins rules to a NULL blueprint_id), so the partial
-- predicate needs no kind clause. The mirror of the copy-only step index — that
-- one keeps a prompt in one blueprint, this one keeps a blueprint behind one
-- event. Events still fan out 1:many (one event_type may fire many blueprints
-- via many distinct trigger rows); this only bounds the per-blueprint side.
CREATE UNIQUE INDEX event_handlers_one_trigger_per_blueprint ON event_handlers(blueprint_id) WHERE blueprint_id IS NOT NULL;

-- === Org default templates ================================================
-- The org-level template a new team's prompts + handlers are copied from at
-- team creation. Org-scoped (no team_id): the template is the *source*, not a
-- team-owned set — BootstrapNewTeam materializes a per-team copy of it. Mirrors
-- the prompts / event_handlers content columns so the copy reproduces them
-- byte-for-byte. system_slug is NOT NULL and stable: the real shipped slug for
-- source='system' rows, a generated tmpl-<uuid> for admin-authored rows, so the
-- per-team copy dedupes on (org_id, team_id, system_slug) the same way the
-- shipped seed does. Local mode (N=1) never populates these — the template is a
-- multi-mode concept; seedDefaultPrompts still seeds the sole team from the
-- shipped lists directly.
CREATE TABLE org_template_prompts (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    -- Stable identifier the per-team copy keys on. Shipped rows carry the real
    -- slug ("system-ci-fix"); admin-authored rows get a generated tmpl-<uuid>.
    system_slug   TEXT NOT NULL,
    name          TEXT NOT NULL,
    body          TEXT NOT NULL,
    -- source drives the per-team copy's source (and the editor's delete-vs-keep
    -- affordance): 'system' rows copy as system (creator NULL); 'user' rows are
    -- admin-authored org defaults and copy as team-owned user prompts.
    -- App-validated, not CHECK-constrained (mirrors prompts), so new provenance
    -- values copy through without a table rebuild.
    source        TEXT NOT NULL DEFAULT 'user',
    allowed_tools TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- Idempotency key for the org-create seed (ON CONFLICT) and the parent of
    -- the handler→prompt composite FK below.
    CONSTRAINT org_template_prompts_org_slug_unique UNIQUE (org_id, system_slug)
);
CREATE UNIQUE INDEX org_template_prompts_id_org_unique ON org_template_prompts (id, org_id);

-- Template blueprints: the org-level blueprint a new team's blueprints are
-- copied from at team creation. Org-scoped (no team_id) like the other
-- org_template_* tables — the template is the *source*; MaterializeIntoTeam
-- deep-copies it (header + steps' prompts) into the team's blueprints /
-- blueprint_steps. Mirrors blueprints minus the team scoping. system_slug is
-- NOT NULL and stable (the real shipped slug for source='system' rows, a
-- generated tmpl-<uuid> for admin-authored rows), so the per-team copy dedupes
-- on (org_id, team_id, system_slug) like the shipped seed. A template trigger
-- fires a template blueprint (org_template_handlers.blueprint_id below).
CREATE TABLE org_template_blueprints (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    system_slug   TEXT NOT NULL,
    name          TEXT NOT NULL,
    -- source app-validated, not CHECK-constrained (mirrors blueprints).
    source        TEXT NOT NULL DEFAULT 'user',
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT org_template_blueprints_org_slug_unique UNIQUE (org_id, system_slug)
);
CREATE UNIQUE INDEX org_template_blueprints_id_org_unique ON org_template_blueprints (id, org_id);

-- Ordered step list for a template blueprint (length >= 1). Org-scoped; each
-- step references a template prompt in the same org. Deleting a template
-- blueprint drops its steps (CASCADE); a template prompt can't be deleted while
-- a template blueprint steps through it (RESTRICT) — mirrors blueprint_steps'
-- CASCADE/RESTRICT split, with org_id as the composite-FK partner in place of
-- the team-table's team_id.
CREATE TABLE org_template_blueprint_steps (
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    blueprint_id    TEXT NOT NULL,
    step_index      INTEGER NOT NULL,           -- 0-based; densely packed by ReplaceSteps
    step_prompt_id  TEXT NOT NULL,
    brief           TEXT NOT NULL DEFAULT '',
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (blueprint_id, step_index),
    FOREIGN KEY (blueprint_id, org_id)   REFERENCES org_template_blueprints (id, org_id) ON DELETE CASCADE,
    FOREIGN KEY (step_prompt_id, org_id) REFERENCES org_template_prompts (id, org_id) ON DELETE RESTRICT
);
-- Copy-only prompts (template mirror): a template prompt is a step in at most
-- one template blueprint.
CREATE UNIQUE INDEX idx_org_template_blueprint_steps_step_prompt ON org_template_blueprint_steps(step_prompt_id);

CREATE TABLE org_template_handlers (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    system_slug     TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('rule','trigger')),
    event_type      TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,
    -- enabled flows to the per-team copy verbatim: an org admin enabling a
    -- trigger in the template makes every new team get it enabled (the value
    -- prop — shipped triggers ship disabled, but the org opts in once here).
    enabled         BOOLEAN NOT NULL DEFAULT 1,
    -- source app-validated, not CHECK-constrained (mirrors event_handlers).
    source          TEXT NOT NULL DEFAULT 'user',
    name             TEXT,
    default_priority REAL,
    sort_order       INTEGER,
    blueprint_id     TEXT,
    breaker_threshold INTEGER,
    min_autonomy_suitability REAL,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- A template trigger may only fire a template blueprint in the same org;
    -- deleting a template blueprint cascades its triggers (mirrors the team-table
    -- blueprint→trigger CASCADE). NULL blueprint_id (rules) skips the FK.
    FOREIGN KEY (blueprint_id, org_id) REFERENCES org_template_blueprints (id, org_id) ON DELETE CASCADE,
    CHECK (kind <> 'rule' OR (
        blueprint_id IS NULL
        AND breaker_threshold IS NULL
        AND min_autonomy_suitability IS NULL
        AND name IS NOT NULL
        AND default_priority IS NOT NULL
        AND sort_order IS NOT NULL
    )),
    CHECK (kind <> 'trigger' OR (
        blueprint_id IS NOT NULL
        AND breaker_threshold IS NOT NULL
        AND min_autonomy_suitability IS NOT NULL
        AND default_priority IS NULL
        AND sort_order IS NULL
        AND name IS NULL
    )),
    CONSTRAINT org_template_handlers_org_slug_unique UNIQUE (org_id, system_slug)
);
CREATE INDEX idx_org_template_handlers_kind ON org_template_handlers(org_id, kind);
-- Mirror of the team-table backstop: a template trigger fires exactly one
-- template blueprint.
CREATE UNIQUE INDEX org_template_handlers_one_trigger_per_blueprint ON org_template_handlers(blueprint_id) WHERE blueprint_id IS NOT NULL;

-- === Tasks + runs ========================================================
CREATE TABLE tasks (
    id                   TEXT PRIMARY KEY,
    entity_id            TEXT NOT NULL REFERENCES entities(id),
    event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    dedup_key            TEXT NOT NULL DEFAULT '',
    primary_event_id     TEXT NOT NULL REFERENCES events(id),
    -- status CHECK is KEPT (deliberately, unlike the source / max-tier CHECKs
    -- dropped elsewhere in this baseline). This is the board state-machine: a
    -- closed, stable 6-value set that the UI filters on heavily. The "queue"
    -- is a derived filter over these, not a seventh value, and resume is a
    -- run-level concept — so the enum is complete and not expected to grow.
    -- The DB CHECK is the backstop against a typo'd status from any write path.
    status               TEXT NOT NULL DEFAULT 'queued'
                            CHECK (status IN ('queued','in_progress','in_review','done','dismissed','snoozed')),
    priority_score       REAL,
    ai_summary           TEXT,
    autonomy_suitability REAL,
    priority_reasoning   TEXT,
    scoring_status       TEXT NOT NULL DEFAULT 'pending',
    severity             TEXT,
    relevance_reason     TEXT,
    source_status        TEXT,
    snooze_until         DATETIME,
    close_reason         TEXT,
    close_event_type     TEXT REFERENCES events_catalog(id),
    closed_at            DATETIME,
    created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id               TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    -- team_id is the owning/attributed team and is NULLABLE: NULL = the
    -- owner is unresolved (author-centric routing couldn't pick a single
    -- team). An unowned task is still visible via task_teams and is the
    -- auto-fire gate for free (empty team disables auto-delegation); it
    -- resolves on the first human claim. The DEFAULT carries the local
    -- sentinel for callers that omit the column.
    team_id              TEXT DEFAULT '00000000-0000-0000-0000-000000000010',
    creator_user_id      TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100',
    visibility           TEXT NOT NULL DEFAULT 'team'
                            CHECK (visibility IN ('private','team','org')),
    -- D-Claims: two claim cols + XOR.
    claimed_by_agent_id  TEXT REFERENCES agents(id) ON DELETE SET NULL,
    claimed_by_user_id   TEXT REFERENCES users(id)  ON DELETE SET NULL,
    CONSTRAINT tasks_claim_xor CHECK (claimed_by_agent_id IS NULL OR claimed_by_user_id IS NULL),
    -- A claimed task must have a resolved owner: claiming (human or bot)
    -- consolidates the card to a single team. The unclaimed residual may
    -- carry team_id NULL (unresolved owner, visible via task_teams).
    CONSTRAINT tasks_claimed_requires_team CHECK (
        (claimed_by_user_id IS NULL AND claimed_by_agent_id IS NULL) OR team_id IS NOT NULL
    )
);
-- One task per real situation: identity is (entity, event_type,
-- dedup_key), independent of team. The set of teams an event is
-- relevant to is visibility (task_teams), never count — one event
-- matching N teams' rules yields one task, not N.
CREATE UNIQUE INDEX idx_tasks_active_entity_event_dedup
    ON tasks(entity_id, event_type, dedup_key)
    WHERE status NOT IN ('done', 'dismissed');
CREATE INDEX        idx_tasks_status          ON tasks(status);
CREATE INDEX        idx_tasks_entity          ON tasks(entity_id);
CREATE INDEX        idx_tasks_status_priority ON tasks(status, priority_score DESC);
CREATE UNIQUE INDEX tasks_id_org_unique       ON tasks (id, org_id);
CREATE INDEX        tasks_claimed_agent_idx   ON tasks(claimed_by_agent_id) WHERE claimed_by_agent_id IS NOT NULL;
CREATE INDEX        tasks_claimed_user_idx    ON tasks(claimed_by_user_id)  WHERE claimed_by_user_id  IS NOT NULL;

-- task_teams is the visibility set: the teams whose handlers matched the
-- event that spawned the task. A team sees an unclaimed task iff it is
-- in task_teams (or it is the owning team_id); once claimed the card
-- consolidates to the owning team_id. Local mode (N=1) carries a single
-- row per task for the one team.
CREATE TABLE task_teams (
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    team_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    PRIMARY KEY (task_id, team_id)
);
CREATE INDEX        idx_task_teams_team       ON task_teams(team_id);

CREATE TABLE runs (
    id              TEXT PRIMARY KEY,
    -- task_id / prompt_id are NULLABLE (relaxed from NOT NULL in this
    -- baseline) so a run need not descend from a tracked entity's task or a saved
    -- prompt. Today's runs are all blueprint steps and always carry both; the
    -- runs_origin_requires_parents CHECK below enforces that for origin
    -- 'blueprint'. The headroom is for a future user-initiated *interactive*
    -- run (the "pick repos/branches and just type" surface) — no upstream
    -- event, no task, no saved prompt; its instruction lives in run_messages.
    -- Differentiating that properly needed nullable here, which is a
    -- table-rebuild in a later migration, so it lands now. The interactive parent itself
    -- (an agent_sessions table + a runs.agent_session_id FK — note: NOT the
    -- existing session_id column, which is the Claude SDK resume handle) is
    -- deliberately deferred: CREATE TABLE + ALTER ADD COLUMN are cheap in both
    -- dialects in a later migration, so the undesigned session shape ships with its
    -- feature, not here.
    task_id         TEXT REFERENCES tasks(id),
    prompt_id       TEXT REFERENCES prompts(id),
    trigger_id      TEXT REFERENCES event_handlers(id),
    trigger_type    TEXT NOT NULL DEFAULT 'manual',
    -- origin discriminates how the run was created: 'blueprint' (a step of a
    -- blueprint_run — every run today) vs a future user-initiated kind
    -- ('interactive', …). App-validated, NOT value-CHECK-constrained (mirrors
    -- source / max_llm_model_tier) so new origins need no DDL; the pairing CHECK
    -- below only constrains the 'blueprint' shape, tolerating any other value.
    origin          TEXT NOT NULL DEFAULT 'blueprint',
    status          TEXT NOT NULL DEFAULT 'cloning',
    model           TEXT,
    session_id      TEXT,
    worktree_path   TEXT,
    result_summary  TEXT,
    -- outcome is the parsed terminal-envelope outcome
    -- (continue|finish|abort); NULL on infra-error runs and on blueprint steps
    -- that ended without a recognized conclusion. outcome_reason carries the
    -- natural-language "why I stopped" populated only on abort, kept distinct
    -- from result_summary's "what I did". A turn that ends with no conclusion at
    -- all leaves the run 'open' (not concluded, not executing) rather than
    -- writing an outcome.
    outcome         TEXT,
    outcome_reason  TEXT,
    stop_reason     TEXT,
    started_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at    DATETIME,
    -- parked_at is stamped when the run enters the `open` parked state and
    -- cleared on resume, so the snapshot-retention sweep can key an open run off
    -- its last park rather than started_at (which never resets across resumes).
    -- NULL whenever the run is not parked open; the pending_approval and
    -- completed+abort terminals use completed_at for the same purpose.
    parked_at       DATETIME,
    duration_ms     INTEGER,
    num_turns       INTEGER,
    total_cost_usd  REAL,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    team_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000010',
    -- creator_user_id is nullable for trigger_type='event' rows
    -- (system-fired runs have no human creator); the DEFAULT remains
    -- the synthetic local user so trigger_type='manual' rows whose
    -- callers omit the column land at the sentinel rather than NULL.
    creator_user_id TEXT DEFAULT '00000000-0000-0000-0000-000000000100' REFERENCES users(id),
    visibility      TEXT NOT NULL DEFAULT 'team'
                       CHECK (visibility IN ('private','team','org')),
    actor_agent_id  TEXT REFERENCES agents(id) ON DELETE SET NULL,
    -- blueprint_run_id / blueprint_step_index link a step run back to its
    -- parent blueprint instance. NULLABLE (relaxed from NOT NULL in this
    -- baseline): every run is a blueprint step *today* (a single prompt is a
    -- 1-step blueprint), and the runs_origin_requires_parents CHECK pins it for
    -- origin 'blueprint', so the uniform-execution invariant is unchanged in
    -- practice. The NULL headroom is for a future origin<>'blueprint' run that
    -- isn't a blueprint step (see task_id/origin above). See the Blueprints
    -- section below.
    blueprint_run_id     TEXT REFERENCES blueprint_runs(id),
    blueprint_step_index INTEGER,
    -- triggering_event_id is the event instance that auto-fired this run.
    -- NULL for manual runs and blueprint-step runs. The
    -- runs_event_trigger_fence partial unique index below makes
    -- (triggering_event_id, trigger_id) at-most-once, so a replayed event
    -- under the at-least-once router queue can't spawn a second
    -- delegated run. Forward-only provenance — written by the event path's
    -- CreateIfNotFiredSystem, not read back into the run projection.
    triggering_event_id  TEXT REFERENCES events(id),
    -- Run-queue claim columns (mirror event_queue). A blueprint step is
    -- enqueued as a run row in status='queued'; the dispatcher claims it
    -- (queued -> running) stamping claimed_at and bumping attempts. Both stay
    -- NULL/0 for the legacy never-queued shape; the queue path and the
    -- startup reconcile read them.
    claimed_at           DATETIME,
    attempts             INTEGER NOT NULL DEFAULT 0,
    -- executor_id records which executor instance owns a run's live
    -- process while it is running. Stamped when the run goes live; NULL
    -- for queued / never-live / terminal-parked rows. At N=1 it's a
    -- single per-process instance id (forward-compat ownership hook for
    -- horizontal scaling, where it becomes the lease the control plane
    -- signals through). Not a status — purely the run→executor pointer.
    executor_id          TEXT,
    -- Pair trigger_type with creator_user_id nullability so the
    -- seeder can't drift back to lying.
    CONSTRAINT runs_creator_matches_trigger_type CHECK (
        (trigger_type = 'manual' AND creator_user_id IS NOT NULL)
        OR
        (trigger_type = 'event'  AND creator_user_id IS NULL)
    ),
    CONSTRAINT runs_team_visibility_requires_team CHECK (
        visibility <> 'team' OR team_id IS NOT NULL
    ),
    -- A 'blueprint'-origin run carries its full parentage (it is a step of a
    -- blueprint_run, descended from a task, executing a prompt) — preserving
    -- the previous NOT-NULL invariant for every run today. The tolerant
    -- origin <> 'blueprint' branch leaves a future interactive/ad-hoc run
    -- unconstrained here; its own integrity (an agent_session_id link, etc.)
    -- is enforced app-side and by additive columns when that feature lands.
    CONSTRAINT runs_origin_requires_parents CHECK (
        (origin = 'blueprint'
            AND blueprint_run_id IS NOT NULL
            AND task_id IS NOT NULL
            AND prompt_id IS NOT NULL)
        OR origin <> 'blueprint'
    )
);
CREATE INDEX        idx_runs_task           ON runs(task_id);
CREATE INDEX        idx_runs_prompt_started ON runs(prompt_id, started_at DESC);
CREATE INDEX        idx_runs_trigger        ON runs(trigger_id);
CREATE INDEX        idx_runs_status         ON runs(status);
CREATE UNIQUE INDEX runs_id_org_unique      ON runs (id, org_id);
CREATE INDEX        runs_actor_agent_idx    ON runs(actor_agent_id) WHERE actor_agent_id IS NOT NULL;
CREATE INDEX        idx_runs_blueprint      ON runs(blueprint_run_id, blueprint_step_index);
-- Fired-fence: one event firing one trigger materializes at most
-- one run. Partial WHERE triggering_event_id IS NOT NULL so manual and
-- blueprint-step runs (NULL) never participate — multiple manual runs of one
-- task stay allowed, and two distinct event instances still fire independently.
CREATE UNIQUE INDEX runs_event_trigger_fence ON runs (triggering_event_id, trigger_id) WHERE triggering_event_id IS NOT NULL;
-- Claim index for the run queue: the dispatcher claims the globally-oldest
-- 'queued' run (FIFO by started_at, id). Partial so it only spans the small
-- set of unclaimed work, mirroring idx_event_queue_pending.
CREATE INDEX idx_runs_queued ON runs(started_at, id) WHERE status = 'queued';

-- === Blueprint steps + runs ==============================================
-- blueprint_steps is the ordered step list for a blueprint (length >= 1).
-- Each step references a prompt; a multi-step blueprint runs its steps as
-- fresh Claude sessions sharing one worktree, communicating via a handoff
-- file. Each step's terminal runs.outcome (continue/finish/abort) drives
-- advancement. Per-step runtime state stays on runs (linked via
-- runs.blueprint_run_id); blueprint-wide abort/complete state lives on
-- blueprint_runs.

CREATE TABLE blueprint_steps (
    blueprint_id    TEXT NOT NULL,
    step_index      INTEGER NOT NULL,           -- 0-based; densely packed by ReplaceSteps
    step_prompt_id  TEXT NOT NULL,
    -- team_id is the blueprint's owning team. The composite FKs below pin
    -- both the blueprint AND each step to this team, so a blueprint can only
    -- step through prompts its own team owns. Sentinel default keeps
    -- local-mode (N=1) inserts implicit.
    team_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000010',
    -- Author-supplied one-liner shown in the wrapper user prompt and
    -- in the run-detail UI. Falls back to step_prompt.name when empty.
    brief           TEXT NOT NULL DEFAULT '',
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (blueprint_id, step_index),
    -- Same-team guards: the blueprint resolves against blueprints(id, team_id)
    -- and every step against prompts(id, team_id), so a cross-team step is
    -- refused. CASCADE on the blueprint (deleting it drops its steps);
    -- RESTRICT on the step (a prompt used as a step can't be deleted out from
    -- under it).
    FOREIGN KEY (blueprint_id, team_id)   REFERENCES blueprints (id, team_id) ON DELETE CASCADE,
    FOREIGN KEY (step_prompt_id, team_id) REFERENCES prompts (id, team_id) ON DELETE RESTRICT
);
-- Copy-only prompts: a prompt is a step in AT MOST ONE blueprint. The unique
-- index on step_prompt_id is the durable enforcement of that invariant — to
-- reuse a prompt you copy it. (It also forbids a prompt at two step_indexes
-- within one blueprint, which is intended.) The handler pre-checks for a clean
-- 422; this index is the backstop.
CREATE UNIQUE INDEX idx_blueprint_steps_step_prompt ON blueprint_steps(step_prompt_id);

CREATE TABLE blueprint_runs (
    id              TEXT PRIMARY KEY,
    blueprint_id    TEXT NOT NULL REFERENCES blueprints(id),
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    trigger_type    TEXT NOT NULL DEFAULT 'manual',
    trigger_id      TEXT REFERENCES event_handlers(id),
    -- creator_user_id mirrors runs: the sentinel local user for manual runs,
    -- NULL for event-fired runs (no human author). The trigger-type pairing
    -- CHECK below pins the two together so the firing path can't drift. No FK
    -- in SQLite (the creator_user_id deliberate-divergence category — N=1, the
    -- sentinel user is never deleted); PG FK-constrains it ON DELETE CASCADE.
    -- This closes the runs-has-it / blueprint_runs-doesn't inconsistency.
    creator_user_id TEXT DEFAULT '00000000-0000-0000-0000-000000000100',
    -- triggering_event_id is the event instance that auto-fired this blueprint
    -- run. NULL for manual runs. The blueprint_run is the firing unit, so the
    -- replay fence lives here (relocated off runs): the
    -- blueprint_runs_event_trigger_fence partial unique index below makes
    -- (triggering_event_id, trigger_id) at-most-once, so a replayed event under
    -- the at-least-once router queue can't mint a second blueprint_run. Step
    -- runs are not separately fenced (orchestrator-internal).
    triggering_event_id TEXT REFERENCES events(id),
    -- 'running' | 'completed' | 'aborted' | 'failed' | 'cancelled'. App-validated,
    -- NOT CHECK-constrained on purpose (deliberate-divergence item 6): the
    -- Postgres twin carries a CHECK (cheap to ALTER pre-tenant), but widening a
    -- SQLite CHECK means a full-table rebuild, so the absence of the CHECK IS the
    -- headroom on a table that can't cheaply ALTER. 'aborted' is the
    -- blueprint-level status, distinct from a step run's outcome='abort'.
    status          TEXT NOT NULL DEFAULT 'running',
    -- Durable sequencing for the queue-driven reactor: current_step_index is
    -- the 0-based step the blueprint is on (the step that is queued / running /
    -- parked). The reactor bumps it as it enqueues the next step, so the
    -- sequencing lives in the row rather than on a goroutine stack and a
    -- mid-flight blueprint resumes by re-enqueuing this step at boot.
    current_step_index INTEGER NOT NULL DEFAULT 0,
    -- cancel_requested is the DB sequence-cancel signal: a user/system cancel
    -- sets it, the claim never claims a queued step of a cancel-requested
    -- blueprint, and the reactor finalizes the blueprint 'cancelled' instead of
    -- enqueuing the next step. The active subprocess kill stays in-memory
    -- (s.cancels); this only stops the *sequence* from advancing.
    cancel_requested   INTEGER NOT NULL DEFAULT 0,
    -- step_plan is the resolved step list frozen at mint: a JSON array of
    -- {step_index, prompt_id, prompt_name, prompt_body, source, allowed_tools,
    -- model, brief}, one entry per step. The dispatcher/reactor/resume sequence
    -- off this snapshot rather than re-reading blueprint_steps/prompts, so an
    -- in-flight run executes the plan it was minted with — edits to the
    -- blueprint (ReplaceSteps/SplitAt/MergeInto, a prompt-body edit, a future
    -- prompt-delete) are forward-only and can't mutate a running execution.
    -- Snapshots full prompt content, not just ids, so body edits can't leak in.
    step_plan       TEXT NOT NULL,
    abort_reason    TEXT,
    aborted_at_step INTEGER,
    worktree_path   TEXT NOT NULL,
    started_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at    DATETIME,
    -- Pair trigger_type with creator_user_id nullability (mirrors
    -- runs_creator_matches_trigger_type) so the firing path can't drift back to
    -- a sentinel creator on an event run or a NULL creator on a manual one.
    -- This CHECK is exhaustive over the two current trigger_type values: a
    -- future third value would fail BOTH branches and be rejected, so adding
    -- one needs a new migration to widen this CHECK (and the runs twin). That's
    -- the deliberate contrast with blueprint_runs.status (§D item 6), which is
    -- left app-validated precisely because its value SET is expected to grow;
    -- the trigger_type pairing is a structural invariant, not an open set.
    CONSTRAINT blueprint_runs_creator_matches_trigger_type CHECK (
        (trigger_type = 'manual' AND creator_user_id IS NOT NULL)
        OR
        (trigger_type = 'event'  AND creator_user_id IS NULL)
    )
);
CREATE INDEX idx_blueprint_runs_task   ON blueprint_runs(task_id);
CREATE INDEX idx_blueprint_runs_status ON blueprint_runs(status);
-- Replay fence (relocated from runs): one event firing one trigger materializes
-- at most one blueprint_run. Partial WHERE triggering_event_id IS NOT NULL so
-- manual blueprint runs (NULL) never participate.
CREATE UNIQUE INDEX blueprint_runs_event_trigger_fence ON blueprint_runs (triggering_event_id, trigger_id) WHERE triggering_event_id IS NOT NULL;

-- === Task <-> event mapping + firing queue ===============================
CREATE TABLE task_events (
    task_id    TEXT NOT NULL REFERENCES tasks(id)  ON DELETE CASCADE,
    event_id   TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    PRIMARY KEY (task_id, event_id)
);
CREATE INDEX idx_task_events_task  ON task_events(task_id);
CREATE INDEX idx_task_events_event ON task_events(event_id);

CREATE TABLE pending_firings (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id           TEXT NOT NULL REFERENCES entities(id)       ON DELETE CASCADE,
    task_id             TEXT NOT NULL REFERENCES tasks(id)          ON DELETE CASCADE,
    trigger_id          TEXT NOT NULL REFERENCES event_handlers(id) ON DELETE CASCADE,
    triggering_event_id TEXT NOT NULL REFERENCES events(id),
    status              TEXT NOT NULL DEFAULT 'pending',
    skip_reason         TEXT,
    queued_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    drained_at          DATETIME,
    -- fired_run_id records the blueprint_run a firing produced. The firing unit
    -- is the blueprint_run now (every delegation mints one before any step run
    -- exists), so this references blueprint_runs(id), not runs(id) — the
    -- spawner returns the blueprint_run id synchronously at fire time, long
    -- before the first step run row is created.
    fired_run_id        TEXT REFERENCES blueprint_runs(id),
    org_id              TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);
CREATE INDEX        idx_pending_firings_entity_pending ON pending_firings(entity_id, queued_at) WHERE status = 'pending';
CREATE UNIQUE INDEX idx_pending_firings_dedup          ON pending_firings(task_id, trigger_id)  WHERE status = 'pending';

-- === Durable router event queue =========================================
-- The in-memory event bus drops events for slow subscribers under burst
-- (Publish is a non-blocking send onto a 256-deep channel). The router is
-- the system of record — it persists event rows and creates tasks — so it
-- must not ride that lossy delivery. This is the transactional-outbox
-- queue it drains instead: the events audit row and a queue row are
-- inserted atomically at ingest, so a recorded event is always routable
-- and survives restarts. Sibling to pending_firings above.
--
-- Status lifecycle: pending -> processing -> done | failed. 'processing'
-- rows left by a crash reset to pending at boot (single worker) and
-- replay. SQLite/local is single-worker; the Postgres baseline's
-- equivalent table is drained with FOR UPDATE SKIP LOCKED so multiple
-- router workers can drain concurrently (groundwork; running N is a
-- non-goal).
CREATE TABLE event_queue (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id     TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    entity_id    TEXT REFERENCES entities(id),
    event_type   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',   -- pending | processing | done | failed
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT,
    enqueued_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    claimed_at   DATETIME,
    processed_at DATETIME,
    org_id       TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);
CREATE INDEX        idx_event_queue_pending          ON event_queue(id)                  WHERE status = 'pending';
CREATE INDEX        idx_event_queue_entity           ON event_queue(entity_id);
CREATE INDEX        idx_event_queue_status_processed ON event_queue(status, processed_at);
CREATE UNIQUE INDEX idx_event_queue_event            ON event_queue(event_id);

-- === Run artifacts / messages / memory / worktrees =======================
CREATE TABLE run_artifacts (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    url           TEXT,
    title         TEXT,
    metadata_json TEXT,
    is_primary    BOOLEAN NOT NULL DEFAULT 0,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);
CREATE UNIQUE INDEX idx_run_artifacts_primary_per_run ON run_artifacts(run_id) WHERE is_primary = 1;
CREATE INDEX        idx_run_artifacts_run             ON run_artifacts(run_id);
CREATE INDEX        idx_run_artifacts_kind_created    ON run_artifacts(kind, created_at DESC);

CREATE TABLE run_messages (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id                TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    role                  TEXT NOT NULL,
    content               TEXT,
    subtype               TEXT DEFAULT 'text',
    tool_calls            TEXT,
    tool_call_id          TEXT,
    is_error              BOOLEAN DEFAULT 0,
    metadata              TEXT,
    model                 TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_read_tokens     INTEGER,
    cache_creation_tokens INTEGER,
    created_at            DATETIME DEFAULT CURRENT_TIMESTAMP,
    org_id                TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);
CREATE INDEX idx_run_messages_run ON run_messages(run_id);

CREATE TABLE run_memory (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs(id)     ON DELETE CASCADE,
    entity_id      TEXT NOT NULL REFERENCES entities(id),
    -- Denormalized from the run (like entity_id): the blueprint run this
    -- memory belongs to, or NULL for a standalone run. Groups every step of
    -- one blueprint run's memory together so a step reads its siblings as its
    -- handoff; the on-disk namespace folder falls back to run_id when NULL.
    -- ON DELETE SET NULL so deleting a blueprint run doesn't destroy the
    -- durable memory rows. Single-column FK here (vs the composite
    -- (blueprint_run_id, org_id) the Postgres baseline uses for tenant
    -- isolation): local mode is N=1, blueprint_runs has no org_id column, and
    -- cross-org references are impossible with one org — so there's nothing to
    -- scope. Mirrors the documented "Postgres-only scoping; SQLite/local stays
    -- unscoped" rule.
    blueprint_run_id TEXT REFERENCES blueprint_runs(id) ON DELETE SET NULL,
    agent_content  TEXT,
    human_content  TEXT,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    UNIQUE(run_id)
);
CREATE INDEX idx_run_memory_entity_created   ON run_memory(entity_id, created_at ASC);
CREATE INDEX idx_run_memory_entity_blueprint ON run_memory(entity_id, blueprint_run_id);
CREATE INDEX idx_run_memory_run              ON run_memory(run_id);

CREATE TABLE run_worktrees (
    run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    repo_id        TEXT NOT NULL,
    path           TEXT NOT NULL,
    feature_branch TEXT NOT NULL,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    PRIMARY KEY (run_id, repo_id)
);
CREATE INDEX idx_run_worktrees_run ON run_worktrees(run_id);

-- === Pending PRs =========================================================
CREATE TABLE pending_prs (
    id             TEXT PRIMARY KEY,
    -- run_id is NOT unique (the UNIQUE was dropped at this baseline): one
    -- run may open PRs across several repos (a multi-repo blueprint step, or a
    -- future interactive session). One-PR-per-run is no longer a DB invariant;
    -- where it still holds it is validated app-side. The non-unique index below
    -- keeps the by-run lookup fast. pending_reviews.run_id is likewise
    -- non-unique (the two-PRs-reviewed-in-one-run case is symmetric).
    run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    owner          TEXT NOT NULL,
    repo           TEXT NOT NULL,
    head_branch    TEXT NOT NULL,
    head_sha       TEXT NOT NULL,
    base_branch    TEXT NOT NULL,
    title          TEXT NOT NULL,
    body           TEXT,
    original_title TEXT,
    original_body  TEXT,
    locked         INTEGER NOT NULL DEFAULT 0,
    submitted_at   DATETIME,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    draft          INTEGER NOT NULL DEFAULT 0,
    org_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);
CREATE INDEX idx_pending_prs_run ON pending_prs(run_id);

-- === Pending reviews + comments ==========================================
CREATE TABLE pending_reviews (
    id                    TEXT PRIMARY KEY,
    pr_number             INTEGER NOT NULL,
    owner                 TEXT NOT NULL,
    repo                  TEXT NOT NULL,
    commit_sha            TEXT NOT NULL,
    diff_lines            TEXT,
    -- run_id is FK'd ON DELETE SET NULL (matches PG and the pending_prs.run_id
    -- sibling, which is FK'd on both dialects): a delegated run that opened a
    -- pending review can be deleted without orphaning the review, and the
    -- review survives detached. Nullable to match the SET NULL behavior.
    run_id                TEXT REFERENCES runs(id) ON DELETE SET NULL,
    review_body           TEXT,
    review_event          TEXT,
    created_at            DATETIME DEFAULT CURRENT_TIMESTAMP,
    original_review_body  TEXT,
    original_review_event TEXT,
    diff_hunks            TEXT NOT NULL DEFAULT '',
    org_id                TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);

CREATE TABLE pending_review_comments (
    id            TEXT PRIMARY KEY,
    review_id     TEXT NOT NULL REFERENCES pending_reviews(id),
    path          TEXT NOT NULL,
    line          INTEGER NOT NULL,
    start_line    INTEGER,
    body          TEXT NOT NULL,
    created_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
    original_body TEXT,
    org_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    severity      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_pending_review_comments_review_id ON pending_review_comments(review_id);

-- === Swipe events ========================================================
CREATE TABLE swipe_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    action          TEXT NOT NULL,
    hesitation_ms   INTEGER,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100'
);
CREATE INDEX idx_swipe_events_task           ON swipe_events(task_id);
CREATE INDEX idx_swipe_events_action_created ON swipe_events(action, created_at);

-- === Poller / repo state =================================================
CREATE TABLE poller_state (
    source     TEXT NOT NULL,
    source_id  TEXT NOT NULL,
    state_json TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    org_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    PRIMARY KEY (source, source_id)
);

CREATE TABLE repo_profiles (
    id              TEXT PRIMARY KEY,
    owner           TEXT NOT NULL,
    repo            TEXT NOT NULL,
    description     TEXT,
    has_readme      BOOLEAN DEFAULT 0,
    has_claude_md   BOOLEAN DEFAULT 0,
    has_agents_md   BOOLEAN DEFAULT 0,
    profile_text    TEXT,
    clone_url       TEXT,
    default_branch  TEXT,
    base_branch     TEXT,
    profiled_at     DATETIME,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    clone_status    TEXT NOT NULL DEFAULT 'pending',
    clone_error     TEXT,
    clone_error_kind TEXT,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    -- pulls_etag / pulls_polled_at back the GitHub poller's conditional
    -- open-PR discovery: pulls_etag is the last ETag GitHub returned for
    -- GET /repos/{o}/{r}/pulls?state=open; pulls_polled_at is
    -- the last successful list (200 or 304). A 304 against the stored ETag
    -- means the open-PR set is unchanged and discovery skips the repo.
    pulls_etag      TEXT,
    pulls_polled_at DATETIME,
    UNIQUE(owner, repo)
);
CREATE INDEX idx_repo_profiles_owner_repo ON repo_profiles(owner, repo);

-- === Curator =============================================================
CREATE TABLE curator_requests (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'queued',
    user_input      TEXT NOT NULL,
    error_msg       TEXT,
    cost_usd        REAL NOT NULL DEFAULT 0,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    num_turns       INTEGER NOT NULL DEFAULT 0,
    started_at      DATETIME,
    finished_at     DATETIME,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100'
);
CREATE INDEX idx_curator_requests_project_created ON curator_requests(project_id, created_at);
CREATE INDEX idx_curator_requests_in_flight       ON curator_requests(project_id) WHERE status IN ('queued', 'running');

CREATE TABLE curator_messages (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id            TEXT NOT NULL REFERENCES curator_requests(id) ON DELETE CASCADE,
    role                  TEXT NOT NULL,
    subtype               TEXT NOT NULL DEFAULT 'text',
    content               TEXT NOT NULL DEFAULT '',
    tool_calls            TEXT,
    tool_call_id          TEXT,
    is_error              BOOLEAN NOT NULL DEFAULT 0,
    metadata              TEXT,
    model                 TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_read_tokens     INTEGER,
    cache_creation_tokens INTEGER,
    created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id                TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    creator_user_id       TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100'
);
CREATE INDEX idx_curator_messages_request_created ON curator_messages(request_id, created_at, id);

CREATE TABLE curator_pending_context (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id             TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    curator_session_id     TEXT NOT NULL,
    change_type            TEXT NOT NULL,
    baseline_value         TEXT NOT NULL,
    consumed_at            DATETIME,
    consumed_by_request_id TEXT REFERENCES curator_requests(id) ON DELETE SET NULL,
    created_at             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id                 TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    creator_user_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100'
);
CREATE UNIQUE INDEX idx_curator_pending_context_one_pending_per_type
    ON curator_pending_context(project_id, curator_session_id, change_type)
    WHERE consumed_at IS NULL;
CREATE INDEX        idx_curator_pending_context_consumer
    ON curator_pending_context(consumed_by_request_id)
    WHERE consumed_by_request_id IS NOT NULL;

-- === Cross-table id_org composite-unique indexes =========================
-- Required by the (prompt_id, org_id) composite FK on event_handlers and
-- mirrored across every tenant-scoped table for symmetry with Postgres.
CREATE UNIQUE INDEX prompts_id_org_unique          ON prompts          (id, org_id);
-- Parent key for the same-team composite FKs (blueprint_steps.step_prompt_id
-- references prompts(id, team_id) so a step can only bind a prompt its own
-- team owns).
CREATE UNIQUE INDEX prompts_id_team_unique         ON prompts          (id, team_id);
-- Parent keys for the blueprint composite FKs: event_handlers.blueprint_id
-- and blueprint_steps.blueprint_id reference blueprints(id, org_id) /
-- (id, team_id) so a trigger / step can only bind a blueprint its own team owns.
CREATE UNIQUE INDEX blueprints_id_org_unique        ON blueprints       (id, org_id);
CREATE UNIQUE INDEX blueprints_id_team_unique       ON blueprints       (id, team_id);
CREATE UNIQUE INDEX projects_id_org_unique         ON projects         (id, org_id);
CREATE UNIQUE INDEX entities_id_org_unique         ON entities         (id, org_id);
CREATE UNIQUE INDEX events_id_org_unique           ON events           (id, org_id);
CREATE UNIQUE INDEX pending_reviews_id_org_unique  ON pending_reviews  (id, org_id);
CREATE UNIQUE INDEX curator_requests_id_org_unique ON curator_requests (id, org_id);

-- === System registries (NOT tenant data) =================================
-- No tenancy seed rows here anymore: provisioning the synthetic local
-- org/team/user/memberships/settings is an explicit, user-triggered
-- action ("Start your factory" → db.BootstrapLocalOrg), not a
-- migration- or boot-time side effect. A fresh local DB boots with zero
-- tenant rows; deleted shipped defaults stay deleted across restarts
-- because nothing re-seeds at boot. Multi-mode never seeded tenant rows
-- in its baseline either — both modes now create tenants only via a
-- deliberate provision action through the shared BootstrapNewOrg chain.
--
-- What stays seeded below is system-registry data, identical for every
-- install and not owned by any tenant: instance_config (a singleton) and
-- events_catalog (the event-type registry the events FK targets).
--
-- instance_config is a singleton (id=1 enforced by CHECK). The seed
-- materializes the row so the boot-time read in main.go always finds it
-- instead of degrading to sql.ErrNoRows; the column DEFAULT supplies
-- server_port.
INSERT OR IGNORE INTO instance_config (id) VALUES (1);

-- events_catalog is seeded dynamically at runtime by db.SeedEventTypes
-- (called from db.Migrate after migrations apply), reconciled from
-- domain.AllEventTypes() — not by a static INSERT block here. See
-- internal/db/event_types.go.

-- +goose Down
SELECT 'down not supported';
