package curator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDispatch_AdmissionHoldsTurnQueued pins the gate's position in the turn
// lifecycle: a turn waiting on admission stays visibly 'queued', never
// reaches the agent, and a cancel during the wait lands the row cancelled —
// the wait must not burn a capacity slot or strand the row.
func TestDispatch_AdmissionHoldsTurnQueued(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "gated")
	c := New(sqlitestore.New(database), nil, "")
	t.Cleanup(c.Shutdown)

	var agentRan atomic.Bool
	c.runAgent = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		agentRan.Store(true)
		return nil, errors.New("must not run while the gate holds the turn")
	}
	c.SetAdmission(func(ctx context.Context) (func(), error) {
		<-ctx.Done() // the gate never opens; only a cancel ends the wait
		return nil, ctx.Err()
	})

	reqID, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// While the gate holds the turn, the row must sit in 'queued'.
	time.Sleep(100 * time.Millisecond)
	got, err := db.GetCuratorRequest(database, reqID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if got.Status != "queued" {
		t.Fatalf("status while gated = %q, want queued", got.Status)
	}
	if agentRan.Load() {
		t.Fatal("agent ran while the admission gate held the turn")
	}

	// A cancel during the wait must land the row cancelled. Retried in a
	// loop because CancelLocal is a no-op in the narrow window before the
	// session goroutine arms the in-flight cancel handle.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.CancelLocal(projectID)
		got, err = db.GetCuratorRequest(database, reqID)
		if err != nil {
			t.Fatalf("get request: %v", err)
		}
		if got.Status == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status after cancel = %q, want cancelled", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agentRan.Load() {
		t.Fatal("agent ran for a turn cancelled while waiting on admission")
	}
}

// TestDispatch_AdmittedTurnRunsAndReleases pins the other half of the
// contract: an admitted turn reaches the agent and hands its slot back when
// the turn ends, success or failure — a leaked slot would permanently
// shrink the host's capacity.
func TestDispatch_AdmittedTurnRunsAndReleases(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "admitted")
	c := New(sqlitestore.New(database), nil, "claude-sonnet-4-6")
	t.Cleanup(c.Shutdown)

	var agentRan, released atomic.Bool
	c.runAgent = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		agentRan.Store(true)
		return nil, errors.New("stub failure")
	}
	c.SetAdmission(func(ctx context.Context) (func(), error) {
		return func() { released.Store(true) }, nil
	})

	reqID, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var last string
	for {
		got, gerr := db.GetCuratorRequest(database, reqID)
		if gerr != nil {
			t.Fatalf("get request: %v", gerr)
		}
		last = got.ErrorMsg
		if got.Status == "failed" && released.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q released = %v, want failed + released (error_msg = %q)", got.Status, released.Load(), got.ErrorMsg)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !agentRan.Load() {
		t.Fatalf("admitted turn never reached the agent (error_msg = %q)", last)
	}
}
