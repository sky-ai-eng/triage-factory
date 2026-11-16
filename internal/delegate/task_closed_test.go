package delegate

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// closedTaskFixture is a conversation an event started, working on its task
// under an active claim.
type closedTaskFixture struct {
	s        *Spawner
	database *sql.DB
	stores   db.Stores
	convID   string
	taskID   string
	entityID string
}

func newClosedTaskFixture(t *testing.T, convID string) closedTaskFixture {
	t.Helper()
	database := newDelegateTestDB(t)
	seedConversation(t, database, convID, "sess", "/tmp/wt")
	markEngaged(t, database, convID)
	if _, err := database.Exec(`
		UPDATE conversations SET trigger_type = 'event', creator_user_id = NULL
		 WHERE id = ?`, convID); err != nil {
		t.Fatalf("make the conversation event-triggered: %v", err)
	}
	stores := testSpawnerStores(database)
	f := closedTaskFixture{
		s:        NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6"),
		database: database,
		stores:   stores,
		convID:   convID,
	}
	if err := database.QueryRow(`SELECT t.id, t.entity_id FROM conversations c JOIN tasks t ON t.id = c.task_id WHERE c.id = ?`, convID).Scan(&f.taskID, &f.entityID); err != nil {
		t.Fatalf("read task: %v", err)
	}
	return f
}

// event records an event on the fixture's entity and returns it as stored.
func (f closedTaskFixture) event(t *testing.T, eventType, metadataJSON string) domain.Event {
	t.Helper()
	ctx := context.Background()
	id, err := f.stores.Events.Record(ctx, runmode.LocalDefaultOrgID, domain.Event{EventType: eventType, EntityID: &f.entityID, MetadataJSON: metadataJSON})
	if err != nil {
		t.Fatalf("record %s: %v", eventType, err)
	}
	evt, err := f.stores.Events.GetSystem(ctx, runmode.LocalDefaultOrgID, id)
	if err != nil || evt == nil {
		t.Fatalf("read %s: %v", eventType, err)
	}
	return *evt
}

// closeOnEvent closes the task on evt and returns the conversations the
// close reports for its caller to tell.
func (f closedTaskFixture) closeOnEvent(t *testing.T, reason string, evt domain.Event) []string {
	t.Helper()
	out, err := f.stores.Tasks.CloseOnEventSystem(context.Background(), runmode.LocalDefaultOrgID, f.taskID, reason, evt.EventType, evt.ID)
	if err != nil || !out.Closed {
		t.Fatalf("CloseOnEventSystem = (%+v, %v), want closed", out, err)
	}
	return out.WorkingConversationIDs
}

// closeByPerson dismisses or completes the task as a person does.
func (f closedTaskFixture) closeByPerson(t *testing.T, status string) {
	t.Helper()
	version := dbtest.TaskVersion(t, f.stores.Tasks, runmode.LocalDefaultOrgID, f.taskID)
	if _, err := f.stores.Tasks.CloseByUser(context.Background(), runmode.LocalDefaultOrgID, f.taskID, status, version); err != nil {
		t.Fatalf("CloseByUser(%s): %v", status, err)
	}
}

// notes flushes the close notes staged on the fixture's conversation.
func (f closedTaskFixture) notes(t *testing.T) []domain.StagedInjection {
	t.Helper()
	return f.notesOn(t, f.convID)
}

// notesOn flushes the close notes staged on conversationID.
func (f closedTaskFixture) notesOn(t *testing.T, conversationID string) []domain.StagedInjection {
	t.Helper()
	notes, err := f.s.stagedInjections.FlushPendingSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("flush %s: %v", conversationID, err)
	}
	return notes
}

// chat adds a conversation a person works the fixture's task in, which
// belongs to no blueprint run, working under an active claim as the
// fixture's conversation is.
func (f closedTaskFixture) chat(t *testing.T, conversationID string) {
	t.Helper()
	dbtest.SeedConversation(t, f.database, domain.Conversation{ID: conversationID, TaskID: f.taskID})
	if _, err := f.database.Exec(`UPDATE conversations SET type = ? WHERE id = ?`, domain.ConversationTypeInteractive, conversationID); err != nil {
		t.Fatalf("make %s a chat: %v", conversationID, err)
	}
	markEngaged(t, f.database, conversationID)
}

