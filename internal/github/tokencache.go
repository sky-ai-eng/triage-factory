package github

import (
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// tokenExpiryGuard is how long before a token's stated expiry the cache
// treats it as already gone. Installation tokens live ~1h; re-minting a few
// minutes early costs one JWT sign + round-trip, while handing out a token
// that 401s mid-request costs a user-visible failure. The margin buys
// against clock skew between this host and GitHub.
const tokenExpiryGuard = 5 * time.Minute

// TokenCache stores minted GitHub App installation access tokens so
// resolutions within a token's lifetime reuse it instead of re-minting. Get
// must treat a token within tokenExpiryGuard of expiry as a miss so callers
// never receive one that could 401.
//
// Keyed by (orgID, host, installationID), not installationID alone:
// installation IDs are unique only per GitHub host, so two tenants on different
// GHES appliances can collide on the same numeric ID, and so can one org's
// installations on the two hosts it has been pointed at. Scoping by org keeps
// one tenant from ever being served another's token; scoping by host keeps an
// org that moved hosts from being served a token another deployment minted.
// host is the GitHubHost the token was minted against.
type TokenCache interface {
	Get(orgID, host, installationID string) (githubapp.Token, bool)
	Set(orgID, host, installationID string, tok githubapp.Token)

	// Invalidate drops every entry for (orgID, installationID), on any host.
	// Wired to the installation.deleted and installation.suspend webhooks (via
	// the server's onInstallationTokensInvalid hook) so an installation whose
	// tokens GitHub has stopped honouring isn't served from cache until their
	// natural expiry. The resolver drops a suspended installation's entry on
	// read as well, for the suspension a reconcile discovered rather than a
	// delivery. Dropping a same-numbered installation's entry on another host
	// costs that one a re-mint, which is the safe direction.
	Invalidate(orgID, installationID string)
}

// tokenCacheKey is one cached token's coordinates. A struct key, so no
// org/host/installation triple can alias another by concatenation.
type tokenCacheKey struct{ orgID, host, installationID string }

// memoryTokenCache is the process-local TokenCache. A single TF process owns
// one of these; tokens don't need to survive a restart (a fresh process
// re-mints on first use).
type memoryTokenCache struct {
	mu     sync.Mutex
	tokens map[tokenCacheKey]githubapp.Token
	now    func() time.Time // injectable clock for tests; nil → time.Now
}

// NewMemoryTokenCache returns an empty in-memory TokenCache.
func NewMemoryTokenCache() TokenCache {
	return &memoryTokenCache{tokens: make(map[tokenCacheKey]githubapp.Token)}
}

func (c *memoryTokenCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *memoryTokenCache) Get(orgID, host, installationID string) (githubapp.Token, bool) {
	key := tokenCacheKey{orgID, host, installationID}
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, ok := c.tokens[key]
	if !ok {
		return githubapp.Token{}, false
	}
	// Expired or inside the guard window: treat as a miss and drop the
	// stale entry so the map doesn't accumulate dead tokens.
	if !tok.ExpiresAt.After(c.timeNow().Add(tokenExpiryGuard)) {
		delete(c.tokens, key)
		return githubapp.Token{}, false
	}
	return tok, true
}

func (c *memoryTokenCache) Set(orgID, host, installationID string, tok githubapp.Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[tokenCacheKey{orgID, host, installationID}] = tok
}

func (c *memoryTokenCache) Invalidate(orgID, installationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.tokens {
		if key.orgID == orgID && key.installationID == installationID {
			delete(c.tokens, key)
		}
	}
}
