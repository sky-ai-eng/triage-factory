package repoprofile

import (
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/repoevent"
	"github.com/sky-ai-eng/triage-factory/internal/syslimit"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Manager owns per-org repo-profiling Runners, mirroring ai.Manager. Each
// org gets its own Runner — its own buffered trigger channel and
// single-flight cycle gate — so a slow profiling cycle on one tenant
// doesn't head-of-line-block profiling on others. The whole point of the
// split is that an org with a slow GitHub host (a sluggish GHES) can't
// stall every other tenant's profiling.
//
// Runners are lazy-created on first Trigger(orgID) and live for the process
// lifetime — like ai.Manager, the runners map is not pruned when an org is
// deleted or stops polling, so it grows with the number of distinct orgs
// ever triggered. In multi mode with heavy org churn this is a slow,
// bounded-by-org-count leak; it mirrors the scorer's accepted trade-off and
// is cleaned up wholesale by Stop() at shutdown.
type Manager struct {
	profiler *Profiler

	// newRunnerFor builds the per-org Runner. Defaulted in NewManager to a
	// Runner whose cycle is profiler.RunOrg; overridable so tests can drive
	// the trigger/lazy-create/single-flight machinery with a fake cycle.
	newRunnerFor func(orgID string, onComplete func(string)) *Runner

	mu              sync.Mutex
	onCycleComplete func(orgID string) // guarded by mu (write in SetOnCycleComplete, read in Trigger)
	runners         map[string]*Runner
	stopped         bool
}

// NewManager builds a profiling Manager with the same constructor deps as
// NewProfiler (resolver, secrets, repo store, orgs store, recorder, shared
// system-job limiter, WS hub). The wrapped Profiler is shared across every
// per-org Runner — it is stateless, reading credentials fresh per repo through
// the resolver, so a config-change hot-swap is honored without rebuilding it.
func NewManager(resolver github.Resolver, secrets agentproc.SecretsReader, llmResolve llmResolveFunc, repos db.RepositoryStore, orgs db.OrgsStore, recorder *systemllm.Recorder, limiter *syslimit.Limiter, ws *websocket.Hub) *Manager {
	profiler := NewProfiler(resolver, secrets, llmResolve, repos, orgs, recorder, limiter, ws)
	return &Manager{
		profiler: profiler,
		runners:  make(map[string]*Runner),
		newRunnerFor: func(orgID string, onComplete func(string)) *Runner {
			return newRunner(profiler.RunOrg, orgID, onComplete)
		},
	}
}

// SetOnCycleComplete registers a per-org callback fired after each
// profiling cycle for that org completes. It is the seam that chains
// bootstrapBareClones (which reads the clone_url profiling populates) off
// profile-cycle completion rather than off the credential callback. Set
// once before any Trigger; nil is a no-op. Takes mu so it's race-free
// against a concurrent Trigger (which reads onCycleComplete under the same
// lock), though in practice it's wired at boot before any trigger fires.
func (m *Manager) SetOnCycleComplete(fn func(orgID string)) {
	m.mu.Lock()
	m.onCycleComplete = fn
	m.mu.Unlock()
}

// SetRecipients forwards to the wrapped Profiler — see
// Profiler.SetRecipients. Multi-mode wiring only, once at boot before
// any Trigger.
func (m *Manager) SetRecipients(recipients repoevent.RecipientsFunc) {
	m.profiler.SetRecipients(recipients)
}

// Trigger signals the profiling runner for orgID. force bypasses the 3-day
// TTL (the explicit "Re-profile" button / a repo-set change wanting
// immediacy); a non-forced trigger is the per-poll-cycle TTL-gated pass, so
// steady-state cost is ~one staleness check per repo per cycle. If no runner
// exists yet for orgID, one is lazy-created and started; subsequent triggers
// merge into the runner's buffered channel (single-flight per org). A
// pending force sticks, so a forced re-profile is never downgraded by a
// coincident TTL-gated trigger. Distinct orgs run concurrently.
//
// Empty orgID is dropped with a log line: every caller has an orgID (events
// carry evt.OrgID, the re-profile handler has the request orgID), so an
// empty value points at an emitter bug worth surfacing rather than papering
// over by routing to the local sentinel.
func (m *Manager) Trigger(orgID string, force bool) {
	if orgID == "" {
		repoprofileLog.Warn("empty org on profiler trigger; dropping signal")
		return
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	r, ok := m.runners[orgID]
	if !ok {
		r = m.newRunnerFor(orgID, m.onCycleComplete)
		r.Start()
		m.runners[orgID] = r
	}
	m.mu.Unlock()
	r.Trigger(force)
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
