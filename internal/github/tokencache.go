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

// TokenCache stores minted GitHub App installation access tokens keyed by
// installation ID so resolutions within a token's lifetime reuse it instead
// of re-minting. Get must treat a token within tokenExpiryGuard of expiry
// as a miss so callers never receive one that could 401.
type TokenCache interface {
	Get(installationID string) (githubapp.Token, bool)
	Set(installationID string, tok githubapp.Token)

	// Invalidate drops the entry for installationID. Wired to the
	// installation.deleted webhook (via the server's onInstallationRemoved
	// hook) so a removed installation's token isn't served from cache until
	// its natural expiry.
	Invalidate(installationID string)
}

// memoryTokenCache is the process-local TokenCache. A single TF process owns
// one of these; tokens don't need to survive a restart (a fresh process
// re-mints on first use).
type memoryTokenCache struct {
	mu     sync.Mutex
	tokens map[string]githubapp.Token
	now    func() time.Time // injectable clock for tests; nil → time.Now
}

// NewMemoryTokenCache returns an empty in-memory TokenCache.
func NewMemoryTokenCache() TokenCache {
	return &memoryTokenCache{tokens: make(map[string]githubapp.Token)}
}

func (c *memoryTokenCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *memoryTokenCache) Get(installationID string) (githubapp.Token, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, ok := c.tokens[installationID]
	if !ok {
		return githubapp.Token{}, false
	}
	// Expired or inside the guard window: treat as a miss and drop the
	// stale entry so the map doesn't accumulate dead tokens.
	if !tok.ExpiresAt.After(c.timeNow().Add(tokenExpiryGuard)) {
		delete(c.tokens, installationID)
		return githubapp.Token{}, false
	}
	return tok, true
}

func (c *memoryTokenCache) Set(installationID string, tok githubapp.Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[installationID] = tok
}

func (c *memoryTokenCache) Invalidate(installationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, installationID)
}
