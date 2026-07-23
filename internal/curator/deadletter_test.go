package curator

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
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

// corruptProject breaks the project row so BeginTurn fails permanently (the
// pinned_repos JSON no longer parses) — the poisoned-input shape the
// dead-letter cap exists for.
func corruptProject(t *testing.T, database *sql.DB, projectID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE projects SET pinned_repos = 'not-json' WHERE id = ?`, projectID); err != nil {
		t.Fatalf("corrupt project: %v", err)
	}
}

func healProject(t *testing.T, database *sql.DB, projectID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE projects SET pinned_repos = '[]' WHERE id = ?`, projectID); err != nil {
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
	// The wire request id IS the message id.
	msgID, err := strconv.ParseInt(reqID, 10, 64)
	if err != nil {
		t.Fatalf("parse request id: %v", err)
	}

	// Attempt 1 (the in-process send) fails at BeginTurn and leaves the
	// message queued for a later scan.
	claims := waitForClaims(t, stores, conv.ID, 1)
	if claims[0].Outcome != "failed" || !strings.Contains(claims[0].Error, "begin turn") {
		t.Fatalf("attempt 1 claim = %+v, want failed at begin turn", claims[0])
	}
	if status, _ := turnState(t, stores, projectID, reqID); status != "queued" {
		t.Fatalf("status after attempt 1 = %q, want queued (still claimable)", status)
	}

	// Attempt 2 (a re-feed, as the claim loop would) fails the same way.
	if !c.EnqueueClaimed(org, projectID, conv.ID, msgID, user) {
		t.Fatal("re-feed 2 rejected")
	}
	waitForClaims(t, stores, conv.ID, 2)

	// Feed 3 hits the cap: dead-letter, no third pickup.
	if !c.EnqueueClaimed(org, projectID, conv.ID, msgID, user) {
		t.Fatal("re-feed 3 rejected")
	}
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
	claims = waitForClaims(t, stores, conv.ID, 3)
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
	// The wire request id IS the message id.
	msgID, err := strconv.ParseInt(reqID, 10, 64)
	if err != nil {
		t.Fatalf("parse request id: %v", err)
	}
	waitForClaims(t, stores, conv.ID, 1)

	// The fault clears; the re-feed must dispatch normally (reach the agent)
	// rather than dead-letter.
	healProject(t, database, projectID)
	if !c.EnqueueClaimed(org, projectID, conv.ID, msgID, user) {
		t.Fatal("re-feed rejected")
	}
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

// TestFailQueued_RacedCancelSettlesCancelled pins the raced-cancel gate: a
// turn cancelled (its queued row deleted) between an admission error and
// failQueued's pickup must settle cancelled — the canceller's terminal state
// stands, and no 'failed' release lands on top of it.
func TestFailQueued_RacedCancelSettlesCancelled(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "raced-cancel")
	stores := sqlitestore.New(database)
	c := New(stores, nil, "")
	t.Cleanup(c.Shutdown)

	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	var convID string
	var msgID int64
	if err := stores.Tx.SyntheticClaimsWithTx(t.Context(), org, user, func(ts db.TxStores) error {
		conv, err := ts.Curator.GetOrCreateConversation(t.Context(), org, projectID, user)
		if err != nil {
			return err
		}
		convID = conv.ID
		msgID, err = ts.Curator.EnqueueUserMessage(t.Context(), org, conv.ID, user, "doomed")
		return err
	}); err != nil {
		t.Fatalf("seed turn: %v", err)
	}

	// The raced cancel: the queued row is withdrawn before failQueued runs.
	if err := stores.Tx.SyntheticClaimsWithTx(t.Context(), org, user, func(ts db.TxStores) error {
		deleted, err := ts.Curator.DeleteQueuedTurn(t.Context(), org, convID, msgID)
		if err == nil && !deleted {
			return errors.New("queued row not deleted")
		}
		return err
	}); err != nil {
		t.Fatalf("cancel queued turn: %v", err)
	}

	s := &projectSession{curator: c, projectID: projectID, orgID: org, ctx: context.Background()}
	s.failQueued(queueItem{
		conversationID: convID,
		messageID:      msgID,
		requestID:      turnRequestID(msgID),
		orgID:          org,
		creatorUserID:  user,
	}, "turn admission: gate exploded")

	claims, err := stores.Curator.ListClaims(t.Context(), org, convID)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want exactly the withdrawn pickup's release", len(claims))
	}
	got := claims[0]
	if got.ReleasedAt == nil || got.Outcome != "cancelled" || got.Error != "queued turn withdrawn before pickup" {
		t.Errorf("claim = (released=%v, outcome=%q, error=%q), want the quiet cancelled release — failed must not overwrite a raced cancel", got.ReleasedAt != nil, got.Outcome, got.Error)
	}
}
