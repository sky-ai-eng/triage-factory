package delegate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// handOffFixture is a conversation under a running blueprint, standing in for
// the step a blueprint has just advanced to, with a trigger and an event a
// firing can be queued for.
func handOffFixture(t *testing.T, convID string) (s *Spawner, database *sql.DB, br *domain.BlueprintRun, conv *domain.Conversation, entityID, taskID, triggerID string) {
	t.Helper()
	database = newDelegateTestDB(t)
	seedConversation(t, database, convID, "sess", "/tmp/wt")
	s = NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	entityID = entityIDForConversation(t, database, convID)
	taskID = taskIDForConversation(t, database, convID)
	triggerID = seedFakeTrigger(t, database, convID)
	var err error
	if conv, err = s.conversations.GetSystem(ctx, org, convID); err != nil || conv == nil {
		t.Fatalf("read conversation: %v", err)
	}
	if br, err = s.blueprints.GetRunSystem(ctx, org, conv.BlueprintRunID); err != nil || br == nil {
		t.Fatalf("read blueprint run: %v", err)
	}
	if br.Status != domain.BlueprintRunStatusRunning {
		t.Fatalf("fixture blueprint run is %q, want running", br.Status)
	}
	return s, database, br, conv, entityID, taskID, triggerID
}

func readTask(t *testing.T, s *Spawner, taskID string) *domain.Task {
	t.Helper()
	task, err := s.tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, taskID)
	if err != nil || task == nil {
		t.Fatalf("read task: %v", err)
	}
	return task
}

// requireStillQueued asserts the entity's one firing is ready and nothing was
// staged for the conversation.
func requireStillQueued(t *testing.T, s *Spawner, entityID, convID string) {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	rows, err := s.pendingFirings.ListForEntity(ctx, org, entityID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("firings = %d rows, err=%v, want 1", len(rows), err)
	}
	if rows[0].Status != workitem.StatusReady || rows[0].HandedNoteID != "" {
		t.Errorf("firing = %+v, want it still ready and not handed over", rows[0])
	}
	if notes, _ := s.stagedInjections.FlushPendingSystem(ctx, org, convID); len(notes) != 0 {
		t.Errorf("staged notes = %+v, want none", notes)
	}
}

// queueFiring admits a firing for a fresh event on the task, with the
// timeline row the router records when the event bumps the task.
func queueFiring(t *testing.T, s *Spawner, database *sql.DB, entityID, taskID, triggerID string) (eventID string) {
	t.Helper()
	eventID = seedFakeEvent(t, database)
	if _, err := database.Exec(`INSERT INTO task_events (task_id, event_id, kind) VALUES (?, ?, 'bumped')`, taskID, eventID); err != nil {
		t.Fatalf("seed task event: %v", err)
	}
	if _, _, err := s.pendingFirings.Enqueue(context.Background(), runmode.LocalDefaultOrgID, entityID, taskID, triggerID, eventID, db.AgentClaimStamp{}); err != nil {
		t.Fatalf("enqueue firing: %v", err)
	}
	return eventID
}

// An event queued while the blueprint was between steps reaches the next step
// as the note a folded-in event is. The firing names the note and stays
// queued for the worker to settle.
func TestHandOffQueuedFirings_HandsTheEventToTheStartingStep(t *testing.T) {
	s, database, br, conv, entityID, taskID, triggerID := handOffFixture(t, "step-next")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	eventID := queueFiring(t, s, database, entityID, taskID, triggerID)

	s.handOffQueuedFirings(ctx, org, br, conv, readTask(t, s, taskID))
	// A second engagement finds the firing already handed over.
	s.handOffQueuedFirings(ctx, org, br, conv, readTask(t, s, taskID))

	rows, err := s.pendingFirings.ListForEntity(ctx, org, entityID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("firings = %d rows, err=%v, want 1", len(rows), err)
	}
	if rows[0].Status != workitem.StatusReady || rows[0].HandedNoteID == "" || rows[0].SkipReason != "" {
		t.Errorf("firing = %+v, want ready and naming the note", rows[0])
	}
	notes, err := s.stagedInjections.FlushPendingSystem(ctx, org, conv.ID)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("staged notes = %+v, want one", notes)
	}
	if n := notes[0]; n.ID != rows[0].HandedNoteID || n.Subtype != domain.MessageSubtypeInjectionEvent || n.Provenance.EventID != eventID ||
		!strings.Contains(n.Body, domain.EventGitHubPRCICheckFailed) {
		t.Errorf("note = %+v, want the event injection the firing names", n)
	}
}

