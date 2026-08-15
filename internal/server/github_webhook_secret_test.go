package server

import (
	"strconv"
	"testing"
	"time"
)

// TestWebhookSecretCache_ExpiredEntryIsAMiss pins the TTL as the backstop it
// is: explicit invalidation is what makes a rotation take effect immediately,
// but a secret rotated by any path that forgets to call it must still stop
// being served once the entry ages out.
func TestWebhookSecretCache_ExpiredEntryIsAMiss(t *testing.T) {
	s := &Server{}
	const org = "org-1"

	s.webhookSecretCachePut(org, "secret-a")
	if got, ok := s.webhookSecretCacheGet(org); !ok || got != "secret-a" {
		t.Fatalf("get = (%q, %v), want (secret-a, true)", got, ok)
	}

	// Age the entry past its TTL. Reaching into the entry rather than sleeping
	// keeps the test instant and pins the expiry rule rather than the clock.
	s.webhookSecretMu.Lock()
	e := s.webhookSecretCache[org]
	e.expiresAt = time.Now().Add(-time.Second)
	s.webhookSecretCache[org] = e
	s.webhookSecretMu.Unlock()

	if got, ok := s.webhookSecretCacheGet(org); ok {
		t.Errorf("get after expiry = (%q, true), want a miss", got)
	}
}

// TestWebhookSecretCache_NegativeIsCached distinguishes the two empty answers.
// A cached negative — "this org has nothing to verify against" — has to be a
// hit, or the flood it exists to absorb (deliveries aimed at an org with no
// App, the cheapest one to mount since the caller picks the org id) re-reads
// the stores on every request.
func TestWebhookSecretCache_NegativeIsCached(t *testing.T) {
	s := &Server{}
	s.webhookSecretCachePut("org-1", "")
	if got, ok := s.webhookSecretCacheGet("org-1"); !ok || got != "" {
		t.Errorf("get = (%q, %v), want (\"\", true) — a cached negative is a hit", got, ok)
	}
	if _, ok := s.webhookSecretCacheGet("org-2"); ok {
		t.Error("an org that was never resolved reports a hit")
	}
}

// TestWebhookSecretCache_Invalidate drops exactly the one org.
func TestWebhookSecretCache_Invalidate(t *testing.T) {
	s := &Server{}
	s.webhookSecretCachePut("org-1", "secret-a")
	s.webhookSecretCachePut("org-2", "secret-b")

	s.invalidateWebhookSecret("org-1")

	if _, ok := s.webhookSecretCacheGet("org-1"); ok {
		t.Error("org-1 still cached after invalidate")
	}
	if got, ok := s.webhookSecretCacheGet("org-2"); !ok || got != "secret-b" {
		t.Errorf("org-2 = (%q, %v) after invalidating org-1, want (secret-b, true)", got, ok)
	}
	// Invalidating an org that was never cached is a no-op, not a panic — the
	// App-lifecycle paths call it unconditionally.
	s.invalidateWebhookSecret("org-never-seen")
}

// TestWebhookSecretCache_BoundedUnderDistinctOrgIDs is the flood case for the
// cache itself. The org id comes from an unauthenticated URL, so the key space
// is attacker-chosen: a run of well-formed ids that resolve to nothing must not
// grow the map without limit, even though every entry inserted is still inside
// its TTL and so survives the sweep.
func TestWebhookSecretCache_BoundedUnderDistinctOrgIDs(t *testing.T) {
	s := &Server{}
	for i := 0; i < webhookSecretCacheMax*2; i++ {
		s.webhookSecretCachePut("org-"+strconv.Itoa(i), "")
	}
	s.webhookSecretMu.Lock()
	n := len(s.webhookSecretCache)
	s.webhookSecretMu.Unlock()
	if n > webhookSecretCacheMax {
		t.Errorf("cache holds %d entries after %d distinct org ids, want at most %d",
			n, webhookSecretCacheMax*2, webhookSecretCacheMax)
	}
	// Still a working cache after the reclaim.
	s.webhookSecretCachePut("org-live", "secret-a")
	if got, ok := s.webhookSecretCacheGet("org-live"); !ok || got != "secret-a" {
		t.Errorf("get after reclaim = (%q, %v), want (secret-a, true)", got, ok)
	}
}
