package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// siblingWorkFixture is one conversation on its task, and the means to put
// other tasks on that task's entity.
type siblingWorkFixture struct {
	s        *Spawner
	database *sql.DB
	stores   db.Stores
	convID   string
	// taskID and entityID are the conversation's own.
	taskID   string
	entityID string
}

// newSiblingWorkFixture seeds convID on a fresh task. The conversation is
// left as seedConversation stores it; a test sets the state it needs.
func newSiblingWorkFixture(t *testing.T, convID string) siblingWorkFixture {
	t.Helper()
	database := newDelegateTestDB(t)
	seedConversation(t, database, convID, "sess-"+convID, t.TempDir())
	stores := testSpawnerStores(database)
	f := siblingWorkFixture{
		s:        NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6"),
		database: database,
		stores:   stores,
		convID:   convID,
	}
	if err := database.QueryRow(`SELECT t.id, t.entity_id FROM conversations c JOIN tasks t ON t.id = c.task_id WHERE c.id = ?`, convID).Scan(&f.taskID, &f.entityID); err != nil {
		t.Fatalf("read the conversation's task: %v", err)
	}
	return f
}

// heldTask puts a task of eventType and dedupKey on the conversation's
// entity and has the local user claim it.
func (f siblingWorkFixture) heldTask(t *testing.T, eventType, dedupKey string) string {
	t.Helper()
	ctx := context.Background()
	eventID, err := f.stores.Events.Record(ctx, runmode.LocalDefaultOrgID, domain.Event{EventType: eventType, EntityID: &f.entityID, DedupKey: dedupKey})
	if err != nil {
		t.Fatalf("record %s: %v", eventType, err)
	}
	task, _, err := testTaskStore(f.database).FindOrCreate(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, f.entityID, eventType, dedupKey, eventID, 0.5)
	if err != nil {
		t.Fatalf("create the %s task: %v", eventType, err)
	}
	if ok, err := f.stores.Tasks.ClaimQueuedForUser(ctx, runmode.LocalDefaultOrgID, task.ID, runmode.LocalDefaultUserID,
		dbtest.TaskVersion(t, f.stores.Tasks, runmode.LocalDefaultOrgID, task.ID)); err != nil || !ok {
		t.Fatalf("ClaimQueuedForUser = %v (err %v)", ok, err)
	}
	return task.ID
}

// notes flushes the notes staged on the conversation under producer.
func (f siblingWorkFixture) notes(t *testing.T, producer string) []domain.StagedInjection {
	t.Helper()
	staged, err := f.s.stagedInjections.FlushPendingSystem(context.Background(), runmode.LocalDefaultOrgID, f.convID)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	var out []domain.StagedInjection
	for _, n := range staged {
		if n.Producer == producer {
			out = append(out, n)
		}
	}
	return out
}

// assignedEvent is the bus event a claim by the local user publishes.
func assignedEvent(t *testing.T, meta events.SystemTaskAssignedMetadata) domain.Event {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return domain.Event{OrgID: runmode.LocalDefaultOrgID, EventType: domain.EventSystemTaskAssigned, MetadataJSON: string(raw)}
}

// noteFor renders the note the task's stored assignment composes, as a
// recurrence or not.
func (f siblingWorkFixture) noteFor(t *testing.T, taskID string, recurrence bool) string {
	t.Helper()
	a, err := f.stores.Tasks.AssignmentSystem(context.Background(), runmode.LocalDefaultOrgID, taskID)
	if err != nil || a == nil {
		t.Fatalf("AssignmentSystem(%s) = %v (err %v)", taskID, a, err)
	}
	a.Recurrence = recurrence
	return domain.TaskAssignedNote(*a)
}

