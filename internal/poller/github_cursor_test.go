package poller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRotateFromCursor pins the round-robin rotation helper (TFAC-571): it
// starts iteration at the cursor's repo and wraps around, and is
// churn-tolerant — a cursor naming a repo no longer in the list (removed
// from the tracked set between cycles) falls back to the head rather than
// panicking or skipping the whole list.
func TestRotateFromCursor(t *testing.T) {
	repos := []string{"a", "b", "c", "d"}

	tests := []struct {
		name   string
		cursor string
		want   []string
	}{
		{"no cursor starts at head", "", []string{"a", "b", "c", "d"}},
		{"cursor at head is a no-op", "a", []string{"a", "b", "c", "d"}},
		{"cursor mid-list rotates", "c", []string{"c", "d", "a", "b"}},
		{"cursor at tail rotates fully", "d", []string{"d", "a", "b", "c"}},
		{"cursor removed from tracked set falls back to head", "removed", []string{"a", "b", "c", "d"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rotateFromCursor(repos, tt.cursor)
			if len(got) != len(tt.want) {
				t.Fatalf("rotateFromCursor(%v, %q) = %v; want %v", repos, tt.cursor, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("rotateFromCursor(%v, %q) = %v; want %v", repos, tt.cursor, got, tt.want)
				}
			}
		})
	}
}

// TestRotateFromCursor_EmptyRepos guards the degenerate case: an empty
// tracked set must not panic (the cursor search loop simply never matches).
func TestRotateFromCursor_EmptyRepos(t *testing.T) {
	got := rotateFromCursor(nil, "anything")
	if len(got) != 0 {
		t.Errorf("rotateFromCursor(nil, ...) = %v; want empty", got)
	}
}

// freshClientPerCallResolver mints a brand-new *ghclient.Client on every
// ClientFor call, matching production's resolver (internal/github/resolver.go
// constructs NewClient(...) fresh per call — App installation tokens expire
// hourly, so the client is never hoisted across cycles). This matters for
// TestPollGitHubOnce_RateLimitCursorResumesAcrossCycles: reusing one client
// across cycles would carry over its rate-limit memory (awaitBudget's
// pre-flight check) and keep refusing requests until the real reset time
// passed, which the test can't wait out.
type freshClientPerCallResolver struct {
	url string
}

func (r *freshClientPerCallResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	return ghclient.NewClient(r.url, "pat"), nil
}

func (r *freshClientPerCallResolver) ClientForRepo(ctx context.Context, orgID, owner, repo string) (*ghclient.Client, error) {
	return r.ClientFor(ctx, orgID, owner)
}

func (r *freshClientPerCallResolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	return githubapp.Token{}, nil
}

func (r *freshClientPerCallResolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	return ghbase.DefaultBaseURL, nil
}

func (r *freshClientPerCallResolver) OrgIdentityFor(ctx context.Context, orgID string) (string, string, bool) {
	return "", "", false
}

