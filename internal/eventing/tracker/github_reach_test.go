package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestGitHubReach_Holds(t *testing.T) {
	grant := ReachOfGrant([]ghclient.UserRepo{
		{ID: 101, FullName: "Acme/One"},
		{ID: 202, FullName: "acme/reused"},
	})
	cases := []struct {
		name  string
		reach GitHubReach
		repo  *domain.Repository
		want  bool
	}{
		{"a row is matched by its id", grant, &domain.Repository{Owner: "acme", Repo: "renamed-since", ExternalID: "101"}, true},
		{"a row with an id the grant lists under its name with another id is not held", grant, &domain.Repository{Owner: "acme", Repo: "reused", ExternalID: "999"}, false},
		{"a row with no id is matched by its name, folded", grant, &domain.Repository{Owner: "acme", Repo: "one"}, true},
		{"a row with no id the grant does not name is not held", grant, &domain.Repository{Owner: "acme", Repo: "other"}, false},
		{"a nil row is not held by a grant", grant, nil, false},
		{"the zero reach holds nothing", GitHubReach{}, &domain.Repository{Owner: "acme", Repo: "one", ExternalID: "101"}, false},
		{"a personal token's reach holds a nil row", ReachAll(), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.reach.holds(tc.repo); got != tc.want {
				t.Errorf("holds = %v, want %v", got, tc.want)
			}
		})
	}
}

// untrackedRefreshFixture is octo/repo#7, in a repository no team tracks,
// stored with snapshot, against a fake GitHub that answers every batch
// refresh with node and counts the batches.
type untrackedRefreshFixture struct {
	tr        *Tracker
	client    *ghclient.Client
	entityID  string
	refreshes *atomic.Int32
	age       func(time.Duration)
}

func newUntrackedRefreshFixture(t *testing.T, snapshot, node string) untrackedRefreshFixture {
	t.Helper()
	refreshes := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		refreshes.Add(1)
		_, _ = w.Write([]byte(`{"data":{"nodes":[` + node + `]}}`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	e, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", "https://github.com", "octo/repo#7", "", "pr", "PR 7", "https://github.com/octo/repo/pull/7")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, org, e.ID, snapshot, e.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}
	return untrackedRefreshFixture{
		tr:        New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Deliverables, stores.ExternalActions, stores.EventQueue, stores.PRMergeability, org),
		client:    ghclient.NewClient(srv.URL, "tok"),
		entityID:  e.ID,
		refreshes: refreshes,
		age: func(d time.Duration) {
			t.Helper()
			if _, err := database.Exec(`UPDATE entities SET last_polled_at = ? WHERE id = ?`, time.Now().Add(-d), e.ID); err != nil {
				t.Fatalf("age last_polled_at: %v", err)
			}
		},
	}
}

func (f untrackedRefreshFixture) poll(t *testing.T) {
	t.Helper()
	if _, _, err := f.tr.RefreshGitHub(context.Background(), "https://github.com", f.client, domain.GitHubAccount{}, nil, ReachAll(), nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}
}

const untrackedOpenSnapshot = `{"node_id":"PR_node7","number":7,"repo":"octo/repo","state":"OPEN","author":"aidan","head_sha":"sha7","updated_at":"2026-10-01T08:00:00Z","check_runs":[]}`

// An untracked pull request GitHub answers null for is stamped as polled, so
// it waits for the floor again instead of going out on every cycle.
func TestRefreshGitHub_UntrackedNullAnswerWaitsForTheFloor(t *testing.T) {
	f := newUntrackedRefreshFixture(t, untrackedOpenSnapshot, "null")
	f.age(time.Hour)
	f.poll(t)
	if n := f.refreshes.Load(); n != 1 {
		t.Fatalf("refreshes after the floor passed = %d, want 1", n)
	}
	f.poll(t)
	if n := f.refreshes.Load(); n != 1 {
		t.Errorf("refreshes on the next cycle = %d, want still 1: the null answer restarts the floor", n)
	}
}

// An untracked pull request whose stored snapshot is already terminal on an
// active entity is refreshed on every cycle, floor or not: that refresh emits
// the close obligation the lost close owes.
func TestRefreshGitHub_UntrackedTerminalSnapshotRefreshesEveryCycle(t *testing.T) {
	merged := strings.Replace(untrackedOpenSnapshot, `"state":"OPEN"`, `"state":"MERGED","merged":true`, 1)
	node := `{"id":"PR_node7","number":7,"title":"PR 7","author":{"login":"aidan"},"state":"MERGED","merged":true,
		"url":"https://github.com/octo/repo/pull/7","repository":{"nameWithOwner":"octo/repo"},
		"createdAt":"2026-09-01T00:00:00Z","updatedAt":"2026-10-01T08:00:00Z"}`
	f := newUntrackedRefreshFixture(t, merged, node)
	f.poll(t)
	if n := f.refreshes.Load(); n != 1 {
		t.Errorf("refreshes = %d, want 1 although the last refresh was just now", n)
	}
}
