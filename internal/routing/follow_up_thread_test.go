package routing

import (
	"database/sql"
	"slices"
	"sync/atomic"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// followUpBetweenTurns takes the fixture's working conversation through what
// leaves a person's thread on a finished blueprint: the step's verdict, the
// run completing with its task left open, a follow-up's resume, and the end
// of the follow-up's turn. Each is the store write production makes, so the
// row the router then reads is the one those writes leave.
func (f reopenFixture) followUpBetweenTurns(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID
	convs := sqlitestore.New(f.database).Conversations
	endTurn := func(what, outcome string) *domain.Conversation {
		t.Helper()
		claim, err := convs.SetExecutorSystem(ctx, org, f.convID, "follow-up-thread-test", 1)
		if err != nil || claim == nil {
			t.Fatalf("%s: claim the conversation: claim=%v err=%v", what, claim, err)
		}
		row, err := convs.CompleteForClaimSystem(ctx, org, f.convID, claim.ID, domain.StatusOpen, 0, 0, 1, what, outcome, "", "")
		if err != nil || row == nil {
			t.Fatalf("%s: end the turn: row=%v err=%v", what, row, err)
		}
		return row
	}

	if row := endTurn("the step's verdict", string(domain.ConversationOutcomeFinish)); !row.Concluded() {
		t.Fatalf("setup: the step's turn end = %+v, want its verdict recorded under the running blueprint", row)
	}
	if _, err := f.database.Exec(`UPDATE blueprint_runs SET status = ? WHERE id = ?`, domain.BlueprintRunStatusCompleted, f.runID); err != nil {
		t.Fatalf("finish the blueprint: %v", err)
	}
	if ok, err := convs.MarkQueuedForResume(ctx, org, f.convID); err != nil || !ok {
		t.Fatalf("resume for the person's follow-up: ok=%v err=%v", ok, err)
	}
	if row := endTurn("the follow-up's answer", string(domain.ConversationOutcomeContinue)); row.Status != domain.StatusOpen || row.Concluded() {
		t.Fatalf("setup: the follow-up's turn end = %+v, want it parked with no verdict", row)
	}
}

// TestFollowUpThread_OwnEventFoldsIntoThePersonsThread: a blueprint finished
// and left its task open, and a person is discussing the work with the agent
// on the run's conversation. Between two of their messages the task's own
// event arrives again. The thread is live between its turns, so the event
// folds into it as a note: no new blueprint run starts on the task, and the
// thread is not ended under the person.
func TestFollowUpThread_OwnEventFoldsIntoThePersonsThread(t *testing.T) {
	f := newReopenFixture(t, "owner/repo#follow-up-thread")
	f.followUpBetweenTurns(t)
	if status, _, _ := taskRow(t, f.database, f.taskID); status != "in_progress" {
		t.Fatalf("setup: task status = %q, want it still open after its blueprint finished", status)
	}

	emitCI(t, f.router, f.entityID, "aidan")

	disp := f.lastDisposition(t)
	if disp.Disposition != events.DispositionTaskBumped || disp.TaskID != f.taskID {
		t.Errorf("disposition = %q on task %q, want %q on %q", disp.Disposition, disp.TaskID, events.DispositionTaskBumped, f.taskID)
	}
	if disp.TriggersFired != 0 || disp.InjectedConversationID != f.convID {
		t.Errorf("triggers fired = %d, injected conversation = %q, want 0 and the person's thread %q", disp.TriggersFired, disp.InjectedConversationID, f.convID)
	}
	if !slices.Equal(f.fold.folded, []string{f.convID}) {
		t.Errorf("folded into = %v, want the person's thread %s", f.fold.folded, f.convID)
	}
	var kind string
	if err := f.database.QueryRow(`SELECT kind FROM task_events WHERE task_id = ? AND event_id = ?`, f.taskID, disp.EventID).Scan(&kind); err != nil {
		t.Fatalf("read the recurrence's task_event: %v", err)
	}
	if kind != "injected" {
		t.Errorf("task_events.kind for the recurrence = %q, want injected", kind)
	}

	var runs int
	if err := f.database.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = ?`, f.taskID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("blueprint runs on the task = %d, want only the one that finished", runs)
	}
	if n := atomic.LoadInt64(&f.fold.calls); n != 1 {
		t.Errorf("delegations = %d, want only the first event's", n)
	}
	firings, err := sqlitestore.New(f.database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, f.entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(firings) != 0 {
		t.Errorf("pending firings = %+v, want none: the thread took the event", firings)
	}

	var endedAt sql.NullString
	var endedReason sql.NullString
	if err := f.database.QueryRow(`SELECT ended_at, ended_reason FROM conversations WHERE id = ?`, f.convID).Scan(&endedAt, &endedReason); err != nil {
		t.Fatalf("read the thread's boundary: %v", err)
	}
	if endedAt.Valid || endedReason.Valid {
		t.Errorf("the thread's boundary = (%q, %q), want it not ended", endedAt.String, endedReason.String)
	}
	live, err := sqlitestore.New(f.database).Conversations.LiveConversationIDForTaskSystem(t.Context(), runmode.LocalDefaultOrgID, f.taskID)
	if err != nil || live != f.convID {
		t.Errorf("the task's live conversation = %q err=%v, want the person's thread %s", live, err, f.convID)
	}
	if got := f.ciTaskIDs(t); !slices.Equal(got, []string{f.taskID}) {
		t.Errorf("ci tasks on the entity = %v, want only %s", got, f.taskID)
	}
}
