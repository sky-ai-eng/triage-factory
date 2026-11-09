package routing

import (
	"context"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/tracker"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestUntrackedPR_MergeClosesItsTasksOnTheFloor: a pull request in a
// repository no team tracks keeps the tasks it was given while a team tracked
// it. The poll refreshes it once its last refresh is older than the refresh
// floor, and not on the cycles before, and the refresh that sees it merged
// closes those tasks and the entity.
func TestUntrackedPR_MergeClosesItsTasksOnTheFloor(t *testing.T) {
	ctx := context.Background()
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "octo/repo#9")
	seedSnapshot(t, database, entityID, prSnapshotJSON(t, "OPEN"))
	gh := newFakeGitHub(t, "MERGED")

	st := sqlitestore.New(database)
	tr := tracker.New(database, noopPublisher{}, st.Tasks, st.Entities, st.Repos, st.JiraProjects, st.Deliverables, st.ExternalActions, st.EventQueue, st.PRMergeability, runmode.LocalDefaultOrgID)
	poll := func() {
		t.Helper()
		// No repository is tracked, so the pass lists none.
		if _, _, err := tr.RefreshGitHub(ctx, "https://github.com", ghclient.NewClient(gh.srv.URL, "tok"), domain.GitHubAccount{}, nil, tracker.ReachAll(), nil); err != nil {
			t.Fatalf("RefreshGitHub: %v", err)
		}
		if err := r.drainEventQueue(ctx); err != nil {
			t.Fatalf("drainEventQueue: %v", err)
		}
	}

	// The snapshot was stored just now, so the floor has not passed.
	poll()
	if n := gh.refreshes.Load(); n != 0 {
		t.Fatalf("refreshes = %d, want 0: the pull request was refreshed less than the floor ago", n)
	}
	if n := activeTaskCount(t, database, entityID); n != 1 {
		t.Fatalf("active tasks = %d, want 1", n)
	}

	if _, err := database.Exec(`UPDATE entities SET last_polled_at = ? WHERE id = ?`, time.Now().Add(-time.Hour), entityID); err != nil {
		t.Fatalf("age the last refresh: %v", err)
	}
	poll()
	if n := gh.refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d, want 1: the floor has passed", n)
	}
	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Errorf("active tasks = %d, want 0: the merge closes the task %s", n, taskID)
	}
	if got := entityState(t, database, entityID); got != "closed" {
		t.Errorf("entity state = %q, want closed", got)
	}
}
