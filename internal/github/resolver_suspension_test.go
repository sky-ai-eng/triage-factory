package github

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// suspensionResolver builds a resolver over one mirrored installation and a
// cache the test owns, so it can prime the cache the way a prior resolution
// would have and then observe what the next one does with it.
func suspensionResolver(t *testing.T, inst domain.OrgGitHubAppInstallation) (*resolver, TokenCache, *ghTestServer) {
	t.Helper()
	gh := newGHTestServer(t)
	cache := NewMemoryTokenCache()
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t)}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{inst}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		cache,
	).(*resolver)
	return r, cache, gh
}

// suspendedInstall is installOn plus a suspension — the state in which the
// grant survives but GitHub refuses every token minted from the installation.
func suspendedInstall(login string) domain.OrgGitHubAppInstallation {
	inst := installOn(login)
	inst.SuspendedAt = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	inst.SuspendedBy = "octocat"
	return inst
}

// TestInstallationToken_SuspendedIsNeverServedFromCache is the invariant on the
// read side, and the half that covers a suspension no webhook delivered: GitHub
// does not re-deliver a missed installation.suspend and local mode receives no
// deliveries at all, so for those deployments the mirrored row is the only
// place that knows. A token minted before the suspension is already dead, and
// serving it from cache turns a suspension into an hour of unexplained 403s.
func TestInstallationToken_SuspendedIsNeverServedFromCache(t *testing.T) {
	inst := suspendedInstall("acme")
	r, cache, gh := suspensionResolver(t, inst)
	cache.Set("org-1", inst.InstallationID, githubapp.Token{
		Value:     "ghs_minted_before_the_suspension",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	tok, err := r.installationToken(context.Background(), "org-1", resolvedApp{org: activeApp()}, inst, gh.srv.URL)
	if err != nil {
		t.Fatalf("installationToken: %v", err)
	}
	if tok.Value == "ghs_minted_before_the_suspension" {
		t.Fatal("resolved the token cached before the suspension; want it dropped")
	}
	if got := atomic.LoadInt32(&gh.mintCalls); got != 1 {
		t.Errorf("mint calls = %d; want 1 (the cache was bypassed, not just ignored)", got)
	}
	// The stale entry is gone, not merely stepped over — a caller that reaches
	// the cache by another path must not find it either.
	if cached, ok := cache.Get("org-1", inst.InstallationID); ok && cached.Value == "ghs_minted_before_the_suspension" {
		t.Error("the pre-suspension token is still in the cache; want it invalidated")
	}
}

// TestInstallationToken_UnsuspendedStillServesFromCache is the control: the
// guard costs an unsuspended installation nothing. Without this, "never serve a
// suspended installation from cache" could be satisfied by never serving
// anything from cache — one mint per API call, against a rate budget the cache
// exists to protect.
func TestInstallationToken_UnsuspendedStillServesFromCache(t *testing.T) {
	inst := installOn("acme")
	r, cache, gh := suspensionResolver(t, inst)
	cache.Set("org-1", inst.InstallationID, githubapp.Token{
		Value:     "ghs_cached",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	tok, err := r.installationToken(context.Background(), "org-1", resolvedApp{org: activeApp()}, inst, gh.srv.URL)
	if err != nil {
		t.Fatalf("installationToken: %v", err)
	}
	if tok.Value != "ghs_cached" {
		t.Errorf("token = %q; want the cached %q", tok.Value, "ghs_cached")
	}
	if got := atomic.LoadInt32(&gh.mintCalls); got != 0 {
		t.Errorf("mint calls = %d; want 0 (served from cache)", got)
	}
}
