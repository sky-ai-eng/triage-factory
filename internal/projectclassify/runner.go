package projectclassify

import (
	"context"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/syslimit"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// stage1Func runs one broad-pass Stage 1 Haiku classification. stage2Func runs
// one agent-mode Stage 2 call (it takes the project KB dir as cwd). Both are
// the classifier's unit-test seam — per-instance fields on the Runner,
// defaulted in NewRunner to the real implementations, overridable in tests.
// They replace the package-level mutable vars `runStage1Haiku` /
// `runStage2Haiku`, mirroring the repo-profiler's batchFn pattern.
// orgID is carried explicitly (rather than read off the receiver inside the
// seam) so a stub can assert the Runner's org threads through to the model
// call; secrets/recorder/limiter are read off the receiver by the real impls.
type stage1Func func(ctx context.Context, orgID, prompt string) (int, string, error)
type stage2Func func(ctx context.Context, orgID, prompt, cwd string) (int, string, error)

// Runner drives project classification for a single org as a background loop.
// It mirrors ai.Runner: a buffered trigger channel coalesces signals
// (single-flight) and a stop channel cancels any in-flight Haiku call on
// shutdown. Each cycle classifies the org's unclassified entities against its
// projects via a per-project quorum vote. The per-org split (one Runner per
// org, owned by the Manager) is what keeps a large org's backlog from
// head-of-line-blocking other tenants.
type Runner struct {
	orgID    string
	entities db.EntityStore
	projects db.ProjectStore
	secrets  agentproc.SecretsReader // per-org LLM-credential reader threaded into Classify → Haiku (nil in local; system-door in multi).
	recorder *systemllm.Recorder     // captures per-vote LLM cost + tokens into system_llm_runs (TFAC-451)
	limiter  *syslimit.Limiter       // shared system-job sandbox cap (nil → unlimited).

	// stage1Fn / stage2Fn are the test seam (see stage1Func / stage2Func),
	// defaulted in NewRunner to the real implementations.
	stage1Fn stage1Func
	stage2Fn stage2Func

	trigger  chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	running  bool
}

func NewRunner(entities db.EntityStore, projects db.ProjectStore, orgID string, secrets agentproc.SecretsReader, recorder *systemllm.Recorder, limiter *syslimit.Limiter) *Runner {
	r := &Runner{
		orgID:    orgID,
		entities: entities,
		projects: projects,
		secrets:  secrets,
		recorder: recorder,
		limiter:  limiter,
		trigger:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
	r.stage1Fn = r.realRunStage1Haiku
	r.stage2Fn = r.realRunStage2Haiku
	return r
}

// Trigger signals the runner to check for unclassified entities.
// Non-blocking — if a cycle is already pending, the signal is merged.
func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *Runner) Start() {
	// Derive a ctx that cancels when Stop() closes r.stop, so any
	// in-flight Haiku call (which now goes through agentproc.Run → SDK
	// subprocess) gets SIGKILL'd on server shutdown rather than
	// blocking until the model times out on its own.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-r.stop
		cancel()
	}()
	go func() {
		for {
			select {
			case <-r.trigger:
				r.run(ctx)
			case <-r.stop:
				return
			}
		}
	}()
}

// Stop cancels the runner's loop and any in-flight cycle. Idempotent via
// stopOnce — a second close(r.stop) would panic. The Manager is the sole
// caller today, but guarding it makes a direct or repeat Stop safe and matches
// the repo-profiler runner, so all three background runners behave alike.
func (r *Runner) Stop() { r.stopOnce.Do(func() { close(r.stop) }) }

// run classifies this org's unclassified entities against its projects for one
// cycle. Single-flight (the running guard) so an accidental overlapping caller
// can't run two cycles for the same org concurrently. Per-org by construction
// — the org-enumeration loop that lived here previously moved up to the
// Manager's per-org Trigger. Local mode collapses to N=1 (the
// runmode.LocalDefaultOrgID sentinel) so behavior is unchanged.
func (r *Runner) run(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	entities, err := r.entities.ListUnclassifiedSystem(ctx, r.orgID)
	if err != nil {
		classifyLog.Error("list unclassified entities failed", "org", r.orgID, "error", err)
		return
	}
	if len(entities) == 0 {
		return
	}

	projects, err := r.projects.ListSystem(ctx, r.orgID)
	if err != nil {
		classifyLog.Error("list projects failed", "org", r.orgID, "error", err)
		return
	}

	if len(projects) == 0 {
		// No projects to vote — stamp classified_at on every unclassified
		// entity so we don't re-fire on every poll cycle. The
		// project-creation popup is the path to retro-assign these once
		// projects exist.
		for _, e := range entities {
			if err := r.entities.AssignProjectSystem(ctx, r.orgID, e.ID, nil, ""); err != nil {
				classifyLog.Warn("stamp classified_at failed", "org", r.orgID, "entity", e.ID, "error", err)
			}
		}
		return
	}

	classifyLog.Info("classifying entities against projects", "org", r.orgID, "entities", len(entities), "projects", len(projects))

	assigned := 0
	skipped := 0
	for _, e := range entities {
		winner, votes := r.classify(ctx, projects, e)
		// All votes errored — leave classified_at NULL so the entity
		// resurfaces next cycle. Stamping it here would permanently
		// freeze the entity at unassigned even if the underlying
		// failure (claude CLI unavailable, transient network) clears.
		if allErrored(votes) {
			skipped++
			classifyLog.Warn("all votes errored, leaving unclassified for retry", "entity", e.ID, "votes", len(votes))
			continue
		}
		rationale := bestRationale(votes)
		if winner != nil {
			classifyLog.Info("entity classified to project", "entity", e.ID, "project", *winner)
			assigned++
		} else {
			best := -1
			for _, v := range votes {
				if v.Err == nil && v.Score > best {
					best = v.Score
				}
			}
			classifyLog.Info("entity unassigned, best score below threshold", "entity", e.ID, "best_score", best, "threshold", ConfidenceThreshold)
		}
		if err := r.entities.AssignProjectSystem(ctx, r.orgID, e.ID, winner, rationale); err != nil {
			classifyLog.Error("assign entity failed", "entity", e.ID, "error", err)
		}
	}
	classifyLog.Info("cycle complete", "org", r.orgID, "assigned", assigned, "total", len(entities), "retried_next_cycle", skipped)
}

// allErrored returns true iff there's at least one vote and every vote
// carries an Err. Used to decide whether the runner should stamp
// classified_at: a fully-failed cycle (likely a systemic CLI/network
// outage) should retry on the next trigger rather than permanently
// freezing the entity.
func allErrored(votes []Vote) bool {
	if len(votes) == 0 {
		return false
	}
	for _, v := range votes {
		if v.Err == nil {
			return false
		}
	}
	return true
}

// bestRationale picks the rationale of the highest-scoring vote (winner
// or runner-up), so unassigned entities still record "closest match was
// X at N/100, because: …". Errored votes are skipped. Returns empty
// string if no successful vote exists.
func bestRationale(votes []Vote) string {
	bestScore := -1
	best := ""
	for _, v := range votes {
		if v.Err != nil {
			continue
		}
		if v.Score > bestScore {
			bestScore = v.Score
			best = v.Rationale
		}
	}
	return best
}
