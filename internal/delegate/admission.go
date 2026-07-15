// Off-queue turn admission. Curator turns execute on this process without
// riding the run queue, but they spawn the same class of agent subprocess a
// dispatched run does. Admitting them through the dispatcher's own memory
// guardrail and concurrency semaphore keeps one host's sandbox fan-out
// bounded by a single capacity envelope regardless of which subsystem
// spawned the work — and makes the instance heartbeat's occupancy snapshot
// (which reads that same semaphore) reflect the host's true load instead of
// just the run queue's share of it.

package delegate

import (
	"context"
	"sync"
	"time"
)

// defaultTurnGateRecheck is how often AcquireTurnSlot re-probes the memory
// guardrail while gated — the dispatcher's own scan cadence, so a gated host
// admits a waiting turn no later than it would claim a queued run.
const defaultTurnGateRecheck = 2 * time.Second

// AcquireTurnSlot admits one off-queue agent turn into the dispatcher's
// capacity envelope: wait out the memory guardrail, then take a slot on the
// process-wide concurrency semaphore. Blocks until admitted; the only error
// is ctx's, so a cancelled caller stops waiting immediately and never holds
// a slot. The returned release hands the slot back — idempotent, safe to
// defer alongside other exits.
//
// Deliberately NOT consulted here: draining and the identity/partition
// fences. Those gate claiming new work from the shared run queue, where any
// other executor can pick the run up instead; an off-queue turn was routed
// to this process by its own lifecycle (curator homing), which handles
// executor death and re-home itself — refusing the turn here would wedge
// work no other pod is allowed to execute.
func (s *Spawner) AcquireTurnSlot(ctx context.Context) (release func(), err error) {
	// Capture the channel once so acquire and release act on the same
	// instance even if SetMaxConcurrentRuns swaps it mid-wait.
	sem := s.semaphore()

	// Same ordering as the dispatcher's drain loop: probe the gate before
	// taking a slot, so a memory-starved host parks with nothing held.
	for s.dispatchMemGated() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.turnGateRecheckInterval()):
		}
	}
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { <-sem }) }, nil
}

// turnGateRecheckInterval returns the effective gate re-probe cadence,
// falling back to the default when unset.
func (s *Spawner) turnGateRecheckInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnGateRecheck > 0 {
		return s.turnGateRecheck
	}
	return defaultTurnGateRecheck
}
