package github

import (
	"strings"
	"sync"
	"time"
)

// repoCoverageTTL bounds how long a repo-coverage decision (does the org's App
// installation grant cover owner/repo?) is reused before re-probing. Coverage
// changes only when an admin edits the App's repository selection — a rare,
// out-of-band event this process gets no webhook for — so a few minutes trades
// a small staleness window for dropping a per-request GET /repos/{owner}/{repo}
// probe on the hot dashboard path (PR status / draft toggle resolve per card).
//
// Removal of an installation needs no eviction here: once it's gone,
// tier1AppClient stops resolving for that account, so ClientForRepo skips the
// tier-1 block entirely and never reads the stale entry (it just ages out).
// The one residual staleness is a repo dropped from a *still-present* selective
// install — bounded by this TTL, and there's no webhook for it regardless.
const repoCoverageTTL = 5 * time.Minute

type coverageEntry struct {
	covered bool
	expires time.Time
}

// repoCoverageCache memoizes ClientForRepo's installation-grant probe, keyed by
// (orgID, owner/repo). An account login maps 1:1 to an installation, so the
// owner/repo pair is a sufficient key without the installation ID. Only
// conclusive probe results are stored; an indeterminate probe (GitHub 5xx) is
// never cached, so a transient outage can't pin a wrong answer.
type repoCoverageCache struct {
	mu      sync.Mutex
	entries map[string]coverageEntry
	now     func() time.Time // injectable clock for tests; nil → time.Now
}

func newRepoCoverageCache() *repoCoverageCache {
	return &repoCoverageCache{entries: make(map[string]coverageEntry)}
}

func (c *repoCoverageCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *repoCoverageCache) get(orgID, owner, repo string) (covered, ok bool) {
	key := coverageKey(orgID, owner, repo)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[key]
	if !found {
		return false, false
	}
	if !e.expires.After(c.timeNow()) {
		delete(c.entries, key)
		return false, false
	}
	return e.covered, true
}

func (c *repoCoverageCache) set(orgID, owner, repo string, covered bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[coverageKey(orgID, owner, repo)] = coverageEntry{
		covered: covered,
		expires: c.timeNow().Add(repoCoverageTTL),
	}
}

// coverageKey joins org + slug with a NUL so no pair can alias another by
// concatenation; the slug is lowercased to match GitHub's case-insensitive
// owner/repo handling.
func coverageKey(orgID, owner, repo string) string {
	return orgID + "\x00" + strings.ToLower(owner+"/"+repo)
}
