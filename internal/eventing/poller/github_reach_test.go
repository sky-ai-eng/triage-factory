package poller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// reachRepo is one repository the reach fake serves, with its one open pull
// request.
type reachRepo struct {
	id       int64
	ownerID  int64
	fullName string
	number   int
	nodeID   string
}

// reachGitHub is a fake GitHub that tells App installations apart by their
// installation token, as GitHub does. Each token's /installation/repositories
// answers its installation's grant, each open-PR listing carries an ETag and
// answers 304 to a matching If-None-Match, and the batch refresh answers null,
// with a NOT_FOUND error entry, for a node the token cannot read. It records
// the node ids every token sends in a batch refresh, and counts every request.
type reachGitHub struct {
	t     *testing.T
	srv   *httptest.Server
	repos []reachRepo
	// grants is each token's installation grant, by repository full name.
	grants map[string][]string
	// readable is the repositories each token can read: its grant, and a
	// public repository too.
	readable map[string][]string

	mu   sync.Mutex
	sent map[string][]string
	// merged is the pull requests, by node id, the batch refresh answers as
	// merged.
	merged map[string]bool
	// requests counts every request of any kind.
	requests int
}

func newReachGitHub(t *testing.T, repos []reachRepo, grants, readable map[string][]string) *reachGitHub {
	t.Helper()
	f := &reachGitHub{t: t, repos: repos, grants: grants, readable: readable, sent: map[string][]string{}, merged: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *reachGitHub) repo(fullName string) (reachRepo, bool) {
	for _, r := range f.repos {
		if strings.EqualFold(r.fullName, fullName) {
			return r, true
		}
	}
	return reachRepo{}, false
}

func (f *reachGitHub) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/installation/repositories"):
		var repos []string
		for _, name := range f.grants[token] {
			repo, _ := f.repo(name)
			owner, _, _ := strings.Cut(repo.fullName, "/")
			repos = append(repos, fmt.Sprintf(`{"id":%d,"full_name":%q,"owner":{"login":%q,"id":%d}}`, repo.id, repo.fullName, owner, repo.ownerID))
		}
		_, _ = fmt.Fprintf(w, `{"total_count":%d,"repositories":[%s]}`, len(repos), strings.Join(repos, ","))
	case strings.HasSuffix(r.URL.Path, "/graphql"):
		f.serveGraphQL(w, r, token)
	case isBranchRefRead(r):
		http.NotFound(w, r)
	default:
		name, ok := repoFromPullsPath(r.URL.Path)
		repo, known := f.repo(name)
		if !ok || !known {
			f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		etag := fmt.Sprintf(`"%s-v1"`, repo.fullName)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = fmt.Fprintf(w, `[{"number":%d,"node_id":%q,"title":"PR %d","state":"open",
			"html_url":"https://github.com/%s/pull/%d","created_at":"2026-06-01T00:00:00Z","updated_at":"2026-06-20T00:00:00Z",
			"user":{"login":"alice"},"head":{"sha":"sha%d","ref":"feature","repo":{"full_name":%q}},
			"base":{"ref":"main","repo":{"full_name":%q}}}]`,
			repo.number, repo.nodeID, repo.number, repo.fullName, repo.number, repo.number, repo.fullName, repo.fullName)
	}
}