// TestPollGitHubOnce_RateLimitCursorResumesAcrossCycles is TFAC-571's core
// acceptance criterion: a poll cycle that can't finish (rate budget
// exhausted) must resume from where it left off next cycle rather than
// restarting from the head of the repo list — otherwise, on a large tracked
// set, repos early in the list get refreshed every cycle while repos at the
// tail may never complete a first refresh.
//
// Fixture: 5 repos (a..e), concurrency pinned to 1 for a deterministic
// dispatch order. The fake server is ETag-aware (mirrors real GitHub's
// conditional open-PR listing): a repo's first successful listing issues an
// ETag, and any later request carrying a matching If-None-Match gets a cheap
// 304 rather than a second full listing — so "every repo refreshed exactly
// once" means exactly one full (non-304) listing per repo across both
// cycles, and repos that already succeeded are merely cheap-confirmed when
// the rotation wraps back around to them (per the tracker's own "304s are
// free on the primary rate limit" discovery design).
//
// Cycle 1's server allows a, b, c to list successfully, then 403s repo d as
// an exhausted primary rate-limit budget (reset far beyond the client's
// retry-wait ceiling, so it surfaces immediately as ErrRateLimited instead of
// sleeping it out) — e is never even reached (concurrency=1 means the
// fan-out stops queuing the moment d fails). Cycle 2 flips the fake server to
// allow d and — because PollGitHubOnce bypasses the scheduler entirely —
// runs immediately rather than waiting out real time; it must start at the
// saved cursor (d), then wrap around through e, a, b, c to complete.
func TestPollGitHubOnce_RateLimitCursorResumesAcrossCycles(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	runmode.SetForTest(t, runmode.ModeMulti)

	repos := []string{"octo/a", "octo/b", "octo/c", "octo/d", "octo/e"}
	resetAt := time.Now().Add(time.Hour) // beyond maxRateLimitWait — immediate ErrRateLimited, no retry-sleep

	var mu sync.Mutex
	etags := map[string]string{}    // repo -> issued ETag, once it's listed successfully
	fullFetches := map[string]int{} // non-304 listings per repo — must end at 1 for every repo
	var fetchOrder []string         // order of first successful listing, across both cycles
	var allowD atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			_, _ = w.Write([]byte(`{"data":{"nodes":[]}}`))
			return
		}
		repo, ok := repoFromPullsPath(r.URL.Path)
		if !ok {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}

		mu.Lock()
		etag, hasEtag := etags[repo]
		mu.Unlock()
		if hasEtag && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		if repo == "octo/d" && !allowD.Load() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt.Unix()))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 127.0.0.1."}`))
			return
		}

		newEtag := fmt.Sprintf(`"%s-v1"`, repo)
		mu.Lock()
		etags[repo] = newEtag
		fullFetches[repo]++
		fetchOrder = append(fetchOrder, repo)
		mu.Unlock()
		w.Header().Set("ETag", newEtag)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, repos)

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	m := &Manager{
		database: database,
		pub:      busPublisher{bus: bus},
		tasks:    stores.Tasks,
		entities: stores.Entities,
		repos:    stores.Repos,
		resolver: &freshClientPerCallResolver{url: srv.URL},
	}

	// Cycle 1: a, b, c succeed; d rate-limits; e is never reached.
	m.PollGitHubOnce(ctx, org)

	mu.Lock()
	cycle1Fetches := map[string]int{"octo/a": fullFetches["octo/a"], "octo/b": fullFetches["octo/b"], "octo/c": fullFetches["octo/c"], "octo/d": fullFetches["octo/d"], "octo/e": fullFetches["octo/e"]}
	mu.Unlock()
	for _, r := range []string{"octo/a", "octo/b", "octo/c"} {
		if cycle1Fetches[r] != 1 {
			t.Errorf("cycle 1 full fetches[%s] = %d; want 1", r, cycle1Fetches[r])
		}
	}
	if cycle1Fetches["octo/d"] != 0 {
		t.Errorf("cycle 1 full fetches[octo/d] = %d; want 0 (the attempt was rate-limited, not a successful listing)", cycle1Fetches["octo/d"])
	}
	if cycle1Fetches["octo/e"] != 0 {
		t.Errorf("cycle 1 full fetches[octo/e] = %d; want 0 (never reached — fan-out stopped at d)", cycle1Fetches["octo/e"])
	}

	if got := m.githubCursor(org); got != "octo/d" {
		t.Fatalf("cursor after cycle 1 = %q; want octo/d", got)
	}

	// "Rate limit window resets" — d now succeeds too. PollGitHubOnce bypasses
	// the scheduler, so this runs immediately rather than waiting out real time.
	allowD.Store(true)
	m.PollGitHubOnce(ctx, org)

	mu.Lock()
	defer mu.Unlock()
	for _, r := range repos {
		if fullFetches[r] != 1 {
			t.Errorf("full fetches[%s] = %d across both cycles; want exactly 1 (every repo refreshed exactly once)", r, fullFetches[r])
		}
	}
	wantOrder := []string{"octo/a", "octo/b", "octo/c", "octo/d", "octo/e"}
	if len(fetchOrder) != len(wantOrder) {
		t.Fatalf("fetch order = %v; want %v", fetchOrder, wantOrder)
	}
	for i, want := range wantOrder {
		if fetchOrder[i] != want {
			t.Errorf("fetch order = %v; want %v (cycle 2 must start at the cursor (d) and wrap through e, a, b, c)", fetchOrder, wantOrder)
			break
		}
	}

	if got := m.githubCursor(org); got != "" {
		t.Errorf("cursor after cycle 2 = %q; want \"\" (full wrap completed, cursor reset)", got)
	}
}

