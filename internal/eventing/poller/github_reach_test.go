package poller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
// the node ids every token sends in a batch refresh.
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
}

func newReachGitHub(t *testing.T, repos []reachRepo, grants, readable map[string][]string) *reachGitHub {
	t.Helper()
	f := &reachGitHub{t: t, repos: repos, grants: grants, readable: readable, sent: map[string][]string{}}
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
	f.mu.Unlock()

	nodes := make([]string, len(req.Variables.IDs))
	var errs []string
	for i, id := range req.Variables.IDs {
		nodes[i] = "null"
		for _, repo := range f.repos {
			if repo.nodeID == id && slices.Contains(f.readable[token], repo.fullName) {
				nodes[i] = fmt.Sprintf(`{"id":%q,"number":%d,"title":"PR %d","author":{"login":"alice"},"state":"OPEN",
					"url":"https://github.com/%s/pull/%d","repository":{"nameWithOwner":%q},"baseRefName":"main",
					"createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-20T00:00:00Z",
					"commits":{"nodes":[{"commit":{"oid":"sha%d"}}]}}`,
					id, repo.number, repo.number, repo.fullName, repo.number, repo.fullName, repo.number)
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
