package routing

import (
	"context"
	"sync"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
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
	// gen counts invalidations per org. A read captures it before dispatching
	// and drops its answer on the way back if it moved, so a read already in
	// flight when an admin flipped the switch cannot install the answer from
	// before the flip. Without it, invalidate() is a no-op against a read that
	// has not written yet, and the stale answer lands AFTER the invalidation
	// with a fresh timestamp — a full TTL of events routed as though the
	// switch had never been thrown. Same guard, same reasoning, as the
	// frontend's own per-org generation on this read.
	gen map[string]uint64
	// now is the clock, injectable so a test can age an entry past the TTL
	// without sleeping. nil means time.Now.
	now func() time.Time
}

type sourceGateEntry struct {
	paused map[string]bool
	readAt time.Time
}

func newSourceGate(store dbpkg.OrgEventSourceStore) *sourceGate {
	return &sourceGate{store: store, byOrg: map[string]sourceGateEntry{}, gen: map[string]uint64{}}
}

func (g *sourceGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// disabled reports whether orgID has turned kind off. The error is the caller's
// to propagate rather than swallow: answering "not turned off" on a failed read
// is the one wrong answer that lets events through after an admin turned them
// off, and the router's caller retries an errored event rather than consuming
// it.
func (g *sourceGate) disabled(ctx context.Context, orgID, kind string) (bool, error) {
	// A source with no off switch is never off — and this is the arm that
	// matters most, because it is the one every GitHub event takes. It answers
	// from the vocabulary rather than the cache so a stored row naming a
	// required source cannot drop the events the whole product is built on;
	// the column is FK-free by design, so nothing at the schema level says it
	// cannot exist.
	if !eventsource.Disableable(kind) {
		return false, nil
	}
	g.mu.Lock()
	entry, ok := g.byOrg[orgID]
	fresh := ok && g.clock().Sub(entry.readAt) < sourceGateTTL
	gen := g.gen[orgID]
	g.mu.Unlock()
	if fresh {
		return entry.paused[kind], nil
	}

	// Stamp the age from when the read was DISPATCHED, not when it landed. The
	// TTL bounds how long a stale answer may be served, and a read that took a
	// second to come back was already a second stale when it did.
	startedAt := g.clock()

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
	// Only cache an answer no invalidation has overtaken. The generation moves
	// on both directions of the switch, so a mismatch is not evidence the
	// source is off and this read is not re-run: THIS event is answered from
	// what the read found, which is inherent — an event crossing the router
	// while an admin commits the write is a race with no total order, and the
	// switch is forward-only either way. What must not happen is that one
	// racing answer becoming every subsequent event's answer for a TTL.
	if g.gen[orgID] == gen {
		g.byOrg[orgID] = sourceGateEntry{paused: paused, readAt: startedAt}
	}
	g.mu.Unlock()
	return paused[kind], nil
}

// invalidate drops one org's cached answer so the next event re-reads it. Its
// caller is the admin write, in process or relayed over tf_ctl.
//
// Bumping the generation is the half that matters when nothing is cached yet:
// deleting an absent entry is a no-op, and the read that is mid-flight right
// now is exactly the one holding the pre-write answer.
func (g *sourceGate) invalidate(orgID string) {
	g.mu.Lock()
	g.gen[orgID]++
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
// single funnel every source's events cross, and the poll-cycle skip beside it
// guarantees something different (no API calls, not no tasks).
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
