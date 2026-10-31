package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A pull request whose merge conflicts clear closes its conflicts task, gated
// on the stored snapshot still reading MERGEABLE, and held until the run ends
// when the run's own push resolved them.

// seedConflictsTaskOnEntity drains a conflicts event so the entity ends up
// with exactly one live task, and returns it.
func seedConflictsTaskOnEntity(t *testing.T, r *Router, database *sql.DB, sourceID string) (entityID, taskID string) {
	t.Helper()
	seedMatchAllRule(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRConflicts)
	entityID = newEntity(t, database, sourceID)
	enqueueMergeableEvent(t, database, entityID, domain.EventGitHubPRConflicts, events.GitHubPRConflictsMetadata{
		Author: "aidan", Repo: "owner/repo", PRNumber: 1, HeadSHA: "before",
	})
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the conflicts event: %v", err)
	}
	live, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(live) != 1 || live[0].EventType != domain.EventGitHubPRConflicts {
		t.Fatalf("setup: active tasks = %+v (err %v), want one conflicts task", live, err)
	}
	return entityID, live[0].ID
}

func enqueueMergeableEvent(t *testing.T, database *sql.DB, entityID, eventType string, metadata any) {
	t.Helper()
	eid := entityID
	meta, _ := json.Marshal(metadata)
	if _, err := sqlitestore.New(database).EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EntityID:     &eid,
		EventType:    eventType,
		MetadataJSON: string(meta),
	}, ""); err != nil {
		t.Fatalf("enqueue %s: %v", eventType, err)
	}
}

// enqueueConflictsResolved durably enqueues a conflicts_resolved event
// observed at headSHA.
func enqueueConflictsResolved(t *testing.T, database *sql.DB, entityID, headSHA string) {
	t.Helper()
	enqueueMergeableEvent(t, database, entityID, domain.EventGitHubPRConflictsResolved, events.GitHubPRConflictsResolvedMetadata{
		Author: "aidan", Repo: "owner/repo", PRNumber: 1, HeadSHA: headSHA,
	})
}

// seedPRMergeable writes a PR snapshot whose mergeable is the given value,
// standing in for the tracker's post-refresh commit.
func seedPRMergeable(t *testing.T, database *sql.DB, entityID, mergeable string) {
	t.Helper()
	snap, _ := json.Marshal(domain.PRSnapshot{Mergeable: mergeable})
	entities := sqlitestore.New(database).Entities
	ent, err := entities.Get(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || ent == nil {
		t.Fatalf("read entity %s: ent=%v err=%v", entityID, ent, err)
	}
	if ok, err := entities.UpdateSnapshotCASSystem(t.Context(), runmode.LocalDefaultOrgID, entityID, string(snap), ent.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}
}

func TestCloseConflictsResolved_ClosesTheConflictsTask(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	entityID, taskID := seedConflictsTaskOnEntity(t, r, database, "owner/repo#resolved")

	seedPRMergeable(t, database, entityID, "MERGEABLE")
	enqueueConflictsResolved(t, database, entityID, "after")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the resolution: %v", err)
	}

	if status, reason, eventType := taskRow(t, database, taskID); status != "done" || reason != "auto_closed_by_event" || eventType != domain.EventGitHubPRConflictsResolved {
		t.Errorf("task = (%q, %q, %q), want closed by %s", status, reason, eventType, domain.EventGitHubPRConflictsResolved)
	}
}

func TestCloseConflictsResolved_ConflictingAgainClosesNothing(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	entityID, taskID := seedConflictsTaskOnEntity(t, r, database, "owner/repo#conflicting-again")

	seedPRMergeable(t, database, entityID, "CONFLICTING")
	enqueueConflictsResolved(t, database, entityID, "after")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the resolution: %v", err)
	}

	if status, _, _ := taskRow(t, database, taskID); status == "done" {
		t.Error("the conflicts task closed while the snapshot says CONFLICTING")
	}
}

func TestCloseConflictsResolved_RunsOwnPushDefersTheClose(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp
	entityID, taskID := seedConflictsTaskOnEntity(t, r, database, "owner/repo#own-resolution")
	runID, convID := seedRunOnTask(t, database, taskID, "running", "")
	recordAgentPush(t, database, convID, pushedSHA)

	seedPRMergeable(t, database, entityID, "MERGEABLE")
	enqueueConflictsResolved(t, database, entityID, pushedSHA)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the resolution: %v", err)
	}

	if status, _, _ := taskRow(t, database, taskID); status == "done" {
		t.Fatal("the conflicts run's task closed under it")
	}
	if eventID, deferredRun := taskDeferral(t, database, taskID); eventID == "" || deferredRun != runID {
		t.Errorf("deferral = (%q, %q), want the resolution and run %s", eventID, deferredRun, runID)
	}
	if slices.Contains(sp.stoppedIDs(), convID) || cancelRequested(t, database, runID) {
		t.Error("the conflicts run was stopped")
	}
}
