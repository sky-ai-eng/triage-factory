package reachcache

import (
	"context"
	"sync"
)

// Manager owns per-org reachable-mirror Runners, mirroring repoprofile.Manager
// and ai.Manager. Each org gets its own Runner — its own buffered trigger
// channel and single-flight cycle gate — so a slow refresh on one tenant (a
// 3,000-repo account behind a sluggish GHES) cannot head-of-line-block the
// picker read of another.
//
// Runners are lazy-created on first Trigger(orgID) and live for the process
// lifetime — like its siblings, the runners map is not pruned when an org is
// deleted or stops polling, so it grows with the number of distinct orgs ever
// triggered. In multi mode with heavy org churn this is a slow, bounded-by-org-
// count leak; it mirrors the accepted trade-off next door and is cleaned up
// wholesale by Stop() at shutdown.
//
// The single-flight gate is also the rate-limit bound the picker relies on: N
// picker opens inside one TTL coalesce into at most one enumeration, because the
// merged triggers land on one Runner whose cycle then finds the mirror fresh.
type Manager struct {
	refresher *Refresher

	// newRunnerFor builds the per-org Runner. Defaulted in NewManager to a
	// Runner whose cycle is refresher.RunOrg; overridable so tests can drive the
	// trigger / lazy-create / single-flight machinery with a fake cycle.
	newRunnerFor func(orgID string) *Runner

	mu      sync.Mutex
	runners map[string]*Runner
	stopped bool
}

// NewManager builds a refresh Manager over a shared Refresher. The Refresher is
// stateless and shared across every per-org Runner — it reads credentials fresh
// per pass through the resolver — so a config-change hot-swap is honored without
// rebuilding it.
func NewManager(refresher *Refresher) *Manager {
	return &Manager{
		refresher: refresher,
		runners:   make(map[string]*Runner),
		newRunnerFor: func(orgID string) *Runner {
			return newRunner(refresher.RunOrg, orgID)
		},
	}
}

// Trigger signals the refresh runner for orgID. force bypasses the TTL (the
// picker's explicit refresh control, and every credential change); a non-forced
// trigger is the TTL-gated pass a stale-mirror read kicks. If no runner exists
// yet for orgID, one is lazy-created and started; subsequent triggers merge into
// the runner's buffered channel (single-flight per org). A pending force sticks,
// so a forced refresh is never downgraded by a coincident TTL-gated trigger.
// Distinct orgs run concurrently.
//
// Empty orgID is dropped with a log line: every caller has one (a request's
// orgID, a credential-change callback's), so an empty value points at a caller
// bug worth surfacing rather than papering over by routing to the local
// sentinel.
func (m *Manager) Trigger(orgID string, force bool) {
	if orgID == "" {
		reachLog.Warn("empty org on reachable-cache trigger; dropping signal")
		return
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	r, ok := m.runners[orgID]
	if !ok {
		r = m.newRunnerFor(orgID)
		r.Start()
		m.runners[orgID] = r
	}
	m.mu.Unlock()
	r.Trigger(force)
}

// Stop tears down every per-org runner. After Stop, Trigger is a no-op. Safe to
// call multiple times.
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

// cycleFunc refreshes one org's mirror for a single cycle. force bypasses the
// TTL. The returned bool reports whether the cycle actually wrote. The Manager
// wires this to Refresher.RunOrg; tests inject a fake.
type cycleFunc func(ctx context.Context, orgID string, force bool) (bool, error)