// One event that queued a firing per trigger is told to the step once, and
// both firings name the one note.
func TestHandOffQueuedFirings_NotesAnEventOnce(t *testing.T) {
	s, database, br, conv, entityID, taskID, triggerID := handOffFixture(t, "step-two-triggers")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	eventID := queueFiring(t, s, database, entityID, taskID, triggerID)
	// A trigger names its own blueprint, so the second one borrows another
	// conversation's.
	seedConversation(t, database, "step-other-blueprint", "sess-other", "/tmp/wt-other")
	second := seedFakeTrigger(t, database, "step-other-blueprint")
	if _, _, err := s.pendingFirings.Enqueue(ctx, org, entityID, taskID, second, eventID, db.AgentClaimStamp{}); err != nil {
		t.Fatalf("enqueue second firing: %v", err)
	}

	s.handOffQueuedFirings(ctx, org, br, conv, readTask(t, s, taskID))

	rows, err := s.pendingFirings.ListForEntity(ctx, org, entityID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("firings = %d rows, err=%v, want 2", len(rows), err)
	}
	if rows[0].HandedNoteID == "" || rows[0].HandedNoteID != rows[1].HandedNoteID {
		t.Errorf("firings name notes %q and %q, want the same one", rows[0].HandedNoteID, rows[1].HandedNoteID)
	}
	if notes, err := s.stagedInjections.FlushPendingSystem(ctx, org, conv.ID); err != nil || len(notes) != 1 {
		t.Errorf("staged notes = %+v (err=%v), want one for the one event", notes, err)
	}
}

// A second firing for an event an earlier pass already handed over names that
// pass's note; the event is not staged twice.
func TestHandOffQueuedFirings_ALaterPassSharesTheEarlierNote(t *testing.T) {
	s, database, br, conv, entityID, taskID, triggerID := handOffFixture(t, "step-two-passes")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	eventID := queueFiring(t, s, database, entityID, taskID, triggerID)
	s.handOffQueuedFirings(ctx, org, br, conv, readTask(t, s, taskID))

	seedConversation(t, database, "step-other-blueprint", "sess-other", "/tmp/wt-other")
	second := seedFakeTrigger(t, database, "step-other-blueprint")
	if _, _, err := s.pendingFirings.Enqueue(ctx, org, entityID, taskID, second, eventID, db.AgentClaimStamp{}); err != nil {
		t.Fatalf("enqueue second firing: %v", err)
	}
	s.handOffQueuedFirings(ctx, org, br, conv, readTask(t, s, taskID))

	rows, err := s.pendingFirings.ListForEntity(ctx, org, entityID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("firings = %d rows, err=%v, want 2", len(rows), err)
	}
	if rows[0].HandedNoteID == "" || rows[0].HandedNoteID != rows[1].HandedNoteID {
		t.Errorf("firings name notes %q and %q, want the same one", rows[0].HandedNoteID, rows[1].HandedNoteID)
	}
	if notes, err := s.stagedInjections.FlushPendingSystem(ctx, org, conv.ID); err != nil || len(notes) != 1 {
		t.Errorf("staged notes = %+v (err=%v), want one for the one event", notes, err)
	}
}

// The run was briefed with the event it fired from, so a second trigger's
// firing for that event has nothing to tell it and is left alone.
func TestHandOffQueuedFirings_LeavesAFiringForTheRunsOwnEvent(t *testing.T) {
	s, database, br, conv, entityID, taskID, triggerID := handOffFixture(t, "step-own-event")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	eventID := queueFiring(t, s, database, entityID, taskID, triggerID)

	own := *br
	own.TriggeringEventID = eventID
	s.handOffQueuedFirings(ctx, org, &own, conv, readTask(t, s, taskID))

	requireStillQueued(t, s, entityID, conv.ID)
}

// A conversation whose blueprint has finished takes nothing: the firing is the
// worker's, to fire on the open task or to reroute.
func TestHandOffQueuedFirings_LeavesFiringsOnceTheBlueprintHasFinished(t *testing.T) {
	s, database, br, conv, entityID, taskID, triggerID := handOffFixture(t, "step-finished")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	queueFiring(t, s, database, entityID, taskID, triggerID)

	finished := *br
	finished.Status = domain.BlueprintRunStatusCompleted
	s.handOffQueuedFirings(ctx, org, &finished, conv, readTask(t, s, taskID))

	requireStillQueued(t, s, entityID, conv.ID)
}

// A firing queued before the run started was waiting for a run of its own
// when this one was fired ahead of it, and still is.
func TestHandOffQueuedFirings_LeavesFiringsQueuedBeforeTheRun(t *testing.T) {
	s, database, br, conv, entityID, taskID, triggerID := handOffFixture(t, "step-fifo")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	queueFiring(t, s, database, entityID, taskID, triggerID)

	later := *br
	later.StartedAt = time.Now().Add(time.Hour)
	s.handOffQueuedFirings(ctx, org, &later, conv, readTask(t, s, taskID))

	requireStillQueued(t, s, entityID, conv.ID)
}