func (f *reachGitHub) serveGraphQL(w http.ResponseWriter, r *http.Request, token string) {
	var req struct {
		Query     string `json:"query"`
		Variables struct {
			IDs []string `json:"ids"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !strings.Contains(req.Query, "nodes(ids: $ids)") {
		f.t.Errorf("unexpected GraphQL request under %s: %v %.80q", token, err, req.Query)
		http.Error(w, "unexpected", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.sent[token] = append(f.sent[token], req.Variables.IDs...)
	merged := maps.Clone(f.merged)
	f.mu.Unlock()

	nodes := make([]string, len(req.Variables.IDs))
	var errs []string
	for i, id := range req.Variables.IDs {
		nodes[i] = "null"
		for _, repo := range f.repos {
			if repo.nodeID == id && slices.Contains(f.readable[token], repo.fullName) {
				state := "OPEN"
				if merged[id] {
					state = "MERGED"
				}
				nodes[i] = fmt.Sprintf(`{"id":%q,"number":%d,"title":"PR %d","author":{"login":"alice"},"state":%q,"merged":%t,
					"url":"https://github.com/%s/pull/%d","repository":{"nameWithOwner":%q},"baseRefName":"main",
					"createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-20T00:00:00Z",
					"commits":{"nodes":[{"commit":{"oid":"sha%d"}}]}}`,
					id, repo.number, repo.number, state, merged[id], repo.fullName, repo.number, repo.fullName, repo.number)
			}
		}
		if nodes[i] == "null" {
			errs = append(errs, fmt.Sprintf(`{"type":"NOT_FOUND","path":["nodes",%d],"message":"Could not resolve to a node with the global id of '%s'"}`, i, id))
		}
	}
	body := `{"data":{"nodes":[` + strings.Join(nodes, ",") + `]}`
	if len(errs) > 0 {
		body += `,"errors":[` + strings.Join(errs, ",") + `]`
	}
	_, _ = w.Write([]byte(body + `}`))
}

// merge makes the batch refresh answer nodeID as merged from now on.
func (f *reachGitHub) merge(nodeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged[nodeID] = true
}

// takeRequests returns how many requests arrived since the last call.
func (f *reachGitHub) takeRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.requests
	f.requests = 0
	return n
}

// takeSent returns the node ids each token sent since the last call.
func (f *reachGitHub) takeSent() map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	sent := f.sent
	f.sent = map[string][]string{}
	return sent
}

// tokenPerInstallationResolver mints a client per installation under the
// token tok-<installation id>, the way each installation's token is its own.
type tokenPerInstallationResolver struct {
	url string
}

func (r *tokenPerInstallationResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	return nil, ghclient.ErrNoGitHubCredentials
}

func (r *tokenPerInstallationResolver) ClientForInstallation(ctx context.Context, orgID string, inst domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	return ghclient.NewClient(r.url, "tok-"+inst.InstallationID), nil
}

func (r *tokenPerInstallationResolver) ClientForRepo(ctx context.Context, orgID, owner, repo string) (*ghclient.Client, error) {
	return nil, ghclient.ErrNoGitHubCredentials
}

func (r *tokenPerInstallationResolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	return githubapp.Token{}, nil
}

func (r *tokenPerInstallationResolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	return ghbase.DefaultBaseURL(), nil
}

func (r *tokenPerInstallationResolver) OrgIdentityFor(ctx context.Context, orgID string) (string, string, bool) {
	return "", "", false
}

// newReachManager is a multi-mode Manager for an org on an active App with
// installs, each installation's client minted under its own token against gh.
func newReachManager(t *testing.T, database *sql.DB, stores db.Stores, gh *reachGitHub, installs []domain.OrgGitHubAppInstallation) *Manager {
	t.Helper()
	bus := eventbus.New()
	t.Cleanup(bus.Close)
	return &Manager{
		database: database,
		pub:      busPublisher{bus: bus},
		tasks:    stores.Tasks,
		entities: stores.Entities, eventQueue: stores.EventQueue, mergeability: stores.PRMergeability,
		repos: stores.Repos,
		orgs:  stores.Orgs,
		apps: &fakeInstallsStore{
			app:      &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "1", Active: true},
			installs: installs,
		},
		resolver: &tokenPerInstallationResolver{url: gh.srv.URL},
	}
}

var reachRepos = []reachRepo{
	{id: 101, ownerID: 11, fullName: "acme/one", number: 1, nodeID: "PR_one"},
	{id: 202, ownerID: 22, fullName: "beta/two", number: 2, nodeID: "PR_two"},
}

// TestRunGitHubCycleForOrg_EachInstallationRefreshesOnlyItsGrant: with two App
// installations, each one's pass sends GitHub only the node ids of the pull
// requests in its own grant, whether or not its token could read the other
// installation's repository. Once both pull requests are settled and the
// listings answer 304, neither pass sends anything.
func TestRunGitHubCycleForOrg_EachInstallationRefreshesOnlyItsGrant(t *testing.T) {
	grants := map[string][]string{"tok-1": {"acme/one"}, "tok-2": {"beta/two"}}
	cases := []struct {
		name     string
		readable map[string][]string
	}{
		{name: "private repositories", readable: grants},
		{name: "each token reads both repositories", readable: map[string][]string{
			"tok-1": {"acme/one", "beta/two"}, "tok-2": {"acme/one", "beta/two"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			ctx := context.Background()
			database := newMigratedSQLiteForPoller(t)
			stores := sqlitestore.New(database)
			org := runmode.LocalDefaultOrgID
			trackRepos(t, stores, org, []string{"acme/one", "beta/two"})
			seedBYOAppCredentialClass(t, stores, org)

			gh := newReachGitHub(t, reachRepos, grants, tc.readable)
			m := newReachManager(t, database, stores, gh, []domain.OrgGitHubAppInstallation{
				{InstallationID: "1", AccountLogin: "acme"},
				{InstallationID: "2", AccountLogin: "beta"},
			})

			for cycle := 1; cycle <= 3; cycle++ {
				m.runGitHubCycleForOrg(ctx, org)
				sent := gh.takeSent()
				for token, own := range map[string]string{"tok-1": "PR_one", "tok-2": "PR_two"} {
					for _, id := range sent[token] {
						if id != own {
							t.Errorf("cycle %d: %s sent %s, outside its installation's grant", cycle, token, id)
						}
					}
				}
				switch cycle {
				case 1:
					// The first refresh of each pull request reads its check runs.
					for token, own := range map[string]string{"tok-1": "PR_one", "tok-2": "PR_two"} {
						if !slices.Equal(sent[token], []string{own}) {
							t.Errorf("cycle 1: %s sent %v, want [%s]", token, sent[token], own)
						}
					}
				default:
					if len(sent) != 0 {
						t.Errorf("cycle %d: sent %v, want nothing: both listings answered 304 and both pull requests are settled", cycle, sent)
					}
				}
			}
		})
	}
}

// TestRunGitHubCycleForOrg_UntrackedRepositoryRefreshesOnTheFloor: a pull
// request TF holds in a repository no team tracks any more is refreshed by the
// pass whose installation grants the repository, once its last refresh is
// older than staleRefreshFloor, and not on the cycles between.
func TestRunGitHubCycleForOrg_UntrackedRepositoryRefreshesOnTheFloor(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"acme/one", "beta/two"})
	seedBYOAppCredentialClass(t, stores, org)

	both := []string{"acme/one", "beta/two"}
	gh := newReachGitHub(t, reachRepos, map[string][]string{"tok-1": both}, map[string][]string{"tok-1": both})
	m := newReachManager(t, database, stores, gh, []domain.OrgGitHubAppInstallation{{InstallationID: "1", AccountLogin: "acme"}})

	m.runGitHubCycleForOrg(ctx, org)
	if sent := gh.takeSent(); !slices.Equal(sortedCopy(sent["tok-1"]), []string{"PR_one", "PR_two"}) {
		t.Fatalf("cycle 1 sent %v, want both pull requests' first refresh", sent)
	}

	trackRepos(t, stores, org, []string{"acme/one"})
	for cycle := 2; cycle <= 3; cycle++ {
		m.runGitHubCycleForOrg(ctx, org)
		if sent := gh.takeSent(); len(sent) != 0 {
			t.Errorf("cycle %d sent %v, want nothing: beta/two's pull request was refreshed less than the floor ago", cycle, sent)
		}
	}

	untracked, err := stores.Entities.GetBySource(ctx, org, "github", "https://github.com", "beta/two#2")
	if err != nil || untracked == nil {
		t.Fatalf("read beta/two#2: %v, %v", untracked, err)
	}
	if _, err := database.Exec(`UPDATE entities SET last_polled_at = ? WHERE id = ?`, time.Now().Add(-time.Hour), untracked.ID); err != nil {
		t.Fatalf("age beta/two#2's last refresh: %v", err)
	}
	m.runGitHubCycleForOrg(ctx, org)
	if sent := gh.takeSent(); !slices.Equal(sent["tok-1"], []string{"PR_two"}) || len(sent) != 1 {
		t.Errorf("cycle 4 sent %v, want only beta/two's pull request, refreshed past the floor", sent)
	}
	m.runGitHubCycleForOrg(ctx, org)
	if sent := gh.takeSent(); len(sent) != 0 {
		t.Errorf("cycle 5 sent %v, want nothing: the floor restarts at the refresh", sent)
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// queuedEvents counts the events of eventType the tracker queued for entityID.
func queuedEvents(t *testing.T, database *sql.DB, entityID, eventType string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM event_queue WHERE entity_id = ? AND event_type = ?`, entityID, eventType).Scan(&n); err != nil {
		t.Fatalf("count queued %s events: %v", eventType, err)
	}
	return n
}

// ageLastRefresh moves key's last refresh past staleRefreshFloor.
func ageLastRefresh(t *testing.T, database *sql.DB, stores db.Stores, key string) *domain.Entity {
	t.Helper()
	e, err := stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "github", "https://github.com", key)
	if err != nil || e == nil {
		t.Fatalf("read %s: %v, %v", key, e, err)
	}
	if _, err := database.Exec(`UPDATE entities SET last_polled_at = ? WHERE id = ?`, time.Now().Add(-time.Hour), e.ID); err != nil {
		t.Fatalf("age %s's last refresh: %v", key, err)
	}
	return e
}

