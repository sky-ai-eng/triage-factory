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
// closes those tasks and the entity. That holds for a RefreshGitHub pass whose
// credential reads the repository, and for the RefreshHeldGitHub pass the
// poller runs under a credential no RefreshGitHub pass runs under: an
// installation whose grant holds the repository and no tracked one.
func TestUntrackedPR_MergeClosesItsTasksOnTheFloor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh func(ctx context.Context, tr *tracker.Tracker, client *ghclient.Client) error
	}{
		{name: "RefreshGitHub", refresh: func(ctx context.Context, tr *tracker.Tracker, client *ghclient.Client) error {
			// No repository is tracked, so the pass lists none.
			_, _, err := tr.RefreshGitHub(ctx, "https://github.com", client, domain.GitHubAccount{}, nil, tracker.ReachAll(), nil)
			return err
		}},
		{name: "RefreshHeldGitHub", refresh: func(ctx context.Context, tr *tracker.Tracker, client *ghclient.Client) error {
			grant := tracker.ReachOfGrant([]ghclient.UserRepo{{FullName: "octo/repo"}})
			_, err := tr.RefreshHeldGitHub(ctx, "https://github.com", client, domain.GitHubAccount{}, grant, nil)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { testUntrackedMergeCloses(t, tc.refresh) })
	}
}

func testUntrackedMergeCloses(t *testing.T, refresh func(ctx context.Context, tr *tracker.Tracker, client *ghclient.Client) error) {
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
		if err := refresh(ctx, tr, ghclient.NewClient(gh.srv.URL, "tok")); err != nil {
			t.Fatalf("refresh: %v", err)
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
