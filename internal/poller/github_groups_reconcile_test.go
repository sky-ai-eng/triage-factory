package poller

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// reconcileManager is a Manager wired for reconcileGitHubGroups alone: the
// mapping store, and every GitHub call answered by srv.
func reconcileManager(t *testing.T, database *sql.DB, srv *httptest.Server) *Manager {
	t.Helper()
	bus := eventbus.New()
	t.Cleanup(bus.Close)
	return &Manager{
		database:     database,
		pub:          busPublisher{bus: bus},
		githubGroups: sqlitestore.New(database).TeamGitHubGroups,
		resolver:     &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
	}
}

// TestReconcileGitHubGroups_PrunesDeletedTeams pins the deletion reconcile
// running on the GitHub poll cycle: a mapping whose GitHub team id is no
// longer returned by GET /orgs/{org}/teams is pruned, while a still-present
// team's mapping survives and takes the names GitHub gives it now.
func TestReconcileGitHubGroups_PrunesDeletedTeams(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/orgs/octo/teams"):
			// "legacy" (12) was deleted on GitHub, and backend (11) renamed.
			_, _ = w.Write([]byte(`[{"id":11,"slug":"server","name":"Server"}]`))
		case strings.HasSuffix(r.URL.Path, "/orgs/octo"):
			// The organization renamed itself too: the owner the repositories
			// still show is its old login, which GitHub redirects.
			_, _ = w.Write([]byte(`{"id":9,"login":"octo-corp"}`))
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
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "octo", TeamSlug: "backend"},
		{GitHubTeamID: "12", GitHubOrgID: "9", OrgLogin: "octo", TeamSlug: "legacy"},
	}); err != nil {
		t.Fatalf("seed mappings: %v", err)
	}
	// The same ids on another host name other teams: the teams listed on
	// this host say nothing about them, so the mapping survives.
	const otherHost = "https://ghe.example.com"
	if err := stores.TeamGitHubGroups.SetForTeam(ctx, team, otherHost, []domain.TeamGitHubGroup{
		{GitHubTeamID: "12", GitHubOrgID: "9", OrgLogin: "octo", TeamSlug: "legacy"},
	}); err != nil {
		t.Fatalf("seed other host's mapping: %v", err)
	}

	reconcileManager(t, database, srv).reconcileGitHubGroups(ctx, org, dbtest.TestGitHubHost, []string{"octo/repo"})

	got, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	want := []domain.TeamGitHubGroup{{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "octo-corp", TeamSlug: "server"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after reconcile, mappings = %+v; want %+v (legacy pruned, backend renamed)", got, want)
	}
	other, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, otherHost)
	if err != nil {
		t.Fatalf("ListForTeam on the other host: %v", err)
	}
	if len(other) != 1 || other[0].TeamSlug != "legacy" {
		t.Errorf("after reconcile, the other host's mappings = %+v; want octo/legacy untouched", other)
	}
}

// TestReconcileGitHubGroups_IdentifiesARowStoredWithoutIDs pins the one place
// a mapping is identified by its handle: a local row stored before GitHub's
// ids were recorded takes them from the organization's listing, and is not
// pruned while it has none.
func TestReconcileGitHubGroups_IdentifiesARowStoredWithoutIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/orgs/octo/teams"):
			_, _ = w.Write([]byte(`[{"id":11,"slug":"backend","name":"Backend"}]`))
		case strings.HasSuffix(r.URL.Path, "/orgs/octo"):
			_, _ = w.Write([]byte(`{"id":9,"login":"octo"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	team := runmode.LocalDefaultTeamID
	for _, slug := range []string{"backend", "unknown"} {
		if _, err := database.Exec(
			`INSERT INTO team_github_groups (team_id, host, github_org_login, github_team_slug) VALUES (?, ?, 'octo', ?)`,
			team, dbtest.TestGitHubHost, slug,
		); err != nil {
			t.Fatalf("seed row without ids: %v", err)
		}
	}

	reconcileManager(t, database, srv).reconcileGitHubGroups(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, []string{"octo/repo"})

	got, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	want := []domain.TeamGitHubGroup{
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "octo", TeamSlug: "backend"},
		{OrgLogin: "octo", TeamSlug: "unknown"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after reconcile, mappings = %+v; want %+v", got, want)
	}
}

// TestReconcileGitHubGroups_EmptyFetchDoesNotPrune pins the safety guard: an
// empty team list (a user account, or a credential that can see no teams) is
// ambiguous, so the reconcile must NOT wipe the org's mappings.
func TestReconcileGitHubGroups_EmptyFetchDoesNotPrune(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/orgs/octo") {
			_, _ = w.Write([]byte(`{"id":9,"login":"octo"}`))
			return
		}
		// Every page of the team list comes back empty.
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	team := runmode.LocalDefaultTeamID

	if err := stores.TeamGitHubGroups.SetForTeam(ctx, team, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "octo", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("seed mappings: %v", err)
	}

	reconcileManager(t, database, srv).reconcileGitHubGroups(ctx, org, dbtest.TestGitHubHost, []string{"octo/repo"})

	got, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("after empty-fetch reconcile, mappings = %+v; want octo/backend preserved (empty fetch must not prune)", got)
	}
}

// TestReconcileGitHubGroups_UnreadableOrgDoesNotPrune: a team list GitHub
// returned, for an organization whose own read then failed, prunes nothing —
// the mappings are matched by the organization's id, which the failed read
// never produced.
func TestReconcileGitHubGroups_UnreadableOrgDoesNotPrune(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/orgs/octo/teams") {
			_, _ = w.Write([]byte(`[{"id":13,"slug":"other","name":"Other"}]`))
			return
		}
		http.Error(w, "nope", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	team := runmode.LocalDefaultTeamID
	if err := stores.TeamGitHubGroups.SetForTeam(ctx, team, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "octo", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("seed mappings: %v", err)
	}

	reconcileManager(t, database, srv).reconcileGitHubGroups(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, []string{"octo/repo"})

	got, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("after an unreadable organization, mappings = %+v; want octo/backend preserved", got)
	}
}
