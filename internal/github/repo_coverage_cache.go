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
// (orgID, owner/repo). Within one org an account login maps to at most one
// active installation — the mirror's partial unique on (org_id, account_login)
// WHERE removed_at IS NULL makes it so — which is what lets the owner/repo pair
// serve as a key without the installation id. Note what that rests on: a login
// is a renameable handle, so the key is stable only for as long as the handle
// keeps pointing at the same account (see below).
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
// needs no eviction here — once it's gone the tier-1 App token stops resolving,
// so ClientForRepo skips the tier-1 block and never reads a stale entry.
//
// A login changing hands (one account renames away, another claims the freed
// handle) is the same staleness in a rarer shape: an entry probed against the
// old account's installation is reused for the new one's until the TTL expires.
// It self-heals on the same clock, and only ever in the permissive direction on
// a probe TF made itself — never a token handed to the wrong account, since the
// token comes from installationFor, not from here.
//
// One event evicts rather than waits: a repository rename. An entry says a
// SLUG is covered, which is only a statement about a repository for as long as
// that slug denotes the same one — and a rename moves exactly that. So both
// slugs go stale at once, each as a positive now vouching for the wrong thing:
// the old one for a name this repository no longer answers to (and which
// another may claim), the new one — if TF ever probed it — for whatever
// repository held that name before. Unlike a grant edit, TF is the process
// that just learned about it, so waiting out the TTL is a choice rather than
// the only option. See RepoCoverageInvalidator.
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

// forget drops owner/repo's entry so the next call re-probes. A miss is a
// no-op — the cache holds positives only, so "not there" is already the
// answer forget produces.
func (c *repoCoverageCache) forget(orgID, owner, repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.expires, coverageKey(orgID, owner, repo))
}

// coverageKey joins org + slug with a NUL so no pair can alias another by
// concatenation; the slug is lowercased to match GitHub's case-insensitive
// owner/repo handling.
func coverageKey(orgID, owner, repo string) string {
	return orgID + "\x00" + strings.ToLower(owner+"/"+repo)
}

// RepoCoverageInvalidator is the optional Resolver extension that drops a
// cached repo-coverage decision. Same optional-extension shape as
// ScopedResolver / RateLimitReader: a Resolver that doesn't implement it (a
// test fake) has no cache to evict from, so the caller type-asserts and skips.
//
// The one caller is the rename handler. Coverage is keyed on the slug, so a
// rename invalidates the entry for BOTH names at once. The cache holds
// positives only, and after the rename each of them vouches for the wrong
// repository: the old slug's for one that no longer answers to that name, the
// new slug's for whatever repository was called that before. Both have to be
// re-probed rather than inherited.
type RepoCoverageInvalidator interface {
	InvalidateRepoCoverage(orgID, owner, repo string)
}

func (r *resolver) InvalidateRepoCoverage(orgID, owner, repo string) {
	r.coverage.forget(orgID, owner, repo)
}