// currentMetadata is an event's metadata as a note carries it.
func currentMetadata(evt domain.Event) string {
	if current, err := events.Current(context.Background(), runmode.LocalDefaultOrgID, evt.EventType, evt.MetadataJSON); err == nil {
		return current
	}
	return evt.MetadataJSON
}

// TestTellTaskClosed_EachClosePathSendsOneNoteOfItsKind: every close of a
// task tells the conversation working on it once, with the note for that
// kind of close, keyed on the close. The expected body is composed by the
// same domain function from the kind the path should resolve to, so the test
// pins which note is sent and not what it says.
func TestTellTaskClosed_EachClosePathSendsOneNoteOfItsKind(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// close closes the task and tells its working conversations, as the
		// path does, and returns the close the note should describe.
		close func(t *testing.T, f closedTaskFixture) domain.TaskClose
	}{
		{name: "sibling_event", close: func(t *testing.T, f closedTaskFixture) domain.TaskClose {
			evt := f.event(t, domain.EventGitHubPRCICheckPassed, `{"check_name":"build"}`)
			f.s.TellTaskClosed(ctx, org, f.taskID, f.closeOnEvent(t, db.SiblingEventCloseReason, evt))
			return domain.TaskClose{Kind: domain.TaskClosedByEvent, EventID: evt.ID, EventType: evt.EventType, MetadataJSON: currentMetadata(evt)}
		}},
		{name: "terminating_event", close: func(t *testing.T, f closedTaskFixture) domain.TaskClose {
			evt := f.event(t, domain.EventGitHubPRMerged, `{"merged_by":"octocat"}`)
			f.s.TellTaskClosed(ctx, org, f.taskID, f.closeOnEvent(t, "entity_closed", evt))
			return domain.TaskClose{Kind: domain.TaskClosedByEvent, EventID: evt.ID, EventType: evt.EventType, MetadataJSON: currentMetadata(evt)}
		}},
		// The poll's close obligation is the closing event of a reconciled
		// close, and names no event an agent would recognize.
		{name: "reconciled", close: func(t *testing.T, f closedTaskFixture) domain.TaskClose {
			evt := f.event(t, domain.EventSystemEntityCloseOwed, `{}`)
			f.s.TellTaskClosed(ctx, org, f.taskID, f.closeOnEvent(t, "reconciled", evt))
			return domain.TaskClose{Kind: domain.TaskClosedByEvent}
		}},
		{name: "reassigned", close: func(t *testing.T, f closedTaskFixture) domain.TaskClose {
			evt := f.event(t, domain.EventLinearIssueAssigned, `{"issue_identifier":"ENG-7","assignee":"Bob Rivera","assignee_user_id":"lu-bob"}`)
			f.s.TellTaskClosed(ctx, org, f.taskID, f.closeOnEvent(t, db.ReassignedCloseReason, evt))
			return domain.TaskClose{Kind: domain.TaskClosedReassigned, EventID: evt.ID, EventType: evt.EventType, MetadataJSON: currentMetadata(evt)}
		}},
		{name: "person_completed", close: func(t *testing.T, f closedTaskFixture) domain.TaskClose {
			f.closeByPerson(t, "done")
			f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
			return domain.TaskClose{Kind: domain.TaskClosedCompleted}
		}},
		{name: "person_dismissed", close: func(t *testing.T, f closedTaskFixture) domain.TaskClose {
			f.closeByPerson(t, "dismissed")
			f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
			return domain.TaskClose{Kind: domain.TaskClosedDismissed}
		}},
		// What a duplicate merge leaves: the dismissed task beside the active
		// one that holds its slot, on one entity.
		{name: "duplicate_merge", close: func(t *testing.T, f closedTaskFixture) domain.TaskClose {
			var eventType, dedupKey, primaryEvent string
			if err := f.database.QueryRow(`SELECT event_type, dedup_key, primary_event_id FROM tasks WHERE id = ?`, f.taskID).Scan(&eventType, &dedupKey, &primaryEvent); err != nil {
				t.Fatalf("read the task's key: %v", err)
			}
			if _, err := f.database.Exec(`UPDATE tasks SET status = 'dismissed', close_reason = ?, closed_at = ? WHERE id = ?`,
				db.DuplicateEntityMergedCloseReason, time.Now().UTC(), f.taskID); err != nil {
				t.Fatalf("dismiss the duplicate: %v", err)
			}
			survivor, _, err := f.stores.Tasks.FindOrCreate(ctx, org, runmode.LocalDefaultTeamID, f.entityID, eventType, dedupKey, primaryEvent, 0.5)
			if err != nil || survivor == nil || survivor.ID == f.taskID {
				t.Fatalf("create the task the duplicate was merged beside: %+v (err %v)", survivor, err)
			}
			f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
			return domain.TaskClose{Kind: domain.TaskClosedDuplicate, DuplicateOf: "task " + survivor.ID}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClosedTaskFixture(t, "run-closed-"+tc.name)
			want := tc.close(t, f)
			// A second tell of the same close, as a resume after it makes.
			f.s.tellTaskResolved(ctx, org, f.convID, f.taskID)

			task, err := f.stores.Tasks.GetSystem(ctx, org, f.taskID)
			if err != nil || task == nil || task.ClosedAt == nil {
				t.Fatalf("read the closed task: %+v (err %v)", task, err)
			}
			notes := f.notes(t)
			if len(notes) != 1 {
				t.Fatalf("staged notes = %+v, want one", notes)
			}
			n := notes[0]
			if n.Body != domain.TaskClosedNote(want, domain.TaskNoteForBlueprint) {
				t.Errorf("note body = %q\nwant the note for %+v: %q", n.Body, want, domain.TaskClosedNote(want, domain.TaskNoteForBlueprint))
			}
			if n.Producer != domain.StagedInjectionProducerTaskClosing || n.Subtype != domain.MessageSubtypeInjectionSystemNote {
				t.Errorf("note = (%q, %q), want the close note's producer under the system-note subtype", n.Producer, n.Subtype)
			}
			wantProv := domain.NoteProvenance{EventID: want.EventID, EventType: want.EventType, TaskClose: domain.TaskCloseKey(*task.ClosedAt)}
			if n.Provenance != wantProv {
				t.Errorf("note provenance = %+v, want %+v", n.Provenance, wantProv)
			}
		})
	}
}

