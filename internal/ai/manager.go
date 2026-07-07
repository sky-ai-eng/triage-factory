package ai

import (
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/syslimit"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// Manager owns per-org scoring Runners. Each org gets its own Runner —
// its own trigger channel, its own single-flight cycle gate — so a slow
// scoring cycle on one tenant doesn't head-of-line-block scoring on
// others. This matters specifically because OnScoringCompleted drives
// the router's ReDeriveAfterScoring → delegation flow: a stalled cycle
// on org A would otherwise gate every org B's min_autonomy_suitability
// trigger behind it.
//
// Runners are lazy-created on first Trigger(orgID). Every caller has an
// orgID in scope by construction (events carry evt.OrgID, the router
// has it as a method param, the server's carry-over path has the
// request orgID), so the Manager never needs to enumerate the orgs
// table on its own. Local mode collapses to one Runner under the
// runmode.LocalDefaultOrgID sentinel — behavior is functionally
// identical to a single-runner build, just routed through the map.
type Manager struct {
	scores    db.ScoreStore
	entities  db.EntityStore
	secrets   agentproc.SecretsReader // per-org LLM-credential reader (nil in local → ambient subscription; system-door reader in multi).
	recorder  *systemllm.Recorder     // captures per-batch LLM cost + tokens into system_llm_runs (TFAC-451)
	limiter   *syslimit.Limiter       // shared system-job sandbox cap, injected into every per-org Runner.
	callbacks RunnerCallbacks

	mu      sync.Mutex
	runners map[string]*Runner
	stopped bool
}

func NewManager(scores db.ScoreStore, entities db.EntityStore, secrets agentproc.SecretsReader, recorder *systemllm.Recorder, limiter *syslimit.Limiter, callbacks RunnerCallbacks) *Manager {
	return &Manager{
		scores:    scores,
		entities:  entities,
		secrets:   secrets,
		recorder:  recorder,
		limiter:   limiter,
		callbacks: callbacks,
		runners:   make(map[string]*Runner),
	}
}

// Trigger signals the scoring runner for the given org. If no runner
// exists yet for orgID, one is lazy-created and started. Subsequent
// triggers for the same org merge into the runner's buffered trigger
// channel (single-flight per org). Distinct orgs run concurrently —
// that's the whole point of the split.
//
// Empty orgID is dropped with a log line: every caller has an orgID
// available, so an empty value points at an emitter bug we'd rather
// surface than paper over by routing to the local sentinel.
func (m *Manager) Trigger(orgID string) {
	if orgID == "" {
		aiLog.Warn("empty org on trigger; dropping signal")
		return
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	r, ok := m.runners[orgID]
	if !ok {
		r = NewRunner(m.scores, m.entities, orgID, m.secrets, m.recorder, m.limiter, m.callbacks)
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
