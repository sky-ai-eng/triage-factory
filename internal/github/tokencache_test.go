package github

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

func TestMemoryTokenCache_GuardWindow(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	c := &memoryTokenCache{
		tokens: make(map[string]githubapp.Token),
		now:    func() time.Time { return now },
	}

	// A token expiring inside the guard window must read as a miss (and be
	// evicted) so callers never get a token that could 401 mid-request.
	c.Set("inst-near", githubapp.Token{Value: "ghs_near", ExpiresAt: now.Add(tokenExpiryGuard - time.Minute)})
	if _, ok := c.Get("inst-near"); ok {
		t.Error("Get returned a hit for a token inside the expiry guard window")
	}
	if _, present := c.tokens["inst-near"]; present {
		t.Error("near-expiry token was not evicted on Get")
	}

	// A token comfortably beyond the guard window is a hit.
	c.Set("inst-fresh", githubapp.Token{Value: "ghs_fresh", ExpiresAt: now.Add(time.Hour)})
	got, ok := c.Get("inst-fresh")
	if !ok {
		t.Fatal("Get returned a miss for a fresh token")
	}
	if got.Value != "ghs_fresh" {
		t.Errorf("Get returned %q, want ghs_fresh", got.Value)
	}
}

func TestMemoryTokenCache_Invalidate(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	c := &memoryTokenCache{
		tokens: make(map[string]githubapp.Token),
		now:    func() time.Time { return now },
	}
	c.Set("inst-1", githubapp.Token{Value: "ghs_1", ExpiresAt: now.Add(time.Hour)})
	if _, ok := c.Get("inst-1"); !ok {
		t.Fatal("expected hit before invalidate")
	}
	c.Invalidate("inst-1")
	if _, ok := c.Get("inst-1"); ok {
		t.Error("expected miss after Invalidate")
	}
}
