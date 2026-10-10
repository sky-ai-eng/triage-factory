package github

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

func TestMemoryTokenCache_GuardWindow(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	c := &memoryTokenCache{
		tokens: make(map[tokenCacheKey]githubapp.Token),
		now:    func() time.Time { return now },
	}

	// A token expiring inside the guard window must read as a miss (and be
	// evicted) so callers never get a token that could 401 mid-request.
	c.Set("org-1", hostA, "inst-near", githubapp.Token{Value: "ghs_near", ExpiresAt: now.Add(tokenExpiryGuard - time.Minute)})
	if _, ok := c.Get("org-1", hostA, "inst-near"); ok {
		t.Error("Get returned a hit for a token inside the expiry guard window")
	}
	if _, present := c.tokens[tokenCacheKey{"org-1", hostA, "inst-near"}]; present {
		t.Error("near-expiry token was not evicted on Get")
	}

	// A token comfortably beyond the guard window is a hit.
	c.Set("org-1", hostA, "inst-fresh", githubapp.Token{Value: "ghs_fresh", ExpiresAt: now.Add(time.Hour)})
	got, ok := c.Get("org-1", hostA, "inst-fresh")
	if !ok {
		t.Fatal("Get returned a miss for a fresh token")
	}
	if got.Value != "ghs_fresh" {
		t.Errorf("Get returned %q, want ghs_fresh", got.Value)
	}

	// Same installation ID under a different org must not collide (different
	// GHES hosts can reuse numeric installation IDs).
	if _, ok := c.Get("org-2", hostA, "inst-fresh"); ok {
		t.Error("Get returned a cross-org hit; cache key must include orgID")
	}
}

func TestMemoryTokenCache_Invalidate(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	c := &memoryTokenCache{
		tokens: make(map[tokenCacheKey]githubapp.Token),
		now:    func() time.Time { return now },
	}
	c.Set("org-1", hostA, "inst-1", githubapp.Token{Value: "ghs_1", ExpiresAt: now.Add(time.Hour)})
	if _, ok := c.Get("org-1", hostA, "inst-1"); !ok {
		t.Fatal("expected hit before invalidate")
	}
	c.Invalidate("org-1", "inst-1")
	if _, ok := c.Get("org-1", hostA, "inst-1"); ok {
		t.Error("expected miss after Invalidate")
	}
}

// Installation ids are unique only per GitHub host, so one org pointed at two
// hosts can hold two installations with the same number. A token minted on
// host A is never served for host B, and Invalidate — which the webhooks call
// with no host — evicts the installation's entry on every host, while leaving
// another installation and another org alone.
func TestMemoryTokenCache_KeyedByHost(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	c := &memoryTokenCache{
		tokens: make(map[tokenCacheKey]githubapp.Token),
		now:    func() time.Time { return now },
	}
	fresh := now.Add(time.Hour)

	c.Set("org-1", hostA, "456", githubapp.Token{Value: "ghs_on_a", ExpiresAt: fresh})
	if tok, ok := c.Get("org-1", hostB, "456"); ok {
		t.Fatalf("host A's token %q was served for the same installation id on host B", tok.Value)
	}

	c.Set("org-1", hostB, "456", githubapp.Token{Value: "ghs_on_b", ExpiresAt: fresh})
	if tok, ok := c.Get("org-1", hostA, "456"); !ok || tok.Value != "ghs_on_a" {
		t.Fatalf("host A = (%q, %v), want its own ghs_on_a", tok.Value, ok)
	}
	if tok, ok := c.Get("org-1", hostB, "456"); !ok || tok.Value != "ghs_on_b" {
		t.Fatalf("host B = (%q, %v), want its own ghs_on_b", tok.Value, ok)
	}

	c.Set("org-1", hostA, "789", githubapp.Token{Value: "ghs_other_inst", ExpiresAt: fresh})
	c.Set("org-2", hostA, "456", githubapp.Token{Value: "ghs_other_org", ExpiresAt: fresh})

	c.Invalidate("org-1", "456")
	if _, ok := c.Get("org-1", hostA, "456"); ok {
		t.Error("Invalidate left the installation's host A entry")
	}
	if _, ok := c.Get("org-1", hostB, "456"); ok {
		t.Error("Invalidate left the installation's host B entry")
	}
	if _, ok := c.Get("org-1", hostA, "789"); !ok {
		t.Error("Invalidate evicted another installation of the same org")
	}
	if _, ok := c.Get("org-2", hostA, "456"); !ok {
		t.Error("Invalidate evicted another org's same-numbered installation")
	}
}
