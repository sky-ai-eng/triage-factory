package poller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// otherGitHubHost is a host the org polled before its GitHub base URL named the
// one it has now. The org in these tests has no base URL set, so its current
// host is the default (db.EffectiveGitHubHost("")).
const otherGitHubHost = "https://ghe.example.com"

// seedGitHubPR creates an active pull request entity on host with a stored
// snapshot.
func seedGitHubPR(t *testing.T, stores dbpkg.Stores, host, repo string, number int) *domain.Entity {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	sid := repo + "#" + strconv.Itoa(number)
	e, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", host, sid, "", "pr", "seeded", "")
	if err != nil {
		t.Fatalf("seed %s on %s: %v", sid, host, err)
	}
	snap, _ := json.Marshal(domain.PRSnapshot{NodeID: "PR_" + host + sid, Number: number, Repo: repo, Title: "seeded", Author: "alice", State: "OPEN"})
	if ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, org, e.ID, string(snap), e.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot for %s: ok=%v err=%v", sid, ok, err)
	}
	return e
}

// TestRunGitHubCycleForOrg_RetiresAnotherHostsPullRequestsWithNothingTracked:
// an org that has just moved tracks nothing on its new host, so its cycle stops
// at the no_repos skip — and the pull requests it left on the old host still
// retire on that cycle, with scope_changed and no request. The resolver is
// nil, so reaching it would panic.
func TestRunGitHubCycleForOrg_RetiresAnotherHostsPullRequestsWithNothingTracked(t *testing.T) {
	recorder := recordSpans(t)
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	current := dbpkg.EffectiveGitHubHost("")

	stale := seedGitHubPR(t, stores, otherGitHubHost, "octo/old", 3)
	kept := seedGitHubPR(t, stores, current, "octo/live", 4)

	published := &capturingPublisher{}
	m := &Manager{
		database: database, pub: published,
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users,
	}
	m.runGitHubCycleForOrg(context.Background(), org)

	if got := spanOutcome(t, recorder, "poll.github.org"); got != "no_repos" {
		t.Errorf("span outcome = %q, want no_repos", got)
	}
	evts := published.ofType(domain.EventGitHubPRUnreachable)
	if len(evts) != 1 || evts[0].EntityID == nil || *evts[0].EntityID != stale.ID {
		t.Fatalf("unreachable events = %+v, want one for the other host's pull request", evts)
	}
	var meta events.GitHubPRUnreachableMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Host != otherGitHubHost || meta.Reason != events.GitHubUnreachableScopeChanged || meta.Repo != "octo/old" || meta.PRNumber != 3 {
		t.Errorf("metadata = %+v, want octo/old#3 on %s with scope_changed", meta, otherGitHubHost)
	}
	for _, evt := range published.ofType(domain.EventGitHubPRUnreachable) {
		if evt.EntityID != nil && *evt.EntityID == kept.ID {
			t.Error("the current host's pull request was retired")
		}
	}
}

