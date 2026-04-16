package db

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

// DB wraps a sql.DB connection for passing to subsystems.
type DB struct {
	Conn *sql.DB
}

// Open returns a connection to the SQLite database at ~/.triagefactory/triagefactory.db.
// Creates the directory if it doesn't exist.
func Open() (*sql.DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(home, ".triagefactory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(dir, "triagefactory.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

// Migrate runs the schema migration on the database.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

// schema is the pristine end-state schema per docs/data-model-target.md.
// No ALTER TABLE migrations — this file is the source of truth and assumes the
// on-disk DB can be wiped between major rewrites. Backwards compatibility is
// deliberately **not maintained** as we are still pre-release.
//
// Dependency order: prompts → events_catalog → entities → entity_links →
// events → task_rules / prompt_triggers → tasks → task_events → runs →
// run_artifacts / run_messages / run_memory → ancillary (swipes, reviews,
// poller state, repo profiles, preferences).
const schema = `
-- === Prompts ==============================================================
CREATE TABLE IF NOT EXISTS prompts (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    body TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'user',
    usage_count INTEGER DEFAULT 0,
    hidden BOOLEAN DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- === Events catalog (read-only system registry) ===========================
-- Behavioral + UI-preference concerns live on task_rules / prompt_triggers.
-- Cascades are hardcoded in internal/events/cascades.go (future ticket).
CREATE TABLE IF NOT EXISTS events_catalog (
    id TEXT PRIMARY KEY,                -- the event_type string
    source TEXT NOT NULL,               -- github | jira | linear | slack | system
    category TEXT NOT NULL,             -- pr | issue | review | ...
    label TEXT NOT NULL,                -- human-readable
    description TEXT NOT NULL           -- tooltip copy
);

-- === Entities =============================================================
-- Long-lived source entities (PRs, issues, eventually Slack threads). Replaces
-- Replaces tracked_items (now deleted). Lives from first-poll until closed/merged.
CREATE TABLE IF NOT EXISTS entities (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,               -- github | jira | linear | slack
    source_id TEXT NOT NULL,            -- owner/repo#18, SKY-123, etc.
    kind TEXT NOT NULL,                 -- pr | issue | epic | message
    title TEXT,
    url TEXT,
    snapshot_json TEXT,                 -- opaque poller state (head_sha, CI, draft, etc.)
    state TEXT NOT NULL DEFAULT 'active',  -- active | closed
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_polled_at DATETIME,
    closed_at DATETIME,
    UNIQUE(source, source_id)
);

CREATE INDEX IF NOT EXISTS idx_entities_state ON entities(state);
CREATE INDEX IF NOT EXISTS idx_entities_source_polled ON entities(source, last_polled_at);

-- === Entity links =========================================================
-- Cross-source or within-source relationships. Directional; memory and
-- predicate walks union both directions.
CREATE TABLE IF NOT EXISTS entity_links (
    from_entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,                 -- implements | parent | related
    origin TEXT NOT NULL,               -- branch_name | body_mention | agent | user
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (from_entity_id, to_entity_id, kind)
);

CREATE INDEX IF NOT EXISTS idx_entity_links_from_kind ON entity_links(from_entity_id, kind);
CREATE INDEX IF NOT EXISTS idx_entity_links_to_kind ON entity_links(to_entity_id, kind);

-- === Events (append-only audit log) =======================================
-- Every poller detection or system emission. entity_id nullable for system
-- events without entity context. No idempotency — dedup happens downstream.
CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    entity_id TEXT REFERENCES entities(id),
    event_type TEXT NOT NULL REFERENCES events_catalog(id),
    dedup_key TEXT NOT NULL DEFAULT '',  -- open-set discriminator (label name, status name); empty when event_type alone is the dedup unit
    metadata_json TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_events_entity_created ON events(entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_type_created ON events(event_type, created_at DESC);

-- === Task rules (declarative task creation) ===============================
-- Independent of automation. A user who just wants surfacing (no auto-fire)
-- configures a rule with no matching trigger.
CREATE TABLE IF NOT EXISTS task_rules (
    id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,          -- typed per event type; null = match-all
    enabled BOOLEAN NOT NULL DEFAULT 1,
    name TEXT NOT NULL,
    default_priority REAL NOT NULL DEFAULT 0.5,
    sort_order INTEGER NOT NULL DEFAULT 0,
    source TEXT NOT NULL DEFAULT 'user', -- system (seeded) | user
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_task_rules_event_type_enabled
    ON task_rules(event_type) WHERE enabled = 1;

-- === Prompt triggers (automation rules) ===================================
-- When a trigger's predicate matches an event, fire the bound prompt against
-- the resulting task (or implicitly create a task — see forgiving path).
CREATE TABLE IF NOT EXISTS prompt_triggers (
    id TEXT PRIMARY KEY,
    prompt_id TEXT NOT NULL REFERENCES prompts(id) ON DELETE CASCADE,
    trigger_type TEXT NOT NULL DEFAULT 'event',  -- v1: event only
    event_type TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,          -- typed per event type; null = match-all
    breaker_threshold INTEGER NOT NULL DEFAULT 4,
    cooldown_seconds INTEGER NOT NULL DEFAULT 60,
    min_autonomy_suitability REAL NOT NULL DEFAULT 0.0,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_prompt_triggers_prompt_event_trigger_unique
    ON prompt_triggers(prompt_id, event_type, trigger_type);
CREATE INDEX IF NOT EXISTS idx_prompt_triggers_event_type ON prompt_triggers(event_type) WHERE enabled = 1;
CREATE INDEX IF NOT EXISTS idx_prompt_triggers_prompt_created ON prompt_triggers(prompt_id, created_at);

-- === Tasks ================================================================
-- Actionable situations. Spawned by a rule/trigger match on an event. Dedup
-- via partial unique index: at most one ACTIVE task per (entity, event_type).
CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    entity_id TEXT NOT NULL REFERENCES entities(id),
    event_type TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    dedup_key TEXT NOT NULL DEFAULT '',  -- inherited from primary event; participates in the partial unique index below
    primary_event_id TEXT NOT NULL REFERENCES events(id),
    status TEXT NOT NULL DEFAULT 'queued',  -- queued | claimed | delegated | done | dismissed | snoozed
    priority_score REAL,
    ai_summary TEXT,
    autonomy_suitability REAL,
    priority_reasoning TEXT,
    scoring_status TEXT NOT NULL DEFAULT 'pending', -- pending | in_progress | scored
    severity TEXT,
    relevance_reason TEXT,
    source_status TEXT,
    snooze_until DATETIME,
    close_reason TEXT,                  -- run_completed | user_claimed | user_dismissed | auto_closed_by_event | entity_closed
    close_event_type TEXT REFERENCES events_catalog(id),
    closed_at DATETIME,
    -- Repo match: AI scorer writes the repo IDs this task's work should run
    -- against (critical for Jira tasks whose entity doesn't carry a repo).
    -- The spawner reads matched_repos to pick the worktree. blocked_reason
    -- captures why delegation is blocked (e.g., "multi_repo", "no_repo_match").
    matched_repos TEXT,                 -- JSON array of repo IDs, or NULL
    blocked_reason TEXT,                -- "multi_repo" | "no_repo_match" | NULL
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Partial unique index: at most one active task per (entity, event_type, dedup_key).
-- Most events use dedup_key='' so this collapses to (entity, event_type); open-set
-- discriminator events (label_added, status_changed) get separate tasks per value.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_active_entity_event_dedup
    ON tasks(entity_id, event_type, dedup_key) WHERE status NOT IN ('done', 'dismissed');
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_entity ON tasks(entity_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status_priority ON tasks(status, priority_score DESC);

-- === task_events (junction) ===============================================
-- Every event that contributed to a task, with the kind of contribution.
CREATE TABLE IF NOT EXISTS task_events (
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    event_id TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,                 -- spawned | bumped | closed
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (task_id, event_id)
);

CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id);
CREATE INDEX IF NOT EXISTS idx_task_events_event ON task_events(event_id);

-- === Runs (renamed from agent_runs) =======================================
-- One prompt execution against one task.
CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    prompt_id TEXT NOT NULL REFERENCES prompts(id),
    trigger_id TEXT REFERENCES prompt_triggers(id),
    trigger_type TEXT NOT NULL DEFAULT 'manual',  -- manual | event
    status TEXT NOT NULL DEFAULT 'cloning',
    model TEXT,
    session_id TEXT,                    -- Claude Code session_id for --resume
    worktree_path TEXT,
    result_summary TEXT,
    stop_reason TEXT,
    started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME,
    duration_ms INTEGER,
    num_turns INTEGER,
    total_cost_usd REAL,
    memory_missing BOOLEAN NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_runs_task ON runs(task_id);
CREATE INDEX IF NOT EXISTS idx_runs_prompt_started ON runs(prompt_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_trigger ON runs(trigger_id);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

-- === Run artifacts ========================================================
-- Structured output produced by a run — PRs opened, reviews posted, branches
-- pushed, comments added. Partial unique index enforces exactly one primary.
CREATE TABLE IF NOT EXISTS run_artifacts (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,                 -- github:pr | github:review | jira:issue | link | ...
    url TEXT,
    title TEXT,
    metadata_json TEXT,
    is_primary BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_run_artifacts_primary_per_run
    ON run_artifacts(run_id) WHERE is_primary = 1;
CREATE INDEX IF NOT EXISTS idx_run_artifacts_run ON run_artifacts(run_id);
CREATE INDEX IF NOT EXISTS idx_run_artifacts_kind_created ON run_artifacts(kind, created_at DESC);

-- === Run messages (renamed from agent_messages) ===========================
CREATE TABLE IF NOT EXISTS run_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    content TEXT,
    subtype TEXT DEFAULT 'text',
    tool_calls TEXT,
    tool_call_id TEXT,
    is_error BOOLEAN DEFAULT 0,
    metadata TEXT,
    model TEXT,
    input_tokens INTEGER,
    output_tokens INTEGER,
    cache_read_tokens INTEGER,
    cache_creation_tokens INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_run_messages_run ON run_messages(run_id);

-- === Run memory (renamed from task_memory, with denormalized entity_id) ===
-- Queried by entity via JOIN through runs/tasks — entity_id denormalized for
-- fast entity-scoped lookup during memory materialization.
CREATE TABLE IF NOT EXISTS run_memory (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    entity_id TEXT NOT NULL REFERENCES entities(id),
    content TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(run_id)
);

CREATE INDEX IF NOT EXISTS idx_run_memory_entity_created ON run_memory(entity_id, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_run_memory_run ON run_memory(run_id);

-- === Swipe events (UI interaction log, separate from events) ==============
-- Different subject (UI interaction, not entity state) + different consumers
-- (scorer, analytics — not router/triggers). Kept distinct on purpose.
CREATE TABLE IF NOT EXISTS swipe_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    action TEXT NOT NULL,               -- claim | delegate | dismiss | snooze
    hesitation_ms INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_swipe_events_task ON swipe_events(task_id);
CREATE INDEX IF NOT EXISTS idx_swipe_events_action_created ON swipe_events(action, created_at);

-- === Poller state =========================================================
CREATE TABLE IF NOT EXISTS poller_state (
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    state_json TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source, source_id)
);

-- === Repo profiles ========================================================
CREATE TABLE IF NOT EXISTS repo_profiles (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    repo TEXT NOT NULL,
    description TEXT,
    has_readme BOOLEAN DEFAULT 0,
    has_claude_md BOOLEAN DEFAULT 0,
    has_agents_md BOOLEAN DEFAULT 0,
    profile_text TEXT,
    clone_url TEXT,
    default_branch TEXT,
    base_branch TEXT,
    profiled_at DATETIME,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(owner, repo)
);

CREATE INDEX IF NOT EXISTS idx_repo_profiles_owner_repo ON repo_profiles(owner, repo);

-- === Pending reviews (PR review approval queue) ===========================
CREATE TABLE IF NOT EXISTS pending_reviews (
    id TEXT PRIMARY KEY,
    pr_number INTEGER NOT NULL,
    owner TEXT NOT NULL,
    repo TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    diff_lines TEXT,
    run_id TEXT,
    review_body TEXT,
    review_event TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS pending_review_comments (
    id TEXT PRIMARY KEY,
    review_id TEXT NOT NULL REFERENCES pending_reviews(id),
    path TEXT NOT NULL,
    line INTEGER NOT NULL,
    start_line INTEGER,
    body TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_pending_review_comments_review_id ON pending_review_comments(review_id);

-- === Preferences ==========================================================
CREATE TABLE IF NOT EXISTS preferences (
    id INTEGER PRIMARY KEY,
    summary_md TEXT,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
`
