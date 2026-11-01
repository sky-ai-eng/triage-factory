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

// TestTaskClaim_Boundary pins which self-claims end the task's conversation:
// taking a task the agent held does, while picking up an unclaimed task or
// re-claiming your own takes it from nobody. Every arm leaves the caller holding
// the task in progress, with the agent's draft PR and staged review carried over.
func TestTaskClaim_Boundary(t *testing.T) {
	cases := []struct {
		name  string
		setup string // run against the task first; "" leaves it agent-claimed
		args  []any
		ended domain.EndedReason // "" means no stamp
	}{
		{"takeover from the agent", "", nil, domain.EndedTakenOver},
		{"pickup of an unclaimed task",
			`UPDATE tasks SET claimed_by_agent_id = NULL, status = 'queued' WHERE id = ?`, nil, ""},
		{"re-claim of your own task",
			`UPDATE tasks SET claimed_by_agent_id = NULL, claimed_by_user_id = ?, status = 'in_progress' WHERE id = ?`,
			[]any{runmode.LocalDefaultUserID}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t)
			taskID, conversationID, reviewID := pendingApprovalFixture(t, s.db)
			prID := seedCarriedDraftPR(t, s.db, conversationID)
			if c.setup != "" {
				execSQL(t, s.db, c.setup, append(c.args, taskID)...)
			}

			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if c.ended == "" {
				assertUnstamped(t, s.db, conversationID)
			} else {
				assertStamped(t, s.db, conversationID, c.ended)
			}
			assertPendingApprovalCarried(t, s.db, taskID, conversationID, reviewID, prID, "in_progress")

			var body struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode claim response: %v", err)
			}
			if body.Status != "in_progress" {
				t.Errorf("claim response status = %q, want in_progress", body.Status)
			}
			var claimedAgent, claimedUser sql.NullString
			if err := s.db.QueryRow(`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = ?`, taskID).
				Scan(&claimedAgent, &claimedUser); err != nil {
				t.Fatalf("scan claim: %v", err)
			}
			if claimedAgent.Valid || claimedUser.String != runmode.LocalDefaultUserID {
				t.Errorf("claims = (agent %v, user %v), want only the caller's", claimedAgent, claimedUser)
			}
		})
	}
}

// TestTaskDelegate_StampsBeforeStepZeroExists: delegating ends the prior
// conversation before the next is spawned, so the task never holds two live
// ones, and carries the last run's deliverables over. With no spawner wired,
// the route stops right after the boundary.
func TestTaskDelegate_StampsBeforeStepZeroExists(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, reviewID := pendingApprovalFixture(t, s.db)
	prID := seedCarriedDraftPR(t, s.db, conversationID)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertStamped(t, s.db, conversationID, domain.EndedDelegated)
	assertPendingApprovalCarried(t, s.db, taskID, conversationID, reviewID, prID, "in_progress")

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
		`INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index, completed_at)
		 SELECT ?, task_id, prompt_id, 'open', trigger_type, blueprint_run_id, blueprint_step_index, CURRENT_TIMESTAMP
		   FROM conversations WHERE id = 'r_pa'`, conversationID,
	); err != nil {
		t.Fatalf("seed a uuid-addressed sibling conversation: %v", err)
	}

	before := readConversation(t, s, conversationID)
	if _, ok := before["ended_at"]; ok {
		t.Errorf("ended_at present on an open conversation: %v", before["ended_at"])
	}
	if _, ok := before["ended_reason"]; ok {
		t.Errorf("ended_reason present on an open conversation: %v", before["ended_reason"])
	}

	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", nil); rec.Code != http.StatusOK {
		t.Fatalf("requeue = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	after := readConversation(t, s, conversationID)
	if after["ended_at"] == nil || after["ended_at"] == "" {
		t.Errorf("ended_at = %v, want the stamp", after["ended_at"])
	}
	if after["ended_reason"] != string(domain.EndedRequeued) {
		t.Errorf("ended_reason = %v, want %q", after["ended_reason"], domain.EndedRequeued)
	}
}