// TestRunGitHubCycleForOrg_CursorSurvivesRepoRemoval is TFAC-571's
// churn-safety acceptance: the tracked-repo set can change between cycles
// (a config save). If the org's saved cursor names a repo that's no longer
// tracked, the next cycle must resume sanely — no skip-all, no panic —
// rather than getting stuck because rotateFromCursor can't find its anchor.
func TestRunGitHubCycleForOrg_CursorSurvivesRepoRemoval(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	runmode.SetForTest(t, runmode.ModeMulti)

	resetAt := time.Now().Add(time.Hour)
	var mu sync.Mutex
	visited := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			_, _ = w.Write([]byte(`{"data":{"nodes":[]}}`))
			return
		}
		repo, ok := repoFromPullsPath(r.URL.Path)
		if !ok {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		if repo == "octo/d" {
			// d never succeeds — models "d was removed"; if the poller ever
			// tried to reach it again after removal, this would still 200 it
			// fine, but ListConfiguredNames won't hand it back once
			// removed, so a request here would indicate a bug regardless.
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt.Unix()))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 127.0.0.1."}`))
			return
		}
		mu.Lock()
		visited[repo] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	initial := []string{"octo/a", "octo/b", "octo/c", "octo/d", "octo/e"}
	trackRepos(t, stores, org, initial)

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	m := &Manager{
		database: database,
		pub:      busPublisher{bus: bus},
		tasks:    stores.Tasks,
		entities: stores.Entities,
		repos:    stores.Repos,
		resolver: &freshClientPerCallResolver{url: srv.URL},
	}

	// Cycle 1: a, b, c succeed; d rate-limits; e never reached. Cursor saves at d.
	m.PollGitHubOnce(ctx, org)
	if got := m.githubCursor(org); got != "octo/d" {
		t.Fatalf("cursor after cycle 1 = %q; want octo/d", got)
	}

	// Config save between cycles: d is removed from the tracked set entirely.
	remaining := []string{"octo/a", "octo/b", "octo/c", "octo/e"}
	trackRepos(t, stores, org, remaining)

	// Cycle 2 must not panic and must not skip the whole list just because
	// its cursor's anchor repo is gone.
	m.PollGitHubOnce(ctx, org)

	mu.Lock()
	defer mu.Unlock()
	for _, r := range remaining {
		if !visited[r] {
			t.Errorf("repo %s never visited across both cycles; want every currently-tracked repo covered", r)
		}
	}
	if visited["octo/d"] {
		t.Error("octo/d was requested after being removed from the tracked set")
	}
}

// perAccountFailResolver mints a fresh client per call (see
// freshClientPerCallResolver) but fails ClientFor outright for accounts named
// in failAccounts — modeling one App installation's credential being broken
// while others resolve fine.
type perAccountFailResolver struct {
	url          string
	failAccounts map[string]bool
}

func (r *perAccountFailResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	if r.failAccounts[target] {
		return nil, fmt.Errorf("simulated credential failure for %s", target)
	}
	return ghclient.NewClient(r.url, "pat"), nil
}

func (r *perAccountFailResolver) ClientForRepo(ctx context.Context, orgID, owner, repo string) (*ghclient.Client, error) {
	return r.ClientFor(ctx, orgID, owner)
}

