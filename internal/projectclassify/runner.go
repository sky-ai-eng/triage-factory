package projectclassify

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/syslimit"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
)

// stage1Func runs one broad-pass classification call. It is the
// classifier's unit-test seam — a per-instance field on the Runner,
// defaulted in NewRunner to the real implementation, overridable in tests —
// mirroring the repo-profiler's batchFn pattern.
// orgID is carried explicitly (rather than read off the receiver inside the
// seam) so a stub can assert the Runner's org threads through to the model
// call; secrets/recorder/limiter are read off the receiver by the real impl.
//
// model is the org's background-jobs model, resolved once at the top of the
// cycle and passed down rather than read per vote — a cycle spends on one
// model, and every vote it casts is recorded against that one.
type stage1Func func(ctx context.Context, orgID string, p votePrompt, model string) (int, string, error)

// Runner drives project classification for a single org as a background loop.
// It mirrors ai.Runner: a buffered trigger channel coalesces signals
// (single-flight) and a stop channel cancels any in-flight vote call on
// shutdown. Each cycle classifies the org's unclassified entities against its
// projects via a per-project quorum vote. The per-org split (one Runner per
// org, owned by the Manager) is what keeps a large org's backlog from
// head-of-line-blocking other tenants.
type Runner struct {
	orgID      string
	entities   db.EntityStore
	projects   db.ProjectStore
	secrets    agentproc.SecretsReader // per-org LLM-credential reader threaded into Classify → the vote call (nil in local; system-door in multi).
	llmResolve llmResolveFunc          // brain-side role-aware LLM resolver (nil in local/tests).
	recorder   *systemllm.Recorder     // captures per-vote LLM cost + tokens into system_llm_runs (TFAC-451)
	limiter    *syslimit.Limiter       // shared system-job sandbox cap (nil → unlimited).
	models     systemllm.ModelFunc     // resolves the org's background-jobs model; a cycle with no usable one skips.
	kb         *kbstore.Store          // multi-mode KB blob store; set by the Manager, nil in local/tests.

	// stage1Fn is the test seam (see stage1Func), defaulted in NewRunner
	// to the real implementation.
	stage1Fn stage1Func

	trigger  chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	running  bool
}

