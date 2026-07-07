package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDiscoverGitHub_BoundedConcurrency_MaxInFlight is TFAC-570's core
// acceptance: with N repos and TF_POLL_REPO_CONCURRENCY=4, no more than 4
// per-repo listings are ever in flight at once, and the wall-clock cost is
// close to N/4 rounds of the fake server's per-request delay rather than N
// (fully serial).
func TestDiscoverGitHub_BoundedConcurrency_MaxInFlight(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "4")

	const numRepos = 8
	const delay = 80 * time.Millisecond

	var mu sync.Mutex
	current, maxSeen := 0, 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current++
		if current > maxSeen {
			maxSeen = current
		}
		mu.Unlock()

		time.Sleep(delay)

		mu.Lock()
		current--
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	repos := make([]string, numRepos)
	for i := range repos {
		repos[i] = fmt.Sprintf("octo/repo%d", i)
	}
	if err := stores.Repos.SetConfigured(ctx, org, repos); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	tr := &Tracker{repos: stores.Repos, orgID: org}
	client := ghclient.NewClient(srv.URL, "tok")

	start := time.Now()
	if _, _, err := tr.discoverGitHub(ctx, client, "", repos); err != nil {
		t.Fatalf("discoverGitHub: %v", err)
	}
	elapsed := time.Since(start)

	if maxSeen != 4 {
		t.Errorf("max concurrent in-flight repo listings = %d; want exactly 4 (TF_POLL_REPO_CONCURRENCY=4)", maxSeen)
	}

	// 8 repos at concurrency 4 is 2 rounds of `delay`; serial would be 8
	// rounds. Generous tolerance for scheduling jitter — this is a sanity
	// check, not a precise timing assertion (the max-in-flight check above is
	// the precise one).
	if elapsed > delay*6 {
		t.Errorf("elapsed = %v; want well under the serial cost of %v at concurrency 4", elapsed, delay*numRepos)
	}
}

// TestDiscoverGitHub_ConcurrencyOne_IsFullySerial pins the other end of the
// knob: TF_POLL_REPO_CONCURRENCY=1 must reproduce today's one-at-a-time
// behavior exactly — never more than one repo listing in flight.
func TestDiscoverGitHub_ConcurrencyOne_IsFullySerial(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")

	const numRepos = 5
	var mu sync.Mutex
	current, maxSeen := 0, 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current++
		if current > maxSeen {
			maxSeen = current
		}
		mu.Unlock()

		time.Sleep(10 * time.Millisecond)

		mu.Lock()
		current--
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	repos := make([]string, numRepos)
	for i := range repos {
		repos[i] = fmt.Sprintf("octo/repo%d", i)
	}
	if err := stores.Repos.SetConfigured(ctx, org, repos); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	tr := &Tracker{repos: stores.Repos, orgID: org}
	client := ghclient.NewClient(srv.URL, "tok")

	if _, _, err := tr.discoverGitHub(ctx, client, "", repos); err != nil {
		t.Fatalf("discoverGitHub: %v", err)
	}

	if maxSeen != 1 {
		t.Errorf("max concurrent in-flight repo listings = %d; want exactly 1 (TF_POLL_REPO_CONCURRENCY=1 must be fully serial)", maxSeen)
	}
}