// TestTellTaskClosed_TheFiveNotesDiffer: for each audience, each kind of
// close has a note of its own, so a path that resolved to the wrong kind
// would send a different body and fail the test above. The work's own close
// has none for either audience.
func TestTellTaskClosed_TheFiveNotesDiffer(t *testing.T) {
	for _, a := range []domain.TaskNoteAudience{domain.TaskNoteForBlueprint, domain.TaskNoteForChat} {
		seen := map[string]domain.TaskCloseKind{}
		for _, kind := range []domain.TaskCloseKind{
			domain.TaskClosedByEvent, domain.TaskClosedCompleted, domain.TaskClosedDismissed,
			domain.TaskClosedDuplicate, domain.TaskClosedReassigned,
		} {
			body := domain.TaskClosedNote(domain.TaskClose{Kind: kind}, a)
			if body == "" {
				t.Errorf("audience %d: kind %d has no note", a, kind)
			}
			if other, dup := seen[body]; dup {
				t.Errorf("audience %d: kinds %d and %d share a note", a, other, kind)
			}
			seen[body] = kind
		}
		if body := domain.TaskClosedNote(domain.TaskClose{Kind: domain.TaskClosedWorkCompleted}, a); body != "" {
			t.Errorf("audience %d: the work's own close has a note: %q", a, body)
		}
	}
}