// TestRunGitHubCycleForOrg_InstallationWithNothingTrackedRefreshesWhatItHolds:
// with two App installations and every repository under one of them
// untracked, no RefreshGitHub pass runs under that installation, and the pull
// request TF holds there is refreshed under its token once its last refresh is
// older than staleRefreshFloor. The refresh that sees it merged queues the
// merge, which is what closes the tasks left on it.
func TestRunGitHubCycleForOrg_InstallationWithNothingTrackedRefreshesWhatItHolds(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"acme/one", "beta/two"})
	seedBYOAppCredentialClass(t, stores, org)

	grants := map[string][]string{"tok-1": {"acme/one"}, "tok-2": {"beta/two"}}
	gh := newReachGitHub(t, reachRepos, grants, grants)
	m := newReachManager(t, database, stores, gh, []domain.OrgGitHubAppInstallation{
		{InstallationID: "1", AccountLogin: "acme"},
		{InstallationID: "2", AccountLogin: "beta"},
	})

	m.runGitHubCycleForOrg(ctx, org)
	if sent := gh.takeSent(); !slices.Equal(sent["tok-1"], []string{"PR_one"}) || !slices.Equal(sent["tok-2"], []string{"PR_two"}) {
		t.Fatalf("cycle 1 sent %v, want each pull request's first refresh under its own installation", sent)
	}

	trackRepos(t, stores, org, []string{"acme/one"})
	for cycle := 2; cycle <= 3; cycle++ {
		m.runGitHubCycleForOrg(ctx, org)
		if sent := gh.takeSent(); len(sent) != 0 {
			t.Errorf("cycle %d sent %v, want nothing: beta/two's pull request was refreshed less than the floor ago", cycle, sent)
		}
	}

	held := ageLastRefresh(t, database, stores, "beta/two#2")
	gh.merge("PR_two")
	m.runGitHubCycleForOrg(ctx, org)
	if sent := gh.takeSent(); !slices.Equal(sent["tok-2"], []string{"PR_two"}) || len(sent) != 1 {
		t.Errorf("cycle 4 sent %v, want only beta/two's pull request, under its own installation's token", sent)
	}
	if n := queuedEvents(t, database, held.ID, domain.EventGitHubPRMerged); n != 1 {
		t.Errorf("queued merged events for beta/two#2 = %d, want 1", n)
	}
}