func (r *perAccountFailResolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	return githubapp.Token{}, nil
}

func (r *perAccountFailResolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	return ghbase.DefaultBaseURL, nil
}

func (r *perAccountFailResolver) OrgIdentityFor(ctx context.Context, orgID string) (string, string, bool) {
	return "", "", false
}

// TestRunGitHubCycleForOrg_UnresolvedInstallationDoesNotFalselyResetCursor is
// the multi-installation App-path counterpart to the rate-limit cursor test:
// if one installation's credential can't even be resolved this cycle (not a
// rate limit — its repos are simply never reached), the cycle must not be
// treated as a full wrap just because a LATER installation happens to
// succeed. Fixture: two installations, "acme" (broken credential) and "beta"
// (healthy), each owning one configured repo. Cycle 1 must save the cursor at
// acme's repo — not reset it to "" — so a later cycle (once acme's credential
// is fixed) prioritizes retrying it instead of silently never catching up.
func TestRunGitHubCycleForOrg_UnresolvedInstallationDoesNotFalselyResetCursor(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[]}}`))
		case strings.Contains(r.URL.Path, "/installation/repositories"):
			// Only reached by beta's client — acme's ClientFor already
			// failed, so acme's client never makes this call.
			_, _ = w.Write([]byte(`{"total_count": 1, "repositories": [{"full_name": "beta/r1"}]}`))
		case strings.Contains(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	repos := []string{"acme/r1", "beta/r1"}
	trackRepos(t, stores, org, repos)
	seedBYOAppCredentialClass(t, stores, org)

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	m := &Manager{
		database: database,
		pub:      busPublisher{bus: bus},
		tasks:    stores.Tasks,
		entities: stores.Entities,
		repos:    stores.Repos,
		orgs:     stores.Orgs,
		apps: &fakeInstallsStore{
			app: &domain.OrgGitHubApp{OrgID: org, AppID: "1", Active: true},
			installs: []domain.OrgGitHubAppInstallation{
				{InstallationID: "1", AccountLogin: "acme"},
				{InstallationID: "2", AccountLogin: "beta"},
			},
		},
		resolver: &perAccountFailResolver{url: srv.URL, failAccounts: map[string]bool{"acme": true}},
	}

	m.runGitHubCycleForOrg(ctx, org)

	if got := m.githubCursor(org); got != "acme/r1" {
		t.Fatalf("cursor after cycle 1 = %q; want acme/r1 (acme's installation never resolved, so its repo is still outstanding — the cursor must not reset to \"\" just because beta succeeded)", got)
	}

	// "acme's credential gets fixed" — now every installation resolves. The
	// first fake server only ever granted beta/r1 (acme's ClientFor never
	// even reached it in cycle 1), so a second server grants both repos,
	// modeling the fixed-credential installation now covering acme/r1 too.
	// A cycle starting at the saved cursor should fully wrap and reset it.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[]}}`))
		case strings.Contains(r.URL.Path, "/installation/repositories"):
			_, _ = w.Write([]byte(`{"total_count": 2, "repositories": [{"full_name": "acme/r1"}, {"full_name": "beta/r1"}]}`))
		case strings.Contains(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv2.Close)
	m.resolver = &freshClientPerCallResolver{url: srv2.URL}

	m.runGitHubCycleForOrg(ctx, org)

	if got := m.githubCursor(org); got != "" {
		t.Errorf("cursor after cycle 2 = %q; want \"\" (every installation resolved and the wrap completed)", got)
	}
}

// repoFromPullsPath extracts "owner/repo" from a REST open-PR listing path
// (".../repos/{owner}/{repo}/pulls" — NewClient prepends "/api/v3" for a
// non-github.com host, so this searches for the "repos" segment rather than
// assuming it's first); ok is false for any other shape.
func repoFromPullsPath(path string) (repo string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		if p == "repos" && i+3 < len(parts) && parts[i+3] == "pulls" {
			return parts[i+1] + "/" + parts[i+2], true
		}
	}
	return "", false
}