// TestTellTaskClosed_EachConversationIsToldForItsAudience: a close of a task
// that a blueprint conversation and a chat are both working on tells each
// of them once, with the note for its own audience: whether it belongs to a
// blueprint run, which is what offers it stop_blueprint.
func TestTellTaskClosed_EachConversationIsToldForItsAudience(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	f := newClosedTaskFixture(t, "run-closed-beside-a-chat")
	const chatID = "chat-closed-beside-a-run"
	f.chat(t, chatID)

	evt := f.event(t, domain.EventGitHubPRMerged, `{"merged_by":"octocat"}`)
	working := f.closeOnEvent(t, "entity_closed", evt)
	if !slices.Contains(working, f.convID) || !slices.Contains(working, chatID) {
		t.Fatalf("setup: the close found %v working, want %s and %s", working, f.convID, chatID)
	}
	f.s.TellTaskClosed(ctx, org, f.taskID, working)

	c := domain.TaskClose{Kind: domain.TaskClosedByEvent, EventID: evt.ID, EventType: evt.EventType, MetadataJSON: currentMetadata(evt)}
	if domain.TaskClosedNote(c, domain.TaskNoteForBlueprint) == domain.TaskClosedNote(c, domain.TaskNoteForChat) {
		t.Fatal("the two audiences share a note; this test cannot tell which one a conversation got")
	}
	for id, a := range map[string]domain.TaskNoteAudience{f.convID: domain.TaskNoteForBlueprint, chatID: domain.TaskNoteForChat} {
		notes := f.notesOn(t, id)
		if len(notes) != 1 || notes[0].Body != domain.TaskClosedNote(c, a) {
			t.Errorf("%s: staged notes = %+v, want the one note for audience %d", id, notes, a)
		}
	}
}

// TestTellTaskClosed_TheWorksOwnCloseTellsNothing: a task its blueprint's
// finish closed sends no note. The workflow the note would end is over, and
// the conversations still working on such a task are follow-ups a person
// asked for.
func TestTellTaskClosed_TheWorksOwnCloseTellsNothing(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	f := newClosedTaskFixture(t, "run-closed-by-its-work")
	if _, err := f.stores.Tasks.CloseSystem(ctx, org, f.taskID, db.RunCompletedCloseReason, ""); err != nil {
		t.Fatalf("CloseSystem: %v", err)
	}

	f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
	f.s.tellTaskResolved(ctx, org, f.convID, f.taskID)

	if notes := f.notes(t); len(notes) != 0 {
		t.Errorf("staged notes = %+v, want none", notes)
	}
}

// TestTellTaskClosed_WhoStartedTheWorkChangesNothing: a conversation a person
// delegated and has written to gets the note an event-started one gets.
func TestTellTaskClosed_WhoStartedTheWorkChangesNothing(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	bodies := map[string]string{}
	for _, who := range []string{"event", "person"} {
		f := newClosedTaskFixture(t, "run-closed-for-"+who)
		if who == "person" {
			if _, err := f.database.Exec(`UPDATE conversations SET trigger_type = 'manual', creator_user_id = ? WHERE id = ?`, runmode.LocalDefaultUserID, f.convID); err != nil {
				t.Fatalf("make the conversation a person's: %v", err)
			}
			delivered := true
			message := pendingUserInput(f.convID, runmode.LocalDefaultUserID, "fix the nits before I approve", domain.TurnSettings{})
			message.Delivered = &delivered
			if _, err := f.s.conversations.InsertMessage(ctx, org, message); err != nil {
				t.Fatalf("record the person's message: %v", err)
			}
		}
		f.closeByPerson(t, "dismissed")
		f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
		notes := f.notes(t)
		if len(notes) != 1 {
			t.Fatalf("%s: staged notes = %+v, want one", who, notes)
		}
		bodies[who] = notes[0].Body
	}
	if bodies["event"] != bodies["person"] {
		t.Errorf("the note differs by who started the work:\n event:  %q\n person: %q", bodies["event"], bodies["person"])
	}
}

// TestTellTaskResolved_AResumeAfterTheCloseTellsOnce: a conversation that was
// not working when its task closed was told nothing then. Its next
// engagement tells it, and the one after that does not tell it again.
func TestTellTaskResolved_AResumeAfterTheCloseTellsOnce(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	f := newClosedTaskFixture(t, "run-parked-at-close")
	if _, err := f.database.Exec(`UPDATE claims SET released_at = CURRENT_TIMESTAMP WHERE conversation_id = ?`, f.convID); err != nil {
		t.Fatalf("release the claim: %v", err)
	}
	if _, err := f.database.Exec(`UPDATE conversations SET status = 'open' WHERE id = ?`, f.convID); err != nil {
		t.Fatalf("park the conversation: %v", err)
	}
	f.closeByPerson(t, "done")
	f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
	if notes := f.notes(t); len(notes) != 0 {
		t.Fatalf("setup: the close told a parked conversation: %+v", notes)
	}

	f.s.tellTaskResolved(ctx, org, f.convID, f.taskID)
	notes := f.notes(t)
	if len(notes) != 1 || notes[0].Body != domain.TaskClosedNote(domain.TaskClose{Kind: domain.TaskClosedCompleted}, domain.TaskNoteForBlueprint) {
		t.Fatalf("notes at the resume = %+v, want the one close note", notes)
	}
	// The note is delivered now, and still counts as told.
	f.s.tellTaskResolved(ctx, org, f.convID, f.taskID)
	if again := f.notes(t); len(again) != 0 {
		t.Errorf("notes at the next resume = %+v, want none", again)
	}
}

