package db

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// newTestDB opens an in-memory SQLite database with foreign keys enabled
// and runs the schema migration. Returned DB is ready for use; test
// cleanup closes it.
//
// Connection pool is pinned to a single connection via SetMaxOpenConns(1).
// This is not about performance — it's correctness. SQLite's `:memory:`
// URI creates a fresh in-memory database per connection, so if Go's
// default sql pool opens a second connection (during parallel execution,
// after an idle close, or during connection recycling), queries on the
// new connection hit a completely empty database. A write on connection
// A followed by a read on connection B would intermittently return "no
// rows found" for a row we just inserted.
//
// Pinning to one connection forces every operation in the test to share
// the same in-memory db. The alternative — `file:<name>?mode=memory&cache=shared`
// — works too but needs a unique db name per test to avoid cross-test
// pollution, which is more machinery for the same guarantee.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", ":memory:?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := Migrate(database); err != nil {
		database.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// seedTaskAndRun inserts the minimum parent rows (tasks + agent_runs)
// needed to satisfy the task_memory foreign keys. Returns the task ID
// and run ID for the test to reference.
func seedTaskAndRun(t *testing.T, database *sql.DB) (taskID, runID string) {
	t.Helper()
	taskID = "task-" + t.Name()
	runID = "run-" + t.Name()

	_, err := database.Exec(`
		INSERT INTO tasks (id, source, source_id, source_url, title, created_at, fetched_at)
		VALUES (?, 'test', 'test-1', 'http://example.com', 'test task', ?, ?)
	`, taskID, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}

	_, err = database.Exec(`
		INSERT INTO agent_runs (id, task_id, status)
		VALUES (?, ?, 'running')
	`, runID, taskID)
	if err != nil {
		t.Fatalf("insert agent_run: %v", err)
	}
	return taskID, runID
}

func TestSaveAndGetTaskMemory_RoundTrip(t *testing.T) {
	database := newTestDB(t)
	taskID, runID := seedTaskAndRun(t, database)

	want := domain.TaskMemory{
		ID:        "mem-1",
		TaskID:    taskID,
		RunID:     runID,
		Content:   "Observed flaky test in user_flow_test.go. Bumped timeout to 30s. Retry if still failing, consider deadlock.",
		Source:    "agent",
		CreatedAt: time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC),
	}
	if err := SaveTaskMemory(database, want); err != nil {
		t.Fatalf("SaveTaskMemory: %v", err)
	}

	// Read back by run_id
	got, err := GetTaskMemoryByRun(database, runID)
	if err != nil {
		t.Fatalf("GetTaskMemoryByRun: %v", err)
	}
	if got == nil {
		t.Fatal("GetTaskMemoryByRun: got nil, want row")
	}
	if got.ID != want.ID || got.Content != want.Content || got.Source != want.Source {
		t.Errorf("GetTaskMemoryByRun: got %+v, want %+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt mismatch: got %v, want %v", got.CreatedAt, want.CreatedAt)
	}
}

func TestGetTaskMemoriesForTask_OrdersByCreatedAt(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := seedTaskAndRun(t, database)

	// Second run so we have two distinct foreign keys
	_, err := database.Exec(`INSERT INTO agent_runs (id, task_id, status) VALUES ('run-b', ?, 'running')`, taskID)
	if err != nil {
		t.Fatalf("insert second run: %v", err)
	}

	newer := domain.TaskMemory{
		ID: "mem-newer", TaskID: taskID, RunID: "run-b",
		Content: "newer", Source: "agent",
		CreatedAt: time.Date(2026, 4, 11, 13, 0, 0, 0, time.UTC),
	}
	older := domain.TaskMemory{
		ID: "mem-older", TaskID: taskID, RunID: "run-TestGetTaskMemoriesForTask_OrdersByCreatedAt",
		Content: "older", Source: "agent",
		CreatedAt: time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC),
	}
	// Insert out of order to prove ORDER BY is doing the work, not insertion order
	if err := SaveTaskMemory(database, newer); err != nil {
		t.Fatalf("SaveTaskMemory newer: %v", err)
	}
	if err := SaveTaskMemory(database, older); err != nil {
		t.Fatalf("SaveTaskMemory older: %v", err)
	}

	got, err := GetTaskMemoriesForTask(database, taskID)
	if err != nil {
		t.Fatalf("GetTaskMemoriesForTask: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].ID != "mem-older" || got[1].ID != "mem-newer" {
		t.Errorf("order wrong: got [%s, %s], want [mem-older, mem-newer]", got[0].ID, got[1].ID)
	}
}

