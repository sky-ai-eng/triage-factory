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

// Open returns a connection to the SQLite database at ~/.todotinder/todotinder.db.
// Creates the directory if it doesn't exist.
func Open() (*sql.DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(home, ".todotinder")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(dir, "todotinder.db")
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
    snooze_until DATETIME,
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
    result_summary TEXT
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

CREATE TABLE IF NOT EXISTS prompt_bindings (
    prompt_id TEXT NOT NULL REFERENCES prompts(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    is_default BOOLEAN DEFAULT 0,
    PRIMARY KEY (prompt_id, event_type)
);

CREATE INDEX IF NOT EXISTS idx_prompt_bindings_event_type ON prompt_bindings(event_type);
CREATE INDEX IF NOT EXISTS idx_agent_runs_prompt_id ON agent_runs(prompt_id);

CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);
CREATE INDEX IF NOT EXISTS idx_events_task_id ON events(task_id);
CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at);
CREATE INDEX IF NOT EXISTS idx_tasks_event_type ON tasks(event_type);
`
