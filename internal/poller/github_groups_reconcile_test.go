package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestReconcileGitHubGroups_PrunesDeletedTeams pins the deletion-reconcile
// floor running on the GitHub poll cycle: a mapping whose GitHub team is no
// longer returned by GET /orgs/{org}/teams is pruned, while a still-present
// team's mapping survives.
func TestReconcileGitHubGroups_PrunesDeletedTeams(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/orgs/octo/teams"):
			// "legacy" was deleted on GitHub — only "backend" remains.
			_, _ = w.Write([]byte(`[{"slug":"backend","name":"Backend"}]`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	team := runmode.LocalDefaultTeamID

	if err := stores.TeamGitHubGroups.SetForTeam(ctx, team, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		{OrgLogin: "octo", TeamSlug: "backend"},
		{OrgLogin: "octo", TeamSlug: "legacy"},
	}); err != nil {
		t.Fatalf("seed mappings: %v", err)
	}
	// The same org login on another host is another organization: the teams
	// listed on this host say nothing about its teams, so its mapping survives.
	const otherHost = "https://ghe.example.com"
	if err := stores.TeamGitHubGroups.SetForTeam(ctx, team, otherHost, []domain.TeamGitHubGroup{
		{OrgLogin: "octo", TeamSlug: "legacy"},
	}); err != nil {
		t.Fatalf("seed other host's mapping: %v", err)
	}

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	m := &Manager{
		database:     database,
		pub:          busPublisher{bus: bus},
		githubGroups: stores.TeamGitHubGroups,
		resolver:     &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
	}

	m.reconcileGitHubGroups(ctx, org, dbtest.TestGitHubHost, []string{"octo/repo"})

	got, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	if len(got) != 1 || got[0].TeamSlug != "backend" {
		t.Errorf("after reconcile, mappings = %+v; want only octo/backend (legacy pruned)", got)
	}
	other, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, otherHost)
	if err != nil {
		t.Fatalf("ListForTeam on the other host: %v", err)
	}
	if len(other) != 1 || other[0].TeamSlug != "legacy" {
		t.Errorf("after reconcile, the other host's mappings = %+v; want octo/legacy untouched", other)
	}
}

// TestReconcileGitHubGroups_EmptyFetchDoesNotPrune pins the safety guard: an
// empty team list (a user account, or a credential that can see no teams) is
// ambiguous, so the reconcile must NOT wipe the org's mappings.
func TestReconcileGitHubGroups_EmptyFetchDoesNotPrune(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every page comes back empty.
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	team := runmode.LocalDefaultTeamID

	if err := stores.TeamGitHubGroups.SetForTeam(ctx, team, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		{OrgLogin: "octo", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("seed mappings: %v", err)
	}

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	m := &Manager{
		database:     database,
		pub:          busPublisher{bus: bus},
		githubGroups: stores.TeamGitHubGroups,
		resolver:     &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
	}

	m.reconcileGitHubGroups(ctx, org, dbtest.TestGitHubHost, []string{"octo/repo"})

	got, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("after empty-fetch reconcile, mappings = %+v; want octo/backend preserved (empty fetch must not prune)", got)
	}
}
