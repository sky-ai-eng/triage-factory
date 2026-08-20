package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestFirstUnreachableRepo(t *testing.T) {
	reachable := map[string]struct{}{
		"owner/api": {},
		"owner/web": {},
	}
	repo := func(o, r string) domain.TeamGitHubRepo { return domain.TeamGitHubRepo{Owner: o, Repo: r} }

	t.Run("fail-open when not checked", func(t *testing.T) {
		// checked=false must never reject, regardless of the input — this
		// is the GitHub-unreachable / no-credentials path.
		if slug, reject := firstUnreachableRepo(nil, false, []domain.TeamGitHubRepo{repo("ghost", "repo")}); reject {
			t.Errorf("unchecked set must not reject; got reject on %q", slug)
		}
	})

	t.Run("rejects first repo absent from a checked set", func(t *testing.T) {
		slug, reject := firstUnreachableRepo(reachable, true, []domain.TeamGitHubRepo{
			repo("owner", "api"),
			repo("owner", "ghost"),
		})
		if !reject || slug != "owner/ghost" {
			t.Errorf("got (%q, %v); want (owner/ghost, true)", slug, reject)
		}
	})

	t.Run("all-present passes", func(t *testing.T) {
		if slug, reject := firstUnreachableRepo(reachable, true, []domain.TeamGitHubRepo{repo("owner", "api"), repo("owner", "web")}); reject {
			t.Errorf("all-reachable input must pass; got reject on %q", slug)
		}
	})

	t.Run("owner/repo matched case-insensitively", func(t *testing.T) {
		if slug, reject := firstUnreachableRepo(reachable, true, []domain.TeamGitHubRepo{repo("Owner", "API")}); reject {
			t.Errorf("case-variant of a reachable repo must pass; got reject on %q", slug)
		}
	})
}

// TestFirstUnreachableViaFanOut_MixedOutcomes drives the cold-path
// orchestration with an injected check covering the four response shapes
// (200 / 404 / 403 / 5xx). 200 passes, 404 and 403 are conclusive rejections,
// and a 5xx (indeterminate) fails open — never rejected. The reported slug is
// deterministic (first in input order) regardless of completion order.
func TestFirstUnreachableViaFanOut_MixedOutcomes(t *testing.T) {
	repo := func(o, r string) domain.TeamGitHubRepo { return domain.TeamGitHubRepo{Owner: o, Repo: r} }

	// Maps each slug to (reachable, conclusive) — the shape CheckRepoAccess
	// returns for 200 / 404 / 403 / 5xx respectively.
	outcomes := map[string][2]bool{
		"owner/ok":      {true, true},   // 200
		"owner/missing": {false, true},  // 404
		"owner/forbid":  {false, true},  // 403
		"owner/flaky":   {false, false}, // 5xx → indeterminate
	}
	var calls atomic.Int32
	check := func(_ context.Context, r domain.TeamGitHubRepo) (bool, bool) {
		calls.Add(1)
		o := outcomes[r.Slug()]
		return o[0], o[1]
	}

	t.Run("all reachable or flaky passes", func(t *testing.T) {
		if slug, reject := firstUnreachableViaFanOut(context.Background(),
			[]domain.TeamGitHubRepo{repo("owner", "ok"), repo("owner", "flaky")}, 20, check); reject {
			t.Errorf("reachable+indeterminate must pass; got reject on %q", slug)
		}
	})

	t.Run("404 rejected", func(t *testing.T) {
		slug, reject := firstUnreachableViaFanOut(context.Background(),
			[]domain.TeamGitHubRepo{repo("owner", "ok"), repo("owner", "missing")}, 20, check)
		if !reject || slug != "owner/missing" {
			t.Errorf("got (%q,%v); want (owner/missing,true)", slug, reject)
		}
	})

	t.Run("403 rejected", func(t *testing.T) {
		slug, reject := firstUnreachableViaFanOut(context.Background(),
			[]domain.TeamGitHubRepo{repo("owner", "forbid")}, 20, check)
		if !reject || slug != "owner/forbid" {
			t.Errorf("got (%q,%v); want (owner/forbid,true)", slug, reject)
		}
	})

	t.Run("first unreachable in input order is reported", func(t *testing.T) {
		// Two unreachable; the earlier-in-input one must be named even though
		// the fan-out completes concurrently.
		slug, reject := firstUnreachableViaFanOut(context.Background(),
			[]domain.TeamGitHubRepo{repo("owner", "ok"), repo("owner", "forbid"), repo("owner", "missing")}, 20, check)
		if !reject || slug != "owner/forbid" {
			t.Errorf("got (%q,%v); want (owner/forbid,true)", slug, reject)
		}
	})

	t.Run("flaky alone fails open", func(t *testing.T) {
		if _, reject := firstUnreachableViaFanOut(context.Background(),
			[]domain.TeamGitHubRepo{repo("owner", "flaky")}, 20, check); reject {
			t.Error("an all-indeterminate selection must fail open")
		}
	})
}

