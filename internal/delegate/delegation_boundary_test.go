// The boundary a delegation stamps on the task it is opening on: every door
// into Spawner.Delegate ends the conversations a prior run left behind, so
// step 0 never opens beside an un-ended one.

package delegate

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// concludeRun retires a task's engagement the way a run that finished but left
// the task open does: the step reads `completed`, the blueprint_run behind it
// is no longer running, and neither carries a boundary. That is the shape the
// next event lands on — a draft PR is up, so the task stayed open and the row
// nobody ended is still the newest one.
func concludeRun(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var conversationID string
	if err := database.QueryRow(
		`SELECT id FROM conversations WHERE task_id = ? ORDER BY started_at DESC, id LIMIT 1`, taskID,
	).Scan(&conversationID); err != nil {
		t.Fatalf("read the task's newest conversation: %v", err)
	}
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed' WHERE id = ?`, conversationID); err != nil {
		t.Fatalf("conclude the conversation: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ?`, taskID,
	); err != nil {
		t.Fatalf("conclude the blueprint run: %v", err)
	}
	return conversationID
}

// TestDelegate_EndsTheTasksConcludedConversations: the stamp lives in Delegate,
// so a second delegation onto a task whose prior run concluded without ending
// ends it — and the row Delegate is minting is not in the set.
//
// The consequences of the gap are what make this load-bearing: two un-ended
// rows on one task strand a person's follow-up at the claim arm, which reaches
// only the newest, and a conversation that never ended owes no memory, so the
// new run opens without its predecessor's account of the work.
func TestDelegate_EndsTheTasksConcludedConversations(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "delegate-boundary")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	var rung []string
	s.SetOnMemoryOwed(func(_, conversationID string) { rung = append(rung, conversationID) })

	opts := DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	}
	if _, err := s.Delegate(task, opts); err != nil {
		t.Fatalf("first Delegate: %v", err)
	}
	concluded := concludeRun(t, database, task.ID)
	assertNotEnded(t, database, concluded)

	if _, err := s.Delegate(task, opts); err != nil {
		t.Fatalf("second Delegate: %v", err)
	}

	assertEnded(t, database, concluded, domain.EndedDelegated)
	if len(rung) != 1 || rung[0] != concluded {
		t.Errorf("memory doorbell rang for %v, want exactly [%s] — one ring per row the stamp committed", rung, concluded)
	}

	// The row this delegation just minted is the task's live one and must have
	// stayed out of the set: the stamp runs before the insert.
	var live []string
	rows, err := database.Query(`SELECT id FROM conversations WHERE task_id = ? AND ended_at IS NULL`, task.ID)
	if err != nil {
		t.Fatalf("read un-ended conversations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		live = append(live, id)
	}
	if len(live) != 1 || live[0] == concluded {
		t.Errorf("un-ended conversations on the task = %v, want exactly the new step 0", live)
	}
}

// TestDelegate_TaskBusyRefusal_EndsNothing: a delegation the one-active-run
// index refuses must leave the live engagement it lost to exactly as it found
// it. The stamp reaches terminal rows only, which is what makes running it
// before the insert safe rather than something the refusal has to unwind.
func TestDelegate_TaskBusyRefusal_EndsNothing(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "delegate-boundary-busy")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	rang := 0
	s.SetOnMemoryOwed(func(string, string) { rang++ })

	opts := DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	}
	if _, err := s.Delegate(task, opts); err != nil {
		t.Fatalf("first Delegate: %v", err)
	}
	var liveConversation string
	if err := database.QueryRow(`SELECT id FROM conversations WHERE task_id = ?`, task.ID).Scan(&liveConversation); err != nil {
		t.Fatalf("read the live conversation: %v", err)
	}

	if _, err := s.Delegate(task, opts); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("second Delegate onto a live task = %v, want ErrTaskBusy", err)
	}
	assertNotEnded(t, database, liveConversation)
	if rang != 0 {
		t.Errorf("memory doorbell rang %d times on a refused delegation, want 0", rang)
	}
}

// TestDelegate_AfterTheRouteAlreadyStamped_FindsNothing: the delegate route
// ends the task's conversations itself, because it also has to stop the live
// ones. Delegate's stamp is a no-op behind it — and specifically must not
// rewrite a boundary that already happened.
func TestDelegate_AfterTheRouteAlreadyStamped_FindsNothing(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "delegate-boundary-route")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	opts := DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	}
	if _, err := s.Delegate(task, opts); err != nil {
		t.Fatalf("first Delegate: %v", err)
	}
	concluded := concludeRun(t, database, task.ID)

	// What the route does before it calls Delegate again.
	ctx := context.Background()
	stores := sqlitestore.New(database)
	if _, err := stores.Conversations.EndConversationsForTask(
		ctx, runmode.LocalDefaultOrgID, task.ID, domain.EndedDelegated,
	); err != nil {
		t.Fatalf("the route's own stamp: %v", err)
	}
	beforeEndedAt, _ := boundaryOf(t, database, concluded)

	rang := 0
	s.SetOnMemoryOwed(func(string, string) { rang++ })
	if _, err := s.Delegate(task, opts); err != nil {
		t.Fatalf("second Delegate: %v", err)
	}

	if rang != 0 {
		t.Errorf("memory doorbell rang %d times, want 0 — the route's stamp left nothing to end", rang)
	}
	afterEndedAt, reason := boundaryOf(t, database, concluded)
	if afterEndedAt != beforeEndedAt || reason != string(domain.EndedDelegated) {
		t.Errorf("the already-ended row moved to (%v, %q), want its original (%v, delegated)",
			afterEndedAt, reason, beforeEndedAt)
	}
}
