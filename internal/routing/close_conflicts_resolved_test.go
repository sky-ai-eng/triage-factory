package routing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A pull request whose merge conflicts clear closes its conflicts task, gated
// on its stored mergeability still reading MERGEABLE, and held until the run
// ends when the run's own push resolved them.

// seedConflictsTaskOnEntity drains a conflicts event so the entity ends up
// with exactly one live task, and returns it.
func seedConflictsTaskOnEntity(t *testing.T, r *Router, database *sql.DB, sourceID string) (entityID, taskID string) {
	t.Helper()
	seedMatchAllRule(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRConflicts)
	entityID = newEntity(t, database, sourceID)
	enqueueRoutedEvent(t, database, entityID, domain.EventGitHubPRConflicts, "", events.GitHubPRConflictsMetadata{
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

// seedMergeability writes the pull request's stored mergeability, standing
// in for the probe worker's answer.
func seedMergeability(t *testing.T, database *sql.DB, entityID, mergeable string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO pull_request_mergeability (entity_id, mergeable, mergeable_head_sha, checked_at)
		VALUES (?, ?, 'head', ?)
		ON CONFLICT (entity_id) DO UPDATE SET mergeable = excluded.mergeable
	`, entityID, mergeable, time.Now().UTC()); err != nil {
		t.Fatalf("seed mergeability: %v", err)
	}
}

// enqueueConflictsResolved durably enqueues a conflicts_resolved event
// observed at headSHA.
func enqueueConflictsResolved(t *testing.T, database *sql.DB, entityID, headSHA string) {
	t.Helper()
	enqueueRoutedEvent(t, database, entityID, domain.EventGitHubPRConflictsResolved, "", events.GitHubPRConflictsResolvedMetadata{
		Author: "aidan", Repo: "owner/repo", PRNumber: 1, HeadSHA: headSHA,
	})
}

func TestCloseConflictsResolved_ClosesTheConflictsTask(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	entityID, taskID := seedConflictsTaskOnEntity(t, r, database, "owner/repo#resolved")

	seedMergeability(t, database, entityID, domain.MergeableMergeable)
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

	seedMergeability(t, database, entityID, domain.MergeableConflicting)
	enqueueConflictsResolved(t, database, entityID, "after")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the resolution: %v", err)
	}

	if status, _, _ := taskRow(t, database, taskID); status == "done" {
		t.Error("the conflicts task closed while the stored mergeability says CONFLICTING")
	}
}
