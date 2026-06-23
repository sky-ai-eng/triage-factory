package curator

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

func seedProject(t *testing.T, database *sql.DB, name string) string {
	t.Helper()
	id, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: name})
	if err != nil {
		t.Fatalf("create project %q: %v", name, err)
	}
	return id
}

// TestCurator_SendMessage_RejectsAfterShutdown pins the contract that
// downstream HTTP handlers can rely on: once Shutdown has been called,
// SendMessage refuses the request AND, if a row was already inserted
// before the closed check (which is the realistic interleaving — DB
// write happens before getOrStartSession), that row is flipped to
// cancelled rather than left dangling in `queued`.
func TestCurator_SendMessage_RejectsAfterShutdown(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "p")
	c := New(sqlitestore.New(database), nil, "")
	c.Shutdown()

	_, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi")
	if err == nil {
		t.Fatal("expected error after shutdown, got nil")
	}

	// The row that SendMessage persisted before the closed check
	// must not be left in `queued` — otherwise it would dangle
	// forever (no goroutine ever picks it up).
	requests, err := db.ListCuratorRequestsByProject(database, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("expected 1 request row, got %d", len(requests))
	}
	if requests[0].Status != "cancelled" {
		t.Errorf("post-shutdown row status = %q, want cancelled", requests[0].Status)
	}
}

// TestCurator_CancelProject_FlipsQueuedRows is the project-delete
// hook: queued rows for the deleted project must land terminal so
// they aren't left dangling. Verified without spawning agentproc by
// pre-seeding queued rows directly and checking status post-cancel.
func TestCurator_CancelProject_FlipsQueuedRows(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "p")

	id1, _ := db.CreateCuratorRequest(database, projectID, "first")
	id2, _ := db.CreateCuratorRequest(database, projectID, "second")

	c := New(sqlitestore.New(database), nil, "")
	t.Cleanup(c.Shutdown)
	c.CancelProject(runmode.LocalDefaultOrgID, projectID, "project deleted")

	for _, id := range []string{id1, id2} {
		got, _ := db.GetCuratorRequest(database, id)
		if got.Status != "cancelled" {
			t.Errorf("request %s status = %q, want cancelled", id, got.Status)
		}
	}
}

// TestCurator_CancelProject_KillsActiveSession checks that a project
// already running has its goroutine torn down. We spawn the goroutine
// by sending a message (which puts it on the queue), then immediately
// cancel the project — the goroutine should exit cleanly without
// leaking. Verified by Shutdown returning quickly and no goroutines
// blocking the test harness.
func TestCurator_CancelProject_KillsActiveSession(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "active")

	hub := websocket.NewHub()
	c := New(sqlitestore.New(database), hub, "")
	t.Cleanup(c.Shutdown)

	// SendMessage spawns the per-project goroutine if absent. The
	// goroutine will try to invoke claude — that'll fail fast on
	// CI / dev boxes without claude on PATH, but the failure is
	// masked by the immediate CancelProject anyway.
	if _, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	c.CancelProject(runmode.LocalDefaultOrgID, projectID, "project deleted")

	// Wait a moment for the goroutine to observe ctx.Done and exit.
	// The contract is "tears down deterministically", not "instantly,"
	// but it should be sub-second.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		_, stillThere := c.sessions[projectID]
		c.mu.Unlock()
		if !stillThere {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("session for project %s still in map after CancelProject", projectID)
}

// TestCurator_CrossProjectParallel checks the multiplexing contract:
// SendMessage to two different projects spawns two goroutines, and
// canceling one doesn't stop the other. We use the post-cancel
// session map to assert state without depending on agentproc actually
// running.
func TestCurator_CrossProjectParallel(t *testing.T) {
	database := newTestDB(t)
	projectA := seedProject(t, database, "A")
	projectB := seedProject(t, database, "B")

	c := New(sqlitestore.New(database), nil, "")
	t.Cleanup(c.Shutdown)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = c.SendMessage(t.Context(), projectA, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi from A")
	}()
	go func() {
		defer wg.Done()
		_, _ = c.SendMessage(t.Context(), projectB, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi from B")
	}()
	wg.Wait()

	c.mu.Lock()
	bothPresent := c.sessions[projectA] != nil && c.sessions[projectB] != nil
	c.mu.Unlock()
	if !bothPresent {
		t.Error("both projects should have an active session goroutine")
	}

	c.CancelProject(runmode.LocalDefaultOrgID, projectA, "project deleted")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		_, aGone := c.sessions[projectA]
		_, bStill := c.sessions[projectB]
		c.mu.Unlock()
		if !aGone && bStill {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("project A should be evicted, project B should remain")
}

func TestKnowledgeDir_RequiresProjectID(t *testing.T) {
	if _, err := KnowledgeDir(runmode.LocalDefaultOrgID, ""); err == nil {
		t.Error("empty project id should error")
	}
}

func TestRenderEnvelope_IncludesProjectName(t *testing.T) {
	got := renderEnvelope(envelopeInputs{ProjectName: "My Project", BinaryPath: "/usr/bin/triagefactory"})
	if got == "" {
		t.Fatal("empty system prompt")
	}
	if !contains(got, "My Project") {
		t.Errorf("system prompt missing project name: %q", got)
	}
	// Sanity: every templated placeholder must be substituted.
	for _, placeholder := range []string{
		"{{PROJECT_NAME}}",
		"{{PROJECT_DESCRIPTION}}",
		"{{PINNED_REPOS_BLOCK}}",
		"{{TRACKERS_BLOCK}}",
		"{{BINARY_PATH}}",
		"{{TOOLS_REFERENCE}}",
	} {
		if contains(got, placeholder) {
			t.Errorf("placeholder %s left unsubstituted in envelope", placeholder)
		}
	}
}

func TestRenderEnvelope_FallbackForEmptyName(t *testing.T) {
	got := renderEnvelope(envelopeInputs{BinaryPath: "/usr/bin/triagefactory"})
	if got == "" {
		t.Fatal("empty system prompt for empty name")
	}
	// The fallback shouldn't look like a templating artifact ("project ''").
	if contains(got, `""`) {
		t.Errorf("fallback leaks empty quotes: %q", got)
	}
}

func contains(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && stringIndex(s, substr) >= 0)
}

func stringIndex(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
