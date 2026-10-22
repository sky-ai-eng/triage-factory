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

var octoBackend = domain.TeamGitHubGroup{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "octo", TeamSlug: "backend"}

func seedGroups(t *testing.T, database *sql.DB, host string, groups ...domain.TeamGitHubGroup) {
	t.Helper()
	if err := sqlitestore.New(database).TeamGitHubGroups.SetForTeam(context.Background(), runmode.LocalDefaultTeamID, host, groups); err != nil {
		t.Fatalf("seed mappings on %s: %v", host, err)
	}
}

// reconcileGroups runs reconcileGitHubGroups against a GitHub that lists teams
// at GET /orgs/octo/teams and answers GET /orgs/octo with org (a 403 when org
// is empty), and returns the team's mappings on the test host afterwards.
func reconcileGroups(t *testing.T, database *sql.DB, teams, org string) []domain.TeamGitHubGroup {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/orgs/octo/teams"):
			_, _ = w.Write([]byte(teams))
		case strings.HasSuffix(r.URL.Path, "/orgs/octo") && org != "":
			_, _ = w.Write([]byte(org))
		case strings.HasSuffix(r.URL.Path, "/orgs/octo"):
			http.Error(w, "nope", http.StatusForbidden)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	bus := eventbus.New()
	t.Cleanup(bus.Close)
	groups := sqlitestore.New(database).TeamGitHubGroups
	m := &Manager{
		database:     database,
		pub:          busPublisher{bus: bus},
		githubGroups: groups,
		resolver:     &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
	}
	m.reconcileGitHubGroups(context.Background(), runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, []string{"octo/repo"})
	got, err := groups.ListForTeam(context.Background(), runmode.LocalDefaultTeamID, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	return got
}

// A mapping whose GitHub team id GET /orgs/{org}/teams no longer returns is
// pruned, while a still-present team's mapping survives and takes the names
// GitHub gives it now. The same ids on another host name other teams, so that
// host's mapping survives.
func TestReconcileGitHubGroups_PrunesDeletedTeams(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	database := newMigratedSQLiteForPoller(t)
	legacy := domain.TeamGitHubGroup{GitHubTeamID: "12", GitHubOrgID: "9", OrgLogin: "octo", TeamSlug: "legacy"}
	seedGroups(t, database, dbtest.TestGitHubHost, octoBackend, legacy)
	const otherHost = "https://ghe.example.com"
	seedGroups(t, database, otherHost, legacy)

	// Legacy was deleted, backend renamed, and the organization renamed itself:
	// the owner the repositories still show is its old login, which GitHub
	// redirects.
	got := reconcileGroups(t, database, `[{"id":11,"slug":"server","name":"Server"}]`, `{"id":9,"login":"octo-corp"}`)
	want := []domain.TeamGitHubGroup{{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "octo-corp", TeamSlug: "server"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after reconcile, mappings = %+v; want %+v (legacy pruned, backend renamed)", got, want)
	}
	other, err := sqlitestore.New(database).TeamGitHubGroups.ListForTeam(context.Background(), runmode.LocalDefaultTeamID, otherHost)
	if err != nil {
		t.Fatalf("ListForTeam on the other host: %v", err)
	}
	if len(other) != 1 || other[0].TeamSlug != "legacy" {
		t.Errorf("after reconcile, the other host's mappings = %+v; want octo/legacy untouched", other)
	}
}

// The one place a mapping is identified by its handle: a local row stored
// before GitHub's ids were recorded takes them from the organization's
// listing, and is not pruned while it has none.
func TestReconcileGitHubGroups_IdentifiesARowStoredWithoutIDs(t *testing.T) {
	database := newMigratedSQLiteForPoller(t)
	for _, slug := range []string{"backend", "unknown"} {
		if _, err := database.Exec(
			`INSERT INTO team_github_groups (team_id, host, github_org_login, github_team_slug) VALUES (?, ?, 'octo', ?)`,
			runmode.LocalDefaultTeamID, dbtest.TestGitHubHost, slug,
		); err != nil {
			t.Fatalf("seed row without ids: %v", err)
		}
	}

	got := reconcileGroups(t, database, `[{"id":11,"slug":"backend","name":"Backend"}]`, `{"id":9,"login":"octo"}`)
	if want := []domain.TeamGitHubGroup{octoBackend, {OrgLogin: "octo", TeamSlug: "unknown"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("after reconcile, mappings = %+v; want %+v", got, want)
	}
}

// An empty team list (a user account, or a credential that can see no teams)
// is ambiguous, and a team list for an organization whose own read failed has
// no organization id to match the mappings by: neither prunes anything.
func TestReconcileGitHubGroups_AmbiguousFetchDoesNotPrune(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	for name, c := range map[string]struct{ teams, org string }{
		"empty team list":         {`[]`, `{"id":9,"login":"octo"}`},
		"unreadable organization": {`[{"id":13,"slug":"other","name":"Other"}]`, ""},
	} {
		database := newMigratedSQLiteForPoller(t)
		seedGroups(t, database, dbtest.TestGitHubHost, octoBackend)
		if got := reconcileGroups(t, database, c.teams, c.org); len(got) != 1 {
			t.Errorf("%s: mappings = %+v; want octo/backend preserved", name, got)
		}
	}
}
