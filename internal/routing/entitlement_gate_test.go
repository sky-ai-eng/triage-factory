package routing

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// testGateFeature is the synthetic feature these tests gate a real source
// (jira, or github for the rederive case) behind — no real gated event
// source exists yet (TFAC-524 is the generic mechanism; TFAC-509/510 will
// call GateEventSource("slack", FeatureSlack)).
const testGateFeature = entitlements.Feature("test-gate")

// TestHandleEvent_GatedSource_RecordsButCreatesNoTask pins the Part 6 router
// freeze: with jira gated off, HandleEvent records the event (the
// append-only log stays an honest record of what happened) but creates no
// task — the close phase, task creation, and trigger firing all skip.
// Granting the feature restores normal task creation on the next event,
// mirroring TestAssigneeCentric_LocalUserAssignee_OneTeam's dispatch shape.
func TestHandleEvent_GatedSource_RecordsButCreatesNoTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)
	seedJiraUserOnTeam(t, database, runmode.LocalDefaultTeamID, "acct-aidan", "aidan")
	seedSystemJiraRule(t, database, runmode.LocalDefaultTeamID, domain.EventJiraIssueAssigned)

	entityID := jiraEntity(t, database, "SKY-gated")

	entitlements.GateEventSource("jira", testGateFeature)
	t.Cleanup(entitlements.Reset)

	emitJiraAssigned(reviewRouter(database), entityID, "acct-aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("gated source: got %d active tasks, want 0 (task creation must be frozen)", len(active))
	}

	var eventCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM events WHERE entity_id = ?`, entityID).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("events recorded for entity = %d, want 1 (the append-only log must still record a frozen event)", eventCount)
	}

	// Grant the feature — normal task creation resumes on the next event.
	entitlements.RegisterProvider(entitlements.Static(testGateFeature))

	emitJiraAssigned(reviewRouter(database), entityID, "acct-aidan")

	active, err = testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks after grant: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("after granting the feature: got %d active tasks, want 1", len(active))
	}
	if teamIDValue(&active[0]) != runmode.LocalDefaultTeamID {
		t.Errorf("owner = %q, want the one team %q", teamIDValue(&active[0]), runmode.LocalDefaultTeamID)
	}
}

// TestHandleEvent_UngatedSource_Unaffected is the negative control: gating
// jira must not touch github routing at all (the source-prefix scoping in
// Part 1).
func TestHandleEvent_UngatedSource_Unaffected(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	entitlements.GateEventSource("jira", testGateFeature)
	t.Cleanup(entitlements.Reset)

	entityID := reviewEntity(t, database, "owner/repo#gate")
	emitCI(reviewRouter(database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("github event with jira gated off: got %d active tasks, want 1 (unaffected)", len(active))
	}
}

// TestReDeriveTask_GatedSource_FiresNothing pins the rederive.go half of the
// Part 6 freeze: a task whose event type's source is gated off must not
// fire its deferred trigger during the post-scoring re-derive pass, even
// though the score crosses the trigger's threshold and every other gate is
// open.
func TestReDeriveTask_GatedSource_FiresNothing(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	entitlements.GateEventSource("github", testGateFeature)
	t.Cleanup(entitlements.Reset)

	if err := updateScores(t, database, []domain.TaskScoreUpdate{{
		ID: taskID, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "test",
	}}); err != nil {
		t.Fatalf("update scores: %v", err)
	}

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), sqlitestore.New(database).Conversations, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings,
		sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())
	router.ReDeriveAfterScoring(runmode.LocalDefaultOrgID, []string{taskID})

	if stub.calls != 0 {
		t.Errorf("gated task delegated (%d calls), want 0 (rederive must freeze a task on a gated-off event type)", stub.calls)
	}
	task, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if task.Status != "queued" {
		t.Errorf("Status = %q, want queued (gated task must not be promoted)", task.Status)
	}
}
