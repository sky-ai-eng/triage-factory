package delegate

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// markNative stamps a seeded conversation as the native runtime, the ratchet
// SendMessage routes on.
func markNative(t *testing.T, database *sql.DB, runID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE conversations SET runtime='native' WHERE id=?`, runID); err != nil {
		t.Fatalf("mark native: %v", err)
	}
}

// pendingRows returns the conversation's undelivered user rows, oldest-first.
func pendingRows(t *testing.T, s *Spawner, runID string) []domain.Message {
	t.Helper()
	rows, err := s.agentRuns.ListForAssemblySystem(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("list for assembly: %v", err)
	}
	var out []domain.Message
	for _, r := range rows {
		if r.Delivered != nil && !*r.Delivered {
			out = append(out, r)
		}
	}
	return out
}

// TestSendMessage_NativeRunningQueuesWithoutSteering pins the native routing:
// a message to a RUNNING native conversation is delivered by queueing it, so
// no controller round trip happens and nothing 409s because the work is on
// another pod. The loop drains it before its next call.
func TestSendMessage_NativeRunningQueuesWithoutSteering(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-native", "", "/tmp/wt-native")
	markEngaged(t, database, "r-native")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	markNative(t, database, "r-native")
	fc := &fakeController{}
	s.controller = fc

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-native", runmode.LocalDefaultUserID, "also check the tests"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if fc.steerCalls != 0 {
		t.Errorf("a native conversation has one input door — the messages table; controller steer calls = %d", fc.steerCalls)
	}
	pending := pendingRows(t, s, "r-native")
	if len(pending) != 1 || pending[0].Content != "also check the tests" {
		t.Fatalf("the message must be queued as an undelivered row: %+v", pending)
	}
	if pending[0].Role != "user" {
		t.Errorf("role = %q, want user", pending[0].Role)
	}

	// The engagement is untouched: a running conversation already has a
	// driver, so queueing is delivery and there is nothing to re-queue.
	if !hasActiveClaim(t, database, "r-native") {
		t.Error("the live engagement was released by a steer that should only have queued a row")
	}
	if st := storedStatus(t, database, "r-native"); st != "" {
		t.Errorf("stored status = %q, want none", st)
	}
}

// TestSendMessage_NativeParkedQueuesAndRequeues pins the other half: a parked
// native conversation gets the message queued AND the row flipped back to
// `queued` so an executor picks it up — with no pending-input side-table row,
// because the loop's own drain is what delivers it.
func TestSendMessage_NativeParkedQueuesAndRequeues(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-parked", "", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-parked'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	markNative(t, database, "r-parked")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-parked", runmode.LocalDefaultUserID, "pick it back up"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if st := storedStatus(t, database, "r-parked"); st != "" {
		t.Errorf("stored status = %q, want none — a parked conversation woken by input goes back to mid-flight, which is what makes it claimable again", st)
	}
	pending := pendingRows(t, s, "r-parked")
	if len(pending) != 1 || pending[0].Content != "pick it back up" {
		t.Fatalf("the message must be queued as an undelivered row: %+v", pending)
	}
}

// TestSendMessage_NativeTerminalNotSteerable pins that a genuinely terminal
// native conversation still refuses a message — queueing is delivery only
// while something can still drive the conversation. `completed` in any of its
// outcomes is deliberately absent: concluded work is followed up on, not
// refused (see TestSendMessage_NativeCompletedIsResumable).
func TestSendMessage_NativeTerminalNotSteerable(t *testing.T) {
	for _, tc := range []struct{ name, status, outcome string }{
		{"failed", "failed", ""},
		{"cancelled", "cancelled", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			seedRun(t, database, "r-term", "", "/tmp/wt-term")
			if _, err := database.Exec(`UPDATE conversations SET status=?, outcome=NULLIF(?, '') WHERE id='r-term'`, tc.status, tc.outcome); err != nil {
				t.Fatalf("set terminal: %v", err)
			}
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
			markNative(t, database, "r-term")

			err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-term", runmode.LocalDefaultUserID, "hello?")
			if !errors.Is(err, ErrRunNotSteerable) {
				t.Fatalf("err = %v, want ErrRunNotSteerable", err)
			}
			if got := pendingRows(t, s, "r-term"); len(got) != 0 {
				t.Errorf("a refused message must not leave a row behind: %+v", got)
			}
		})
	}
}

// TestSendMessage_NativeCompletedIsResumable: every completed outcome takes a
// follow-up, `finish` included. The agent stopping mid-work and the agent
// finishing are both states a person picks back up, and both left a workspace
// behind to pick it up in.
func TestSendMessage_NativeCompletedIsResumable(t *testing.T) {
	for _, outcome := range []string{"abort", "finish", "continue"} {
		t.Run(outcome, func(t *testing.T) {
			database := newDelegateTestDB(t)
			seedRun(t, database, "r-done", "", t.TempDir())
			if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome=? WHERE id='r-done'`, outcome); err != nil {
				t.Fatalf("complete: %v", err)
			}
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
			markNative(t, database, "r-done")

			if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-done", runmode.LocalDefaultUserID, "try again"); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			if st := storedStatus(t, database, "r-done"); st != "" {
				t.Errorf("stored status = %q, want none (the un-terminal write clears the outcome)", st)
			}
			if got := pendingRows(t, s, "r-done"); len(got) != 1 {
				t.Errorf("follow-up rows = %d, want 1 queued for the next claim", len(got))
			}
		})
	}
}