// TestTeamReposPut_MirrorIsAuthoritative proves the mirror decides membership:
// with only owner/tracked reachable, a PUT of owner/ghost is rejected even
// though the per-repo probe (cold path) would have accepted it. Asserting 400
// proves the gate consulted the mirror rather than re-probing.
func TestTeamReposPut_MirrorIsAuthoritative(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The enumeration the mirror is built from lists only owner/tracked.
		if r.URL.Path == "/api/v3/user/repos" {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"full_name":"owner/tracked"}]`))
			return
		}
		// Every per-repo probe would say reachable — so a cold-path fall-through
		// would wrongly accept owner/ghost. The mirror must win.
		if strings.HasPrefix(r.URL.Path, "/api/v3/repos/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"full_name":"x"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: ts.URL,
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	wireReachRefresh(t, srv).refresh(t, runmode.LocalDefaultOrgID, true)

	// Mirrored, and owner/ghost is not in it → 400.
	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"owner/ghost"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unreachable PUT against a fresh mirror = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "owner/ghost") {
		t.Errorf("rejection should name the offending repo; body=%s", body)
	}

	// And a slug that IS mirrored is accepted without a probe.
	if rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"owner/tracked"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("reachable PUT against a fresh mirror = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamReposPut_ColdPathFailsOpenOn5xx proves the cold-path fan-out keeps
// the documented fail-open posture: when the per-repo probe 5xxs (GitHub
// outage), the write proceeds rather than coupling write availability to
// GitHub's uptime. No cache is warmed, so the PUT takes the cold path.
func TestTeamReposPut_ColdPathFailsOpenOn5xx(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var probed atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user/repos" {
			// Empty enumeration — irrelevant here, the test never warms the cache.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v3/repos/") {
			probed.Store(true)
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: ts.URL,
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"owner/whatever"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("cold-path 5xx PUT = %d, want 200 (fail-open); body=%s", rec.Code, rec.Body.String())
	}
	if !probed.Load() {
		t.Error("expected the cold path to actually probe the per-repo endpoint")
	}
}

// fakeReposServer stands in for GitHub, serving the subset of endpoints the
// repo-tracking gate touches against a fixed reachable set (the given
// full_names):
//
//   - GET /user/repos — the picker enumeration (cache-warm path). Returns the
//     full_names on page 1, an empty page afterward so the paginator stops.
//   - GET /repos/{owner}/{repo} — the cold-path reachability probe
//     (CheckRepoAccess). 200 when the slug is in the reachable set, 404
//     otherwise — exactly the signal the per-repo fan-out classifies.
//
// Previously the gate only used the enumeration; now a cache-miss PUT probes
// per-repo, so the helper answers both. The test *cases* are unchanged: they
// still pass a reachable set and assert reachable→200 / unreachable→400.
func fakeReposServer(t *testing.T, fullNames ...string) *httptest.Server {
	t.Helper()
	reachable := make(map[string]bool, len(fullNames))
	for _, fn := range fullNames {
		reachable[strings.ToLower(fn)] = true
	}
	writeRepoJSON := func(w http.ResponseWriter, fullName string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ghclient.UserRepo{FullName: fullName})
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Picker enumeration (cache-warm path).
		if r.URL.Path == "/api/v3/user/repos" {
			w.Header().Set("Content-Type", "application/json")
			page := []ghclient.UserRepo{}
			if r.URL.Query().Get("page") == "1" {
				for _, fn := range fullNames {
					page = append(page, ghclient.UserRepo{FullName: fn})
				}
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		// Per-repo reachability probe (cold-path fan-out).
		if slug, ok := strings.CutPrefix(r.URL.Path, "/api/v3/repos/"); ok && slug != "" {
			if reachable[strings.ToLower(slug)] {
				writeRepoJSON(w, slug)
				return
			}
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestTeamReposPut_RejectsUnreachableRepo proves the write-time guard:
// with credentials that can reach owner/tracked but not owner/ghost, the
// PUT accepts the former and 400s the latter.
func TestTeamReposPut_RejectsUnreachableRepo(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ts := fakeReposServer(t, "owner/tracked")
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: ts.URL,
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	// Reachable repo → accepted.
	if rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"owner/tracked"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("reachable repo PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Unreachable repo → rejected before any write.
	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"owner/tracked", "owner/ghost"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unreachable repo PUT = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "owner/ghost") {
		t.Errorf("rejection should name the offending repo; body=%s", body)
	}
}

// TestTeamReposPut_StaleTrackedRepoStaysRemovable proves the guard only
// checks NEWLY-ADDED repos: a repo tracked while reachable that later
// becomes unreachable (creds revoked, App uninstalled) must not wedge the
// picker. The user has to be able to save a corrected set — including one
// that still carries the stale repo, or that drops it — without the guard
// 400ing on a repo it can no longer reach.
func TestTeamReposPut_StaleTrackedRepoStaysRemovable(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	// Phase 1: creds can reach owner/stale + owner/other → track both.
	wide := fakeReposServer(t, "owner/stale", "owner/other")
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: wide.URL,
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed wide creds: %v", err)
	}
	if rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"owner/stale", "owner/other"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("initial track PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Phase 2: access to owner/stale is revoked — creds now reach only
	// owner/other (and a new owner/fresh the user wants to add).
	narrow := fakeReposServer(t, "owner/other", "owner/fresh")
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: narrow.URL,
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed narrow creds: %v", err)
	}

	// Re-saving the current set (still carrying the now-unreachable
	// owner/stale) must succeed — it's already tracked, so it's not
	// reachability-checked.
	if rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"owner/stale", "owner/other"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("re-save with stale tracked repo = %d, want 200 (already tracked, not re-checked); body=%s", rec.Code, rec.Body.String())
	}

	// Dropping owner/stale while ADDING a reachable owner/fresh must
	// succeed: the only newly-added repo (owner/fresh) is reachable.
	if rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"owner/other", "owner/fresh"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("drop-stale + add-reachable PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Control: a NEWLY-added unreachable repo is still rejected — the guard
	// didn't go slack, it just scopes to additions.
	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"owner/other", "owner/ghost"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("newly-added unreachable PUT = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "owner/ghost") {
		t.Errorf("rejection should name the offending repo; body=%s", body)
	}
}

// TestTeamReposPut_FailsOpenWithoutCredentials proves the guard never
// blocks a write when it can't enumerate the reachable set — no
// credentials configured means checked=false, so the save proceeds.
func TestTeamReposPut_FailsOpenWithoutCredentials(t *testing.T) {
	srv := newTestServer(t)
	// No credentials seeded → fanOutUnreachableRepo returns checked=false.
	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-repos", map[string]any{
		"repos": []string{"anyone/anything"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("no-credential PUT = %d, want 200 (fail-open); body=%s", rec.Code, rec.Body.String())
	}
}
