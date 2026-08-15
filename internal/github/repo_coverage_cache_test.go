package github

import "testing"

// A rename is the one event that evicts a coverage entry rather than waiting
// out the TTL, and it evicts BOTH slugs. They go stale in opposite directions:
// the old name's positive vouches for something that no longer resolves, and
// the new name may carry a decision this process made about whatever was
// called that before.
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