// TestTellTaskResolved_ALaunchOnAClosedTaskReadsRunContextAndTheNote: a
// conversation launched once its task is closed reads the close in its
// <run_context>, which names how the task closed, and gets the close's note
// as any conversation not yet told does. An open task's launch says nothing
// about the task's state.
func TestTellTaskResolved_ALaunchOnAClosedTaskReadsRunContextAndTheNote(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	f := newClosedTaskFixture(t, "run-launched-after-close")
	if line := f.s.closedTaskContext(ctx, org, f.taskID); line != "" {
		t.Fatalf("an open task's <run_context> line = %q, want none", line)
	}
	f.closeByPerson(t, "dismissed")

	f.s.tellTaskResolved(ctx, org, f.convID, f.taskID)
	notes := f.notes(t)
	if len(notes) != 1 || notes[0].Body != domain.TaskClosedNote(domain.TaskClose{Kind: domain.TaskClosedDismissed}, domain.TaskNoteForBlueprint) {
		t.Errorf("notes for a launch on the closed task = %+v, want the dismissal's note", notes)
	}

	line := f.s.closedTaskContext(ctx, org, f.taskID)
	if want := domain.TaskClosedRunContext(domain.TaskClose{Kind: domain.TaskClosedDismissed}); line != want {
		t.Errorf("<run_context> line = %q, want %q", line, want)
	}
	if block := runContext("", line, "", "/work", "", "", "", "", "", ""); !strings.Contains(block, line) {
		t.Errorf("the run context does not carry the task's state:\n%s", block)
	}
	// Both runtimes compose their launch's block from the same line.
	if other := domain.TaskClosedRunContext(domain.TaskClose{Kind: domain.TaskClosedCompleted}); other == line {
		t.Error("the <run_context> line does not say how the task closed")
	}
}

// TestTaskClosed_OnlyTheEventMetadataCarriesWhatASourceAuthored: a
// reassignment's event names the new assignee by a display name the source
// wrote. Either audience's note prints it as the event's metadata and nowhere
// else, and <run_context>, which is TF's own words, leaves the metadata out.
func TestTaskClosed_OnlyTheEventMetadataCarriesWhatASourceAuthored(t *testing.T) {
	const assignee, metadata = "Bob Rivera", `{"assignee":"Bob Rivera"}`
	c := domain.TaskClose{Kind: domain.TaskClosedReassigned, EventType: domain.EventLinearIssueAssigned, MetadataJSON: metadata}
	if line := domain.TaskClosedRunContext(c); strings.Contains(line, assignee) {
		t.Errorf("<run_context> line carries text the source authored: %q", line)
	}
	for _, a := range []domain.TaskNoteAudience{domain.TaskNoteForBlueprint, domain.TaskNoteForChat} {
		note := domain.TaskClosedNote(c, a)
		if strings.Count(note, assignee) != 1 || !strings.Contains(note, metadata) {
			t.Errorf("audience %d: the note names the assignee outside the event's metadata, or not at all: %q", a, note)
		}
	}
}