// TestDiscoverGitHub_HangingRepoDoesNotBlockOthers pins the "one repo
// failing/hanging must not starve the others" invariant: a repo whose
// server never responds (until the caller's ctx times out) must not block
// the other repos in the fan-out from completing and being discovered.
func TestDiscoverGitHub_HangingRepoDoesNotBlockOthers(t *testing.T) {
	// Concurrency wide enough that every repo (including the hanging one)
	// gets its own goroutine slot immediately — the hang must not starve a
	// sibling that's queued behind it either.
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "8")

	repos := []string{"octo/hangs", "octo/ok1", "octo/ok2", "octo/ok3"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/hangs/") {
			// Never respond on its own — only unblocks when the request's
			// context is canceled (the test's ctx timeout below).
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openPRBody))
	}))
	t.Cleanup(srv.Close)

	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	if err := stores.Repos.SetConfigured(context.Background(), org, repos); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	tr := &Tracker{repos: stores.Repos, orgID: org}
	client := ghclient.NewClient(srv.URL, "tok")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	var discovered []ghclient.DiscoveredPR
	var discoverErr error
	go func() {
		discovered, _, discoverErr = tr.discoverGitHub(ctx, client, "", repos)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("discoverGitHub did not return — the hanging repo blocked the others")
	}

	if discoverErr != nil {
		t.Fatalf("discoverGitHub: %v", discoverErr)
	}
	if len(discovered) != 3 {
		t.Fatalf("discovered %d PRs; want 3 (the hanging repo's PR must not appear, the other 3 must)", len(discovered))
	}
	for _, d := range discovered {
		if d.Snapshot.Repo == "octo/hangs" {
			t.Errorf("hanging repo's PR was discovered despite its request never completing")
		}
	}
}

// TestDiscoverGitHub_DeterministicOrderAcrossConcurrency is the TFAC-570
// regression pin: the discovered-PR order (and therefore the downstream
// entity-creation / event-emission order) is determined by the repos slice's
// original order, not by which goroutine happens to finish first — so
// TF_POLL_REPO_CONCURRENCY=1 (today's serial order) and a wider concurrency
// produce byte-identical sequences on the same fixture.
func TestDiscoverGitHub_DeterministicOrderAcrossConcurrency(t *testing.T) {
	// Deliberately out of alphabetical order, and with an artificial
	// per-repo delay inversely proportional to position, so a naive
	// completion-order merge (instead of original-repo-order) would
	// reorder the result under concurrency > 1.
	repos := []string{"octo/charlie", "octo/alpha", "octo/bravo"}
	delays := map[string]time.Duration{
		"octo/charlie": 60 * time.Millisecond, // slowest, listed first
		"octo/alpha":   30 * time.Millisecond,
		"octo/bravo":   0,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for repo, d := range delays {
			if strings.Contains(r.URL.Path, "/"+strings.TrimPrefix(repo, "octo/")+"/") {
				time.Sleep(d)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openPRBody))
	}))
	t.Cleanup(srv.Close)

	run := func(t *testing.T, concurrency string) []string {
		t.Helper()
		t.Setenv("TF_POLL_REPO_CONCURRENCY", concurrency)

		ctx := context.Background()
		database := newMigratedSQLite(t)
		stores := sqlitestore.New(database)
		org := runmode.LocalDefaultOrgID
		if err := stores.Repos.SetConfigured(ctx, org, repos); err != nil {
			t.Fatalf("SetConfigured: %v", err)
		}

		tr := &Tracker{repos: stores.Repos, orgID: org}
		client := ghclient.NewClient(srv.URL, "tok")

		discovered, _, err := tr.discoverGitHub(ctx, client, "", repos)
		if err != nil {
			t.Fatalf("discoverGitHub: %v", err)
		}
		order := make([]string, len(discovered))
		for i, d := range discovered {
			order[i] = d.Snapshot.Repo
		}
		return order
	}

	wantOrder := []string{"octo/charlie", "octo/alpha", "octo/bravo"}

	t.Run("concurrency=1", func(t *testing.T) {
		got := run(t, "1")
		assertOrderEqual(t, got, wantOrder)
	})
	t.Run("concurrency=3", func(t *testing.T) {
		got := run(t, "3")
		assertOrderEqual(t, got, wantOrder)
	})
}

func assertOrderEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v; want %v", got, want)
		}
	}
}

