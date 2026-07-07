package poller

import (
	"context"
	"testing"
	"time"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// fakeRateLimitResolver satisfies ghclient.Resolver (embedded nil — any
// unexercised method panics if reached) plus ghclient.RateLimitReader, so
// Health()'s type assertion succeeds without pulling in the real resolver's
// store dependencies.
type fakeRateLimitResolver struct {
	ghclient.Resolver
	states map[string]ghclient.RateLimitState
}

func (f *fakeRateLimitResolver) RateLimitFor(orgID string) (ghclient.RateLimitState, bool) {
	s, ok := f.states[orgID]
	return s, ok
}

var _ ghclient.RateLimitReader = (*fakeRateLimitResolver)(nil)

// TestHealth_GitHubRateLimit_ReadsFromResolver pins Health()'s rate-limit
// signal (TFAC-573 follow-up): only orgs with an actual observation appear
// in the map, keyed by org ID, and an org present in ListActiveSystem but
// absent from the resolver's registry is simply omitted — not zeroed.
func TestHealth_GitHubRateLimit_ReadsFromResolver(t *testing.T) {
	orgs := &fakeOrgsStore{ids: []string{"org-1", "org-2"}}
	reset := time.Unix(1783303600, 0)
	resolver := &fakeRateLimitResolver{states: map[string]ghclient.RateLimitState{
		"org-1": {Remaining: 42, Reset: reset, Used: 7},
	}}
	m := &Manager{orgs: orgs, resolver: resolver}

	snap := m.Health(context.Background())
	if len(snap.GitHubRateLimit) != 1 {
		t.Fatalf("GitHubRateLimit = %+v, want exactly 1 entry (org-2 has no observation)", snap.GitHubRateLimit)
	}
	got, ok := snap.GitHubRateLimit["org-1"]
	if !ok {
		t.Fatal("expected org-1 present")
	}
	if got.Remaining != 42 || got.Used != 7 || !got.Reset.Equal(reset) {
		t.Errorf("org-1 = %+v, want {42 %v 7}", got, reset)
	}
	if _, ok := snap.GitHubRateLimit["org-2"]; ok {
		t.Error("org-2 should be absent (no observation)")
	}
}

// TestHealth_GitHubRateLimit_NilWhenResolverDoesNotImplementReader covers
// the fallback for a resolver that doesn't implement RateLimitReader (a
// bare test fake, or a future alternate implementation) — Health() must
// not panic, and simply reports nothing.
func TestHealth_GitHubRateLimit_NilWhenResolverDoesNotImplementReader(t *testing.T) {
	orgs := &fakeOrgsStore{ids: []string{"org-1"}}
	m := &Manager{orgs: orgs, resolver: nil}

	snap := m.Health(context.Background())
	if snap.GitHubRateLimit != nil {
		t.Errorf("GitHubRateLimit = %+v, want nil", snap.GitHubRateLimit)
	}
}