// TestRunGitHubCycleForOrg_NothingTrackedRefreshesWhatItHolds: an org that
// tracks nothing still refreshes the pull requests it holds, on the floor's
// schedule, under its personal token or under the App installation whose
// grant reads them. Such a cycle lists no repository and is not a completed
// poll: the last-success stamp stays where the last poll of a tracked set
// left it.
func TestRunGitHubCycleForOrg_NothingTrackedRefreshesWhatItHolds(t *testing.T) {
	both := []string{"acme/one", "beta/two"}
	for _, tc := range []struct {
		name  string
		token string
		setup func(t *testing.T, stores db.Stores, m *Manager, gh *reachGitHub)
	}{
		{name: "personal token", token: "tok-pat", setup: func(t *testing.T, _ db.Stores, m *Manager, gh *reachGitHub) {
			m.apps = nil
			m.resolver = &fakeResolver{client: ghclient.NewClient(gh.srv.URL, "tok-pat")}
		}},
		{name: "app installation", token: "tok-1", setup: func(t *testing.T, stores db.Stores, _ *Manager, _ *reachGitHub) {
			seedBYOAppCredentialClass(t, stores, runmode.LocalDefaultOrgID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			recorder := recordSpans(t)
			ctx := context.Background()
			database := newMigratedSQLiteForPoller(t)
			stores := sqlitestore.New(database)
			org := runmode.LocalDefaultOrgID
			trackRepos(t, stores, org, both)

			gh := newReachGitHub(t, reachRepos, map[string][]string{tc.token: both}, map[string][]string{tc.token: both})
			m := newReachManager(t, database, stores, gh, []domain.OrgGitHubAppInstallation{{InstallationID: "1", AccountLogin: "acme"}})
			tc.setup(t, stores, m, gh)

			m.runGitHubCycleForOrg(ctx, org)
			if sent := gh.takeSent(); !slices.Equal(sortedCopy(sent[tc.token]), []string{"PR_one", "PR_two"}) {
				t.Fatalf("cycle 1 sent %v, want both pull requests' first refresh", sent)
			}
			polled := m.successSnapshot("github")[org]
			if polled.IsZero() {
				t.Fatal("cycle 1 polled the tracked set and stamped no success")
			}

			trackRepos(t, stores, org, nil)
			gh.takeRequests()
			m.runGitHubCycleForOrg(ctx, org)
			if got := spanOutcome(t, recorder, "poll.github.org"); got != "held_only" {
				t.Errorf("cycle 2 outcome = %q, want held_only", got)
			}
			if sent := gh.takeSent(); len(sent) != 0 {
				t.Errorf("cycle 2 sent %v, want nothing: both pull requests were refreshed less than the floor ago", sent)
			}

			held := ageLastRefresh(t, database, stores, "beta/two#2")
			gh.merge("PR_two")
			m.runGitHubCycleForOrg(ctx, org)
			if sent := gh.takeSent(); !slices.Equal(sent[tc.token], []string{"PR_two"}) || len(sent) != 1 {
				t.Errorf("cycle 3 sent %v, want only beta/two's pull request, refreshed past the floor", sent)
			}
			if n := queuedEvents(t, database, held.ID, domain.EventGitHubPRMerged); n != 1 {
				t.Errorf("queued merged events for beta/two#2 = %d, want 1", n)
			}
			if got := m.successSnapshot("github")[org]; !got.Equal(polled) {
				t.Errorf("last success moved from %v to %v on cycles that polled no tracked set", polled, got)
			}
			// The App path lists the installation's grant to find what it
			// reads; nothing else but the one refresh goes out.
			wantRequests := 1
			if tc.token == "tok-1" {
				wantRequests += 2
			}
			if n := gh.takeRequests(); n != wantRequests {
				t.Errorf("cycles 2-3 made %d requests, want %d: no listing, no search, one refresh", n, wantRequests)
			}
		})
	}
}

// TestRunGitHubCycleForOrg_NothingTrackedNothingHeldSendsNothing: an org that
// tracks nothing and holds no active pull request outside its tracked set
// makes no request on either path, and its cycle keeps the no_repos outcome. A
// closed pull request holds nothing.
func TestRunGitHubCycleForOrg_NothingTrackedNothingHeldSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		app  bool
	}{{name: "personal token"}, {name: "app installation", app: true}} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			recorder := recordSpans(t)
			ctx := context.Background()
			database := newMigratedSQLiteForPoller(t)
			stores := sqlitestore.New(database)
			org := runmode.LocalDefaultOrgID
			closed := seedGitHubPR(t, stores, "https://github.com", "beta/two", 2)
			if _, err := stores.Entities.CloseSystem(ctx, org, closed.ID); err != nil {
				t.Fatalf("close beta/two#2: %v", err)
			}
			gh := newReachGitHub(t, reachRepos, map[string][]string{"tok-1": {"beta/two"}}, map[string][]string{"tok-1": {"beta/two"}})
			m := newReachManager(t, database, stores, gh, []domain.OrgGitHubAppInstallation{{InstallationID: "1", AccountLogin: "beta"}})
			if tc.app {
				seedBYOAppCredentialClass(t, stores, org)
			} else {
				m.apps = nil
				m.resolver = &fakeResolver{client: ghclient.NewClient(gh.srv.URL, "tok-pat")}
			}

			m.runGitHubCycleForOrg(ctx, org)
			if got := spanOutcome(t, recorder, "poll.github.org"); got != "no_repos" {
				t.Errorf("outcome = %q, want no_repos", got)
			}
			if n := gh.takeRequests(); n != 0 {
				t.Errorf("made %d requests, want none", n)
			}
		})
	}
}
