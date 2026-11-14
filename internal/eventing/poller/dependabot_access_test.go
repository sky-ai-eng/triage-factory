package poller

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/tracker"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// alertsServer is a GitHub that answers the Dependabot alerts list of each
// repository named in answers with its status and body, a 200 carrying an
// ETag over its body that a later request may send back for a 304, and counts
// the requests per repository.
type alertsServer struct {
	mu       sync.Mutex
	requests map[string]int
}

func newAlertsServer(t *testing.T, answer func(repo string) (int, string)) (*alertsServer, *ghclient.Client) {
	t.Helper()
	s := &alertsServer{requests: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, rest, ok := strings.Cut(r.URL.Path, "/repos/")
		repo, suffix, _ := strings.Cut(strings.TrimPrefix(rest, "octo/"), "/")
		if !ok || suffix != "dependabot/alerts" {
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.requests[repo]++
		s.mu.Unlock()
		status, body := answer(repo)
		if status == http.StatusTooManyRequests {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			status = http.StatusForbidden
		}
		if status == http.StatusOK {
			sum := sha256.Sum256([]byte(body))
			etag := `"` + hex.EncodeToString(sum[:8]) + `"`
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return s, ghclient.NewClient(srv.URL, "tok")
}

func (s *alertsServer) count(repo string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[repo]
}

func (s *alertsServer) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.requests {
		n += c
	}
	return n
}

// recordedAccess maps each repository name to its recorded answer.
func recordedAccess(t *testing.T, stores db.Stores) map[string]domain.DependabotAlertsAccess {
	t.Helper()
	ctx := context.Background()
	answers, err := stores.DependabotAccess.ListSystem(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("list answers: %v", err)
	}
	out := map[string]domain.DependabotAlertsAccess{}
	for _, a := range answers {
		row, err := stores.Repos.GetSystem(ctx, runmode.LocalDefaultOrgID, a.RepositoryID)
		if err != nil {
			t.Fatalf("read repository %s: %v", a.RepositoryID, err)
		}
		out[row.Repo] = a.Access
	}
	return out
}

// TestDependabotProbe_RecordsEachAnswer: each tracked repository the
// credential reads gets the answer GitHub gave, a repository no team tracks
// is not asked about, a fresh answer is not asked for again, and one older
// than the recheck interval is.
func TestDependabotProbe_RecordsEachAnswer(t *testing.T) {
	ctx := context.Background()
	gh, client := newAlertsServer(t, func(repo string) (int, string) {
		switch repo {
		case "readable":
			return http.StatusOK, `[]`
		case "disabled":
			return http.StatusForbidden, `{"message":"Dependabot alerts are disabled for this repository."}`
		default:
			return http.StatusForbidden, `{"message":"Resource not accessible by integration"}`
		}
	})
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	trackRepos(t, stores, runmode.LocalDefaultOrgID, []string{"octo/readable", "octo/disabled", "octo/refused"})
	m := &Manager{repos: stores.Repos, entities: stores.Entities, DependabotAccess: stores.DependabotAccess}
	names := []string{"octo/readable", "octo/disabled", "octo/refused", "octo/untracked"}

	probeCycle(ctx, m, dependabotPass{client, names, tracker.ReachAll()})

	want := map[string]domain.DependabotAlertsAccess{
		"readable": domain.DependabotAlertsReadable,
		"disabled": domain.DependabotAlertsDisabled,
		"refused":  domain.DependabotAlertsNoPermission,
	}
	got := recordedAccess(t, stores)
	if len(got) != len(want) {
		t.Errorf("answers = %v, want %v", got, want)
	}
	for repo, access := range want {
		if got[repo] != access {
			t.Errorf("%s = %q, want %q", repo, got[repo], access)
		}
	}
	if n := gh.count("untracked"); n != 0 {
		t.Errorf("an untracked repository was asked about %d times", n)
	}

	// The next cycle finds every answer fresh.
	probeCycle(ctx, m, dependabotPass{client, names, tracker.ReachAll()})
	if n := gh.total(); n != 3 {
		t.Errorf("requests after a second cycle = %d, want the first cycle's 3", n)
	}

	// An answer older than the recheck interval is asked for again.
	stale := time.Now().Add(-domain.DependabotAccessRecheck - time.Minute).UTC()
	if _, err := database.Exec(`UPDATE repository_dependabot_access SET checked_at = ?`, stale); err != nil {
		t.Fatalf("age answers: %v", err)
	}
	probeCycle(ctx, m, dependabotPass{client, names, tracker.ReachAll()})
	for repo := range want {
		if n := gh.count(repo); n != 2 {
			t.Errorf("%s asked %d times, want 2 once its answer aged out", repo, n)
		}
	}
}

// TestDependabotProbe_NoAnswerEndsTheCycle: a request that gets no answer (a
// rate limit, a revoked token) records nothing and ends the cycle's probes,
// across every credential pass.
func TestDependabotProbe_NoAnswerEndsTheCycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"rate limited", http.StatusTooManyRequests, `{"message":"API rate limit exceeded"}`},
		{"revoked token", http.StatusUnauthorized, `{"message":"Bad credentials"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			gh, client := newAlertsServer(t, func(string) (int, string) { return tc.status, tc.body })
			stores := sqlitestore.New(newMigratedSQLiteForPoller(t))
			trackRepos(t, stores, runmode.LocalDefaultOrgID, []string{"octo/a", "octo/b", "octo/c"})
			m := &Manager{repos: stores.Repos, entities: stores.Entities, DependabotAccess: stores.DependabotAccess}

			probeCycle(ctx, m, dependabotPass{client, []string{"octo/a", "octo/b"}, tracker.ReachAll()}, dependabotPass{client, []string{"octo/c"}, tracker.ReachAll()})

			if n := gh.total(); n != 1 {
				t.Errorf("requests = %d, want the one that got no answer", n)
			}
			if got := recordedAccess(t, stores); len(got) != 0 {
				t.Errorf("answers = %v, want none", got)
			}
		})
	}
}

// TestDependabotProbe_CapsEachCycle: a cycle probes at most
// dependabotProbesPerCycle repositories across all of its credential passes,
// and the next cycle takes the ones never probed before any it has an answer
// for.
func TestDependabotProbe_CapsEachCycle(t *testing.T) {
	ctx := context.Background()
	gh, client := newAlertsServer(t, func(string) (int, string) { return http.StatusOK, `[]` })
	stores := sqlitestore.New(newMigratedSQLiteForPoller(t))
	var names []string
	for i := range dependabotProbesPerCycle + 5 {
		names = append(names, fmt.Sprintf("octo/r%02d", i))
	}
	trackRepos(t, stores, runmode.LocalDefaultOrgID, names)
	m := &Manager{repos: stores.Repos, entities: stores.Entities, DependabotAccess: stores.DependabotAccess}
	half := len(names) / 2
	passes := []dependabotPass{{client, names[:half], tracker.ReachAll()}, {client, names[half:], tracker.ReachAll()}}

	probeCycle(ctx, m, passes...)
	if n := gh.total(); n != dependabotProbesPerCycle {
		t.Fatalf("first cycle made %d requests, want %d", n, dependabotProbesPerCycle)
	}
	probeCycle(ctx, m, passes...)
	if n := gh.total(); n != len(names) {
		t.Errorf("after two cycles %d requests, want one per repository (%d)", n, len(names))
	}
	if got := recordedAccess(t, stores); len(got) != len(names) {
		t.Errorf("%d answers, want %d", len(got), len(names))
	}
}

// TestDependabotProbe_NoStoreProbesNothing: a Manager without the store
// makes no request.
func TestDependabotProbe_NoStoreProbesNothing(t *testing.T) {
	gh, client := newAlertsServer(t, func(string) (int, string) { return http.StatusOK, `[]` })
	probeCycle(context.Background(), &Manager{}, dependabotPass{client, []string{"octo/a"}, tracker.ReachAll()})
	if n := gh.total(); n != 0 {
		t.Errorf("requests = %d, want none", n)
	}
}

// probeCycle runs one org cycle's probe over passes.
func probeCycle(ctx context.Context, m *Manager, passes ...dependabotPass) {
	p := m.newDependabotProbe(runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, nil)
	for _, pass := range passes {
		p.add(pass.client, pass.repos, pass.reach)
	}
	p.run(ctx)
}

// listingManager is a Manager with the stores the alert listing writes
// through, publishing into rec.
func listingManager(database *sql.DB, stores db.Stores, rec *eventRecorder) *Manager {
	m := NewManager(database, rec, stores.Users, stores.Tasks, stores.Entities, stores.Repos, stores.Deliverables, stores.ExternalActions, stores.EventQueue, stores.PRMergeability, stores.Orgs, stores.JiraStatusRules, stores.JiraProjects, stores.LinearTeamRules, stores.TeamGitHubGroups, nil, stores.GitHubApps, nil, nil, nil)
	m.DependabotAccess = stores.DependabotAccess
	return m
}

// listCycle runs what one org cycle does about Dependabot alerts, in its
// order: the untracked listings are forgotten, then the probe, then the
// listings over passes.
func listCycle(ctx context.Context, m *Manager, passes ...dependabotPass) {
	m.forgetUntrackedAlertListings(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost)
	p := m.newDependabotProbe(runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, nil)
	for _, pass := range passes {
		p.add(pass.client, pass.repos, pass.reach)
	}
	p.run(ctx)
	p.listAlerts(ctx)
}

// TestDependabotListing_ListsOnlyReadableRepositories: after the probe, the
// alerts of each repository whose answer is readable are listed and its
// packages stored, and a repository whose answer is anything else is not
// listed at all.
func TestDependabotListing_ListsOnlyReadableRepositories(t *testing.T) {
	ctx := context.Background()
	lodash := `[{"number":1,"state":"open","dependency":{"package":{"ecosystem":"npm","name":"lodash"},"manifest_path":"package.json"},"security_advisory":{"ghsa_id":"GHSA-1","severity":"high"},"security_vulnerability":{"severity":"high"}}]`
	gh, client := newAlertsServer(t, func(repo string) (int, string) {
		if repo == "readable" {
			return http.StatusOK, lodash
		}
		return http.StatusForbidden, `{"message":"Resource not accessible by integration"}`
	})
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	trackRepos(t, stores, runmode.LocalDefaultOrgID, []string{"octo/readable", "octo/refused"})
	rec := &eventRecorder{}
	m := listingManager(database, stores, rec)

	listCycle(ctx, m, dependabotPass{client, []string{"octo/readable", "octo/refused"}, tracker.ReachAll()})

	if n := gh.count("readable"); n != 2 {
		t.Errorf("readable: %d requests, want the probe and the listing", n)
	}
	if n := gh.count("refused"); n != 1 {
		t.Errorf("refused: %d requests, want the probe alone", n)
	}
	e, err := stores.Entities.GetBySourceSystem(ctx, runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost, "octo/readable#npm:lodash")
	if err != nil || e == nil || e.State != "active" {
		t.Fatalf("the readable repository's package = %+v, %v; want an active entity", e, err)
	}
	if got := recordedTypes(rec); fmt.Sprint(got) != "["+domain.EventGitHubVulnerabilityAlertOpened+"]" {
		t.Errorf("the listing emitted %v, want the open alert's alert_opened", got)
	}
	a, err := stores.DependabotAccess.GetSystem(ctx, runmode.LocalDefaultOrgID, e.RepositoryID)
	if err != nil || a == nil || a.AlertsETag == "" {
		t.Errorf("readable's listing = %+v, %v; want its ETag stored", a, err)
	}
}

// recordedTypes is the types of the events rec has recorded, in order.
func recordedTypes(rec *eventRecorder) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := []string{}
	for _, e := range rec.events {
		out = append(out, e.EventType)
	}
	return out
}

// TestDependabotListing_AProbeWithNoAnswerDoesNotStopTheListings: a
// repository whose probe gets no answer does not hold up the listing of a
// repository already readable.
func TestDependabotListing_AProbeWithNoAnswerDoesNotStopTheListings(t *testing.T) {
	ctx := context.Background()
	gh, client := newAlertsServer(t, func(repo string) (int, string) {
		if repo == "stuck" {
			return http.StatusUnauthorized, `{"message":"Bad credentials"}`
		}
		return http.StatusOK, leftPad(1)
	})
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	trackRepos(t, stores, runmode.LocalDefaultOrgID, []string{"octo/a", "octo/stuck"})
	row, err := stores.Repos.GetByRefSystem(ctx, runmode.LocalDefaultOrgID, domain.RepoRef{Source: domain.RepoSourceGitHub, Host: dbtest.TestGitHubHost, Owner: "octo", Repo: "a"})
	if err != nil || row == nil {
		t.Fatalf("read octo/a: %+v %v", row, err)
	}
	// a's answer is fresh, so the probe asks only about stuck.
	if _, err := stores.DependabotAccess.RecordSystem(ctx, runmode.LocalDefaultOrgID, row.ID, domain.DependabotAlertsReadable); err != nil {
		t.Fatalf("record: %v", err)
	}
	rec := &eventRecorder{}

	listCycle(ctx, listingManager(database, stores, rec), dependabotPass{client, []string{"octo/a", "octo/stuck"}, tracker.ReachAll()})

	if n := gh.count("stuck"); n != 1 {
		t.Errorf("stuck: %d requests, want its probe", n)
	}
	if n := gh.count("a"); n != 1 {
		t.Errorf("a: %d requests, want its listing", n)
	}
	if got := recordedTypes(rec); fmt.Sprint(got) != "["+domain.EventGitHubVulnerabilityAlertOpened+"]" {
		t.Errorf("the listing emitted %v, want a's alert", got)
	}
}

// leftPad is a Dependabot alerts list body holding open left-pad alerts with
// the given numbers.
func leftPad(numbers ...int) string {
	var objs []string
	for _, n := range numbers {
		objs = append(objs, fmt.Sprintf(`{"number":%d,"state":"open","dependency":{"package":{"ecosystem":"npm","name":"left-pad"}},"security_advisory":{"ghsa_id":"GHSA-%d","severity":"low"}}`, n, n))
	}
	return "[" + strings.Join(objs, ",") + "]"
}

// alertsBody is a Dependabot alerts list body of open alerts, each on the
// named npm package.
func alertsBody(alerts map[int]string) string {
	var objs []string
	for _, n := range slices.Sorted(maps.Keys(alerts)) {
		objs = append(objs, fmt.Sprintf(`{"number":%d,"state":"open","dependency":{"package":{"ecosystem":"npm","name":%q}},"security_advisory":{"ghsa_id":"GHSA-%d","severity":"low"}}`, n, alerts[n], n))
	}
	return "[" + strings.Join(objs, ",") + "]"
}

// TestDependabotListing_AnUntrackedRepositoryIsListedForItsPackages: a
// repository every team untracked is still listed while TF holds a package in
// it. An alert opening on that package is announced, a package TF holds no
// entity for gets none, and the package resolves when its alerts close. Once
// it holds nothing the repository is no longer listed and loses its ETag, so
// a team tracking it again gets the package it skipped from a full listing.
func TestDependabotListing_AnUntrackedRepositoryIsListedForItsPackages(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	open := map[int]string{1: "left-pad"}
	gh, client := newAlertsServer(t, func(repo string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if repo == "b" {
			return http.StatusOK, alertsBody(open)
		}
		return http.StatusOK, `[]`
	})
	setOpen := func(alerts map[int]string) {
		mu.Lock()
		defer mu.Unlock()
		open = alerts
	}
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	both := []string{"octo/a", "octo/b"}
	trackRepos(t, stores, runmode.LocalDefaultOrgID, both)
	rec := &eventRecorder{}
	m := listingManager(database, stores, rec)
	pass := func(names ...string) dependabotPass { return dependabotPass{client, names, tracker.ReachAll()} }
	listCycle(ctx, m, pass(both...))
	pkg := func(name string) *domain.Entity {
		t.Helper()
		e, err := stores.Entities.GetBySourceSystem(ctx, runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost, "octo/b#npm:"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return e
	}
	leftPad := pkg("left-pad")
	if leftPad == nil {
		t.Fatal("left-pad has no entity after the first listing")
	}
	etag := func() string {
		t.Helper()
		a, err := stores.DependabotAccess.GetSystem(ctx, runmode.LocalDefaultOrgID, leftPad.RepositoryID)
		if err != nil || a == nil {
			t.Fatalf("read b's answer: %+v %v", a, err)
		}
		return a.AlertsETag
	}
	newEvents := func(before int) []string {
		t.Helper()
		return recordedTypes(rec)[before:]
	}

	// Every team untracks b. A second alert on left-pad, and an alert on a
	// package TF holds nothing for.
	trackRepos(t, stores, runmode.LocalDefaultOrgID, []string{"octo/a"})
	setOpen(map[int]string{1: "left-pad", 2: "left-pad", 3: "react"})
	before := len(recordedTypes(rec))
	listCycle(ctx, m, pass("octo/a"))
	if got := newEvents(before); fmt.Sprint(got) != "["+domain.EventGitHubVulnerabilityAlertOpened+"]" {
		t.Errorf("the untracked listing emitted %v, want left-pad's new alert", got)
	}
	if pkg("react") != nil {
		t.Error("the untracked listing created an entity for react")
	}
	if etag() != "" {
		t.Error("the listing that left react out stored its ETag")
	}

	// Its alerts close: the package resolves, and the router closes it.
	setOpen(map[int]string{3: "react"})
	before = len(recordedTypes(rec))
	listCycle(ctx, m, pass("octo/a"))
	if got := newEvents(before); fmt.Sprint(got) != "["+domain.EventGitHubVulnerabilityResolved+"]" {
		t.Fatalf("the untracked listing after the alerts closed emitted %v, want left-pad resolved", got)
	}
	if res, err := stores.Entities.CloseTerminalSystem(ctx, runmode.LocalDefaultOrgID, leftPad.ID, nil,
		[]string{domain.EventGitHubVulnerabilityAlertOpened}, "entity_closed", domain.EventGitHubVulnerabilityResolved, ""); err != nil || !res.Closed {
		t.Fatalf("close left-pad: %+v %v", res, err)
	}

	// b holds nothing now, so it is no longer listed.
	requests := gh.count("b")
	listCycle(ctx, m, pass("octo/a"))
	if gh.count("b") != requests {
		t.Error("b was listed though it holds nothing and no team tracks it")
	}

	// Tracked again: react, which the untracked listings left out, is
	// announced.
	trackRepos(t, stores, runmode.LocalDefaultOrgID, both)
	before = len(recordedTypes(rec))
	listCycle(ctx, m, pass(both...))
	if got := newEvents(before); fmt.Sprint(got) != "["+domain.EventGitHubVulnerabilityAlertOpened+"]" || pkg("react") == nil {
		t.Errorf("tracking b again emitted %v with react %+v, want react announced", got, pkg("react"))
	}
}

// TestDependabotListing_AnUntrackedRepositoryOutsideEveryReachIsNotListed: a
// held repository no pass's credential can read is neither probed nor listed.
func TestDependabotListing_AnUntrackedRepositoryOutsideEveryReachIsNotListed(t *testing.T) {
	ctx := context.Background()
	gh, client := newAlertsServer(t, func(string) (int, string) { return http.StatusOK, leftPad(1) })
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	trackRepos(t, stores, runmode.LocalDefaultOrgID, []string{"octo/a", "octo/b"})
	m := listingManager(database, stores, &eventRecorder{})
	listCycle(ctx, m, dependabotPass{client, []string{"octo/a", "octo/b"}, tracker.ReachAll()})
	trackRepos(t, stores, runmode.LocalDefaultOrgID, []string{"octo/a"})

	before := gh.count("b")
	listCycle(ctx, m, dependabotPass{client, []string{"octo/a"}, tracker.ReachOfGrant([]ghclient.UserRepo{{FullName: "octo/a"}})})
	if gh.count("b") != before {
		t.Error("b was listed under a credential whose grant does not hold it")
	}
}

// TestDependabotListing_UnreadableAlertsRetireThePackages: once the probe
// records that a repository's alerts can no longer be read, its packages are
// retired with unreachable, naming the answer, and the repository loses its
// ETag. When its alerts are readable again, the listing is answered in full
// and the alert is announced on a new entity.
func TestDependabotListing_UnreadableAlertsRetireThePackages(t *testing.T) {
	ctx := context.Background()
	_, client := newAlertsServer(t, func(string) (int, string) { return http.StatusOK, leftPad(1) })
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	trackRepos(t, stores, runmode.LocalDefaultOrgID, []string{"octo/a"})
	rec := &eventRecorder{}
	m := listingManager(database, stores, rec)
	pass := dependabotPass{client, []string{"octo/a"}, tracker.ReachAll()}
	listCycle(ctx, m, pass)
	first, err := stores.Entities.GetBySourceSystem(ctx, runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost, "octo/a#npm:left-pad")
	if err != nil || first == nil {
		t.Fatalf("the package after the first listing = %+v, %v", first, err)
	}

	// The probe's next answer: the credential lost the permission.
	if _, err := stores.DependabotAccess.RecordSystem(ctx, runmode.LocalDefaultOrgID, first.RepositoryID, domain.DependabotAlertsNoPermission); err != nil {
		t.Fatalf("record: %v", err)
	}
	before := len(recordedTypes(rec))
	listCycle(ctx, m, pass)
	rec.mu.Lock()
	got := append([]domain.Event(nil), rec.events[before:]...)
	rec.mu.Unlock()
	if len(got) != 1 || got[0].EventType != domain.EventGitHubVulnerabilityUnreachable || !strings.Contains(got[0].MetadataJSON, `"reason":"no_permission"`) {
		t.Fatalf("the cycle after access was lost emitted %v, want unreachable with reason no_permission", got)
	}
	a, err := stores.DependabotAccess.GetSystem(ctx, runmode.LocalDefaultOrgID, first.RepositoryID)
	if err != nil || a == nil || a.AlertsETag != "" {
		t.Errorf("the answer after the retirement = %+v, %v; want its ETag cleared", a, err)
	}
	if res, err := stores.Entities.CloseTerminalSystem(ctx, runmode.LocalDefaultOrgID, first.ID, nil,
		[]string{domain.EventGitHubVulnerabilityAlertOpened}, "entity_closed", domain.EventGitHubVulnerabilityUnreachable, ""); err != nil || !res.Closed {
		t.Fatalf("close the retired package: %+v %v", res, err)
	}

	// Readable again, with nothing changed at GitHub.
	if _, err := stores.DependabotAccess.RecordSystem(ctx, runmode.LocalDefaultOrgID, first.RepositoryID, domain.DependabotAlertsReadable); err != nil {
		t.Fatalf("record: %v", err)
	}
	before = len(recordedTypes(rec))
	listCycle(ctx, m, pass)
	if got := recordedTypes(rec)[before:]; fmt.Sprint(got) != "["+domain.EventGitHubVulnerabilityAlertOpened+"]" {
		t.Errorf("the cycle after access returned emitted %v, want the alert announced again", got)
	}
	next, err := stores.Entities.GetBySourceSystem(ctx, runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost, "octo/a#npm:left-pad")
	if err != nil || next == nil || next.ID == first.ID || next.State != "active" {
		t.Errorf("the package after access returned = %+v, %v; want a new active entity", next, err)
	}
}

// TestDependabotListing_AHostRoundTripAnnouncesTheRetiredAlerts: an org that
// moves to another GitHub host, tracking nothing there, retires its packages
// on the old one and forgets their ETags. Back on the old host with nothing
// changed, the listing is answered in full rather than with a 304, and the
// alert whose package was retired is announced again on a new entity.
func TestDependabotListing_AHostRoundTripAnnouncesTheRetiredAlerts(t *testing.T) {
	ctx := context.Background()
	_, client := newAlertsServer(t, func(string) (int, string) { return http.StatusOK, leftPad(1) })
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	trackRepos(t, stores, runmode.LocalDefaultOrgID, []string{"octo/a"})
	rec := &eventRecorder{}
	m := listingManager(database, stores, rec)
	listCycle(ctx, m, dependabotPass{client, []string{"octo/a"}, tracker.ReachAll()})
	first, err := stores.Entities.GetBySourceSystem(ctx, runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost, "octo/a#npm:left-pad")
	if err != nil || first == nil {
		t.Fatalf("the package after the first listing = %+v, %v", first, err)
	}

	// A cycle on the other host, where the org tracks nothing yet: the
	// package is retired, and the router closes it on the unreachable event.
	if n := m.trackerForOrg(runmode.LocalDefaultOrgID).RetireGitHubOutOfScope(ctx, dbtest.TestOtherGitHubHost); n != 1 {
		t.Fatalf("retired %d entities, want the package", n)
	}
	if res, err := stores.Entities.CloseTerminalSystem(ctx, runmode.LocalDefaultOrgID, first.ID, nil,
		[]string{domain.EventGitHubVulnerabilityAlertOpened}, "entity_closed", domain.EventGitHubVulnerabilityUnreachable, ""); err != nil || !res.Closed {
		t.Fatalf("close the retired package: %+v %v", res, err)
	}
	m.forgetUntrackedAlertListings(ctx, runmode.LocalDefaultOrgID, dbtest.TestOtherGitHubHost)
	before := len(recordedTypes(rec))

	listCycle(ctx, m, dependabotPass{client, []string{"octo/a"}, tracker.ReachAll()})
	if got := recordedTypes(rec)[before:]; fmt.Sprint(got) != "["+domain.EventGitHubVulnerabilityAlertOpened+"]" {
		t.Errorf("returning to the host emitted %v, want the retired alert announced again", got)
	}
	next, err := stores.Entities.GetBySourceSystem(ctx, runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost, "octo/a#npm:left-pad")
	if err != nil || next == nil || next.ID == first.ID || next.State != "active" {
		t.Errorf("the package after the return = %+v, %v; want a new active entity", next, err)
	}
}
