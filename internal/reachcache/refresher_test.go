package reachcache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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

	// stateHosts and replacedHosts record the host each state read and each
	// PAT replace was scoped to, and replacedRows the rows of the latest
	// replace. Guarded by mu: the runner tests drive RunOrg from goroutines.
	mu            sync.Mutex
	stateHosts    []string
	replacedHosts []string
	replacedRows  []domain.ReachableRepository
}

func (m *fakeReachMirror) ReachableStateSystem(_ context.Context, _ string, host string, _ domain.GitHubCredentialClass) (domain.ReachableCacheState, error) {
	m.mu.Lock()
	m.stateHosts = append(m.stateHosts, host)
	m.mu.Unlock()
	return m.state, nil
}

func (m *fakeReachMirror) ReplaceForPATSystem(_ context.Context, _ string, host string, rows []domain.ReachableRepository) error {
	atomic.AddInt32(&m.replaced, 1)
	m.mu.Lock()
	m.replacedHosts = append(m.replacedHosts, host)
	m.replacedRows = rows
	m.mu.Unlock()
	return nil
}

// fakeResolver hands back a real *github.Client pointed at the test server, so
// the walk under test is the production one. A non-nil err is what ClientFor
// answers instead.
type fakeResolver struct {
	github.Resolver
	base string
	err  error
}

func (f fakeResolver) BaseURLFor(context.Context, string) (string, error) { return f.base, nil }

