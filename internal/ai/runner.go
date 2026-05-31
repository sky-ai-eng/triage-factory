package ai

import (
	"context"
	"database/sql"
	"log"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunnerCallbacks are optional hooks fired during the scoring lifecycle.
// The caller wires these to WS broadcasts or other side effects.
type RunnerCallbacks struct {
	// OnScoringStarted fires at the top of a scoring cycle once
	// MarkScoring has stamped the in-progress flag on the picked tasks.
	// orgID identifies the cycle's tenant so subscribers (the WS
	// scoring_started broadcast) can scope per-connection fanout.
	OnScoringStarted func(orgID string, taskIDs []string)
	// OnScoringCompleted fires once per scoring cycle after the
	// task_scores writes commit. orgID is the scoring context (the
	// runner is per-org); the slice is the set of task IDs that
	// received fresh scores. Downstream re-derive needs orgID
	// threaded so its store calls hit the right tenant in multi
	// mode (every task in the slice belongs to orgID by construction).
	OnScoringCompleted func(orgID string, taskIDs []string)
	// OnTasksSkipped fires once per scoring cycle if one or more batches
	// errored. skipped is the exact count of tasks that weren't scored;
	// total is len(tasks) at cycle start. orgID is the scoring context
	// so per-tenant subscribers (toasts in multi-mode) route to the
	// right WS connection. Wired to a warning toast in main so the user
	// knows tasks were skipped without log-diving. Fatal errors (DB
	// failures) go through OnError.
	OnTasksSkipped func(orgID string, skipped, total int)
	// OnError fires on fatal scoring errors (query, write, or scorer-
	// returned errors that abort the cycle). orgID identifies the
	// tenant whose cycle failed; toast wiring in main.go scopes the
	// user-facing notification accordingly.
	OnError func(orgID string, err error)
}

// Runner manages AI scoring as a background process.
// It exposes a Trigger channel that pollers signal after ingesting new tasks.
//
// The 4 DB operations the runner does itself (UnscoredTasks, MarkScoring,
// ResetScoringToPending, UpdateTaskScores) go through db.ScoreStore so
// the same code path serves both SQLite (local) and Postgres (multi).
// The runner still holds the raw *sql.DB because ScoreTasks (the
// LLM-driven scorer in scorer.go) does its own multi-store reads
// against the connection — that path migrates in a later D2 wave.
type Runner struct {
	database  *sql.DB
	scores    db.ScoreStore
	entities  db.EntityStore // SKY-284: scorer bulk-loads entity descriptions for prompt context
	orgID     string         // scoring context org — runmode.LocalDefaultOrg in local mode
	callbacks RunnerCallbacks
	trigger   chan struct{}
	stop      chan struct{}
	mu        sync.Mutex
	running   bool
}

func NewRunner(database *sql.DB, scores db.ScoreStore, entities db.EntityStore, orgID string, callbacks RunnerCallbacks) *Runner {
	return &Runner{
		database:  database,
		scores:    scores,
		entities:  entities,
		orgID:     orgID,
		callbacks: callbacks,
		trigger:   make(chan struct{}, 1),
		stop:      make(chan struct{}),
	}
}

// Trigger signals the runner to check for unscored tasks.
// Non-blocking — if a scoring run is already pending, the signal is merged.
func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
		// already triggered, skip
	}
}

// reportError invokes the OnError callback if set.
func (r *Runner) reportError(err error) {
	if r.callbacks.OnError != nil {
		r.callbacks.OnError(r.orgID, err)
	}
}

