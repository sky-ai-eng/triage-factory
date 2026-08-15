package github

import "testing"

// A rename is the one event that evicts a coverage entry rather than waiting
// out the TTL, and it evicts BOTH slugs. An entry is a positive about a slug,
// which only stands in for a repository while that slug denotes the same one:
// after the rename the old name's positive vouches for a repository that no
// longer answers to it, and the new name's — if this process ever probed it —
// for whatever was called that before. Both are seeded here as positives,
// because a positive is the only thing this cache holds.
func TestRepoCoverageCache_ForgetDropsOneSlug(t *testing.T) {
	c := newRepoCoverageCache()
	c.markCovered("org-1", "octo", "api")
	c.markCovered("org-1", "octo", "platform-api")
	c.markCovered("org-1", "octo", "api-gateway")
	c.markCovered("org-2", "octo", "api")

	c.forget("org-1", "octo", "api")
	c.forget("org-1", "octo", "platform-api")

	if c.covered("org-1", "octo", "api") {
		t.Error("the old slug survived the eviction")
	}
	if c.covered("org-1", "octo", "platform-api") {
		t.Error("the new slug survived the eviction")
	}
	// The key is (org, folded slug), so neither a neighbouring repository nor
	// another tenant's identically named one is touched.
	if !c.covered("org-1", "octo", "api-gateway") {
		t.Error("a neighbouring repository was evicted")
	}
	if !c.covered("org-2", "octo", "api") {
		t.Error("another org's entry was evicted")
	}
}

// The cache holds positives only, so forgetting something it never held is
// already the answer forget produces.
func TestRepoCoverageCache_ForgetIsCaseInsensitiveAndMissTolerant(t *testing.T) {
	c := newRepoCoverageCache()
	c.forget("org-1", "octo", "never-cached")

	c.markCovered("org-1", "Octo", "API")
	c.forget("org-1", "octo", "api")
	if c.covered("org-1", "Octo", "API") {
		t.Error("a casing-differing forget missed the entry; GitHub identifiers are case-insensitive")
	}
}
