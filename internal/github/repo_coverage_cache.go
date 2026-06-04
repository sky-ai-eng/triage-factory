package github

import (
	"strings"
	"sync"
	"time"
)

// repoCoverageTTL bounds how long a *positive* repo-coverage decision (the org's
// App installation grant covers owner/repo) is reused before re-probing.
// Coverage changes only when an admin edits the App's repository selection — a
// rare, out-of-band event this process gets no webhook for — so a few minutes
// trades a small staleness window for dropping a per-request
// GET /repos/{owner}/{repo} probe on the hot dashboard path (PR status / draft
// toggle resolve per card).
const repoCoverageTTL = 5 * time.Minute

// repoCoverageCache memoizes ClientForRepo's installation-grant probe, keyed by
// (orgID, owner/repo). An account login maps 1:1 to an installation, so the
// owner/repo pair is a sufficient key without the installation ID.
//
// It caches the POSITIVE answer only — "this repo is in the grant." That choice
// is deliberate:
//
//   - Not caching the negative means a repo newly *added* to a "Selected
//     repositories" grant is picked up on the very next call, instead of being
//     pinned to the PAT (or, for an App-only org, to ErrNoGitHubCredentials) for
//     up to a TTL. That's the more damaging staleness direction, so it's the one
//     we refuse to cache.
//   - Indeterminate probes (GitHub 5xx) are likewise never cached, so a
//     transient outage can't pin a wrong answer.
//
// The remaining staleness is a repo *removed* from a still-present selective
// grant: a cached positive keeps using the App token until the TTL expires, so
// that repo's call 403s for at most repoCoverageTTL before re-probing self-heals
// it. There's no webhook for grant edits regardless, and an installation removal
// needs no eviction here — once it's gone tier1AppClient stops resolving, so
// ClientForRepo skips the tier-1 block and never reads a stale entry.
type repoCoverageCache struct {
	mu      sync.Mutex
	expires map[string]time.Time
	now     func() time.Time // injectable clock for tests; nil → time.Now
}

func newRepoCoverageCache() *repoCoverageCache {
	return &repoCoverageCache{expires: make(map[string]time.Time)}
}

func (c *repoCoverageCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// covered reports whether owner/repo is known-covered and still within its TTL.
// A false return (miss or expired) means "unknown — probe GitHub."
func (c *repoCoverageCache) covered(orgID, owner, repo string) bool {
	key := coverageKey(orgID, owner, repo)
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.expires[key]
	if !ok {
		return false
	}
	if !exp.After(c.timeNow()) {
		delete(c.expires, key) // drop the stale entry so the map doesn't grow
		return false
	}
	return true
}

// markCovered records owner/repo as covered until now+repoCoverageTTL.
func (c *repoCoverageCache) markCovered(orgID, owner, repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expires[coverageKey(orgID, owner, repo)] = c.timeNow().Add(repoCoverageTTL)
}

// coverageKey joins org + slug with a NUL so no pair can alias another by
// concatenation; the slug is lowercased to match GitHub's case-insensitive
// owner/repo handling.
func coverageKey(orgID, owner, repo string) string {
	return orgID + "\x00" + strings.ToLower(owner+"/"+repo)
}
