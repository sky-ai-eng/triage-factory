// The boundary stamps the task verbs write: which gestures end a task's
// conversations, which deliberately do not, and what the conversation read
// says about a row that ended.

package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// taskBoundary reads a conversation's stamp straight off the row.
func taskBoundary(t *testing.T, database *sql.DB, conversationID string) (endedAt sql.NullString, reason string) {
	t.Helper()
	var r sql.NullString
	if err := database.QueryRow(
		`SELECT ended_at, ended_reason FROM conversations WHERE id = ?`, conversationID,
	).Scan(&endedAt, &r); err != nil {
		t.Fatalf("read boundary of %s: %v", conversationID, err)
	}
	return endedAt, r.String
}

func assertStamped(t *testing.T, database *sql.DB, conversationID string, want domain.EndedReason) {
	t.Helper()
	endedAt, reason := taskBoundary(t, database, conversationID)
	if !endedAt.Valid || reason != string(want) {
		t.Errorf("%s boundary = (ended_at valid=%v, reason=%q), want a stamp with reason %q",
			conversationID, endedAt.Valid, reason, want)
	}
}

func assertUnstamped(t *testing.T, database *sql.DB, conversationID string) {
	t.Helper()
	if endedAt, reason := taskBoundary(t, database, conversationID); endedAt.Valid || reason != "" {
		t.Errorf("%s boundary = (ended_at valid=%v, reason=%q), want none", conversationID, endedAt.Valid, reason)
	}
}

// TestRequeueRoutes_StampTheRequeuedBoundary: both doors onto the requeue —
// the deliberate /requeue and the gesture-reversing /undo — end the task's
// conversations, because both hand the task to whoever claims it next and that
// claimant starts fresh.
//
// The conversation is `completed`, which is the case that makes the point: it
// concluded on its own and would look perfectly resumable from status alone,
// and the boundary is the task moving on, not how any transcript finished.
func TestRequeueRoutes_StampTheRequeuedBoundary(t *testing.T) {
	for _, route := range []string{"requeue", "undo"} {
		t.Run(route, func(t *testing.T) {
			s := newTestServer(t)
			taskID, conversationID, _ := pendingApprovalFixture(t, s.db)
			if route == "undo" {
				seedCallerGesture(t, s.db, taskID, "claim")
			}

			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/"+route, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			assertStamped(t, s.db, conversationID, domain.EndedRequeued)
		})
	}
}

// TestTaskClaim_TakeoverStampsTheBoundary: a person claiming a task the agent
// held ends the agent's conversation. The fixture's task is agent-claimed,
// which is what makes this arm a takeover rather than a pickup.
func TestTaskClaim_TakeoverStampsTheBoundary(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, _ := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertStamped(t, s.db, conversationID, domain.EndedTakenOver)
}

// TestTaskClaim_PickingUpAnUnclaimedTaskStampsNothing is the other half of the
// same route, and the reason selfClaim reports which arm it landed on: after
// the claim the task's columns read identically, but nothing was taken from
// anybody — there is no conversation the task stopped being about, and a stamp
// here would record a handoff that never happened.
func TestTaskClaim_PickingUpAnUnclaimedTaskStampsNothing(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, _ := pendingApprovalFixture(t, s.db)
	if _, err := s.db.Exec(`UPDATE tasks SET claimed_by_agent_id = NULL WHERE id = ?`, taskID); err != nil {
		t.Fatalf("unclaim the task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertUnstamped(t, s.db, conversationID)
}

// TestTaskClaim_IdempotentReClaimStampsNothing: the caller already owns it and
// nothing moved, so the same rule applies as for a pickup.
func TestTaskClaim_IdempotentReClaimStampsNothing(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, _ := pendingApprovalFixture(t, s.db)
	if _, err := s.db.Exec(
		`UPDATE tasks SET claimed_by_agent_id = NULL, claimed_by_user_id = ? WHERE id = ?`,
		runmode.LocalDefaultUserID, taskID,
	); err != nil {
		t.Fatalf("pre-claim the task for the caller: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertUnstamped(t, s.db, conversationID)
}

// TestTaskDelegate_StampsBeforeStepZeroExists pins the ordering the delegate
// arm is written for: the prior conversation is ended before a new delegation
// is spawned, so the task never holds two live conversations at once.
//
// The test server wires no spawner, so the route stops exactly where the
// invariant is checkable — the boundary written, and nothing new minted.
func TestTaskDelegate_StampsBeforeStepZeroExists(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, _ := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertStamped(t, s.db, conversationID, domain.EndedDelegated)

	var live int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE task_id = ? AND ended_at IS NULL`, taskID,
	).Scan(&live); err != nil {
		t.Fatalf("count live conversations: %v", err)
	}
	if live != 0 {
		t.Errorf("live conversations on the task = %d, want 0 at the moment the new delegation is spawned", live)
	}
}

// TestTaskPatch_EndingATaskStampsNothing is the decided non-boundary. A task
// reaching done or dismissed keeps stopping whatever ran, exactly as before,
// and stamps nothing: a blueprint's last step stays resumable on its done task
// for follow-ups, and a manual conversation on a task something closed under
// it is parked and picked back up rather than sealed.
func TestTaskPatch_EndingATaskStampsNothing(t *testing.T) {
	for _, status := range []string{"done", "dismissed"} {
		t.Run(status, func(t *testing.T) {
			s := newTestServer(t)
			taskID, conversationID, _ := pendingApprovalFixture(t, s.db)

			rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
				map[string]any{"status": status, "hesitation_ms": 0})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			assertUnstamped(t, s.db, conversationID)
		})
	}
}

// TestConversationRead_CarriesTheBoundary: both keys are absent while the
// conversation is open and present together once it ends, so a client reads
// "ended" off presence rather than off a sentinel.
func TestConversationRead_CarriesTheBoundary(t *testing.T) {
	s := newTestServer(t)
	taskID, _, _ := pendingApprovalFixture(t, s.db)
	// The route takes a uuid, so read a sibling conversation on the same task
	// rather than the fixture's hand-named row. The requeue below stamps every
	// non-ended top-level conversation the task has, this one included.
	conversationID := uuid.NewString()
	if _, err := s.db.Exec(
		`INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index)
		 SELECT ?, task_id, prompt_id, 'completed', trigger_type, blueprint_run_id, blueprint_step_index
		   FROM conversations WHERE id = 'r_pa'`, conversationID,
	); err != nil {
		t.Fatalf("seed a uuid-addressed sibling conversation: %v", err)
	}

	read := func() map[string]any {
		t.Helper()
		rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	before := read()
	if _, ok := before["ended_at"]; ok {
		t.Errorf("ended_at present on an open conversation: %v", before["ended_at"])
	}
	if _, ok := before["ended_reason"]; ok {
		t.Errorf("ended_reason present on an open conversation: %v", before["ended_reason"])
	}

	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", nil); rec.Code != http.StatusOK {
		t.Fatalf("requeue = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	after := read()
	if after["ended_at"] == nil || after["ended_at"] == "" {
		t.Errorf("ended_at = %v, want the stamp", after["ended_at"])
	}
	if after["ended_reason"] != string(domain.EndedRequeued) {
		t.Errorf("ended_reason = %v, want %q", after["ended_reason"], domain.EndedRequeued)
	}
}
