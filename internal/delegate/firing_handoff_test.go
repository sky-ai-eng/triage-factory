package delegate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
// as the note a folded-in event is, and the firing is settled injected.
func TestHandOffQueuedFirings_HandsTheEventToTheStartingStep(t *testing.T) {
	s, database, br, conv, entityID, taskID, triggerID := handOffFixture(t, "step-next")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	eventID := queueFiring(t, s, database, entityID, taskID, triggerID)

	s.handOffQueuedFirings(ctx, org, br, conv, taskID)
	// A second engagement finds nothing left to hand over.
	s.handOffQueuedFirings(ctx, org, br, conv, taskID)

	rows, err := s.pendingFirings.ListForEntity(ctx, org, entityID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("firings = %d rows, err=%v, want 1", len(rows), err)
	}
	if rows[0].Status != workitem.StatusDone || rows[0].SkipReason != domain.PendingFiringSkipInjected {
		t.Errorf("firing = %+v, want done with skip reason injected", rows[0])
	}
	notes, err := s.stagedInjections.FlushPendingSystem(ctx, org, conv.ID)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("staged notes = %+v, want one", notes)
	}
	if n := notes[0]; n.Subtype != domain.MessageSubtypeInjectionEvent || n.Provenance.EventID != eventID ||
		!strings.Contains(n.Body, domain.EventGitHubPRCICheckFailed) {
		t.Errorf("note = %+v, want the event injection naming the event", n)
	}
	var kind string
	if err := database.QueryRow(`SELECT kind FROM task_events WHERE task_id = ? AND event_id = ?`, taskID, eventID).Scan(&kind); err != nil || kind != "injected" {
		t.Errorf("task event kind = %q (err=%v), want injected", kind, err)
	}
}

// One event that queued a firing per trigger is told to the step once, and
// both firings settle.
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

	s.handOffQueuedFirings(ctx, org, br, conv, taskID)

	rows, err := s.pendingFirings.ListForEntity(ctx, org, entityID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("firings = %d rows, err=%v, want 2", len(rows), err)
	}
	for _, r := range rows {
		if r.Status != workitem.StatusDone || r.SkipReason != domain.PendingFiringSkipInjected {
			t.Errorf("firing = %+v, want done with skip reason injected", r)
		}
	}
	if notes, err := s.stagedInjections.FlushPendingSystem(ctx, org, conv.ID); err != nil || len(notes) != 1 {
		t.Errorf("staged notes = %+v (err=%v), want one for the one event", notes, err)
	}
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
	s.handOffQueuedFirings(ctx, org, &finished, conv, taskID)

	rows, err := s.pendingFirings.ListForEntity(ctx, org, entityID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("firings = %d rows, err=%v, want 1", len(rows), err)
	}
	if rows[0].Status != workitem.StatusReady {
		t.Errorf("firing = %+v, want it still ready", rows[0])
	}
	if notes, _ := s.stagedInjections.FlushPendingSystem(ctx, org, conv.ID); len(notes) != 0 {
		t.Errorf("staged notes = %+v, want none", notes)
	}
}
