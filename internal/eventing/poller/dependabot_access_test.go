package poller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// alertsServer is a GitHub that answers the Dependabot alerts list of each
// repository named in answers with its status and body, and counts the
// requests per repository.
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
	m := &Manager{repos: stores.Repos, DependabotAccess: stores.DependabotAccess}
	names := []string{"octo/readable", "octo/disabled", "octo/refused", "octo/untracked"}

	probeCycle(ctx, m, dependabotPass{client, names})

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
	probeCycle(ctx, m, dependabotPass{client, names})
	if n := gh.total(); n != 3 {
		t.Errorf("requests after a second cycle = %d, want the first cycle's 3", n)
	}

	// An answer older than the recheck interval is asked for again.
	stale := time.Now().Add(-domain.DependabotAccessRecheck - time.Minute).UTC()
	if _, err := database.Exec(`UPDATE repository_dependabot_access SET checked_at = ?`, stale); err != nil {
		t.Fatalf("age answers: %v", err)
	}
	probeCycle(ctx, m, dependabotPass{client, names})
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
			m := &Manager{repos: stores.Repos, DependabotAccess: stores.DependabotAccess}

			probeCycle(ctx, m, dependabotPass{client, []string{"octo/a", "octo/b"}}, dependabotPass{client, []string{"octo/c"}})

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
	m := &Manager{repos: stores.Repos, DependabotAccess: stores.DependabotAccess}
	half := len(names) / 2
	passes := []dependabotPass{{client, names[:half]}, {client, names[half:]}}

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
	probeCycle(context.Background(), &Manager{}, dependabotPass{client, []string{"octo/a"}})
	if n := gh.total(); n != 0 {
		t.Errorf("requests = %d, want none", n)
	}
}

// probeCycle runs one org cycle's probe over passes.
func probeCycle(ctx context.Context, m *Manager, passes ...dependabotPass) {
	p := m.newDependabotProbe(runmode.LocalDefaultOrgID, dbtest.TestGitHubHost)
	for _, pass := range passes {
		p.add(pass.client, pass.repos)
	}
	p.run(ctx)
}