func (f fakeResolver) ClientFor(context.Context, string, string) (*github.Client, error) {
	if f.err != nil {
		return nil, f.err
	}
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

// Expired stamps must be removed, not just ignored: entries only ever appear
// on a decline, and an org that never refreshes again never revisits its own —
// so without pruning, a long-lived process would grow the map with every org
// that ever declined. Removal has two doors, and each gets pinned: the read
// that finds a stamp expired deletes it, and a decline for any org sweeps
// every other org's expired stamp.
func TestDeclinedStampsAreRemovedOnceExpired(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := &Refresher{
		declined: make(map[string]time.Time),
		ttl:      time.Hour,
		now:      func() time.Time { return now },
	}

	r.markDeclined("org-1")
	now = now.Add(r.ttl)
	if r.recentlyDeclined("org-1") {
		t.Fatal("stamp outlived the TTL window")
	}
	if _, ok := r.declined["org-1"]; ok {
		t.Error("recentlyDeclined left an expired stamp behind")
	}

	r.markDeclined("org-1")
	now = now.Add(r.ttl)
	r.markDeclined("org-2")
	if _, ok := r.declined["org-1"]; ok {
		t.Error("another org's decline left an expired stamp behind")
	}
	if !r.recentlyDeclined("org-2") {
		t.Error("the sweep removed the fresh stamp it was stamping")
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

// Both App classes refresh through the grant reconcile, and neither reaches the
// PAT enumeration. A managed workspace routed to the PAT arm would enumerate
// GET /user/repos against a credential it does not have; one routed nowhere at
// all would announce a refresh that ran nothing.
func TestRunOrgAppClassesRefreshThroughTheGrantReconcile(t *testing.T) {
	ctx := context.Background()
	for _, class := range domain.AppTierCredentialClasses() {
		t.Run(string(class), func(t *testing.T) {
			var grants int
			r := NewRefresher(
				NewClassResolver(
					fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: class}},
					fakeApps{app: &domain.OrgGitHubApp{Active: true}},
				),
				&fakeReachMirror{},
				// A resolver with no credential, so the PAT arm — if it were taken —
				// would decline rather than write, and the write assertion below
				// would catch the misrouting.
				fakeResolver{err: github.ErrNoGitHubCredentials},
				func(context.Context, string) error { grants++; return nil },
				nil,
			)

			wrote, err := r.RunOrg(ctx, "org-1", true)
			if err != nil {
				t.Fatalf("RunOrg: %v", err)
			}
			if !wrote {
				t.Error("a forced App-tier refresh reported no write")
			}
			if grants != 1 {
				t.Errorf("grant reconcile ran %d times; want 1", grants)
			}
		})
	}
}

// A PAT refresh is scoped to the org's current GitHub host: the state read
// that gates it and the rows it writes are both under the host org_settings
// resolves to — not the resolver's base, which can fall back to the URL stored
// beside the PAT — so every read of the mirror, which asks about the current
// host, sees what the refresh wrote.
func TestRunOrgPATRefreshWritesUnderTheSettingsHost(t *testing.T) {
	ctx := context.Background()
	var truncate, pages int32
	srv := userReposServer(t, &truncate, &pages)
	mirror := &fakeReachMirror{}
	r := NewRefresher(
		NewClassResolver(fakeOrgs{settings: domain.OrgSettings{
			GitHubCredentialClass: domain.GitHubCredentialClassPAT,
			GitHubBaseURL:         "https://ghe.example.com/",
		}}, nil),
		mirror,
		fakeResolver{base: srv.URL},
		nil,
		nil,
	)

	wrote, err := r.RunOrg(ctx, "org-1", true)
	if err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if !wrote {
		t.Fatal("a completable PAT enumeration did not write")
	}
	const want = "https://ghe.example.com"
	if len(mirror.stateHosts) != 1 || mirror.stateHosts[0] != want {
		t.Errorf("state read on hosts %q; want exactly [%q]", mirror.stateHosts, want)
	}
	if len(mirror.replacedHosts) != 1 || mirror.replacedHosts[0] != want {
		t.Errorf("PAT rows replaced on hosts %q; want exactly [%q]", mirror.replacedHosts, want)
	}
	if len(mirror.replacedRows) == 0 {
		t.Error("the replace carried no rows")
	}
}

// An org with no github_base_url is on the deployment's default host, and its
// PAT rows land there rather than under an empty host no read matches.
func TestRunOrgPATRefreshDefaultsToTheDeploymentHost(t *testing.T) {
	ctx := context.Background()
	var truncate, pages int32
	srv := userReposServer(t, &truncate, &pages)
	mirror := &fakeReachMirror{}
	r := patRefresher(mirror, srv.URL)

	if _, err := r.RunOrg(ctx, "org-1", true); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	want := db.EffectiveGitHubHost("")
	if len(mirror.replacedHosts) != 1 || mirror.replacedHosts[0] != want {
		t.Errorf("PAT rows replaced on hosts %q; want exactly [%q]", mirror.replacedHosts, want)
	}
}

// A PAT validated on another host is refused by the resolver before any
// request is made. The refresh reads that as a decline — nothing on the
// current host is known until the PAT is rebound there — so it writes no rows,
// returns no error, and arms the backoff so the picker's polling does not
// re-run it on every read.
func TestRunOrgPATHostMismatchIsADecline(t *testing.T) {
	ctx := context.Background()
	mirror := &fakeReachMirror{}
	mismatch := &github.PATHostMismatchError{OrgID: "org-1", BoundHost: "https://ghe.old.example.com", CurrentHost: "https://github.com"}
	r := NewRefresher(
		NewClassResolver(fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: domain.GitHubCredentialClassPAT}}, nil),
		mirror,
		fakeResolver{err: mismatch},
		nil,
		nil,
	)

	wrote, err := r.RunOrg(ctx, "org-1", true)
	if err != nil {
		t.Fatalf("RunOrg: %v; a PAT bound on another host is a decline, not an error", err)
	}
	if wrote {
		t.Error("RunOrg reported a write for a PAT it could not use")
	}
	if got := atomic.LoadInt32(&mirror.replaced); got != 0 {
		t.Errorf("mirror replaced %d times; want 0", got)
	}
	if !r.recentlyDeclined("org-1") {
		t.Error("the decline did not arm the backoff; every picker poll would re-run the refresh")
	}

	wrote, err = r.RunOrg(ctx, "org-1", false)
	if err != nil || wrote {
		t.Errorf("a non-forced kick inside the window = (%v, %v); want (false, nil)", wrote, err)
	}
}