// TestRefreshGitHub_RateLimitStopsFanOutAndPropagatesDistinctly is TFAC-570's
// acceptance criterion 4: a repo whose refresh hits the exhausted-budget
// typed error (ghclient.ErrRateLimited) must stop the fan-out from hammering
// the remaining repos, and RefreshGitHub must surface that error distinctly
// (errors.As-able) rather than swallowing it like an ordinary per-repo
// failure.
func TestRefreshGitHub_RateLimitStopsFanOutAndPropagatesDistinctly(t *testing.T) {
	const numRepos = 10
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "3")

	var calls atomic.Int32
	resetAt := time.Now().Add(time.Hour) // far beyond maxRateLimitWait: no retry-sleep, immediate ErrRateLimited

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt.Unix()))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 127.0.0.1."}`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	repos := make([]string, numRepos)
	for i := range repos {
		repos[i] = fmt.Sprintf("octo/repo%d", i)
	}
	if err := stores.Repos.SetConfigured(ctx, org, repos); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, org)
	client := ghclient.NewClient(srv.URL, "tok")

	_, err := tr.RefreshGitHub(ctx, client, "", repos, nil)

	var rl *ghclient.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("RefreshGitHub error = %v; want a *ghclient.ErrRateLimited (propagated distinctly)", err)
	}

	// Every repo's request would have hit the identical 403; the fan-out
	// must have stopped queuing new repos once the first one signaled
	// exhaustion rather than dispatching all numRepos.
	if got := calls.Load(); got >= numRepos {
		t.Errorf("upstream requests = %d; want fewer than %d (fan-out should stop once rate-limited, not hammer every repo)", got, numRepos)
	}

	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 0 {
		t.Errorf("active github entities = %d; want 0 (every repo hit the rate limit before discovering anything)", len(ents))
	}
}

// TestRefreshGitHub_RateLimitSeedsAlreadyDiscoveredReposBeforeStopping guards
// against a real regression found in review: RefreshGitHub must not discard
// the repos that already succeeded before a sibling hit ErrRateLimited.
// Those repos' pulls-etag was already persisted (recordPullsPoll runs inline
// per-repo, on success, inside discoverGitHub) — if RefreshGitHub bailed out
// before running the entity-seeding loop over them, their PRs would never
// become entities, yet the next cycle would see an unchanged (304) listing
// and never get a second chance to discover them. So: repos before the
// rate-limited one must still be seeded; repos at or after it must not be.
func TestRefreshGitHub_RateLimitSeedsAlreadyDiscoveredReposBeforeStopping(t *testing.T) {
	// Concurrency=1 makes discovery fully serial and this test's outcome
	// deterministic: ok1/ok2 are guaranteed to complete before "limited" is
	// even attempted, and ok3/ok4 are guaranteed to never be seeded (either
	// never dispatched, or dispatched but rejected by the pre-flight budget
	// check that "limited"'s response already tripped).
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")

	repos := []string{"octo/ok1", "octo/ok2", "octo/limited", "octo/ok3", "octo/ok4"}
	resetAt := time.Now().Add(time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/limited/") {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt.Unix()))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 127.0.0.1."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openPRBody))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	if err := stores.Repos.SetConfigured(ctx, org, repos); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, org)
	client := ghclient.NewClient(srv.URL, "tok")

	_, err := tr.RefreshGitHub(ctx, client, "", repos, nil)

	var rl *ghclient.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("RefreshGitHub error = %v; want *ghclient.ErrRateLimited", err)
	}

	ents, lerr := stores.Entities.ListActiveSystem(ctx, org, "github")
	if lerr != nil {
		t.Fatalf("ListActiveSystem: %v", lerr)
	}
	seeded := make(map[string]bool, len(ents))
	for _, e := range ents {
		seeded[e.SourceID] = true
	}

	for _, want := range []string{"octo/ok1#42", "octo/ok2#42"} {
		if !seeded[want] {
			t.Errorf("entity %q was not seeded; a repo that succeeded before rate-limiting must still be discovered this cycle", want)
		}
	}
	for _, notWant := range []string{"octo/ok3#42", "octo/ok4#42"} {
		if seeded[notWant] {
			t.Errorf("entity %q was seeded; repos at/after the rate-limited one must not be reached", notWant)
		}
	}
}
