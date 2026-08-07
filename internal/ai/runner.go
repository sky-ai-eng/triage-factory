package ai

import (
	"context"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/syslimit"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
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
	//
	// ctx is the cycle's, and this hook alone takes one: the
	// post-scoring re-derive it drives does durable work (fires
	// deferred triggers), so it needs the cycle's values — trace
	// context above all — rather than starting from nothing. The
	// hook must not treat it as a lifetime; the cycle returns while
	// the re-derive is still running, so a caller that keeps ctx
	// past the call drops its cancellation first.
	OnScoringCompleted func(ctx context.Context, orgID string, taskIDs []string)
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
// The DB operations the runner does itself (ResetStaleScoring,
// UnscoredTasks, MarkScoring, ResetScoringToPending, UpdateTaskScores)
// go through db.ScoreStore so the same code path serves both SQLite
// (local) and Postgres (multi).
type Runner struct {
	scores     db.ScoreStore
	entities   db.EntityStore          // scorer bulk-loads entity descriptions for prompt context
	orgID      string                  // scoring context org — runmode.LocalDefaultOrgID in local mode
	secrets    agentproc.SecretsReader // per-org LLM-credential reader (nil in local → ambient subscription; system-door reader in multi).
	llmResolve llmResolveFunc          // brain-side role-aware LLM resolver (nil in local/tests → Run's built-in resolution).
	recorder   *systemllm.Recorder     // captures per-batch LLM cost + tokens into system_llm_runs (TFAC-451)
	limiter    *syslimit.Limiter       // shared system-job sandbox cap (nil → unlimited); captured by scoreFn.
	callbacks  RunnerCallbacks

	// scoreFn scores one batch. The unit-test seam (replaces a direct
	// scoreBatch call): defaulted in NewRunner to a closure over scoreBatch
	// capturing recorder + limiter, overridable in tests. Mirrors the
	// repo-profiler's batchFn.
	scoreFn batchScoreFn

	trigger  chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	running  bool
}

func NewRunner(scores db.ScoreStore, entities db.EntityStore, orgID string, secrets agentproc.SecretsReader, llmResolve llmResolveFunc, recorder *systemllm.Recorder, limiter *syslimit.Limiter, callbacks RunnerCallbacks) *Runner {
	r := &Runner{
		scores:     scores,
		entities:   entities,
		orgID:      orgID,
		secrets:    secrets,
		llmResolve: llmResolve,
		recorder:   recorder,
		limiter:    limiter,
		callbacks:  callbacks,
		trigger:    make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}
	r.scoreFn = func(ctx context.Context, tasks []TaskInput, orgID string, secrets agentproc.SecretsReader) ([]TaskScore, error) {
		return scoreBatch(ctx, tasks, orgID, secrets, llmResolve, recorder, limiter)
	}
	return r
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

// Stop cancels the runner's loop and any in-flight cycle. Idempotent via
// stopOnce — a second close(r.stop) would panic. The Manager is the sole
// caller today, but guarding it makes a direct or repeat Stop safe and matches
// the repo-profiler runner, so all three background runners behave alike.
func (r *Runner) Stop() { r.stopOnce.Do(func() { close(r.stop) }) }

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

	// After the single-flight guard, so a re-entrant call that bails
	// produces no span — that isn't a cycle.
	ctx, span := telemetry.StartJobCycle(ctx, "scorer", r.orgID)
	defer span.End()

	// Recover the rows a crashed cycle stranded in 'in_progress' before
	// this cycle picks its work. Ordering is the invariant: this runs
	// strictly before the cycle's own MarkScoring, so it can never reset
	// a row this cycle just claimed. Best-effort — a failure here leaves
	// the residue for the next cycle rather than costing this one the
	// genuinely-pending tasks it can still score.
	if stale, err := r.scores.ResetStaleScoring(ctx, r.orgID); err != nil {
		aiLog.WarnContext(ctx, "reset stale in-progress scoring failed", "error", err)
	} else if stale > 0 {
		aiLog.InfoContext(ctx, "recovered tasks stranded mid-scoring by a crashed cycle", "count", stale)
	}

	tasks, err := r.scores.UnscoredTasks(ctx, r.orgID)
	if err != nil {
		span.SetStatus(codes.Error, "fetch unscored tasks")
		aiLog.ErrorContext(ctx, "fetch unscored tasks failed", "error", err)
		r.reportError(err)
		return
	}

	span.SetAttributes(telemetry.Count(len(tasks)))
	if len(tasks) == 0 {
		span.SetAttributes(telemetry.Outcome("nothing_to_score"))
		return
	}

	aiLog.InfoContext(ctx, "scoring unscored tasks", "count", len(tasks))

	// Collect task IDs for callbacks
	taskIDs := make([]string, len(tasks))
	for i, t := range tasks {
		taskIDs[i] = t.ID
	}

	// Persist scoring state before calling AI
	if err := r.scores.MarkScoring(ctx, r.orgID, taskIDs); err != nil {
		aiLog.Error("mark tasks as scoring failed", "error", err)
	}

	if r.callbacks.OnScoringStarted != nil {
		r.callbacks.OnScoringStarted(r.orgID, taskIDs)
	}

	scores, skippedTasks, err := r.scoreTasks(ctx, tasks)
	if err != nil {
		span.SetStatus(codes.Error, "score tasks")
		aiLog.ErrorContext(ctx, "scoring failed", "error", err)
		r.reportError(err)
		// Fatal scoring error — every task was MarkScoring'd but none of
		// them will be transitioned to 'scored'. Reset the whole set back
		// to 'pending' so the next cycle retries them; otherwise they stay
		// stuck forever (UnscoredTasks only picks 'pending').
		if resetErr := r.scores.ResetScoringToPending(ctx, r.orgID, taskIDs); resetErr != nil {
			aiLog.Warn("reset tasks to pending after scoring failure failed", "error", resetErr)
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
				aiLog.Warn("reset skipped tasks to pending failed", "count", len(skippedIDs), "error", resetErr)
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
		aiLog.Error("save scores failed", "error", err)
		r.reportError(err)
		// UpdateTaskScores failing means the in-memory scores are lost AND
		// the scored tasks are still marked 'in_progress'. Reset everything
		// still in that state so the next cycle re-scores. Previously-reset
		// skipped tasks are already 'pending' and the reset is idempotent.
		if resetErr := r.scores.ResetScoringToPending(ctx, r.orgID, taskIDs); resetErr != nil {
			aiLog.Warn("reset tasks to pending after save failure failed", "error", resetErr)
		}
		return
	}

	aiLog.Info("scored tasks successfully", "count", len(updates))

	if r.callbacks.OnScoringCompleted != nil {
		// Pass only the IDs of tasks that actually received fresh scores
		// (the updates slice), not taskIDs (all originally-picked tasks).
		// When some batches fail, the skipped tasks are reset to 'pending'
		// and excluded from updates — calling OnScoringCompleted with their
		// IDs would let ReDeriveAfterScoring fire triggers against stale
		// scores from a prior cycle.
		scoredIDs := make([]string, len(updates))
		for i, u := range updates {
			scoredIDs[i] = u.ID
		}
		r.callbacks.OnScoringCompleted(ctx, r.orgID, scoredIDs)
	}
}
