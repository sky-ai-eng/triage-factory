package github

import (
	"sync"
	"time"
)

// RateLimitState is one org's most recently observed primary GitHub
// rate-limit budget, as last reported by a response's X-RateLimit-*
// headers (see Client.recordRateLimit).
type RateLimitState struct {
	Remaining int
	Reset     time.Time
	Used      int
}

// RateLimitReader is the optional Resolver extension exposing the
// resolver's process-wide, per-org GitHub rate-limit state — a registry
// fed by every *Client the resolver mints (see rateLimitRegistry, wired
// via newObservedClient in resolver.go), so it reflects the org's actual
// remaining budget as of the resolver's last API call for that org,
// without a live call of its own. GET /readyz's soft rate-limit signal
// (TFAC-573) reads through this. A Resolver that doesn't implement it
// (test fakes) simply has nothing to report — same optional-extension
// shape as RepoIdentityResolver/ScopedResolver above.
type RateLimitReader interface {
	RateLimitFor(orgID string) (RateLimitState, bool)
}

// rateLimitRegistry is a process-wide, in-memory, per-org cache of the
// most recently observed rate-limit state. Owned by one *resolver
// instance (constructed fresh in NewResolver): in multi mode many orgs
// share one process, so this is genuinely per-org state, not per-client —
// a *Client itself is short-lived, minted fresh on every resolve
// (ClientFor et al.), so nothing about its own state persists between
// calls without this.
//
// In-memory only, like the resolver's TokenCache: a restart loses it and
// the next observed response repopulates it — acceptable for a soft
// operational signal that self-heals on the next poll cycle.
type rateLimitRegistry struct {
	mu    sync.RWMutex
	byOrg map[string]RateLimitState
}

func newRateLimitRegistry() *rateLimitRegistry {
	return &rateLimitRegistry{byOrg: make(map[string]RateLimitState)}
}

func (r *rateLimitRegistry) record(orgID string, s RateLimitState) {
	r.mu.Lock()
	r.byOrg[orgID] = s
	r.mu.Unlock()
}

func (r *rateLimitRegistry) get(orgID string) (RateLimitState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byOrg[orgID]
	return s, ok
}
