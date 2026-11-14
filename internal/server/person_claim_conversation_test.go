// A person's claim mints the conversation they work the task in, in the
// claim's own transaction, and the task carries its id.

package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// unendedConversations is the ids of the task's un-ended top-level
// conversations, oldest first. A task holds at most one.
func unendedConversations(t *testing.T, database *sql.DB, taskID string) []string {
	t.Helper()
	rows, err := database.Query(`
		SELECT id FROM conversations
		WHERE task_id = ? AND ended_at IS NULL AND parent_conversation_id IS NULL
		ORDER BY started_at, id`, taskID)
	if err != nil {
		t.Fatalf("read un-ended conversations: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan conversation id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("un-ended conversations: %v", err)
	}
	return ids
}

// countAfterWriteTx records how many un-ended conversations the watched task
// holds each time one of the route's write transactions commits, which is
// every moment another reader could see the task between the claim and the
// cleanup that follows it.
type countAfterWriteTx struct {
	db.TxRunner
	database **sql.DB
	taskID   *string
	seen     *[]int
}

func (w countAfterWriteTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	err := w.TxRunner.WithTx(ctx, orgID, userID, fn)
	if *w.taskID == "" {
		return err
	}
	var n int
	if qerr := (*w.database).QueryRow(`
		SELECT COUNT(*) FROM conversations
		WHERE task_id = ? AND ended_at IS NULL AND parent_conversation_id IS NULL`, *w.taskID).Scan(&n); qerr == nil {
		*w.seen = append(*w.seen, n)
	}
	return err
}

// assertUntouchedPersonConversation checks the conversation a claim minted is
// the row the mint writes and nothing since has stopped or ended it: the
// claim route's cleanup stops the agent's runs, and this one is unsettled too.
func assertUntouchedPersonConversation(t *testing.T, database *sql.DB, conversationID string) {
	t.Helper()
	var convType, origin, triggerType, creator, status string
	var endedAt, stopRequestedAt, parkReason, completedAt, blueprintRunID sql.NullString
	var messages int
	if err := database.QueryRow(`
		SELECT type, origin, trigger_type, COALESCE(creator_user_id, ''), COALESCE(status, ''),
		       ended_at, stop_requested_at, park_reason, completed_at, blueprint_run_id,
		       (SELECT COUNT(*) FROM messages m WHERE m.conversation_id = conversations.id)
		FROM conversations WHERE id = ?`, conversationID,
	).Scan(&convType, &origin, &triggerType, &creator, &status,
		&endedAt, &stopRequestedAt, &parkReason, &completedAt, &blueprintRunID, &messages); err != nil {
		t.Fatalf("read conversation %s: %v", conversationID, err)
	}
	if convType != domain.ConversationTypeInteractive || origin != domain.ConversationOriginInteractive {
		t.Errorf("type, origin = %q, %q; want an interactive conversation", convType, origin)
	}
	if triggerType != "manual" || creator != runmode.LocalDefaultUserID {
		t.Errorf("trigger_type, creator = %q, %q; want manual and the claimant", triggerType, creator)
	}
	if status != domain.StatusOpen || completedAt.Valid || blueprintRunID.Valid {
		t.Errorf("status = %q, completed_at set = %v, blueprint run set = %v; want parked with no verdict and no blueprint run",
			status, completedAt.Valid, blueprintRunID.Valid)
	}
	if endedAt.Valid {
		t.Error("the route ended the conversation it minted")
	}
	if stopRequestedAt.Valid || parkReason.Valid || messages != 0 {
		t.Errorf("the route's cleanup stopped the conversation it minted: stop requested = %v, park reason = %q, messages = %d",
			stopRequestedAt.Valid, parkReason.String, messages)
	}
}

// taskConversationID reads the task resource's conversation_id.
func taskConversationID(t *testing.T, s *Server, taskID string) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/tasks/"+taskID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET task = %d; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return body.ConversationID
}