// TestRunGitHubCycleForOrg_RetiresOnceBeforeDiscovery: on a cycle that does
// poll — here through two App installations, so the tracker refreshes twice —
// the old host's pull requests are retired once, and before the first request
// to GitHub.
func TestRunGitHubCycleForOrg_RetiresOnceBeforeDiscovery(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"acme/r1", "beta/r1"})
	seedBYOAppCredentialClass(t, stores, org)
	stale := seedGitHubPR(t, stores, otherGitHubHost, "acme/r1", 3)

	published := &capturingPublisher{}
	var mu sync.Mutex
	retiredAtFirstRequest := -1
	var listed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if retiredAtFirstRequest < 0 {
			retiredAtFirstRequest = len(published.ofType(domain.EventGitHubPRUnreachable))
		}
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[]}}`))
		case strings.Contains(r.URL.Path, "/installation/repositories"):
			_, _ = w.Write([]byte(`{"total_count": 2, "repositories": [{"full_name": "acme/r1"}, {"full_name": "beta/r1"}]}`))
		case strings.Contains(r.URL.Path, "/pulls"):
			mu.Lock()
			listed = append(listed, r.URL.Path)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	m := &Manager{
		database: database, pub: published,
		tasks: stores.Tasks, entities: stores.Entities, eventQueue: stores.EventQueue,
		repos: stores.Repos, orgs: stores.Orgs,
		apps: &fakeInstallsStore{
			app: &domain.OrgGitHubApp{OrgID: org, AppID: "1", Active: true},
			installs: []domain.OrgGitHubAppInstallation{
				{InstallationID: "1", AccountLogin: "acme"},
				{InstallationID: "2", AccountLogin: "beta"},
			},
		},
		resolver: &freshClientPerCallResolver{url: srv.URL},
	}
	m.runGitHubCycleForOrg(context.Background(), org)

	mu.Lock()
	defer mu.Unlock()
	// Each installation's grant lists both repositories here, so each of the
	// two refreshes lists both: four listings across the cycle.
	if len(listed) != 4 {
		t.Fatalf("listed %v, want both repositories listed by each installation's refresh", listed)
	}
	if retiredAtFirstRequest != 1 {
		t.Errorf("unreachable events published before the first GitHub request = %d, want 1", retiredAtFirstRequest)
	}
	evts := published.ofType(domain.EventGitHubPRUnreachable)
	if len(evts) != 1 || evts[0].EntityID == nil || *evts[0].EntityID != stale.ID {
		t.Errorf("unreachable events over the cycle = %+v, want exactly one for the other host's pull request", evts)
	}
}

// backfillUsersStore answers the dashboard-backfill marker as unset and
// records the hosts it is stamped for.
type backfillUsersStore struct {
	dbpkg.UsersStore
	marked []string
}

func (s *backfillUsersStore) DashboardBackfilledAtSystem(context.Context, string, string) (*time.Time, error) {
	return nil, nil
}

func (s *backfillUsersStore) MarkDashboardBackfilledSystem(_ context.Context, _, host, _ string) error {
	s.marked = append(s.marked, host)
	return nil
}

// TestBackfillUserDashboard_OnlyOnTheOrgsCurrentHost: the dashboard history is
// seeded under the org's current host, the scope the poll cycle keys pull
// requests under. A login bound on another host is not one the org's
// credential can search for, so nothing is read or seeded for it and no marker
// is stamped, leaving a bind on the current host free to backfill.
func TestBackfillUserDashboard_OnlyOnTheOrgsCurrentHost(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	current := dbpkg.EffectiveGitHubHost("")

	users := &backfillUsersStore{}
	repos := &recordingRepositoryStore{}
	m := &Manager{orgs: &fakeOrgsStore{ids: []string{"org-1"}}, repos: repos, users: users}

	if err := m.BackfillUserDashboard(context.Background(), "org-1", "user-1", "octocat", otherGitHubHost); err != nil {
		t.Fatalf("BackfillUserDashboard on another host: %v", err)
	}
	if len(repos.visited) != 0 || len(users.marked) != 0 {
		t.Fatalf("a login on another host read tracked sets %v and stamped markers %v, want neither", repos.visited, users.marked)
	}

	// On the current host the org tracks nothing, so the backfill marks itself
	// done after reading that host's tracked set.
	if err := m.BackfillUserDashboard(context.Background(), "org-1", "user-1", "octocat", current); err != nil {
		t.Fatalf("BackfillUserDashboard on the current host: %v", err)
	}
	if len(repos.hosts) != 1 || repos.hosts[0] != current {
		t.Errorf("tracked set read on %v, want [%s]", repos.hosts, current)
	}
	if len(users.marked) != 1 || users.marked[0] != current {
		t.Errorf("markers stamped on %v, want [%s]", users.marked, current)
	}
}

// targetRecordingResolver is freshClientPerCallResolver that records every
// account ClientFor is asked to resolve, so a test can see which installations
// a cycle walked.
type targetRecordingResolver struct {
	*freshClientPerCallResolver
	mu      sync.Mutex
	targets []string
}

func (r *targetRecordingResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	r.mu.Lock()
	r.targets = append(r.targets, target)
	r.mu.Unlock()
	return r.freshClientPerCallResolver.ClientFor(ctx, orgID, target)
}

// TestRunGitHubCycleForOrg_WalksOnlyTheCurrentHostsInstallations: an org that
// moved to another GitHub and holds an installation for one account login on
// both hosts polls through the current host's installation once, and never
// walks the old host's. With only the old host's installation left, the App is
// installed on no accounts here: the cycle reports that, naming the host, and
// makes no request.
func TestRunGitHubCycleForOrg_WalksOnlyTheCurrentHostsInstallations(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	current := dbpkg.EffectiveGitHubHost("")

	run := func(t *testing.T, installs ...domain.OrgGitHubAppInstallation) (targets []string, requests int, reported []error) {
		t.Helper()
		database := newMigratedSQLiteForPoller(t)
		stores := sqlitestore.New(database)
		org := runmode.LocalDefaultOrgID
		trackRepos(t, stores, org, []string{"acme/r1"})
		seedBYOAppCredentialClass(t, stores, org)

		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests++
			mu.Unlock()
			switch {
			case strings.HasSuffix(r.URL.Path, "/graphql"):
				_, _ = w.Write([]byte(`{"data":{"nodes":[]}}`))
			case strings.Contains(r.URL.Path, "/installation/repositories"):
				_, _ = w.Write([]byte(`{"total_count": 1, "repositories": [{"full_name": "acme/r1"}]}`))
			case strings.Contains(r.URL.Path, "/pulls"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected", http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		resolver := &targetRecordingResolver{freshClientPerCallResolver: &freshClientPerCallResolver{url: srv.URL}}
		m := &Manager{
			database: database, pub: &capturingPublisher{},
			tasks: stores.Tasks, entities: stores.Entities, eventQueue: stores.EventQueue,
			repos: stores.Repos, orgs: stores.Orgs,
			apps: &fakeInstallsStore{
				app:      &domain.OrgGitHubApp{OrgID: org, AppID: "1", Active: true},
				installs: installs,
			},
			resolver: resolver,
			OnError:  func(_, _ string, err error) { reported = append(reported, err) },
		}
		m.runGitHubCycleForOrg(context.Background(), org)

		mu.Lock()
		defer mu.Unlock()
		return resolver.targets, requests, reported
	}

	t.Run("both hosts", func(t *testing.T) {
		targets, requests, reported := run(t,
			domain.OrgGitHubAppInstallation{InstallationID: "1", AccountLogin: "acme", GitHubHost: otherGitHubHost},
			domain.OrgGitHubAppInstallation{InstallationID: "2", AccountLogin: "acme", GitHubHost: current},
		)
		if len(targets) != 1 || targets[0] != "acme" {
			t.Errorf("resolved clients for %v, want [acme] once — the old host's installation is not walked", targets)
		}
		if requests == 0 {
			t.Error("the current host's installation made no request; want it polled")
		}
		if len(reported) != 0 {
			t.Errorf("reported %v, want no degraded report", reported)
		}
	})

	t.Run("old host only", func(t *testing.T) {
		targets, requests, reported := run(t,
			domain.OrgGitHubAppInstallation{InstallationID: "1", AccountLogin: "acme", GitHubHost: otherGitHubHost},
		)
		if len(targets) != 0 || requests != 0 {
			t.Errorf("resolved %v and made %d requests, want neither — nothing is installed on this host", targets, requests)
		}
		if len(reported) != 1 || !strings.Contains(reported[0].Error(), "installed on no accounts on "+current) {
			t.Errorf("reported %v, want one report that the App is installed on no accounts on %s", reported, current)
		}
	})
}
