package routing

import (
	"context"
	"sync"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
)

// sourceGateTTL bounds how long a cached answer about an org's paused sources
// may be trusted without re-reading.
//
// The cache exists because HandleEvent runs per event and the answer changes
// about once a quarter — a store read on every event would be a query per event
// for a row that is almost always absent. The TTL exists because the
// invalidation that normally beats it is the tf_ctl relay, which is lossy by
// contract: a message published in a holderless gap is dropped by every pod. So
// the TTL is the floor on correctness, not the mechanism — it decides how long
// a DROPPED invalidation can cost, and 30s of events still minting tasks after
// an admin hit pause is the accepted worst case. An unbounded cache waiting on
// a lossy channel would not be.
const sourceGateTTL = 30 * time.Second

// sourceGate answers "has an org paused this source?" for the router, caching
// each org's paused set for sourceGateTTL.
//
// It is per-org and holds the whole set rather than one kind: an org's events
// arrive interleaved across sources, so caching per (org, kind) would multiply
// the reads by the vocabulary to answer the same question.
type sourceGate struct {
	store dbpkg.OrgEventSourceStore

	mu    sync.Mutex
	byOrg map[string]sourceGateEntry
	// now is the clock, injectable so a test can age an entry past the TTL
	// without sleeping. nil means time.Now.
	now func() time.Time
}

type sourceGateEntry struct {
	paused map[string]bool
	readAt time.Time
}

func newSourceGate(store dbpkg.OrgEventSourceStore) *sourceGate {
	return &sourceGate{store: store, byOrg: map[string]sourceGateEntry{}}
}

func (g *sourceGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// disabled reports whether orgID has paused kind. The error is the caller's to
// propagate rather than swallow: answering "not paused" on a failed read is the
// one wrong answer that lets events through after an admin turned them off, and
// the router's caller retries an errored event rather than consuming it.
func (g *sourceGate) disabled(ctx context.Context, orgID, kind string) (bool, error) {
	g.mu.Lock()
	entry, ok := g.byOrg[orgID]
	fresh := ok && g.clock().Sub(entry.readAt) < sourceGateTTL
	g.mu.Unlock()
	if fresh {
		return entry.paused[kind], nil
	}

	// Read outside the lock: a slow read must not block every other org's
	// events. Two goroutines racing the same org both read and both store the
	// same answer, which costs one duplicate query and nothing else — a
	// single-flight here would be machinery guarding a query that runs about
	// twice a minute per org.
	kinds, err := g.store.ListDisabledSystem(ctx, orgID)
	if err != nil {
		return false, err
	}
	paused := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		paused[k] = true
	}

	g.mu.Lock()
	g.byOrg[orgID] = sourceGateEntry{paused: paused, readAt: g.clock()}
	g.mu.Unlock()
	return paused[kind], nil
}

// invalidate drops one org's cached answer so the next event re-reads it. Its
// caller is the admin write, in process or relayed over tf_ctl.
func (g *sourceGate) invalidate(orgID string) {
	g.mu.Lock()
	delete(g.byOrg, orgID)
	g.mu.Unlock()
}

// SetEventSourceGate wires the store the router reads org event-source policy
// from, post-construction like SetEventQueue and for the same reason: it is a
// late-bound dependency and NewRouter's signature is already wide.
//
// A router with no gate never drops, which is what the ~30 test constructions
// that route events without one rely on. Production wiring is not optional —
// internal/app calls this before the drain worker starts, because this is the
// single funnel every source's events cross and the poller skips beside it are
// only an API-budget optimization.
func (r *Router) SetEventSourceGate(store dbpkg.OrgEventSourceStore) {
	r.sourceGate = newSourceGate(store)
}

// InvalidateSourceGate drops one org's cached event-source policy. Called by
// the admin write on the pod that runs the brain, and by the tf_ctl dispatcher
// on behalf of a write that landed on another pod.
func (r *Router) InvalidateSourceGate(orgID string) {
	if r.sourceGate != nil {
		r.sourceGate.invalidate(orgID)
	}
}
