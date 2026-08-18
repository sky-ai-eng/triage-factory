package reachcache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
)

// A declined pass — an enumeration that completed but was refused as
// truncated — leaves the mirror's stored state exactly as stale as it found
// it. The TTL gate therefore never absorbs the next kick, and the picker's
// discovering state polls its read every few seconds, so without a second gate
// each poll would re-run the full enumeration against the rate-limit budget
// the pollers need. These tests pin that second gate: a declined pass buys the
// same quiet window a write would have, a force punches through it, and it
// expires like the TTL it mirrors.

// fakeReachMirror is the two store methods the PAT refresh touches; the
// embedded interface panics on anything else, which is the point.
type fakeReachMirror struct {
	db.ReachableReposStore
	state    domain.ReachableCacheState
	replaced int32
}

func (m *fakeReachMirror) ReachableStateSystem(context.Context, string, domain.GitHubCredentialClass) (domain.ReachableCacheState, error) {
	return m.state, nil
}

func (m *fakeReachMirror) ReplaceForPATSystem(context.Context, string, string, []domain.ReachableRepository) error {
	atomic.AddInt32(&m.replaced, 1)
	return nil
}

// fakeResolver hands back a real *github.Client pointed at the test server, so
// the walk under test is the production one.
type fakeResolver struct {
	github.Resolver
	base string
}

func (f fakeResolver) BaseURLFor(context.Context, string) (string, error) { return f.base, nil }

func (f fakeResolver) ClientFor(context.Context, string, string) (*github.Client, error) {
	return github.NewClient(f.base, "test-token"), nil
}

// userReposServer serves GET /user/repos pages: full 100-repo pages while
// *truncate is 1 (so the walk hits the page cap and reports incomplete), a
// single short page once it is 0 (so the walk completes). pages counts
// upstream requests — the thing the backoff exists to stop.
func userReposServer(t *testing.T, truncate *int32, pages *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := atomic.AddInt32(pages, 1)
		n := 100
		if atomic.LoadInt32(truncate) == 0 {
			n = 1
		}
		repos := make([]github.UserRepo, n)
		for i := range repos {
			repos[i] = github.UserRepo{FullName: fmt.Sprintf("octo/repo-%d-%d", page, i)}
		}
		data, _ := json.Marshal(repos)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func patRefresher(mirror db.ReachableReposStore, base string) *Refresher {
	return NewRefresher(
		NewClassResolver(fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: domain.GitHubCredentialClassPAT}}, nil),
		mirror,
		fakeResolver{base: base},
		nil,
		nil,
	)
}

func TestRunOrgDeclinedPassArmsTheBackoff(t *testing.T) {
	ctx := context.Background()
	var truncate, pages int32
	atomic.StoreInt32(&truncate, 1)
	srv := userReposServer(t, &truncate, &pages)
	mirror := &fakeReachMirror{}
	r := patRefresher(mirror, srv.URL)

	wrote, err := r.RunOrg(ctx, "org-1", false)
	if err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if wrote {
		t.Fatal("wrote a truncated enumeration into the mirror")
	}
	first := atomic.LoadInt32(&pages)
	if first == 0 {
		t.Fatal("no enumeration ran at all")
	}

	// A second non-forced kick inside the window is absorbed without touching
	// GitHub — this is the read-poll storm the gate exists to stop.
	wrote, err = r.RunOrg(ctx, "org-1", false)
	if err != nil {
		t.Fatalf("RunOrg (gated): %v", err)
	}
	if wrote {
		t.Error("a gated pass reported a write")
	}
	if got := atomic.LoadInt32(&pages); got != first {
		t.Errorf("a declined pass was retried immediately: %d extra upstream requests", got-first)
	}

	// A force punches through, and a pass that writes clears the stamp — the
	// refresh control and a credential save must never be absorbed.
	atomic.StoreInt32(&truncate, 0)
	wrote, err = r.RunOrg(ctx, "org-1", true)
	if err != nil {
		t.Fatalf("RunOrg (forced): %v", err)
	}
	if !wrote {
		t.Error("a forced refresh of a completable listing did not write")
	}
	if atomic.LoadInt32(&mirror.replaced) != 1 {
		t.Errorf("mirror replaced %d times; want 1", atomic.LoadInt32(&mirror.replaced))
	}
	if r.recentlyDeclined("org-1") {
		t.Error("a successful write left the declined stamp armed")
	}
}

func TestRunOrgDeclinedBackoffExpiresWithTheTTL(t *testing.T) {
	ctx := context.Background()
	var truncate, pages int32
	atomic.StoreInt32(&truncate, 1)
	srv := userReposServer(t, &truncate, &pages)
	r := patRefresher(&fakeReachMirror{}, srv.URL)

	now := time.Unix(1700000000, 0)
	r.now = func() time.Time { return now }

	if _, err := r.RunOrg(ctx, "org-1", false); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	first := atomic.LoadInt32(&pages)
	if first == 0 {
		t.Fatal("no enumeration ran at all")
	}

	// The gate is a window, not a latch: once the TTL has elapsed, a non-forced
	// kick enumerates again — same cadence a written mirror's staleness gives.
	now = now.Add(r.ttl)
	if _, err := r.RunOrg(ctx, "org-1", false); err != nil {
		t.Fatalf("RunOrg (after TTL): %v", err)
	}
	if got := atomic.LoadInt32(&pages); got <= first {
		t.Error("the declined stamp outlived the TTL window")
	}
}