// TestTellTaskClosed_AnEarlierClosesEventIsNotThisCloses: the task's newest
// audit row of an event's close belongs to this close only when it is of the
// type this close stamped. A task an event closed, that reopened, and that
// closed again with no audit row of its own is told of the second close
// without the first one's event.
func TestTellTaskClosed_AnEarlierClosesEventIsNotThisCloses(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	f := newClosedTaskFixture(t, "run-closed-by-two-events")
	first := f.event(t, domain.EventGitHubPRCICheckPassed, `{"check_name":"build"}`)
	f.closeOnEvent(t, db.SiblingEventCloseReason, first)
	if _, err := f.database.Exec(`
		UPDATE tasks SET status = 'in_progress', close_reason = NULL, close_event_type = NULL, closed_at = NULL, version = version + 1
		 WHERE id = ?`, f.taskID); err != nil {
		t.Fatalf("reopen the task: %v", err)
	}
	out, err := f.stores.Tasks.CloseOnEventSystem(ctx, org, f.taskID, "entity_closed", domain.EventGitHubPRMerged, "")
	if err != nil || !out.Closed {
		t.Fatalf("CloseOnEventSystem = (%+v, %v), want closed", out, err)
	}

	f.s.TellTaskClosed(ctx, org, f.taskID, out.WorkingConversationIDs)
	notes := f.notes(t)
	want := domain.TaskClose{Kind: domain.TaskClosedByEvent, EventType: domain.EventGitHubPRMerged}
	if len(notes) != 1 || notes[0].Body != domain.TaskClosedNote(want, domain.TaskNoteForBlueprint) || notes[0].Provenance.EventID != "" {
		t.Errorf("notes = %+v, want one naming the second close's event type and no event", notes)
	}
}

// TestTellTaskResolved_ACloseAfterAReopenIsToldAgain: the note is keyed on the
// close, so a task an event reopened and something closed again tells its
// conversation of the second close too.
func TestTellTaskResolved_ACloseAfterAReopenIsToldAgain(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	f := newClosedTaskFixture(t, "run-closed-twice")
	f.closeByPerson(t, "done")
	f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
	if notes := f.notes(t); len(notes) != 1 {
		t.Fatalf("notes at the first close = %+v, want one", notes)
	}

	if _, err := f.database.Exec(`
		UPDATE tasks SET status = 'in_progress', close_reason = NULL, close_event_type = NULL, closed_at = NULL, version = version + 1
		 WHERE id = ?`, f.taskID); err != nil {
		t.Fatalf("reopen the task: %v", err)
	}
	f.s.tellTaskResolved(ctx, org, f.convID, f.taskID)
	if notes := f.notes(t); len(notes) != 0 {
		t.Fatalf("notes on the reopened task = %+v, want none", notes)
	}

	f.closeByPerson(t, "dismissed")
	f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
	notes := f.notes(t)
	if len(notes) != 1 || notes[0].Body != domain.TaskClosedNote(domain.TaskClose{Kind: domain.TaskClosedDismissed}, domain.TaskNoteForBlueprint) {
		t.Errorf("notes at the second close = %+v, want the dismissal's note", notes)
	}
}

// TestTellTaskClosed_AnSDKRunReadsTheNoteAtItsToolBoundaryOrItsResume: the
// SDK is handed the close note with its next tool batch while a process is
// live, and ahead of the person's message on the next engagement when none
// is. Both carry the note's own body inside the bundle.
func TestTellTaskClosed_AnSDKRunReadsTheNoteAtItsToolBoundaryOrItsResume(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	body := domain.TaskClosedNote(domain.TaskClose{Kind: domain.TaskClosedDismissed}, domain.TaskNoteForBlueprint)

	t.Run("live", func(t *testing.T) {
		f := newClosedTaskFixture(t, "run-closed-live")
		f.s.registerProc(org, f.convID, &agentproc.LiveRun{})
		f.s.getProc(f.convID).boundary.take() // armed from registration
		f.closeByPerson(t, "dismissed")
		f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
		if got := f.s.notesAtToolBatch(org, f.convID, "").Text; !strings.Contains(got, body) {
			t.Errorf("the next tool batch handed over %q, want the close note", got)
		}
	})

	t.Run("resume", func(t *testing.T) {
		f := newClosedTaskFixture(t, "run-closed-resume")
		f.closeByPerson(t, "dismissed")
		f.s.TellWorkingTaskClosed(ctx, org, f.taskID)
		block, _ := f.s.stagedInjectionsForEngagement(ctx, org, f.convID)
		if !strings.Contains(block, body) {
			t.Errorf("the engagement's first message carries %q, want the close note", block)
		}
	})
}
