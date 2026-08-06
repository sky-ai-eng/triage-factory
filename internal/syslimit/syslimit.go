// Package syslimit bounds the number of concurrent headless system-job runs
// (scorer, repo-profiler, classifier) so the background fleet can't spawn an
// unbounded number of gVisor sandboxes across orgs in multi-mode.
//
// It is deliberately a small, self-contained seam: a single shared limiter is
// injected into the three background Managers and nothing else. It does NOT
// gate the curator, interactive sessions, or delegated runs (delegated has its
// own cap in internal/delegate). The horizontal-scaling work will eventually
// replace it with fleet-wide sandbox accounting without touching the call sites.
package syslimit

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// DefaultMaxConcurrentSystemRuns is the process-wide cap on concurrent
// background system-job sandboxes, shared across the scorer, repo-profiler,
// and classifier and across all orgs. 8 is chosen to clear the classifier's
// own per-entity fan-out (maxConcurrentVotes = 8) so a single entity's vote
// burst isn't artificially halved, while still bounding the total fleet
// (background + delegated, separately capped at 4, stays ≤ 12 sandboxes).
// Operators on small machines can tune it down.
//
// This is a single shared (not per-org) cap by design: it bounds the *total*
// fleet so the aggregate can't overwhelm the host. Per-org runners already
// prevent head-of-line blocking between tenants; per-org *fairness* (stopping
// one org from transiently monopolizing these shared slots) is deliberately
// deferred to the horizontal-scaling work — fleet-wide, DB-backed sandbox
// accounting that subsumes this process-local cap.
const DefaultMaxConcurrentSystemRuns = 8

// Limiter bounds concurrent headless system-job runs. A nil *Limiter is
// unlimited (a no-op), so call sites never have to nil-check — a caller that
// wants no cap (e.g. a unit test) can simply hold nil. The server wires a real
// cap in both modes (see app.buildAI); nil is not the local-mode default.
type Limiter struct{ sem chan struct{} }

// New returns a Limiter allowing n concurrent runs. n <= 0 returns nil
// (unlimited), matching the nil-safe no-op contract on the methods below.
func New(n int) *Limiter {
	if n <= 0 {
		return nil
	}
	return &Limiter{sem: make(chan struct{}, n)}
}

// Acquire blocks until a slot is free or ctx is done; it returns ctx.Err() on
// cancellation so the caller can fold the result into its existing per-run
// skip path. A nil *Limiter acquires instantly.
func (l *Limiter) Acquire(ctx context.Context) error {
	if l == nil {
		return nil
	}
	// Fast path first, so the spans that do exist all mean "this caller
	// queued" — the signal worth having, since the scorer's per-cycle
	// fan-out is unbounded and a cycle can sit here for an arbitrarily long
	// time looking like a slow LLM call.
	select {
	case l.sem <- struct{}{}:
		return nil
	default:
	}

	ctx, span := tracer.Start(ctx, "syslimit.acquire")
	defer span.End()

	select {
	case l.sem <- struct{}{}:
		span.SetAttributes(telemetry.Outcome("acquired"))
		return nil
	case <-ctx.Done():
		// Not an error status: giving up on a full queue during shutdown,
		// or on the caller's own deadline, is the limiter working.
		span.SetAttributes(telemetry.Outcome("cancelled"))
		return ctx.Err()
	}
}

// Release frees a slot. Must be paired with a successful Acquire. A nil
// *Limiter is a no-op. Calling Release without a matching Acquire panics (like
// sync.Mutex on a double-unlock) rather than blocking forever on the empty
// channel — an unmatched release is a caller bug, and a panic surfaces it
// loudly instead of silently leaking the goroutine.
func (l *Limiter) Release() {
	if l == nil {
		return
	}
	select {
	case <-l.sem:
	default:
		panic("syslimit: Release called without a matching Acquire")
	}
}