func (r *Runner) Start() {
	// Derive a ctx that cancels when Stop() closes r.stop. This ctx is
	// passed into each run() so any in-flight scoring agent (which now
	// goes through agentproc.Run → SDK subprocess) gets SIGKILL'd on
	// server shutdown rather than blocking the shutdown until the model
	// times out on its own.
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

	tasks, err := r.scores.UnscoredTasks(ctx, r.orgID)
	if err != nil {
		log.Printf("[ai] error fetching unscored tasks: %v", err)
		r.reportError(err)
		return
	}

	if len(tasks) == 0 {
		return
	}

	log.Printf("[ai] scoring %d unscored tasks...", len(tasks))

	// Collect task IDs for callbacks
	taskIDs := make([]string, len(tasks))
	for i, t := range tasks {
		taskIDs[i] = t.ID
	}

	// Persist scoring state before calling AI
	if err := r.scores.MarkScoring(ctx, r.orgID, taskIDs); err != nil {
		log.Printf("[ai] error marking tasks as scoring: %v", err)
	}

	if r.callbacks.OnScoringStarted != nil {
		r.callbacks.OnScoringStarted(r.orgID, taskIDs)
	}

	scores, skippedTasks, err := ScoreTasks(ctx, r.database, r.entities, r.orgID, tasks)
	if err != nil {
		log.Printf("[ai] scoring failed: %v", err)
		r.reportError(err)
		// Fatal scoring error — every task was MarkScoring'd but none of
		// them will be transitioned to 'scored'. Reset the whole set back
		// to 'pending' so the next cycle retries them; otherwise they stay
		// stuck forever (UnscoredTasks only picks 'pending').
		if resetErr := r.scores.ResetScoringToPending(ctx, r.orgID, taskIDs); resetErr != nil {
			log.Printf("[ai] warning: failed to reset tasks to pending after scoring failure: %v", resetErr)
		}
		return
	}

	// Reset tasks that were in failed batches back to 'pending' so they
	// retry next cycle. Without this, a per-batch failure leaves those
	// tasks marked 'in_progress' forever since UpdateTaskScores only
	// transitions successfully-scored ones to 'scored'.
	if skippedTasks > 0 {
		scoredIDs := make(map[string]struct{}, len(scores))
		for _, s := range scores {
			scoredIDs[s.ID] = struct{}{}
		}
		var skippedIDs []string
		for _, id := range taskIDs {
			if _, ok := scoredIDs[id]; !ok {
				skippedIDs = append(skippedIDs, id)
			}
		}
		if len(skippedIDs) > 0 {
			if resetErr := r.scores.ResetScoringToPending(ctx, r.orgID, skippedIDs); resetErr != nil {
				log.Printf("[ai] warning: failed to reset %d skipped tasks to pending: %v", len(skippedIDs), resetErr)
			}
		}
		if r.callbacks.OnTasksSkipped != nil {
			r.callbacks.OnTasksSkipped(r.orgID, skippedTasks, len(tasks))
		}
	}

	updates := make([]domain.TaskScoreUpdate, len(scores))
	for i, s := range scores {
		updates[i] = domain.TaskScoreUpdate{
			ID:                  s.ID,
			PriorityScore:       s.PriorityScore,
			AutonomySuitability: s.AutonomySuitability,
			PriorityReasoning:   s.PriorityReasoning,
			Summary:             s.Summary,
		}
	}

	if err := r.scores.UpdateTaskScores(ctx, r.orgID, updates); err != nil {
		log.Printf("[ai] error saving scores: %v", err)
		r.reportError(err)
		// UpdateTaskScores failing means the in-memory scores are lost AND
		// the scored tasks are still marked 'in_progress'. Reset everything
		// still in that state so the next cycle re-scores. Previously-reset
		// skipped tasks are already 'pending' and the reset is idempotent.
		if resetErr := r.scores.ResetScoringToPending(ctx, r.orgID, taskIDs); resetErr != nil {
			log.Printf("[ai] warning: failed to reset tasks to pending after save failure: %v", resetErr)
		}
		return
	}

	log.Printf("[ai] scored %d tasks successfully", len(updates))

	if r.callbacks.OnScoringCompleted != nil {
		r.callbacks.OnScoringCompleted(r.orgID, taskIDs)
	}
}
