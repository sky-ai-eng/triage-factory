package server

import (
	"strconv"
	"testing"
	"time"
)

// putFresh caches a resolution under the generation current right now — the
// uncontended case, where nothing invalidated between the lookup and the write.
// Tests that care about the contended case pass a stale generation by hand.
func putFresh(s *Server, orgID, secret string) {
	_, gen, _ := s.webhookSecretCacheGet(orgID)
	s.webhookSecretCachePut(orgID, secret, gen)
}

// TestWebhookSecretCache_ExpiredEntryIsAMiss pins the TTL as the backstop it
// is: explicit invalidation is what makes a rotation take effect immediately,
// but a secret rotated by any path that forgets to call it must still stop
// being served once the entry ages out.
func TestWebhookSecretCache_ExpiredEntryIsAMiss(t *testing.T) {
	s := &Server{}
	const org = "org-1"

	putFresh(s, org, "secret-a")
	if got, _, ok := s.webhookSecretCacheGet(org); !ok || got != "secret-a" {
		t.Fatalf("get = (%q, %v), want (secret-a, true)", got, ok)
	}

	// Age the entry past its TTL. Reaching into the entry rather than sleeping
	// keeps the test instant and pins the expiry rule rather than the clock.
	s.webhookSecretMu.Lock()
	e := s.webhookSecretCache[org]
	e.expiresAt = time.Now().Add(-time.Second)
	s.webhookSecretCache[org] = e
	s.webhookSecretMu.Unlock()

	if got, _, ok := s.webhookSecretCacheGet(org); ok {
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
	putFresh(s, "org-1", "")
	if got, _, ok := s.webhookSecretCacheGet("org-1"); !ok || got != "" {
		t.Errorf("get = (%q, %v), want (\"\", true) — a cached negative is a hit", got, ok)
	}
	if _, _, ok := s.webhookSecretCacheGet("org-2"); ok {
		t.Error("an org that was never resolved reports a hit")
	}
}

// TestWebhookSecretCache_Invalidate drops exactly the one org.
func TestWebhookSecretCache_Invalidate(t *testing.T) {
	s := &Server{}
	putFresh(s, "org-1", "secret-a")
	putFresh(s, "org-2", "secret-b")

	s.invalidateWebhookSecret("org-1")

	if _, _, ok := s.webhookSecretCacheGet("org-1"); ok {
		t.Error("org-1 still cached after invalidate")
	}
	if got, _, ok := s.webhookSecretCacheGet("org-2"); !ok || got != "secret-b" {
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
		putFresh(s, "org-"+strconv.Itoa(i), "")
	}
	s.webhookSecretMu.Lock()
	n := len(s.webhookSecretCache)
	s.webhookSecretMu.Unlock()
	if n > webhookSecretCacheMax {
		t.Errorf("cache holds %d entries after %d distinct org ids, want at most %d",
			n, webhookSecretCacheMax*2, webhookSecretCacheMax)
	}
	// Still a working cache after the reclaim.
	putFresh(s, "org-live", "secret-a")
	if got, _, ok := s.webhookSecretCacheGet("org-live"); !ok || got != "secret-a" {
		t.Errorf("get after reclaim = (%q, %v), want (secret-a, true)", got, ok)
	}
}

// TestWebhookSecretCache_InFlightResolutionCannotOutliveAnInvalidation is the
// interleaving that makes explicit invalidation worth having at all. A
// resolution runs outside the lock, so a rotation can land while one is in
// flight; if that older resolution's write were taken, it would reinstate the
// pre-rotation secret AFTER the invalidation and serve it for a full TTL — the
// exact staleness the invalidation was added to prevent, on the exact
// deliveries that matter (the first ones under the new secret).
//
// Sequenced by hand rather than with goroutines: the generation is what orders
// these two writers, so the test drives the order directly instead of racing
// for it.
func TestWebhookSecretCache_InFlightResolutionCannotOutliveAnInvalidation(t *testing.T) {
	s := &Server{}
	const org = "org-1"

	// A delivery misses and starts resolving; it reads the old secret.
	_, gen, ok := s.webhookSecretCacheGet(org)
	if ok {
		t.Fatal("empty cache reported a hit")
	}

	// The App is re-registered (or torn down) while that resolution is in
	// flight.
	s.invalidateWebhookSecret(org)

	// The in-flight resolution now returns and tries to cache what it read.
	s.webhookSecretCachePut(org, "secret-before-the-rotation", gen)

	if got, _, ok := s.webhookSecretCacheGet(org); ok {
		t.Errorf("cache serves %q after an invalidation; the in-flight resolution resurrected the old secret", got)
	}

	// And the cache still works afterwards — the generation bump costs one
	// resolution, it doesn't wedge the entry.
	putFresh(s, org, "secret-after-the-rotation")
	if got, _, ok := s.webhookSecretCacheGet(org); !ok || got != "secret-after-the-rotation" {
		t.Errorf("get = (%q, %v), want (secret-after-the-rotation, true)", got, ok)
	}
}
