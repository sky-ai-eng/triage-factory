package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// longAgo is a heartbeat far past the /readyz hard check, standing in for a
// cycle that has been running that long.
func longAgo() time.Time { return time.Now().Add(-10 * time.Minute) }

// hookRepositoryStore calls visit as the GitHub cycle starts each org's poll,
// which then finds nothing tracked.
type hookRepositoryStore struct {
	dbpkg.RepositoryStore
	visit func(orgID string)
}

func (r *hookRepositoryStore) ListTrackedNamesSystem(_ context.Context, orgID string) ([]string, error) {
	r.visit(orgID)
	return nil, nil
}

// hookSourceStore calls visit as the Jira cycle starts each org's poll, and
// reports Jira turned off so the poll stops there.
type hookSourceStore struct {
	dbpkg.OrgEventSourceStore
	visit func(orgID string)
}

func (s *hookSourceStore) ListDisabledSystem(_ context.Context, orgID string) ([]string, error) {
	s.visit(orgID)
	return []string{"jira"}, nil
}

// TestPollCycles_HeartbeatAdvancesAsEachOrgFinishes: the poller is alive as
// long as its cycle is making progress, however many orgs the cycle covers.
// The first org's poll runs past the hard check's window; by the time the
// second org's poll starts, the heartbeat reflects the first org finishing,
// so /readyz still reports the poller alive.
func TestPollCycles_HeartbeatAdvancesAsEachOrgFinishes(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		m := &Manager{orgs: &fakeOrgsStore{ids: []string{"org-a", "org-b"}}, users: &emptyUsersStore{}}
		var alive, sawB bool
		m.repos = &hookRepositoryStore{visit: func(orgID string) {
			switch orgID {
			case "org-a":
				m.SetGitHubHeartbeatForTest(longAgo())
			case "org-b":
				sawB = true
				alive = m.Health(context.Background()).GitHub.Alive
			}
		}}
		m.runGitHubCycle(nil)
		if !sawB {
			t.Fatal("the cycle never reached the second org")
		}
		if !alive {
			t.Error("a cycle that finished one org and moved to the next reads as a dead poller")
		}
	})
	t.Run("jira", func(t *testing.T) {
		m := &Manager{orgs: &fakeOrgsStore{ids: []string{"org-a", "org-b"}}}
		var alive, sawB bool
		m.EventSources = &hookSourceStore{visit: func(orgID string) {
			switch orgID {
			case "org-a":
				m.SetJiraHeartbeatForTest(longAgo())
			case "org-b":
				sawB = true
				alive = m.Health(context.Background()).Jira.Alive
			}
		}}
		m.runJiraCycle(nil)
		if !sawB {
			t.Fatal("the cycle never reached the second org")
		}
		if !alive {
			t.Error("a cycle that finished one org and moved to the next reads as a dead poller")
		}
	})
}

// TestGitHubCycle_HeartbeatAdvancesWithRequests: one org's poll can itself
// run past the hard check's window, against a slow host or one org's long
// tracked set. Each request it completes is progress, so the heartbeat stays
// fresh while the requests keep completing.
func TestGitHubCycle_HeartbeatAdvancesWithRequests(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	var m *Manager
	var alive, sawB atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[]}}`))
			return
		case strings.HasSuffix(r.URL.Path, "/octo/a/pulls"):
			m.SetGitHubHeartbeatForTest(longAgo())
		case strings.HasSuffix(r.URL.Path, "/octo/b/pulls"):
			sawB.Store(true)
			alive.Store(m.Health(r.Context()).GitHub.Alive)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	f := newConnectionFixture(t, newGitHubConnServer(t, map[string]string{"a": listOK, "b": listOK}), "octo/a", "octo/b")
	f.m.resolver = &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")}
	m = f.m

	m.runGitHubCycle(nil)
	if !sawB.Load() {
		t.Fatal("the cycle never listed the second repo")
	}
	if !alive.Load() {
		t.Error("a cycle whose requests keep completing reads as a dead poller")
	}
}
