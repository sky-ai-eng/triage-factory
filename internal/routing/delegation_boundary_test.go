package routing

import (
	"context"
	"database/sql"
	"testing"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// The delegation boundary from the router's side. Spec §4 puts the stamp on
// "human, or an event handler" alike, and the router has no stamp of its own:
// it inherits the one Spawner.Delegate writes. These run the REAL spawner
// rather than a stub delegator for exactly that reason — a stub would assert
// the fixture, not the seam.

// realSpawnerRouter wires a production *delegate.Spawner behind the router, so
// HandleEvent and DrainTask reach the boundary stamp the same way they reach
// the replay fence. Returns the spawner too, for the memory doorbell.
func realSpawnerRouter(t *testing.T, database *sql.DB) (*Router, *delegate.Spawner) {
	t.Helper()
	spawner := delegate.NewSpawner(database, sqlitestore.New(database), nil, websocket.NewHub(), "claude-sonnet-4-6")
	return fenceRouter(database, spawner), spawner
}

// unEndedConversations is every un-ended top-level conversation on the
// entity's tasks — the set the one-live-conversation rule says has at most one
// member once a delegation has settled.
func unEndedConversations(t *testing.T, database *sql.DB, entityID string) []string {
	t.Helper()
	rows, err := database.Query(`
		SELECT c.id FROM conversations c JOIN tasks t ON t.id = c.task_id
		WHERE t.entity_id = ? AND c.ended_at IS NULL AND c.parent_conversation_id IS NULL
		ORDER BY c.started_at, c.id
	`, entityID)
	if err != nil {
		t.Fatalf("read un-ended conversations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func conversationBoundary(t *testing.T, database *sql.DB, conversationID string) (endedAt sql.NullString, reason string) {
	t.Helper()
	var r sql.NullString
	if err := database.QueryRow(
		`SELECT ended_at, ended_reason FROM conversations WHERE id = ?`, conversationID,
	).Scan(&endedAt, &r); err != nil {
		t.Fatalf("read boundary of %s: %v", conversationID, err)
	}
	return endedAt, r.String
}

// TestHandleEvent_SecondFireEndsThePriorConversation is the flow the gap was
// reachable on, one task the whole way: a run concludes leaving a draft PR, so
// the task stays open and bot-claimed with the step `completed` and un-ended.
// The same event fires again, the task is found by dedup and bumped, the gate
// sees nothing live because `completed` is terminal, and run B mints. Minting B
// is right; leaving A un-ended is the gap — the task would carry two un-ended
// conversations, a person's follow-up would strand at the claim arm, and B
// would open without A's memory because A never ended to owe any.
func TestHandleEvent_SecondFireEndsThePriorConversation(t *testing.T) {
	database := newTestDB(t)
	entityID := setupFenceScenario(t, database)
	router, spawner := realSpawnerRouter(t, database)

	var rung []string
	spawner.SetOnMemoryOwed(func(_, conversationID string) { rung = append(rung, conversationID) })

	router.HandleEvent(context.Background(), recordFenceEvent(t, database, entityID))
	first := unEndedConversations(t, database, entityID)
	if len(first) != 1 {
		t.Fatalf("after the first fire: %d un-ended conversations, want 1", len(first))
	}
	concluded := first[0]

	// The run finishes; the task stays open, so nothing ends the row.
	fenceCompleteConversations(t, database, entityID)
	if endedAt, _ := conversationBoundary(t, database, concluded); endedAt.Valid {
		t.Fatalf("the concluded run ended itself; the fixture no longer models the gap")
	}

	router.HandleEvent(context.Background(), recordFenceEvent(t, database, entityID))

	endedAt, reason := conversationBoundary(t, database, concluded)
	if !endedAt.Valid || reason != string(domain.EndedDelegated) {
		t.Errorf("the concluded run's boundary = (ended_at valid=%v, reason=%q), want a `delegated` stamp",
			endedAt.Valid, reason)
	}
	if live := unEndedConversations(t, database, entityID); len(live) != 1 || live[0] == concluded {
		t.Errorf("un-ended conversations = %v, want exactly the new step 0 — the stamp runs before the mint", live)
	}
	if len(rung) != 1 || rung[0] != concluded {
		t.Errorf("memory doorbell rang for %v, want exactly [%s]", rung, concluded)
	}
}

// TestHandleEvent_ReplayedEvent_StampsNothingTwice: the replay's Delegate call
// reaches the stamp before the (event, trigger) fence refuses the insert. That
// is safe because the first fire already ended the task's terminal rows, so
// the second stamp finds nothing — and specifically must not rewrite the
// boundary the first one wrote.
func TestHandleEvent_ReplayedEvent_StampsNothingTwice(t *testing.T) {
	database := newTestDB(t)
	entityID := setupFenceScenario(t, database)
	router, spawner := realSpawnerRouter(t, database)

	// A first run, concluded and un-ended, so the second fire has something to
	// stamp and the replay after it has nothing.
	router.HandleEvent(context.Background(), recordFenceEvent(t, database, entityID))
	concluded := unEndedConversations(t, database, entityID)[0]
	fenceCompleteConversations(t, database, entityID)

	replayed := recordFenceEvent(t, database, entityID)
	router.HandleEvent(context.Background(), replayed)
	firstStamp, firstReason := conversationBoundary(t, database, concluded)
	if !firstStamp.Valid {
		t.Fatalf("the fire did not stamp the concluded run")
	}

	rang := 0
	spawner.SetOnMemoryOwed(func(string, string) { rang++ })
	// The at-least-once router queue redelivers the same event instance. Its
	// Delegate call reaches the stamp before the (event, trigger) fence refuses
	// the insert, so what keeps the replay harmless is that the stamp has
	// nothing left to find: the concluded row carries its boundary already, and
	// the row the first delivery minted is live, which this door never touches.
	router.HandleEvent(context.Background(), replayed)

	if rang != 0 {
		t.Errorf("memory doorbell rang %d times on the replay, want 0 — every terminal row was already stamped", rang)
	}
	againStamp, againReason := conversationBoundary(t, database, concluded)
	if againStamp != firstStamp || againReason != firstReason {
		t.Errorf("the replay moved the boundary to (%v, %q), want the first one (%v, %q)",
			againStamp, againReason, firstStamp, firstReason)
	}
}

// TestDrainTask_EndsThePriorConversation: the drain takes the same fireDelegate
// the immediate path does, so it inherits the stamp with no code of its own.
func TestDrainTask_EndsThePriorConversation(t *testing.T) {
	database := newTestDB(t)
	entityID := setupFenceScenario(t, database)
	router, spawner := realSpawnerRouter(t, database)

	var rung []string
	spawner.SetOnMemoryOwed(func(_, conversationID string) { rung = append(rung, conversationID) })

	// A concluded, un-ended run on the task — what the deferred firing queued
	// behind and what the drain now has to end before it opens step 0.
	router.HandleEvent(context.Background(), recordFenceEvent(t, database, entityID))
	live := unEndedConversations(t, database, entityID)
	if len(live) != 1 {
		t.Fatalf("after the immediate fire: %d un-ended conversations, want 1", len(live))
	}
	concluded := live[0]
	fenceCompleteConversations(t, database, entityID)

	// The drain fires only while the bot's claim still holds; the immediate
	// path's claim rode its own insert, so stamp it the way setupDrainScenario
	// does for a firing inserted directly.
	var taskID string
	if err := database.QueryRow(`SELECT id FROM tasks WHERE entity_id = ?`, entityID).Scan(&taskID); err != nil {
		t.Fatalf("resolve the task: %v", err)
	}
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := testTaskStore(database).SetClaimedByAgent(t.Context(), runmode.LocalDefaultOrgID, taskID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("stamp claim: %v", err)
	}

	firingEventID := recordFenceEvent(t, database, entityID).ID
	if _, _, err := sqlitestore.New(database).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID,
		runmode.LocalDefaultUserID, entityID, taskID, "t-fence", firingEventID, dbpkg.AgentClaimStamp{}); err != nil {
		t.Fatalf("enqueue the deferred firing: %v", err)
	}
	router.DrainTask(runmode.LocalDefaultOrgID, taskID)

	endedAt, reason := conversationBoundary(t, database, concluded)
	if !endedAt.Valid || reason != string(domain.EndedDelegated) {
		t.Errorf("the drained fire left the concluded run at (ended_at valid=%v, reason=%q), want a `delegated` stamp",
			endedAt.Valid, reason)
	}
	if got := unEndedConversations(t, database, entityID); len(got) != 1 || got[0] == concluded {
		t.Errorf("un-ended conversations = %v, want exactly the drain's new step 0", got)
	}
	if len(rung) != 1 || rung[0] != concluded {
		t.Errorf("memory doorbell rang for %v, want exactly [%s]", rung, concluded)
	}
}
