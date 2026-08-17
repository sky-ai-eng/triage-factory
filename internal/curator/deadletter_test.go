package curator

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// corruptProject breaks the project row so BeginTurn fails permanently (its
// stored timestamp no longer scans into a time.Time) — the poisoned-input
// shape the dead-letter cap exists for. Any unparseable column on the row the
// turn has to read would do; this is the one left after pinned_repos moved to
// its own table.
func corruptProject(t *testing.T, database *sql.DB, projectID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE projects SET created_at = 'not-a-timestamp' WHERE id = ?`, projectID); err != nil {
		t.Fatalf("corrupt project: %v", err)
	}
}

func healProject(t *testing.T, database *sql.DB, projectID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE projects SET created_at = CURRENT_TIMESTAMP WHERE id = ?`, projectID); err != nil {
		t.Fatalf("heal project: %v", err)
	}
}

// waitForClaims polls until the conversation has exactly n claims, all
// released, and returns them.
func waitForClaims(t *testing.T, stores db.Stores, convID string, n int) []domain.Claim {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		claims, err := stores.Curator.ListClaims(context.Background(), runmode.LocalDefaultOrgID, convID)
		if err != nil {
			t.Fatalf("list claims: %v", err)
		}
		if len(claims) == n && (n == 0 || claims[n-1].ReleasedAt != nil) {
			return claims
		}
		if time.Now().After(deadline) {
			t.Fatalf("claims = %d (last released=%v), want %d released", len(claims), len(claims) > 0 && claims[len(claims)-1].ReleasedAt != nil, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDispatch_BeginTurnFailureCapDeadLetters pins the retry cap: a turn
// whose BeginTurn fails permanently is retried up to the cap and then
// dead-lettered — delivered out of the claimable scan, its final claim
// released failed with the give-up error — instead of looping forever and
// leaking a zero-message failed claim per scan.
func TestDispatch_BeginTurnFailureCapDeadLetters(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "poisoned")
	corruptProject(t, database, projectID)
	stores := sqlitestore.New(database)
	c := New(stores, nil, "claude-sonnet-4-6")
	c.SetTurnMaxAttempts(2)
	t.Cleanup(c.Shutdown)
	t.Cleanup(startTestClaimLoop(t, stores, c))

	var agentRan atomic.Bool
	c.runAgent = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		agentRan.Store(true)
		return nil, errors.New("must never run for a turn that can't begin")
	}

	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	reqID, err := c.SendMessage(t.Context(), projectID, org, user, "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	conv, err := stores.Curator.GetLiveConversation(t.Context(), org, projectID, user)
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v (%v)", conv, err)
	}

	// Every pickup comes from the one claim loop: a BeginTurn failure
	// releases the claim without delivering the message, which is all it
	// takes for the conversation to be claimable again — so the retries are
	// the loop re-claiming, not a second driver.
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, errMsg := turnState(t, stores, projectID, reqID)
		if status == "failed" {
			if !strings.Contains(errMsg, "curator turn failed 2 times, giving up") || !strings.Contains(errMsg, "begin turn") {
				t.Fatalf("dead-letter error = %q, want the give-up message carrying the last error", errMsg)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want failed (dead-lettered)", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	claims := waitForClaims(t, stores, conv.ID, 3)
	final := claims[2]
	if final.Outcome != "failed" || !strings.Contains(final.Error, "giving up") {
		t.Errorf("final claim = %+v, want the dead-letter release", final)
	}
	// Terminal for good: the message left the claimable set.
	if inFlight, err := stores.Curator.InFlightTurn(t.Context(), org, projectID, user); err != nil || inFlight != nil {
		t.Errorf("InFlightTurn after dead-letter = %+v (err=%v), want nil", inFlight, err)
	}
	if agentRan.Load() {
		t.Error("agent ran for a turn that never began")
	}
}

// TestDispatch_TransientBeginTurnFailureRetries pins the other half of the
// cap's contract: one failed pickup must not retire the turn — a re-feed
// after the fault clears runs the turn normally.
func TestDispatch_TransientBeginTurnFailureRetries(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "transient")
	corruptProject(t, database, projectID)
	stores := sqlitestore.New(database)
	c := New(stores, nil, "claude-sonnet-4-6")
	t.Cleanup(c.Shutdown)
	t.Cleanup(startTestClaimLoop(t, stores, c))

	var agentRan atomic.Bool
	c.runAgent = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		agentRan.Store(true)
		return nil, errors.New("stub failure")
	}

	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	reqID, err := c.SendMessage(t.Context(), projectID, org, user, "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	conv, err := stores.Curator.GetLiveConversation(t.Context(), org, projectID, user)
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v (%v)", conv, err)
	}
	// At least one failed pickup before the fault clears.
	deadline0 := time.Now().Add(5 * time.Second)
	for {
		claims, err := stores.Curator.ListClaims(context.Background(), org, conv.ID)
		if err != nil {
			t.Fatalf("list claims: %v", err)
		}
		if len(claims) >= 1 {
			break
		}
		if time.Now().After(deadline0) {
			t.Fatal("no failed pickup ever happened")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The fault clears; the loop's next claim must dispatch normally (reach
	// the agent) rather than dead-letter — no manual re-feed.
	healProject(t, database, projectID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, errMsg := turnState(t, stores, projectID, reqID)
		if status == "failed" && agentRan.Load() {
			if strings.Contains(errMsg, "giving up") {
				t.Fatalf("transient failure dead-lettered: %q", errMsg)
			}
			if !strings.Contains(errMsg, "stub failure") {
				t.Fatalf("retried turn error = %q, want the agent stub's", errMsg)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q agentRan = %v, want a retried turn reaching the agent", status, agentRan.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCancelProject_DrainsQueuedTurnAndStopsRetry pins the force-stop
// contract: a project cancelled while its turn keeps failing to begin must
// not be resurrectable — the queued row is drained, so the conversation
// stops matching the claim predicate and no further pickup can happen.
func TestCancelProject_DrainsQueuedTurnAndStopsRetry(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "force-stopped")
	corruptProject(t, database, projectID)
	stores := sqlitestore.New(database)
	c := New(stores, nil, "claude-sonnet-4-6")
	t.Cleanup(c.Shutdown)
	stopLoop := startTestClaimLoop(t, stores, c)
	t.Cleanup(stopLoop)

	var agentRan atomic.Bool
	c.runAgent = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		agentRan.Store(true)
		return nil, errors.New("must never run for a force-stopped project")
	}

	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	if _, err := c.SendMessage(t.Context(), projectID, org, user, "doomed"); err != nil {
		t.Fatalf("send: %v", err)
	}
	conv, err := stores.Curator.GetLiveConversation(t.Context(), org, projectID, user)
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v (%v)", conv, err)
	}

	// Wait for at least one failed pickup so the force-stop lands on a
	// turn the loop is actively re-claiming.
	deadline := time.Now().Add(5 * time.Second)
	for {
		claims, err := stores.Curator.ListClaims(context.Background(), org, conv.ID)
		if err != nil {
			t.Fatalf("list claims: %v", err)
		}
		if len(claims) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no pickup ever happened")
		}
		time.Sleep(2 * time.Millisecond)
	}
	c.CancelProject(org, projectID, "team archived")
	// Stop the loop before counting: the drain is what makes the turn
	// unclaimable, and a loop still running would only confirm that by
	// finding nothing — but it would also race the count itself.
	stopLoop()

	before, err := stores.Curator.ListClaims(context.Background(), org, conv.ID)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	msgs, err := stores.Curator.ListConversationMessages(context.Background(), org, conv.ID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	for _, m := range msgs {
		if m.Delivered != nil && !*m.Delivered {
			t.Errorf("undelivered row %d survived the drain — the turn is still claimable", m.ID)
		}
	}
	// And the predicate agrees: nothing is claimable on this conversation.
	if claimed, err := stores.ConversationQueue.ClaimNextConversation(context.Background(), "post-stop", 1, db.ClaimPlacement{}); err != nil {
		t.Fatalf("post-stop claim: %v", err)
	} else if claimed != nil {
		t.Errorf("claimed %s after the force-stop, want nothing", claimed.ID)
	}
	if after, err := stores.Curator.ListClaims(context.Background(), org, conv.ID); err != nil {
		t.Fatalf("list claims: %v", err)
	} else if len(after) != len(before) {
		t.Errorf("claims grew from %d to %d after the force-stop", len(before), len(after))
	}
	if inFlight, err := stores.Curator.InFlightTurn(context.Background(), org, projectID, user); err != nil || inFlight != nil {
		t.Errorf("InFlightTurn after force-stop = %+v (err=%v), want nil", inFlight, err)
	}
	if agentRan.Load() {
		t.Error("agent ran for a force-stopped project")
	}
}
