package marketplacestats

import "sync"

// Manager owns per-org stats-aggregation Runners, mirroring the scorer /
// profiler / classifier / reconciler managers (internal/ai/manager.go,
// internal/reconcile/manager.go). Each org gets its own Runner — its own
// trigger channel, its own single-flight cycle gate — so a slow cycle on
// one tenant doesn't head-of-line-block another's.
//
// Runners are lazy-created on first Trigger(orgID); every caller has an
// orgID in scope (the system:poll: sentinel carries evt.OrgID).
type Manager struct {
	aggregator *Aggregator

	mu      sync.Mutex
	runners map[string]*Runner
	stopped bool
}

// NewManager builds the per-org Manager over a shared Aggregator.
func NewManager(aggregator *Aggregator) *Manager {
	return &Manager{
		aggregator: aggregator,
		runners:    make(map[string]*Runner),
	}
}

// Trigger signals the stats runner for the given org, lazy-creating and
// starting one on first use. Subsequent triggers for the same org merge
// into the runner's buffered channel (single-flight per org); distinct
// orgs run concurrently. Empty orgID is dropped with a log line — every
// caller has one, so an empty value points at an emitter bug.
func (m *Manager) Trigger(orgID string) {
	if orgID == "" {
		marketplaceStatsLog.Warn("empty org on trigger; dropping signal")
		return
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	r, ok := m.runners[orgID]
	if !ok {
		r = NewRunner(m.aggregator, orgID)
		r.Start()
		m.runners[orgID] = r
	}
	m.mu.Unlock()
	r.Trigger()
}

// Stop tears down every per-org runner. After Stop, Trigger is a no-op.
// Safe to call multiple times.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	runners := make([]*Runner, 0, len(m.runners))
	for _, r := range m.runners {
		runners = append(runners, r)
	}
	m.mu.Unlock()
	for _, r := range runners {
		r.Stop()
	}
}