// TestTellTaskAssigned_TellsTheOverlappingConversationOfTheAssignment is the
// sender: a live conversation whose work overlaps a task that gains an
// assignee gets one note, composed from the task as stored and carrying the
// task and the assignee in its provenance. An event the task has moved past
// tells nothing.
func TestTellTaskAssigned_TellsTheOverlappingConversationOfTheAssignment(t *testing.T) {
	f := newSiblingWorkFixture(t, "c-overlap")
	markEngaged(t, f.database, f.convID)
	assigned := f.heldTask(t, domain.EventGitHubPRReviewChangesRequested, "")
	holder := domain.UserAssignee(runmode.LocalDefaultUserID)

	// The agent held the task when this event was published, and a person
	// holds it now: their claim published the event that tells of it.
	f.s.HandleTaskAssigned(assignedEvent(t, events.SystemTaskAssignedMetadata{TaskID: assigned, AgentID: runmode.LocalDefaultAgentID}))
	if got := f.notes(t, domain.StagedInjectionProducerTaskAssigned); len(got) != 0 {
		t.Fatalf("an event naming an assignee the task no longer has staged %d notes, want none", len(got))
	}

	// A publisher names the ids as its caller spelled them. The note is
	// staged all the same, and records them as the store spells them.
	f.s.HandleTaskAssigned(assignedEvent(t, events.SystemTaskAssignedMetadata{TaskID: "{" + assigned + "}", UserID: "{" + runmode.LocalDefaultUserID + "}"}))
	got := f.notes(t, domain.StagedInjectionProducerTaskAssigned)
	if len(got) != 1 {
		t.Fatalf("staged %d assignment notes, want one: %+v", len(got), got)
	}
	note := got[0]
	if note.Subtype != domain.MessageSubtypeInjectionSystemNote {
		t.Errorf("subtype = %q, want a system note", note.Subtype)
	}
	if want := (domain.NoteProvenance{AssignedTask: assigned, Assignee: holder}); note.Provenance != want {
		t.Errorf("provenance = %+v, want %+v", note.Provenance, want)
	}
	if note.Body != f.noteFor(t, assigned, false) {
		t.Errorf("the note is not the stored assignment's:\n%s", note.Body)
	}

	// The task's own conversations are not told of it.
	if own, err := f.s.stagedInjections.FlushPendingSystem(context.Background(), runmode.LocalDefaultOrgID, f.convID); err != nil || len(own) != 0 {
		t.Fatalf("notes left after the flush = %d (err %v)", len(own), err)
	}

	// A close ends the assignment, so an event still in flight tells nothing.
	if _, err := f.stores.Tasks.CloseSystem(context.Background(), runmode.LocalDefaultOrgID, assigned, db.RunCompletedCloseReason, ""); err != nil {
		t.Fatalf("CloseSystem: %v", err)
	}
	f.s.TellTaskAssigned(context.Background(), runmode.LocalDefaultOrgID, assigned, holder)
	if got := f.notes(t, domain.StagedInjectionProducerTaskAssigned); len(got) != 0 {
		t.Errorf("an assignment of a closed task staged %d notes, want none", len(got))
	}
}

// TestTellTaskAssigned_ARecurrenceOfTheRecipientsTaskIsToldAsOne: when the
// assigned task has the entity, event type and dedup key of the recipient's
// own task, which has closed, the note is the recurrence's.
func TestTellTaskAssigned_ARecurrenceOfTheRecipientsTaskIsToldAsOne(t *testing.T) {
	f := newSiblingWorkFixture(t, "c-recurred")
	markEngaged(t, f.database, f.convID)
	ctx := context.Background()
	own, err := f.stores.Tasks.GetSystem(ctx, runmode.LocalDefaultOrgID, f.taskID)
	if err != nil || own == nil {
		t.Fatalf("GetSystem = %v (err %v)", own, err)
	}
	if _, err := f.stores.Tasks.CloseSystem(ctx, runmode.LocalDefaultOrgID, f.taskID, db.SiblingEventCloseReason, domain.EventGitHubPRCICheckPassed); err != nil {
		t.Fatalf("CloseSystem: %v", err)
	}
	recurred := f.heldTask(t, own.EventType, own.DedupKey)
	if recurred == f.taskID {
		t.Fatal("the recurrence found the closed task, want a task of its own")
	}

	f.s.TellTaskAssigned(ctx, runmode.LocalDefaultOrgID, recurred, domain.UserAssignee(runmode.LocalDefaultUserID))

	got := f.notes(t, domain.StagedInjectionProducerTaskAssigned)
	if len(got) != 1 {
		t.Fatalf("staged %d assignment notes, want one: %+v", len(got), got)
	}
	asRecurrence, asSibling := f.noteFor(t, recurred, true), f.noteFor(t, recurred, false)
	if asRecurrence == asSibling {
		t.Fatal("a recurrence's note is the same as any other assignment's")
	}
	if got[0].Body != asRecurrence {
		t.Errorf("the note is not the recurrence's:\n%s", got[0].Body)
	}
}