// TestPersonClaim_MintsTheTasksConversation walks every way a person's claim
// lands: the claim route from the queue, from the agent, and as a re-claim of
// a task the caller already holds, and the Jira stock deck's claim. Each
// leaves the task with exactly one un-ended conversation, the person's, named
// by the task's conversation_id, and never with two. The route's cleanup,
// which stops what was running on the task, leaves that conversation alone.
func TestPersonClaim_MintsTheTasksConversation(t *testing.T) {
	// claimTask claims through the claim route and answers the response's
	// conversation_id.
	claimTask := func(t *testing.T, s *Server, taskID string) string {
		t.Helper()
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", withTaskVersion(t, s, taskID, map[string]any{}))
		if rec.Code != http.StatusOK {
			t.Fatalf("claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			ConversationID  string `json:"conversation_id"`
			ClaimedByUserID string `json:"claimed_by_user_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode claim response: %v", err)
		}
		if body.ClaimedByUserID != runmode.LocalDefaultUserID {
			t.Fatalf("claimed_by_user_id = %q, want the caller", body.ClaimedByUserID)
		}
		return body.ConversationID
	}

	cases := []struct {
		name string
		// run stages a task, claims it, and answers the task, the
		// conversation_id the claim's caller is told, and the conversation the
		// agent held before the claim ("" when the agent held none).
		run func(t *testing.T, s *Server, watch *string) (taskID, toldConversationID, agentConversationID string)
		// wantMinted is how many conversations the claim under test mints.
		wantMinted int
	}{
		{
			name: "from the queue",
			run: func(t *testing.T, s *Server, watch *string) (string, string, string) {
				taskID := seedTaskFixture(t, s.db, taskFixture{name: "queue-claim", status: "queued"})
				*watch = taskID
				return taskID, claimTask(t, s, taskID), ""
			},
			wantMinted: 1,
		},
		{
			name: "from the agent",
			run: func(t *testing.T, s *Server, watch *string) (string, string, string) {
				taskID, agentConversationID, _ := pendingApprovalFixture(t, s.db)
				// Mid-turn, so the cleanup's stop pass has the agent's
				// conversation to stop beside the one it must pass over.
				execSQL(t, s.db, `UPDATE conversations SET completed_at = NULL WHERE id = ?`, agentConversationID)
				*watch = taskID
				return taskID, claimTask(t, s, taskID), agentConversationID
			},
			wantMinted: 1,
		},
		{
			name: "an idempotent re-claim of a task that has its conversation",
			run: func(t *testing.T, s *Server, watch *string) (string, string, string) {
				taskID := seedTaskFixture(t, s.db, taskFixture{name: "re-claim", status: "queued"})
				first := claimTask(t, s, taskID)
				if first == "" {
					t.Fatal("the first claim told its caller no conversation_id")
				}
				*watch = taskID
				again := claimTask(t, s, taskID)
				if again != first {
					t.Errorf("the re-claim answered conversation_id %q, want the one the first claim minted, %q", again, first)
				}
				return taskID, again, ""
			},
			wantMinted: 0,
		},
		{
			name: "the Jira stock deck",
			run: func(t *testing.T, s *Server, watch *string) (string, string, string) {
				seedStockConfig(t, s)
				seedStockTicket(t, s, "SKY-1")
				fake := newJiraIssueFake(t, statusToDoID, "")
				s.jiraResolver = &recordingJiraResolver{userClient: fake.client()}
				if rec, body := postStock(t, s, "claim", "e_stock_SKY-1"); body.Applied != 1 {
					t.Fatalf("stock claim = %d %s, want it applied", rec.Code, rec.Body.String())
				}
				var taskID string
				if err := s.db.QueryRow(`SELECT id FROM tasks WHERE entity_id = 'e_stock_SKY-1'`).Scan(&taskID); err != nil {
					t.Fatalf("find the deck's task: %v", err)
				}
				return taskID, taskConversationID(t, s, taskID), ""
			},
			wantMinted: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			var watch string
			var seen []int
			var database *sql.DB
			s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner {
				return countAfterWriteTx{TxRunner: tx, database: &database, taskID: &watch, seen: &seen}
			})
			database = s.db
			// A spawner, so the cleanup's stop pass runs rather than
			// returning at its first line.
			s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

			var before int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE type = ?`, domain.ConversationTypeInteractive).Scan(&before); err != nil {
				t.Fatalf("count interactive conversations: %v", err)
			}
			taskID, told, agentConversationID := c.run(t, s, &watch)

			live := unendedConversations(t, s.db, taskID)
			if len(live) != 1 {
				t.Fatalf("un-ended conversations on the task = %v, want exactly one", live)
			}
			if told != live[0] {
				t.Errorf("the caller was told conversation_id %q, want the task's conversation %q", told, live[0])
			}
			if got := taskConversationID(t, s, taskID); got != live[0] {
				t.Errorf("the task read answers conversation_id %q, want %q", got, live[0])
			}
			assertUntouchedPersonConversation(t, s.db, live[0])

			var after int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE type = ?`, domain.ConversationTypeInteractive).Scan(&after); err != nil {
				t.Fatalf("count interactive conversations: %v", err)
			}
			minted := after - before
			if c.name == "an idempotent re-claim of a task that has its conversation" {
				// The first claim's mint is staging; the re-claim is the
				// claim under test.
				minted--
			}
			if minted != c.wantMinted {
				t.Errorf("the claim minted %d conversations, want %d", minted, c.wantMinted)
			}
			for i, n := range seen {
				if n > 1 {
					t.Errorf("after write transaction %d the task held %d un-ended conversations; it never holds two", i, n)
				}
			}

			if agentConversationID != "" {
				// The boundary landed with the claim, and the agent's process
				// was asked to stop after it.
				assertStamped(t, s.db, agentConversationID, domain.EndedTakenOver)
				var stopped bool
				if err := s.db.QueryRow(`
					SELECT stop_requested_at IS NOT NULL OR park_reason IS NOT NULL
					FROM conversations WHERE id = ?`, agentConversationID).Scan(&stopped); err != nil {
					t.Fatalf("read the agent's conversation: %v", err)
				}
				if !stopped {
					t.Error("the takeover did not stop the agent's conversation")
				}
			}
		})
	}
}

// TestPersonClaim_ATakeoverCancelsTheTasksQueuedFirings: the conversation a
// person's claim mints keeps the task busy, so a firing queued while the agent
// held it would wait behind the person and fire once they delegated. The
// takeover leaves a cancellation request on it instead, in the claim's
// transaction, and a claim that loses its race leaves none.
func TestPersonClaim_ATakeoverCancelsTheTasksQueuedFirings(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()

	// seedReadyFiring queues one firing on the fixture's task and answers a
	// reader of its cancellation request.
	seedReadyFiring := func(t *testing.T, s *Server, taskID string) func() (requested bool, by, reason, status string) {
		t.Helper()
		triggerID := fixtureUUID("person-claim-trigger")
		execSQL(t, s.db, `
			INSERT INTO event_handlers
				(id, org_id, team_id, kind, event_type, source, system_slug, name, default_priority, sort_order, enabled, creator_user_id)
			VALUES (?, ?, ?, 'rule', 'github:pr:ci_check_passed', 'system', 'person-claim-trigger', 'Trigger', 0.5, 0, 1, NULL)`,
			triggerID, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID)
		inserted, _, err := sqlitestore.New(s.db).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID,
			"e_pa", taskID, triggerID, "ev_pa", db.AgentClaimStamp{})
		if err != nil || !inserted {
			t.Fatalf("queue a firing: inserted=%v err=%v", inserted, err)
		}
		return func() (bool, string, string, string) {
			t.Helper()
			var requestedAt, by, reason sql.NullString
			var status string
			if err := s.db.QueryRow(`
				SELECT cancel_requested_at, cancel_requested_by, cancel_reason, status
				FROM pending_firings WHERE task_id = ?`, taskID).Scan(&requestedAt, &by, &reason, &status); err != nil {
				t.Fatalf("read the firing: %v", err)
			}
			return requestedAt.Valid, by.String, reason.String, status
		}
	}

	t.Run("a takeover that lands", func(t *testing.T) {
		s := newTestServer(t)
		taskID, _, _ := pendingApprovalFixture(t, s.db)
		firing := seedReadyFiring(t, s, taskID)

		rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", withTaskVersion(t, s, taskID, map[string]any{}))
		if rec.Code != http.StatusOK {
			t.Fatalf("claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		requested, by, reason, status := firing()
		if !requested || by != runmode.LocalDefaultUserID || reason != personClaimCancelReason {
			t.Errorf("firing after the takeover: requested=%v by=%q reason=%q; want a request by the claimant, reason %q",
				requested, by, reason, personClaimCancelReason)
		}
		// A request, not a disposition: the firing worker's next claim
		// settles it, whatever the busy task says.
		if status != "ready" {
			t.Errorf("firing status = %q, want it left ready for the next claim to settle", status)
		}
	})

	t.Run("a claim that loses its race", func(t *testing.T) {
		var race, after func()
		s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner {
			return raceAroundWriteTx{TxRunner: tx, before: &race, after: &after}
		})
		taskID, _, _ := pendingApprovalFixture(t, s.db)
		firing := seedReadyFiring(t, s, taskID)

		body := withTaskVersion(t, s, taskID, map[string]any{})
		race = func() { moveTaskOn(t, s.db, taskID) }
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("claim = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		if requested, _, _, _ := firing(); requested {
			t.Error("a claim that did not land left a cancellation request on the task's firing")
		}
		if live := unendedConversations(t, s.db, taskID); len(live) != 1 || live[0] != "r_pa" {
			t.Errorf("un-ended conversations after the refused claim = %v, want only the agent's", live)
		}
	})
}

// TestPersonClaim_AnUntouchedClaimHandedBackOwesNothing: a person claims a
// task and returns it to the queue without typing. The requeue ends the
// conversation their claim minted, as it ends any other, and because that
// conversation never ran it owes no memory: the task does not read
// memory_pending, the doorbell stays quiet, and the next claim mints a fresh
// conversation with nothing to wait behind.
func TestPersonClaim_AnUntouchedClaimHandedBackOwesNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	d := watchMemoryDoorbell(s)
	taskID := seedTaskFixture(t, s.db, taskFixture{name: "hand-back", status: "queued"})

	claim := func() string {
		t.Helper()
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", withTaskVersion(t, s, taskID, map[string]any{}))
		if rec.Code != http.StatusOK {
			t.Fatalf("claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		live := unendedConversations(t, s.db, taskID)
		if len(live) != 1 {
			t.Fatalf("un-ended conversations after the claim = %v, want one", live)
		}
		return live[0]
	}

	first := claim()
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", withTaskVersion(t, s, taskID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("requeue = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertStamped(t, s.db, first, domain.EndedRequeued)
	if live := unendedConversations(t, s.db, taskID); len(live) != 0 {
		t.Errorf("un-ended conversations after the requeue = %v, want none", live)
	}
	if got := d.taken(); len(got) != 0 {
		t.Errorf("the requeue rang the memory doorbell %d times for a conversation that never ran: %v", len(got), got)
	}

	var body struct {
		MemoryPending  bool   `json:"memory_pending"`
		ConversationID string `json:"conversation_id"`
	}
	read := doJSON(t, s, http.MethodGet, "/api/tasks/"+taskID, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("GET task = %d; body=%s", read.Code, read.Body.String())
	}
	if err := json.Unmarshal(read.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if body.MemoryPending {
		t.Error("memory_pending = true after an untouched claim was handed back; nothing ran, so nothing is owed")
	}
	if body.ConversationID != "" {
		t.Errorf("conversation_id = %q on a task nobody holds, want none", body.ConversationID)
	}

	if second := claim(); second == first {
		t.Error("the second claim answered the conversation the requeue ended, want a new one")
	} else {
		assertUntouchedPersonConversation(t, s.db, second)
	}
}
