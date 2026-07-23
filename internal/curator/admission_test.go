package curator

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDispatch_AdmissionHoldsTurnQueued pins the gate's position in the turn
// lifecycle: a turn waiting on admission stays visibly 'queued' (an
// undelivered user message, no claim), never reaches the agent, and a cancel
// during the wait lands the turn cancelled — the wait must not burn a
// capacity slot or strand the turn.
func TestDispatch_AdmissionHoldsTurnQueued(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "gated")
	stores := sqlitestore.New(database)
	c := New(stores, nil, "")
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

	// While the gate holds the turn, it must sit queued.
	time.Sleep(100 * time.Millisecond)
	if status, _ := turnState(t, stores, projectID, reqID); status != "queued" {
		t.Fatalf("status while gated = %q, want queued", status)
	}
	if agentRan.Load() {
		t.Fatal("agent ran while the admission gate held the turn")
	}

	// A cancel during the wait must land the turn cancelled (its undelivered
	// message deleted — it never entered context). Retried in a loop because
	// CancelLocal is a no-op in the narrow window before the session
	// goroutine arms the in-flight cancel handle.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.CancelLocal(projectID)
		status, _ := turnState(t, stores, projectID, reqID)
		if status == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status after cancel = %q, want cancelled", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agentRan.Load() {
		t.Fatal("agent ran for a turn cancelled while waiting on admission")
	}
}

// TestDispatch_AdmissionFailureIsLegible pins the error attribution: a gate
// that fails for a reason OTHER than the turn's ctx (a miswire, an internal
// error) must land the turn 'failed' carrying the gate's own error — not
// 'cancelled', which would pin the failure on the user and mask it.
func TestDispatch_AdmissionFailureIsLegible(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "broken-gate")
	stores := sqlitestore.New(database)
	c := New(stores, nil, "")
	t.Cleanup(c.Shutdown)

	var agentRan atomic.Bool
	c.runAgent = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		agentRan.Store(true)
		return nil, errors.New("must not run when admission itself failed")
	}
	c.SetAdmission(func(context.Context) (func(), error) {
		return nil, errors.New("gate exploded")
	})

	reqID, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		status, errMsg := turnState(t, stores, projectID, reqID)
		if status == "failed" {
			if !strings.Contains(errMsg, "gate exploded") {
				t.Fatalf("claim error = %q, want the gate's own error surfaced", errMsg)
			}
			break
		}
		if status == "cancelled" {
			t.Fatalf("gate failure written as cancelled (error = %q), want failed", errMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want failed", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agentRan.Load() {
		t.Fatal("agent ran for a turn whose admission failed")
	}
}

// TestDispatch_AdmittedTurnRunsAndReleases pins the other half of the
// contract: an admitted turn reaches the agent and hands its slot back when
// the turn ends, success or failure — a leaked slot would permanently
// shrink the host's capacity.
func TestDispatch_AdmittedTurnRunsAndReleases(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProject(t, database, "admitted")
	stores := sqlitestore.New(database)
	c := New(stores, nil, "claude-sonnet-4-6")
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
		status, errMsg := turnState(t, stores, projectID, reqID)
		last = errMsg
		if status == "failed" && released.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q released = %v, want failed + released (error = %q)", status, released.Load(), errMsg)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !agentRan.Load() {
		t.Fatalf("admitted turn never reached the agent (error = %q)", last)
	}
}