// TestWake_AConcludedConversationIsCaughtUpOnOverlappingWork is the wake: a
// follow-up that wakes a concluded conversation stages one note listing the
// work that overlaps it when there is some, and none when there is not. A
// conversation woken from parked was live while it sat and is told nothing
// here.
func TestWake_AConcludedConversationIsCaughtUpOnOverlappingWork(t *testing.T) {
	cases := []struct {
		name      string
		concluded bool
		sibling   bool
		want      int
	}{
		{name: "concluded, with overlapping work", concluded: true, sibling: true, want: 1},
		{name: "concluded, with none", concluded: true},
		{name: "parked, with overlapping work", sibling: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths.SetForTest(t, t.TempDir())
			f := newSiblingWorkFixture(t, "c-woken")
			rest := `UPDATE conversations SET status = 'open' WHERE id = ?`
			if tc.concluded {
				rest = `UPDATE conversations SET status = 'open', completed_at = CURRENT_TIMESTAMP, outcome = 'finish' WHERE id = ?`
			}
			if _, err := f.database.Exec(rest, f.convID); err != nil {
				t.Fatalf("park the conversation: %v", err)
			}
			if tc.concluded {
				finishBlueprint(t, f.database, blueprintRunIDForConversation(t, f.database, f.convID), "completed", 0)
			}
			if tc.sibling {
				f.heldTask(t, domain.EventGitHubPRReviewChangesRequested, "")
			}

			if err := f.s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, f.convID, runmode.LocalDefaultUserID, "one more thing"); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}

			listing, err := f.stores.Tasks.SiblingWorkSystem(context.Background(), runmode.LocalDefaultOrgID, f.convID)
			if err != nil {
				t.Fatalf("SiblingWorkSystem: %v", err)
			}
			if (len(listing) > 0) != tc.sibling {
				t.Fatalf("setup: the listing has %d tasks, sibling work staged = %v", len(listing), tc.sibling)
			}
			got := f.notes(t, domain.StagedInjectionProducerSiblingWork)
			if len(got) != tc.want {
				t.Fatalf("staged %d overlapping-work notes, want %d: %+v", len(got), tc.want, got)
			}
			if tc.want == 0 {
				return
			}
			if got[0].Subtype != domain.MessageSubtypeInjectionSystemNote || got[0].Body != domain.SiblingWorkNote(listing) {
				t.Errorf("the note is not the conversation's listing: subtype %q\n%s", got[0].Subtype, got[0].Body)
			}
		})
	}
}

// TestDispatch_RetiresAStaleAssignmentNoteBeforeTheTurn: a conversation that
// sat idle holding an assignment note about a task since closed starts its
// next turn without it.
func TestDispatch_RetiresAStaleAssignmentNoteBeforeTheTurn(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	f := newSiblingWorkFixture(t, "c-idle")
	ctx := context.Background()
	if _, err := f.database.Exec(`UPDATE conversations SET status = 'open' WHERE id = ?`, f.convID); err != nil {
		t.Fatalf("park the conversation: %v", err)
	}
	// A parked conversation is live, so it is told as the assignments happen.
	stale := f.heldTask(t, domain.EventGitHubPRReviewChangesRequested, "")
	current := f.heldTask(t, domain.EventGitHubPRConflicts, "")
	holder := domain.UserAssignee(runmode.LocalDefaultUserID)
	f.s.TellTaskAssigned(ctx, runmode.LocalDefaultOrgID, stale, holder)
	f.s.TellTaskAssigned(ctx, runmode.LocalDefaultOrgID, current, holder)
	if _, err := f.stores.Tasks.CloseSystem(ctx, runmode.LocalDefaultOrgID, stale, db.RunCompletedCloseReason, ""); err != nil {
		t.Fatalf("CloseSystem: %v", err)
	}

	if err := f.s.SendMessage(ctx, runmode.LocalDefaultOrgID, f.convID, runmode.LocalDefaultUserID, "go on"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	claimAndDispatch(t, f.s, f.database)

	rows, err := f.database.Query(`
		SELECT json_extract(metadata, '$.assigned_task'), delivered = 0 AND window_state = 'inactive'
		  FROM messages
		 WHERE conversation_id = ? AND json_extract(metadata, '$.producer') = ?`, f.convID, domain.StagedInjectionProducerTaskAssigned)
	if err != nil {
		t.Fatalf("read the assignment notes: %v", err)
	}
	defer rows.Close()
	retired := map[string]bool{}
	for rows.Next() {
		var taskID string
		var withdrawn bool
		if err := rows.Scan(&taskID, &withdrawn); err != nil {
			t.Fatalf("scan: %v", err)
		}
		retired[taskID] = withdrawn
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the assignment notes: %v", err)
	}
	if want := map[string]bool{stale: true, current: false}; len(retired) != 2 || retired[stale] != want[stale] || retired[current] != want[current] {
		t.Errorf("retired = %v, want the closed task's note withdrawn and the other kept", retired)
	}
}
