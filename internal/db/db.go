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

// Open returns a connection to the SQLite database at ~/.todotriage/todotriage.db.
// Creates the directory if it doesn't exist.
func Open() (*sql.DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(home, ".todotriage")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(dir, "todotriage.db")
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

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    source_url TEXT NOT NULL,
    title TEXT NOT NULL,
    description TEXT,
    repo TEXT,
    author TEXT,
    labels TEXT,
    severity TEXT,
    diff_additions INTEGER,
    diff_deletions INTEGER,
    files_changed INTEGER,
    ci_status TEXT,
    relevance_reason TEXT,
    source_status TEXT,
    scoring_status TEXT DEFAULT 'unscored',
    event_type TEXT,
    created_at DATETIME NOT NULL,
    fetched_at DATETIME NOT NULL,
    status TEXT DEFAULT 'queued',
    priority_score REAL,
    ai_summary TEXT,
    priority_reasoning TEXT,
    agent_confidence REAL,
    matched_repos TEXT,
    blocked_reason TEXT,
    snooze_until DATETIME,
    consecutive_unsuccessful_runs INTEGER NOT NULL DEFAULT 0,
    UNIQUE(source, source_id)
);

CREATE TABLE IF NOT EXISTS swipe_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    action TEXT NOT NULL,
    hesitation_ms INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS agent_runs (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    prompt_id TEXT REFERENCES prompts(id),
    status TEXT DEFAULT 'running',
    model TEXT,
    started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME,
    total_cost_usd REAL,
    duration_ms INTEGER,
    num_turns INTEGER,
    stop_reason TEXT,
    worktree_path TEXT,
    result_link TEXT,
    result_summary TEXT,
    session_id TEXT,
    memory_missing BOOLEAN DEFAULT 0,
    trigger_type TEXT NOT NULL DEFAULT 'manual',
    trigger_id TEXT REFERENCES prompt_triggers(id)
);

CREATE TABLE IF NOT EXISTS agent_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES agent_runs(id),
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

CREATE TABLE IF NOT EXISTS preferences (
    id INTEGER PRIMARY KEY,
    summary_md TEXT,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_status_priority ON tasks(status, priority_score DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_snooze_until ON tasks(snooze_until);
CREATE INDEX IF NOT EXISTS idx_agent_runs_task_id ON agent_runs(task_id);
CREATE INDEX IF NOT EXISTS idx_agent_messages_run_id ON agent_messages(run_id);
CREATE INDEX IF NOT EXISTS idx_swipe_events_task_id ON swipe_events(task_id);
CREATE INDEX IF NOT EXISTS idx_pending_review_comments_review_id ON pending_review_comments(review_id);

CREATE TABLE IF NOT EXISTS event_types (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    category TEXT NOT NULL,
    label TEXT NOT NULL,
    description TEXT,
    default_priority REAL DEFAULT 0.5,
    enabled BOOLEAN DEFAULT 1,
    sort_order INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type TEXT NOT NULL REFERENCES event_types(id),
    task_id TEXT REFERENCES tasks(id),
    source_id TEXT NOT NULL,
    metadata TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS poller_state (
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    state_json TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source, source_id)
);

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

CREATE INDEX IF NOT EXISTS idx_agent_runs_prompt_id ON agent_runs(prompt_id);

CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);
CREATE INDEX IF NOT EXISTS idx_events_task_id ON events(task_id);
CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at);
CREATE INDEX IF NOT EXISTS idx_tasks_event_type ON tasks(event_type);

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

CREATE TABLE IF NOT EXISTS tracked_items (
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    task_id TEXT,
    node_id TEXT,
    snapshot TEXT NOT NULL DEFAULT '{}',
    tracked_since DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_polled DATETIME,
    terminal_at DATETIME,
    PRIMARY KEY (source, source_id)
);

CREATE INDEX IF NOT EXISTS idx_tracked_items_active ON tracked_items(source, tracked_since) WHERE terminal_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_tracked_items_active_polled ON tracked_items(source, last_polled) WHERE terminal_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_tracked_items_source_polled ON tracked_items(source, last_polled DESC);

CREATE TABLE IF NOT EXISTS task_memory (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    run_id TEXT NOT NULL REFERENCES agent_runs(id),
    content TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'agent',
    created_at DATETIME NOT NULL,
    UNIQUE(run_id)
);

CREATE INDEX IF NOT EXISTS idx_task_memory_task_id ON task_memory(task_id, created_at);

CREATE TABLE IF NOT EXISTS prompt_triggers (
    id TEXT PRIMARY KEY,
    prompt_id TEXT NOT NULL REFERENCES prompts(id) ON DELETE CASCADE,
    trigger_type TEXT NOT NULL,
    event_type TEXT NOT NULL,
    scope_predicate_json TEXT,
    max_iterations INTEGER NOT NULL DEFAULT 3,
    cooldown_seconds INTEGER NOT NULL DEFAULT 60,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_prompt_triggers_event_type ON prompt_triggers(event_type) WHERE enabled = 1;
CREATE INDEX IF NOT EXISTS idx_prompt_triggers_prompt_id_created_at ON prompt_triggers(prompt_id, created_at);
`
