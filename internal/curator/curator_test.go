package curator

import (
	"context"
	"database/sql"
	"strconv"
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
	database, err := sql.Open("sqlite", db.TestDSNMemory)
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
	created, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: name})
	if err != nil {
		t.Fatalf("create project %q: %v", name, err)
	}
	id := created.ID
	return id
}

// turnState derives a turn's wire status + error the way the history
// handler does — from the user message's delivered flag and the
// conversation's claims — so tests can poll turn lifecycle without a
// curator_requests table. A deleted queued message reads as "cancelled"
// (the queued-cancel semantic).
func turnState(t *testing.T, stores db.Stores, projectID, requestID string) (status, errMsg string) {
	t.Helper()
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	conv, err := stores.Curator.GetLiveConversation(ctx, org, projectID, user)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if conv == nil {
		return "cancelled", ""
	}
	msgID, err := strconv.Atoi(requestID)
	if err != nil {
		t.Fatalf("parse request id %q: %v", requestID, err)
	}
	msgs, err := stores.Curator.ListConversationMessages(ctx, org, conv.ID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.ID == msgID {
			found = true
			if m.Delivered != nil && !*m.Delivered {
				return "queued", ""
			}
		}
	}
	if !found {
		return "cancelled", ""
	}
	claims, err := stores.Curator.ListClaims(ctx, org, conv.ID)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	if len(claims) == 0 {
		return "done", ""
	}
	last := claims[len(claims)-1]
	if last.ReleasedAt == nil {
		return "running", ""
	}
	switch last.Outcome {
	case "completed":
		return "done", last.Error
	case "cancelled":
		return "cancelled", last.Error
	default:
		return "failed", last.Error
	}
}

// TestCurator_SendMessage_RejectsAfterShutdown pins the contract downstream
// HTTP handlers rely on: once Shutdown has been called, SendMessage refuses
// the request outright and writes nothing. The check moved ahead of the row
// write when the entry contract became "insert and ring the doorbell" — a
// row inserted here would be durable and claimable by a live pod, which is
// not what the caller asked this process for.
func TestCurator_SendMessage_RejectsAfterShutdown(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "p")
	stores := sqlitestore.New(database)
	c := New(stores, nil, "")
	c.Shutdown()

	_, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi")
	if err == nil {
		t.Fatal("expected error after shutdown, got nil")
	}

	conv, err := stores.Curator.GetLiveConversation(t.Context(), runmode.LocalDefaultOrgID, projectID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if conv != nil {
		t.Error("a refused send must leave no conversation behind")
	}
}

// TestCurator_CancelProject_KillsActiveSession checks that a project already
// running has its goroutine torn down. The claim loop spawns the goroutine
// when it hands the turn over; cancelling the project immediately after
// should exit it cleanly without leaking.
func TestCurator_CancelProject_KillsActiveSession(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "active")

	hub := websocket.NewHub()
	stores := sqlitestore.New(database)
	c := New(stores, hub, "")
	t.Cleanup(c.Shutdown)
	t.Cleanup(startTestClaimLoop(t, stores, c))

	if _, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Wait for the loop to actually start the session, then tear it down.
	deadline := time.Now().Add(5 * time.Second)
	for !c.HasLiveSession(projectID) {
		if time.Now().After(deadline) {
			t.Fatal("the claim loop never started a session for the project")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.CancelProject(runmode.LocalDefaultOrgID, projectID, "project deleted")

	// The contract is "tears down deterministically", not "instantly".
	deadline = time.Now().Add(2 * time.Second)
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

// TestCurator_CrossProjectParallel pins that two projects' turns run in
// parallel: each gets its own session goroutine, and cancelling one leaves
// the other running.
func TestCurator_CrossProjectParallel(t *testing.T) {
	database := newTestDB(t)
	projectA := seedProject(t, database, "A")
	projectB := seedProject(t, database, "B")

	stores := sqlitestore.New(database)
	c := New(stores, nil, "")
	t.Cleanup(c.Shutdown)
	t.Cleanup(startTestClaimLoop(t, stores, c))

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

	// Sessions start when the claim loop hands each project's turn over, so
	// wait for both rather than assume SendMessage started them.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		bothPresent := c.sessions[projectA] != nil && c.sessions[projectB] != nil
		c.mu.Unlock()
		if bothPresent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("both projects should have an active session goroutine")
		}
		time.Sleep(10 * time.Millisecond)
	}

	c.CancelProject(runmode.LocalDefaultOrgID, projectA, "project deleted")

	deadline = time.Now().Add(2 * time.Second)
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

// startTestClaimLoop stands in for the production claim loop, which lives on
// the delegation dispatcher (internal/delegate) and cannot be imported here
// without pointing the dependency arrow the wrong way. Same shape: claim a
// needs-driving conversation, hand it to DriveClaimedTurn, repeat. Returns a
// stop func the test defers.
func startTestClaimLoop(t *testing.T, stores db.Stores, c *Curator) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var inFlight sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if ctx.Err() != nil {
				return
			}
			claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "test-claim-loop", 1, db.ClaimPlacement{})
			if err != nil || claimed == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Millisecond):
				}
				continue
			}
			// Off the loop, like the dispatcher's per-claim goroutine — two
			// projects' turns must be able to overlap.
			inFlight.Add(1)
			go func() {
				defer inFlight.Done()
				if !c.DriveClaimedTurn(claimed.OrgID, claimed.ProjectID, claimed.ID, claimed.ClaimID,
					claimed.ClaimMessageID, claimed.CreatorUserID) {
					_ = stores.ConversationQueue.RequeueConversation(context.Background(), claimed.OrgID, claimed.ID, "test loop handoff refused")
				}
			}()
		}
	}()
	return func() {
		cancel()
		<-done
		inFlight.Wait()
	}
}