func NewRunner(entities db.EntityStore, projects db.ProjectStore, orgID string, secrets agentproc.SecretsReader, llmResolve llmResolveFunc, recorder *systemllm.Recorder, limiter *syslimit.Limiter, models systemllm.ModelFunc) *Runner {
	r := &Runner{
		orgID:      orgID,
		entities:   entities,
		projects:   projects,
		secrets:    secrets,
		llmResolve: llmResolve,
		recorder:   recorder,
		limiter:    limiter,
		models:     models,
		trigger:    make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}
	r.stage1Fn = r.realRunStage1
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
	// in-flight vote call (which now goes through agentproc.Run → SDK
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

	// After the single-flight guard — see the scorer's twin.
	ctx, span := telemetry.StartJobCycle(ctx, "classifier", r.orgID)
	defer span.End()

	entities, err := r.entities.ListUnclassifiedSystem(ctx, r.orgID)
	if err != nil {
		span.SetStatus(codes.Error, "list unclassified entities")
		classifyLog.ErrorContext(ctx, "list unclassified entities failed", "org", r.orgID, "error", err)
		return
	}
	span.SetAttributes(telemetry.Count(len(entities)))
	if len(entities) == 0 {
		span.SetAttributes(telemetry.Outcome("nothing_to_classify"))
		return
	}

	projects, err := r.projects.ListSystem(ctx, r.orgID)
	if err != nil {
		span.SetStatus(codes.Error, "list projects")
		classifyLog.ErrorContext(ctx, "list projects failed", "org", r.orgID, "error", err)
		return
	}

	if len(projects) == 0 {
		span.SetAttributes(telemetry.Outcome("no_projects"))
		// No projects to vote — stamp classified_at on every unclassified
		// entity so we don't re-fire on every poll cycle. The
		// project-creation popup is the path to retro-assign these once
		// projects exist.
		for _, e := range entities {
			if _, err := r.entities.AssignProjectSystem(ctx, r.orgID, e.ID, nil, ""); err != nil {
				classifyLog.Warn("stamp classified_at failed", "org", r.orgID, "entity", e.ID, "error", err)
			}
		}
		return
	}

	// Resolve the org's background-jobs model before any vote is cast. Either way
	// this cycle classifies nothing — it never substitutes a model of TF's
	// choosing — and either way classified_at stays NULL, exactly as it does for
	// an all-errored entity, so everything resurfaces next cycle.
	//
	// The two reasons it can fail are not the same event. An unusable SETTING is
	// a configuration state whose remedy is an org admin picking a model: WARN,
	// and the next cycle asks again. Anything else — the settings row could not
	// be read, nothing was wired to read it with — is a failed cycle, and gets
	// what the two reads above it get, so a wedged database is not quietly
	// reported as an org that has not chosen.
	model, err := r.models.Resolve(ctx, r.orgID)
	switch {
	case errors.Is(err, systemllm.ErrNoModel):
		span.SetAttributes(telemetry.Outcome("no_model"))
		classifyLog.WarnContext(ctx, "skipping classification cycle", "org", r.orgID, "error", err)
		return
	case err != nil:
		span.SetStatus(codes.Error, "resolve background jobs model")
		classifyLog.ErrorContext(ctx, "resolve background jobs model failed", "org", r.orgID, "error", err)
		return
	}

	classifyLog.Info("classifying entities against projects", "org", r.orgID, "entities", len(entities), "projects", len(projects), "model", model)

	assigned := 0
	skipped := 0
	for _, e := range entities {
		winner, votes := r.classify(ctx, projects, e, model)
		// All votes errored — leave classified_at NULL so the entity
		// resurfaces next cycle. Stamping it here would permanently
		// freeze the entity at unassigned even if the underlying
		// failure (claude CLI unavailable, transient network) clears.
		if allErrored(votes) {
			skipped++
			// A provider-backoff skip (systemllm's circuit breaker) is an
			// anticipated, self-healing deferral, not a genuine failure —
			// log it quietly so a boot-time overload doesn't read as a wall
			// of errors.
			if allProviderBackoff(votes) {
				classifyLog.Info("all votes deferred; provider backing off, retrying next cycle", "entity", e.ID, "votes", len(votes))
			} else {
				classifyLog.Warn("all votes errored, leaving unclassified for retry", "entity", e.ID, "votes", len(votes))
			}
			continue
		}

		tiedScore, tied := bestVotes(votes)
		var rationale string
		if len(tied) > 1 {
			// pickWinner already resolved this to nil; bestRationale would
			// otherwise quote one arbitrary tied vote's language as if it
			// were a confident pick, misrepresenting why the entity ended
			// up unassigned.
			rationale = fmt.Sprintf("Classifier confidence tied at %d/100 across %d candidate projects; resolved to unassigned rather than guess.", tiedScore, len(tied))
		} else {
			rationale = bestRationale(votes)
		}

		switch {
		case winner != nil:
			classifyLog.Info("entity classified to project", "entity", e.ID, "project", *winner)
			assigned++
		case len(tied) > 1:
			classifyLog.Info("entity unassigned, exact tie for top score", "entity", e.ID, "score", tiedScore, "tied_projects", len(tied))
		default:
			best := -1
			for _, v := range votes {
				if v.Err == nil && v.Score > best {
					best = v.Score
				}
			}
			classifyLog.Info("entity unassigned, best score below threshold", "entity", e.ID, "best_score", best, "threshold", ConfidenceThreshold)
		}
		if _, err := r.entities.AssignProjectSystem(ctx, r.orgID, e.ID, winner, rationale); err != nil {
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

// allProviderBackoff reports whether every vote failed specifically because
// the upstream provider was in circuit-breaker backoff (see
// systemllm.IsProviderBackoff) — an anticipated, self-healing skip, not a
// genuine failure worth a Warn. Only meaningful when allErrored(votes) is
// already true.
func allProviderBackoff(votes []Vote) bool {
	if len(votes) == 0 {
		return false
	}
	for _, v := range votes {
		if !systemllm.IsProviderBackoff(v.Err) {
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
