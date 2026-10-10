package github

import "testing"

// hostA and hostB are two GitHub deployments one org can be pointed at, in the
// GitHubHost spelling the resolver keys the cache on.
const (
	hostA = "https://github.com"
	hostB = "https://ghe.example.com"
)

// A rename is the one event that evicts a coverage entry rather than waiting
// out the TTL, and it evicts BOTH slugs. An entry is a positive about a slug,
// which only stands in for a repository while that slug denotes the same one:
// after the rename the old name's positive vouches for a repository that no
// longer answers to it, and the new name's — if this process ever probed it —
// for whatever was called that before. Both are seeded here as positives,
// because a positive is the only thing this cache holds.
func TestRepoCoverageCache_ForgetDropsOneSlug(t *testing.T) {
	c := newRepoCoverageCache()
	c.markCovered("org-1", hostA, "octo", "api")
	c.markCovered("org-1", hostA, "octo", "platform-api")
	c.markCovered("org-1", hostA, "octo", "api-gateway")
	c.markCovered("org-2", hostA, "octo", "api")

	c.forget("org-1", hostA, "octo", "api")
	c.forget("org-1", hostA, "octo", "platform-api")

	if c.covered("org-1", hostA, "octo", "api") {
		t.Error("the old slug survived the eviction")
	}
	if c.covered("org-1", hostA, "octo", "platform-api") {
		t.Error("the new slug survived the eviction")
	}
	// The key is (org, host, folded slug), so neither a neighbouring repository nor
	// another tenant's identically named one is touched.
	if !c.covered("org-1", hostA, "octo", "api-gateway") {
		t.Error("a neighbouring repository was evicted")
	}
	if !c.covered("org-2", hostA, "octo", "api") {
		t.Error("another org's entry was evicted")
	}
}

// The cache holds positives only, so forgetting something it never held is
// already the answer forget produces.
func TestRepoCoverageCache_ForgetIsCaseInsensitiveAndMissTolerant(t *testing.T) {
	c := newRepoCoverageCache()
	c.forget("org-1", hostA, "octo", "never-cached")

	c.markCovered("org-1", hostA, "Octo", "API")
	c.forget("org-1", hostA, "octo", "api")
	if c.covered("org-1", hostA, "Octo", "API") {
		t.Error("a casing-differing forget missed the entry; GitHub identifiers are case-insensitive")
	}
}

// A slug names a different repository on another GitHub deployment, so a
// positive recorded for one host never answers for the same slug on another,
// and forgetting it on one host leaves the other's entry standing.
func TestRepoCoverageCache_KeyedByHost(t *testing.T) {
	c := newRepoCoverageCache()
	c.markCovered("org-1", hostA, "octo", "api")

	if c.covered("org-1", hostB, "octo", "api") {
		t.Fatal("host A's positive answered for the same slug on host B")
	}
	if !c.covered("org-1", hostA, "octo", "api") {
		t.Fatal("host A's own positive was lost")
	}

	c.markCovered("org-1", hostB, "octo", "api")
	c.forget("org-1", hostA, "octo", "api")
	if c.covered("org-1", hostA, "octo", "api") {
		t.Error("the forgotten host A entry survived")
	}
	if !c.covered("org-1", hostB, "octo", "api") {
		t.Error("forgetting host A's entry evicted host B's")
	}
}