// An event queued behind a close answers to that close, which is the firing
// worker's skip to make: the step is not told to act on it.
func TestHandOffQueuedFirings_LeavesFiringsOnAClosedTask(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()

	t.Run("closed", func(t *testing.T) {
		s, database, br, conv, entityID, taskID, triggerID := handOffFixture(t, "step-closed")
		queueFiring(t, s, database, entityID, taskID, triggerID)
		if _, err := s.tasks.CloseSystem(ctx, org, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed); err != nil {
			t.Fatalf("close task: %v", err)
		}
		s.handOffQueuedFirings(ctx, org, br, conv, readTask(t, s, taskID))
		requireStillQueued(t, s, entityID, conv.ID)
	})
}

// An event folded into an earlier step that concluded without reading it
// reaches the step starting now: the earlier step's note is withdrawn and the
// firing names the note staged for this one. A firing whose note was read is
// left for the firing worker to settle as injected.
func TestHandOffQueuedFirings_TakesOverAnEarlierStepsUnreadNote(t *testing.T) {
	for _, tc := range []struct {
		name string
		read bool
	}{
		{name: "unread"},
		{name: "read", read: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, database, br, conv, entityID, taskID, triggerID := handOffFixture(t, "step-next")
			ctx := context.Background()
			org := runmode.LocalDefaultOrgID
			stepIdx := 0
			dbtest.SeedConversation(t, database, domain.Conversation{
				ID:                 "step-earlier",
				TaskID:             taskID,
				PromptID:           "test-prompt",
				Status:             "completed",
				Model:              "claude-sonnet-4-6",
				BlueprintRunID:     br.ID,
				BlueprintStepIndex: &stepIdx,
			})

			// The router's fold into the earlier step: the event staged as
			// its note, and the firing it replaced held behind the note.
			eventID := seedFakeEvent(t, database)
			if _, err := database.Exec(`INSERT INTO task_events (task_id, event_id, kind) VALUES (?, ?, 'bumped')`, taskID, eventID); err != nil {
				t.Fatalf("seed task event: %v", err)
			}
			earlierNote, ok := s.stageInjection(ctx, org, "step-earlier", domain.MessageSubtypeInjectionEvent, domain.EventGitHubPRCICheckFailed,
				"the earlier step's note", domain.NoteProvenance{EventID: eventID, EventType: domain.EventGitHubPRCICheckFailed})
			if !ok {
				t.Fatal("stage the earlier step's note failed")
			}
			if _, err := s.pendingFirings.EnqueueHanded(ctx, org, entityID, taskID, triggerID, eventID, earlierNote, db.AgentClaimStamp{}); err != nil {
				t.Fatalf("hold the firing behind the note: %v", err)
			}
			if tc.read {
				if notes, err := s.stagedInjections.FlushPendingSystem(ctx, org, "step-earlier"); err != nil || len(notes) != 1 {
					t.Fatalf("read the earlier step's note: %+v, err=%v", notes, err)
				}
			}

			s.handOffQueuedFirings(ctx, org, br, conv, readTask(t, s, taskID))

			rows, err := s.pendingFirings.ListForEntity(ctx, org, entityID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("firings = %d rows, err=%v, want 1", len(rows), err)
			}
			f := rows[0]
			notes, err := s.stagedInjections.FlushPendingSystem(ctx, org, conv.ID)
			if err != nil {
				t.Fatalf("flush the starting step: %v", err)
			}
			if tc.read {
				if f.Status != workitem.StatusReady || f.HandedNoteID != earlierNote {
					t.Errorf("firing = %+v, want it still naming the read note %q", f, earlierNote)
				}
				if len(notes) != 0 {
					t.Errorf("starting step's notes = %+v, want none for an event already read", notes)
				}
				return
			}
			if f.Status != workitem.StatusReady || f.HandedNoteID == "" || f.HandedNoteID == earlierNote {
				t.Errorf("firing = %+v, want it ready and naming a note other than the earlier step's %q", f, earlierNote)
			}
			if len(notes) != 1 || notes[0].ID != f.HandedNoteID || notes[0].Provenance.EventID != eventID {
				t.Errorf("starting step's notes = %+v, want the one the firing names, for event %s", notes, eventID)
			}
			if left, err := s.stagedInjections.FlushPendingSystem(ctx, org, "step-earlier"); err != nil || len(left) != 0 {
				t.Errorf("earlier step's notes = %+v (err=%v), want its note withdrawn", left, err)
			}
		})
	}
}
