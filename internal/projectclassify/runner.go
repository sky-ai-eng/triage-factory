package projectclassify

import (
	"context"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Runner manages the project-classification background loop. Mirrors
// the shape of internal/ai/Runner: a buffered trigger channel, idempotent
// during an active cycle, started/stopped from main.go. Pollers signal
// `Trigger()` after a poll cycle finishes (via an event-bus subscriber
// in main.go) and the runner picks up any newly-discovered entities
// that haven't been classified yet.
type Runner struct {
	entities db.EntityStore
	projects db.ProjectStore
	orgs     db.OrgsStore            // enumerate active orgs per cycle
	secrets  agentproc.SecretsReader // per-org LLM-credential reader threaded into Classify → Haiku (nil in local; system-door in multi). SKY-389.
	trigger  chan struct{}
	stop     chan struct{}
	mu       sync.Mutex
	running  bool
}

func NewRunner(entities db.EntityStore, projects db.ProjectStore, orgs db.OrgsStore, secrets agentproc.SecretsReader) *Runner {
	return &Runner{
		entities: entities,
		projects: projects,
		orgs:     orgs,
		secrets:  secrets,
		trigger:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
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

func (r *Runner) Stop() {
	close(r.stop)
}

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

	orgIDs, err := r.orgs.ListActiveSystem(ctx)
	if err != nil {
		classifyLog.Error("list active orgs failed", "error", err)
		return
	}
	for _, orgID := range orgIDs {
		r.runOrg(ctx, orgID)
	}
}

// runOrg classifies one org's unclassified entities against its
// projects. Per-org errors are logged and the loop continues — a
// transient failure on one org shouldn't block classification on
// other orgs in the cycle. Local mode collapses to N=1 (the
// runmode.LocalDefaultOrgID sentinel) so behavior is unchanged.
func (r *Runner) runOrg(ctx context.Context, orgID string) {
	entities, err := r.entities.ListUnclassifiedSystem(ctx, orgID)
	if err != nil {
		classifyLog.Error("list unclassified entities failed", "org", orgID, "error", err)
		return
	}
	if len(entities) == 0 {
		return
	}

	projects, err := r.projects.ListSystem(ctx, orgID)
	if err != nil {
		classifyLog.Error("list projects failed", "org", orgID, "error", err)
		return
	}

	if len(projects) == 0 {
		// No projects to vote — stamp classified_at on every unclassified
		// entity so we don't re-fire on every poll cycle. The
		// project-creation popup is the path to retro-assign these once
		// projects exist.
		for _, e := range entities {
			if err := r.entities.AssignProjectSystem(ctx, orgID, e.ID, nil, ""); err != nil {
				classifyLog.Warn("stamp classified_at failed", "org", orgID, "entity", e.ID, "error", err)
			}
		}
		return
	}

	classifyLog.Info("classifying entities against projects", "org", orgID, "entities", len(entities), "projects", len(projects))

	assigned := 0
	skipped := 0
	for _, e := range entities {
		winner, votes := Classify(ctx, orgID, r.secrets, projects, e)
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
		if err := r.entities.AssignProjectSystem(ctx, orgID, e.ID, winner, rationale); err != nil {
			classifyLog.Error("assign entity failed", "entity", e.ID, "error", err)
		}
	}
	classifyLog.Info("cycle complete", "org", orgID, "assigned", assigned, "total", len(entities), "retried_next_cycle", skipped)
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
