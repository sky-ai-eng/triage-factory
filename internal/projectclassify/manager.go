package projectclassify

import (
	"context"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/syslimit"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// Manager owns per-org classification Runners, mirroring ai.Manager. Each org
// gets its own Runner — its own trigger channel and single-flight cycle gate —
// so a large org's classification backlog can't head-of-line-block every other
// tenant's classification until the next poll cycle. Previously the
// classifier was a single Runner that looped over every active org serially
// under one process-wide gate; this brings it in line with the scorer and
// repo-profiler. The per-org work was already correctly scoped (entities
// listed, classified, and written back per org with that org's credentials);
// the only change is latency/fairness in multi-mode. Local mode collapses to
// one Runner under the runmode.LocalDefaultOrgID sentinel.
//
// Runners are lazy-created on first Trigger(orgID) and live for the process
// lifetime — the runners map is not pruned, mirroring the scorer's accepted
// trade-off, and is torn down wholesale by Stop().
type Manager struct {
	entities   db.EntityStore
	projects   db.ProjectStore
	secrets    agentproc.SecretsReader // per-org LLM-credential reader threaded into each Runner's vote calls (nil in local; system-door in multi).
	llmResolve llmResolveFunc          // brain-side role-aware LLM resolver (nil in local/tests).
	recorder   *systemllm.Recorder     // captures per-vote LLM cost + tokens into system_llm_runs (TFAC-451)
	limiter    *syslimit.Limiter       // shared system-job sandbox cap, injected into every per-org Runner.
	models     systemllm.ModelFunc     // resolves each org's background-jobs model, injected into every per-org Runner.
	kb         *kbstore.Store          // multi-mode KB blob store; nil in local, where the KB is read from disk.

	mu      sync.Mutex
	runners map[string]*Runner
	stopped bool
}

// SetKBStore wires the KB blob store so the classifier reads project
// knowledge from the object store in multi mode. Set once at startup; nil in
// local mode leaves readProjectKB on its byte-identical on-disk path. Runners
// created after this inherit the store.
func (m *Manager) SetKBStore(kb *kbstore.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kb = kb
}

// llmResolveFunc is the RunOptions.LLMResolver shape the classifier's vote
// calls carry — the brain-side llmcred adapter minting short-lived STS creds
// for a role-mode Bedrock org (nil in local/tests → Run's built-in
// resolution).
type llmResolveFunc func(ctx context.Context, orgID, model string) (map[string]string, error)

// NewManager builds a classification Manager. It holds entities because
// WaitFor (the spawner's pre-KB-injection block) reads classification state
// through it. Unlike the previous single Runner, the Manager holds no
// OrgsStore: triggering is per-org (the bus subscriber and WaitFor each supply
// an orgID), so nothing enumerates the orgs table anymore.
func NewManager(entities db.EntityStore, projects db.ProjectStore, secrets agentproc.SecretsReader, llmResolve llmResolveFunc, recorder *systemllm.Recorder, limiter *syslimit.Limiter, models systemllm.ModelFunc) *Manager {
	return &Manager{
		entities:   entities,
		projects:   projects,
		secrets:    secrets,
		llmResolve: llmResolve,
		recorder:   recorder,
		limiter:    limiter,
		models:     models,
		runners:    make(map[string]*Runner),
	}
}

// Trigger signals the classification runner for orgID. If no runner exists yet
// for orgID, one is lazy-created and started; subsequent triggers for the same
// org merge into the runner's buffered channel (single-flight per org).
// Distinct orgs run concurrently — the whole point of the split.
//
// Empty orgID is dropped with a log line: every caller has an orgID available
// (the bus subscriber reads evt.OrgID, WaitFor takes the run's orgID), so an
// empty value points at an emitter bug we'd rather surface than route to the
// local sentinel.
func (m *Manager) Trigger(orgID string) {
	if orgID == "" {
		classifyLog.Warn("empty org on classifier trigger; dropping signal")
		return
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	r, ok := m.runners[orgID]
	if !ok {
		r = NewRunner(m.entities, m.projects, orgID, m.secrets, m.llmResolve, m.recorder, m.limiter, m.models)
		r.kb = m.kb
		r.Start()
		m.runners[orgID] = r
	}
	m.mu.Unlock()
	r.Trigger()
}

// Stop tears down every per-org runner. After Stop, Trigger is a no-op. Safe
// to call multiple times.
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