func TestGetTaskMemoryByRun_NotFound(t *testing.T) {
	database := newTestDB(t)
	got, err := GetTaskMemoryByRun(database, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil for missing row", got)
	}
}

func TestSaveTaskMemory_RejectsDuplicateRunID(t *testing.T) {
	database := newTestDB(t)
	taskID, runID := seedTaskAndRun(t, database)

	// First insert succeeds.
	first := domain.TaskMemory{
		ID:        "mem-first",
		TaskID:    taskID,
		RunID:     runID,
		Content:   "first",
		Source:    "agent",
		CreatedAt: time.Now(),
	}
	if err := SaveTaskMemory(database, first); err != nil {
		t.Fatalf("first SaveTaskMemory: %v", err)
	}

	// Second insert for the same run_id must be rejected by the UNIQUE
	// constraint. Without this guarantee, races or double-calls would
	// leave GetTaskMemoryByRun returning an arbitrary row — and
	// downstream joins (SKY-148 circuit-breaker UI) would show
	// duplicated "prior attempt" entries for a single run.
	second := domain.TaskMemory{
		ID:        "mem-second",
		TaskID:    taskID,
		RunID:     runID, // same as first
		Content:   "second",
		Source:    "system",
		CreatedAt: time.Now(),
	}
	if err := SaveTaskMemory(database, second); err == nil {
		t.Fatal("expected duplicate run_id insert to be rejected, but it succeeded")
	}

	// And the first row is still the single source of truth.
	got, err := GetTaskMemoryByRun(database, runID)
	if err != nil {
		t.Fatalf("GetTaskMemoryByRun: %v", err)
	}
	if got == nil || got.ID != "mem-first" || got.Content != "first" {
		t.Errorf("expected first row preserved, got %+v", got)
	}
}

func TestSetAgentRunSessionAndMarkMemoryMissing(t *testing.T) {
	database := newTestDB(t)
	_, runID := seedTaskAndRun(t, database)

	// Initially no session_id, memory_missing = false
	run, err := GetAgentRun(database, runID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if run == nil {
		t.Fatal("GetAgentRun: nil run")
	}
	if run.SessionID != "" {
		t.Errorf("SessionID: got %q, want empty", run.SessionID)
	}
	if run.MemoryMissing {
		t.Error("MemoryMissing: got true, want false")
	}

	// Set session_id and verify
	if err := SetAgentRunSession(database, runID, "abc123-session"); err != nil {
		t.Fatalf("SetAgentRunSession: %v", err)
	}
	run, err = GetAgentRun(database, runID)
	if err != nil {
		t.Fatalf("GetAgentRun after session: %v", err)
	}
	if run.SessionID != "abc123-session" {
		t.Errorf("SessionID: got %q, want %q", run.SessionID, "abc123-session")
	}

	// Mark memory_missing and verify
	if err := MarkAgentRunMemoryMissing(database, runID); err != nil {
		t.Fatalf("MarkAgentRunMemoryMissing: %v", err)
	}
	run, err = GetAgentRun(database, runID)
	if err != nil {
		t.Fatalf("GetAgentRun after mark: %v", err)
	}
	if !run.MemoryMissing {
		t.Error("MemoryMissing: got false, want true after mark")
	}
}
