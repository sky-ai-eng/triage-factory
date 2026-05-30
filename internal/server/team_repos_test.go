package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

// fakeReposServer stands in for GitHub's GET /user/repos, returning the
// given full_names on page 1 and an empty page afterward (so the client's
// paginator terminates).
func fakeReposServer(t *testing.T, fullNames ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user/repos" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[`))
		for i, fn := range fullNames {
			if i > 0 {
				_, _ = w.Write([]byte(`,`))
			}
			_, _ = w.Write([]byte(`{"full_name":"` + fn + `"}`))
		}
		_, _ = w.Write([]byte(`]`))
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
	if rec := doJSON(t, srv, http.MethodPut, "/api/settings/team/default/repos", map[string]any{
		"repos": []string{"owner/tracked"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("reachable repo PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Unreachable repo → rejected before any write.
	rec := doJSON(t, srv, http.MethodPut, "/api/settings/team/default/repos", map[string]any{
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
	if rec := doJSON(t, srv, http.MethodPut, "/api/settings/team/default/repos", map[string]any{
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
	if rec := doJSON(t, srv, http.MethodPut, "/api/settings/team/default/repos", map[string]any{
		"repos": []string{"owner/stale", "owner/other"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("re-save with stale tracked repo = %d, want 200 (already tracked, not re-checked); body=%s", rec.Code, rec.Body.String())
	}

	// Dropping owner/stale while ADDING a reachable owner/fresh must
	// succeed: the only newly-added repo (owner/fresh) is reachable.
	if rec := doJSON(t, srv, http.MethodPut, "/api/settings/team/default/repos", map[string]any{
		"repos": []string{"owner/other", "owner/fresh"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("drop-stale + add-reachable PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Control: a NEWLY-added unreachable repo is still rejected — the guard
	// didn't go slack, it just scopes to additions.
	rec := doJSON(t, srv, http.MethodPut, "/api/settings/team/default/repos", map[string]any{
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
	// No credentials seeded → reachableRepoSet returns checked=false.
	rec := doJSON(t, srv, http.MethodPut, "/api/settings/team/default/repos", map[string]any{
		"repos": []string{"anyone/anything"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("no-credential PUT = %d, want 200 (fail-open); body=%s", rec.Code, rec.Body.String())
	}
}
