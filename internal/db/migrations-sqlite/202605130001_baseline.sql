-- +goose Up
-- v1.11.0 consolidated baseline (2026-05-13). Captures the cumulative
-- post-state of the schema chain that ran from the SKY-245 D1 baseline
-- through every post-baseline migration up to and including 202605120012.
--
-- v1.11.0 is a hard reset: any database that does not have *this*
-- migration applied (via goose_db_version) is refused at boot. There
-- is no upgrade path from pre-v1.11.0 installs — operators wipe
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

-- === Prompts =============================================================
CREATE TABLE prompts (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    body            TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'user'
                        CHECK (source IN ('system', 'user', 'imported')),
    -- kind = 'leaf' (single-prompt, today's default) or 'chain'
    -- (an ordered list of leaf steps in prompt_chain_steps). Triggers
    -- fire prompts by id regardless of kind; only the spawner branches.
    kind            TEXT NOT NULL DEFAULT 'leaf',
    usage_count     INTEGER DEFAULT 0,
    hidden          BOOLEAN DEFAULT 0,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    user_modified   INTEGER NOT NULL DEFAULT 0,
    allowed_tools   TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    team_id         TEXT,
    -- creator_user_id is nullable: source='system' rows have NULL (no human author),
    -- source='user' rows must have a value. Enforced by prompts_system_has_no_creator.
    creator_user_id TEXT,
    visibility      TEXT NOT NULL DEFAULT 'team'
        CHECK (visibility IN ('private','team','org')),
    -- Uses source<>'system' (not source='user') because prompts.source
    -- has three valid values (system|user|imported) — both non-system
    -- variants require a creator. event_handlers, whose enum is just
    -- system|user, uses a tighter source='user' form.
    CONSTRAINT prompts_system_has_no_creator CHECK (
        (source = 'system' AND creator_user_id IS NULL)
        OR (source <> 'system' AND creator_user_id IS NOT NULL)
    ),
    CONSTRAINT prompts_team_visibility_requires_team CHECK (
        visibility <> 'team' OR team_id IS NOT NULL
    )
);

CREATE TABLE system_prompt_versions (
    prompt_id     TEXT PRIMARY KEY REFERENCES prompts(id) ON DELETE CASCADE,
    content_hash  TEXT NOT NULL,
    applied_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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
CREATE TABLE preferences (
    id          INTEGER PRIMARY KEY,
    summary_md  TEXT,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Host-level config: SQLite-only. PG has no analog because in multi mode
-- the port comes from container env and there is no takeover concept.
-- Single-row table (CHECK id=1) so config.Load/Save can upsert
-- without a WHERE-search.
CREATE TABLE instance_config (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    server_port         INTEGER NOT NULL DEFAULT 3000,
    server_takeover_dir TEXT NOT NULL DEFAULT '',
    updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- === Tenancy (orgs / teams / users) ======================================
CREATE TABLE orgs (
    id         TEXT PRIMARY KEY,
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    -- SKY-345 mirror of orgs.is_personal in the Postgres baseline.
    -- Local mode only ever creates the single LocalDefaultOrg seed row
    -- (is_personal=0); the column exists so any cross-backend SELECT
    -- against orgs can reference it without dialect-specific branching.
    is_personal INTEGER NOT NULL DEFAULT 0,
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
    -- preferred_team_id is the user's sticky default team — the team the
    -- per-page filter and the write-time picker seed to when ≥2 teams are
    -- visible. Soft reference (no FK), mirroring default_org_id above: a
    -- deleted team leaves a dangling id that the acting-team resolver
    -- simply ignores (membership re-validation drops it), so no cascade
    -- is needed and the column stays declaration-order-independent.
    preferred_team_id TEXT,
    github_username   TEXT,
    -- jira_account_id is the Atlassian-side stable identifier
    -- (Cloud: accountId; Server/DC: legacy key). Used by the
    -- assignee_in / reporter_in / commenter_in predicate matchers.
    -- jira_display_name is the Jira-side display name, used by stock
    -- handlers for "is this assigned to me" checks and the optimistic
    -- post-claim snapshot update. Both captured from auth.ValidateJira
    -- at PAT setup, persisted by bootstrapLocalJiraIdentity at boot.
    jira_account_id   TEXT,
    jira_display_name TEXT,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
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
    github_clone_protocol   TEXT NOT NULL DEFAULT 'ssh'
                                CHECK (github_clone_protocol IN ('https', 'ssh')),
    jira_base_url           TEXT,
    jira_poll_interval      TEXT NOT NULL DEFAULT '5m0s',
    -- Vault refs (not raw secrets) for Anthropic / Bedrock credentials.
    -- NULL means "use deployment default" on hosted SaaS or "not configured
    -- yet" on self-host. Self-host single-tenant deployments typically leave
    -- both NULL and supply ANTHROPIC_API_KEY via env to the spawner.
    anthropic_api_key_ref   TEXT,
    bedrock_credentials_ref TEXT,
    -- Max Claude tier the org permits teams/users to pick. NULL means no
    -- cap (any tier allowed). The inheritance helper (sibling ticket) reads
    -- this to clamp team_settings.default_model and per-prompt overrides.
    max_llm_model_tier      TEXT
                                CHECK (max_llm_model_tier IS NULL
                                       OR max_llm_model_tier IN ('haiku', 'sonnet', 'opus')),
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
    -- auto_delegate_enabled defaults FALSE — new teams don't auto-spawn
    -- agents until explicitly opted in. Local mode flips this true via
    -- the explicit value config.Default() writes on first Save().
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
    spec_authorship_prompt_id TEXT REFERENCES prompts(id) ON DELETE SET NULL,
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

-- === Event handlers (rules + triggers, post-SKY-259) =====================
CREATE TABLE event_handlers (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    creator_user_id TEXT,
    -- team_id ships with the LocalDefaultTeamID sentinel as DEFAULT to
    -- match tasks/runs. With visibility='team' as the default, any
    -- direct INSERT that omitted team_id would otherwise trip
    -- event_handlers_team_visibility_requires_team. System-source
    -- shipped rows (visibility='org') tolerate any team_id value;
    -- the sentinel is consistent across them.
    team_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000010',
    visibility      TEXT NOT NULL DEFAULT 'team'
                       CHECK (visibility IN ('private','team','org')),

    kind            TEXT NOT NULL CHECK (kind IN ('rule','trigger')),

    event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,
    enabled              BOOLEAN NOT NULL DEFAULT 1,
    source               TEXT NOT NULL DEFAULT 'user'
                            CHECK (source IN ('system', 'user')),

    -- Rule-only fields. NULL for triggers.
    name             TEXT,
    default_priority REAL,
    sort_order       INTEGER,

    -- Trigger-only fields. NULL for rules.
    prompt_id                TEXT,
    breaker_threshold        INTEGER,
    min_autonomy_suitability REAL,

    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,

    FOREIGN KEY (prompt_id, org_id) REFERENCES prompts (id, org_id) ON DELETE CASCADE,

    CHECK (kind <> 'rule' OR (
        prompt_id IS NULL
        AND breaker_threshold IS NULL
        AND min_autonomy_suitability IS NULL
        AND name IS NOT NULL
        AND default_priority IS NOT NULL
        AND sort_order IS NOT NULL
    )),
    CHECK (kind <> 'trigger' OR (
        prompt_id IS NOT NULL
        AND breaker_threshold IS NOT NULL
        AND min_autonomy_suitability IS NOT NULL
        AND default_priority IS NULL
        AND sort_order IS NULL
        AND name IS NULL
    )),
    CONSTRAINT event_handlers_system_has_no_creator CHECK (
        (source = 'system' AND creator_user_id IS NULL)
        OR (source = 'user' AND creator_user_id IS NOT NULL)
    ),
    CONSTRAINT event_handlers_team_visibility_requires_team CHECK (
        visibility <> 'team' OR team_id IS NOT NULL
    )
);
CREATE UNIQUE INDEX event_handlers_id_org_unique          ON event_handlers (id, org_id);
CREATE INDEX        idx_event_handlers_event_type_enabled ON event_handlers(org_id, event_type) WHERE enabled = 1;
CREATE INDEX        idx_event_handlers_kind               ON event_handlers(org_id, kind);
CREATE INDEX        idx_event_handlers_prompt             ON event_handlers(org_id, prompt_id) WHERE prompt_id IS NOT NULL;

-- === Tasks + runs ========================================================
CREATE TABLE tasks (
    id                   TEXT PRIMARY KEY,
    entity_id            TEXT NOT NULL REFERENCES entities(id),
    event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    dedup_key            TEXT NOT NULL DEFAULT '',
    primary_event_id     TEXT NOT NULL REFERENCES events(id),
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
    team_id              TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000010',
    creator_user_id      TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100',
    visibility           TEXT NOT NULL DEFAULT 'team'
                            CHECK (visibility IN ('private','team','org')),
    -- SKY-261 D-Claims: two claim cols + XOR.
    claimed_by_agent_id  TEXT REFERENCES agents(id) ON DELETE SET NULL,
    claimed_by_user_id   TEXT REFERENCES users(id)  ON DELETE SET NULL,
    CONSTRAINT tasks_claim_xor CHECK (claimed_by_agent_id IS NULL OR claimed_by_user_id IS NULL),
    CONSTRAINT tasks_team_visibility_requires_team CHECK (
        visibility <> 'team' OR team_id IS NOT NULL
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
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    prompt_id       TEXT NOT NULL REFERENCES prompts(id),
    trigger_id      TEXT REFERENCES event_handlers(id),
    trigger_type    TEXT NOT NULL DEFAULT 'manual',
    status          TEXT NOT NULL DEFAULT 'cloning',
    model           TEXT,
    session_id      TEXT,
    worktree_path   TEXT,
    result_summary  TEXT,
    stop_reason     TEXT,
    started_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at    DATETIME,
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
    -- chain_run_id / chain_step_index link a step run back to its
    -- parent chain instance. NULL on stand-alone (kind='leaf') runs.
    -- See the Prompt chains section below for the parent table.
    chain_run_id     TEXT REFERENCES chain_runs(id),
    chain_step_index INTEGER,
    -- Pair trigger_type with creator_user_id nullability so the
    -- seeder can't drift back to lying.
    CONSTRAINT runs_creator_matches_trigger_type CHECK (
        (trigger_type = 'manual' AND creator_user_id IS NOT NULL)
        OR
        (trigger_type = 'event'  AND creator_user_id IS NULL)
    ),
    CONSTRAINT runs_team_visibility_requires_team CHECK (
        visibility <> 'team' OR team_id IS NOT NULL
    )
);
CREATE INDEX        idx_runs_task           ON runs(task_id);
CREATE INDEX        idx_runs_prompt_started ON runs(prompt_id, started_at DESC);
CREATE INDEX        idx_runs_trigger        ON runs(trigger_id);
CREATE INDEX        idx_runs_status         ON runs(status);
CREATE UNIQUE INDEX runs_id_org_unique      ON runs (id, org_id);
CREATE INDEX        runs_actor_agent_idx    ON runs(actor_agent_id) WHERE actor_agent_id IS NOT NULL;
CREATE INDEX        idx_runs_chain          ON runs(chain_run_id, chain_step_index);

-- === Prompt chains =======================================================
-- Linear sequences of prompt steps that share one worktree. Each step
-- runs as a fresh Claude session in the same worktree; adjacent steps
-- communicate via a handoff file and record proceed/abort verdicts on
-- run_artifacts(kind='chain:verdict'). Per-step runtime state stays on
-- runs (linked via runs.chain_run_id); chain-wide abort/complete state
-- lives on chain_runs.

CREATE TABLE prompt_chain_steps (
    chain_prompt_id TEXT NOT NULL REFERENCES prompts(id) ON DELETE CASCADE,
    step_index      INTEGER NOT NULL,           -- 0-based; densely packed by ReplaceChainSteps
    step_prompt_id  TEXT NOT NULL REFERENCES prompts(id) ON DELETE RESTRICT,
    -- Author-supplied one-liner shown in the wrapper user prompt and
    -- in the run-detail UI. Falls back to step_prompt.name when empty.
    brief           TEXT NOT NULL DEFAULT '',
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (chain_prompt_id, step_index)
);
CREATE INDEX idx_prompt_chain_steps_step_prompt ON prompt_chain_steps(step_prompt_id);

CREATE TABLE chain_runs (
    id              TEXT PRIMARY KEY,
    chain_prompt_id TEXT NOT NULL REFERENCES prompts(id),
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    trigger_type    TEXT NOT NULL DEFAULT 'manual',
    trigger_id      TEXT REFERENCES event_handlers(id),
    -- 'running' | 'completed' | 'aborted' | 'failed' | 'cancelled'
    status          TEXT NOT NULL DEFAULT 'running',
    abort_reason    TEXT,
    aborted_at_step INTEGER,
    worktree_path   TEXT NOT NULL,
    started_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at    DATETIME
);
CREATE INDEX idx_chain_runs_task   ON chain_runs(task_id);
CREATE INDEX idx_chain_runs_status ON chain_runs(status);

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
    fired_run_id        TEXT REFERENCES runs(id),
    org_id              TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);
CREATE INDEX        idx_pending_firings_entity_pending ON pending_firings(entity_id, queued_at) WHERE status = 'pending';
CREATE UNIQUE INDEX idx_pending_firings_dedup          ON pending_firings(task_id, trigger_id)  WHERE status = 'pending';

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
    agent_content  TEXT,
    human_content  TEXT,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    UNIQUE(run_id)
);
CREATE INDEX idx_run_memory_entity_created ON run_memory(entity_id, created_at ASC);
CREATE INDEX idx_run_memory_run            ON run_memory(run_id);

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
    run_id         TEXT NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE,
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
    run_id                TEXT,
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
CREATE UNIQUE INDEX projects_id_org_unique         ON projects         (id, org_id);
CREATE UNIQUE INDEX entities_id_org_unique         ON entities         (id, org_id);
CREATE UNIQUE INDEX events_id_org_unique           ON events           (id, org_id);
CREATE UNIQUE INDEX pending_reviews_id_org_unique  ON pending_reviews  (id, org_id);
CREATE UNIQUE INDEX curator_requests_id_org_unique ON curator_requests (id, org_id);

-- === Tenancy seed rows ===================================================
-- INSERT OR IGNORE is intentional: the application also writes to these
-- tables (via runmode.LocalDefaultOrg / LocalDefaultUserID), so the
-- seed is idempotent — not legacy compatibility. These IDs are
-- referenced as DEFAULT values across many tables above.
INSERT OR IGNORE INTO orgs (id, slug, name) VALUES
    ('00000000-0000-0000-0000-000000000001', 'local', 'Local');

INSERT OR IGNORE INTO teams (id, org_id, slug, name) VALUES
    ('00000000-0000-0000-0000-000000000010',
     '00000000-0000-0000-0000-000000000001',
     'default',
     'Default');

INSERT OR IGNORE INTO users (id, display_name) VALUES
    ('00000000-0000-0000-0000-000000000100', 'You');

INSERT OR IGNORE INTO org_memberships (user_id, org_id, role) VALUES
    ('00000000-0000-0000-0000-000000000100',
     '00000000-0000-0000-0000-000000000001',
     'owner');

INSERT OR IGNORE INTO memberships (user_id, team_id, role) VALUES
    ('00000000-0000-0000-0000-000000000100',
     '00000000-0000-0000-0000-000000000010',
     'admin');

-- Settings rows for the local sentinel org + team. Listing only the
-- PK lets every other column take its NOT NULL DEFAULT from above —
-- a tiny formal restatement of the schema defaults, so the application
-- never has to special-case "row exists but it's a row of defaults" vs
-- "no row at all." The team_settings seed flips auto_delegate_enabled
-- to 1: the schema default is 0 (multi-mode's "new teams require
-- explicit opt-in" rule), but local mode is the auto-delegate happy
-- path and the deleted internal/config.Default() always reported
-- AutoDelegateEnabled=true in this scope. Keeping the schema default
-- conservative (false) for multi-mode is the right call; local mode
-- overrides on the way in via this seed.
--
-- instance_config is a singleton (id=1 enforced by CHECK). The seed
-- below materializes the row so the boot-time read in main.go always
-- finds it instead of degrading to sql.ErrNoRows; column DEFAULTs
-- supply server_port and server_takeover_dir.
INSERT OR IGNORE INTO instance_config (id) VALUES (1);

INSERT OR IGNORE INTO org_settings (org_id) VALUES
    ('00000000-0000-0000-0000-000000000001');

INSERT OR IGNORE INTO team_settings (team_id, auto_delegate_enabled) VALUES
    ('00000000-0000-0000-0000-000000000010', 1);

-- events_catalog seed (system-managed event type registry). Mirrors the
-- equivalent INSERT block in the Postgres baseline; both must stay in
-- sync with domain.AllEventTypes(). New event types ship as a new
-- forward migration, never an in-place baseline edit.
INSERT OR IGNORE INTO events_catalog (id, source, category, label, description) VALUES
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

-- NOTE: This baseline seed is for fresh installs only. After the removal of
-- the runtime event-type seeding/upsert path, edits here do not propagate to
-- existing installs once an event id has already been seeded.
--   * New event types must ship in a new forward migration.
--   * Label/description fixes for an existing event id must also ship in a
--     forward migration using `INSERT ... ON CONFLICT(id) DO UPDATE`, because
--     this baseline uses `INSERT OR IGNORE` and will otherwise no-op.

-- +goose Down
SELECT 'down not supported';
