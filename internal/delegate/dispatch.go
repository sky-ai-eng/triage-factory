// The run-queue dispatcher + the blueprint state-machine reactor — the
// queue-driven replacement for the in-memory runBlueprint for-loop. A blueprint
// step is enqueued as a runs row in status='queued'; the dispatcher claims it,
// rehydrates the shared workspace, runs the agent, and on terminal the reactor
// advances the blueprint_run (enqueue the next step / finalize / leave parked).
// Sequencing lives on blueprint_runs (current_step_index + cancel_requested), so
// no goroutine holds it and a mid-flight blueprint is crash-recoverable.
//
// Single-process, single worker today (one dispatcher; horizontal executors are
// a separate concern). The same claim mechanism scales to N workers — the
// Postgres claim already uses FOR UPDATE SKIP LOCKED.

package delegate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
)

// maxRunAttempts caps how many times the dispatcher re-claims a single queued
// run before failing it as a poison pill. A healthy step runs on attempt 1;
// repeated attempts mean a deterministic setup crash, and failing it out stops
// the dispatcher from spinning one row while the rest of the queue waits.
const maxRunAttempts = 5

// Default cadences for RunDispatcher, exported so main can tune them and tests
// can drive the loop fast. The scan is the correctness backstop (a dropped wake
// only defers a claim to the next tick); the wake channel is the latency nudge.
const DefaultRunScanInterval = 2 * time.Second

// wakeDispatcher nudges the dispatcher to drain now rather than wait for the
// next scan tick. Best-effort, non-blocking: a full buffer means a wake is
// already pending, so dropping this one loses nothing.
func (s *Spawner) wakeDispatcher() {
	if s.dispatchWake == nil {
		return
	}
	select {
	case s.dispatchWake <- struct{}{}:
	default:
	}
}

// RunDispatcher is the run-queue drain loop — the queue-driven orchestrator's
// worker. On boot it reconciles runs/blueprint_runs stranded by a crash, then
// claims queued steps and drives each through runAgent + the reactor until ctx
// is cancelled. A nil RunQueueStore makes this a logged no-op.
func (s *Spawner) RunDispatcher(ctx context.Context, scanInterval time.Duration) {
	if s.runQueue == nil {
		dispatchLog.Warn("run-queue dispatcher not started: no RunQueueStore wired")
		return
	}

	s.reconcileRunQueue(ctx)

	scan := time.NewTicker(scanInterval)
	defer scan.Stop()

	s.drainRunQueue(ctx) // drain whatever survived the restart / boot reconcile

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.dispatchWake:
			s.drainRunQueue(ctx)
		case <-scan.C:
			s.drainRunQueue(ctx)
		}
	}
}

// reconcileRunQueue is the boot crash-recovery sweep (decision #4). Runs left
// mid-flight by a crash (claimed/running/setup statuses) are re-queued so the
// dispatcher re-claims and re-runs them; a mid-flight blueprint thus resumes by
// re-running its current step. Dormant `open` runs are left parked — they resume
// through their own paths, not the queue.
func (s *Spawner) reconcileRunQueue(ctx context.Context) {
	// No step-plan handling needed: a re-queued mid-flight run is re-claimed by
	// dispatchClaimedRun, which reads the plan frozen on its blueprint_run (off
	// br.StepPlan), so the resumed step runs the same program it was minted with.
	//
	// Ownership-scoped (TFAC-578): this only sweeps rows this instance itself
	// claimed during an earlier boot (executorIdentity()), never a live
	// sibling's claimed/running work — see RunQueueStore.ResetProcessingRuns.
	executorID, bootEpoch := s.executorIdentity()
	n, err := s.runQueue.ResetProcessingRuns(ctx, executorID, bootEpoch)
	if err != nil {
		dispatchLog.Error("boot reconcile: reset in-flight runs failed", "error", err)
		return
	}
	if n > 0 {
		dispatchLog.Info("boot reconcile: re-queued in-flight runs stranded by a crash", "count", n)
	}

	// Mirror sweep for the opposite desync — child runs left
	// non-terminal under an already-terminal blueprint_run. ResetProcessingRuns
	// above only requeues under a *running* parent, so these orphans are
	// invisible to it; left alone, a 'running' orphan keeps the dispatcher on
	// phantom work and pins its feature branch in a worktree, requeuing any
	// sibling fetch forever. Cancel them so the row stops looking live (the
	// worktree.Cleanup sweep already reclaimed the on-disk dir for non-parked
	// runs). The atomic cancel in MarkRunStatus prevents new desyncs; this heals
	// rows broken before that landed.
	c, err := s.runQueue.ReconcileOrphanedRuns(ctx)
	if err != nil {
		dispatchLog.Error("boot reconcile: cancel orphaned child runs failed", "error", err)
		return
	}
	if c > 0 {
		dispatchLog.Info("boot reconcile: cancelled child runs orphaned under a terminal blueprint_run", "count", c)
	}
}

// drainRunQueue claims queued runs and hands each to a goroutine, bounded by
// the process-wide concurrency semaphore, until the queue is empty (or ctx is
// cancelled). It acquires a slot BEFORE each claim, so at capacity it blocks on
// a free slot rather than reserving a run it can't yet run (no claimed-but-idle
// 'running' rows). Each claimed run executes — setup, agent, reactor — off the
// dispatcher, so the loop keeps claiming the next run (up to the cap) without
// waiting for the previous one to finish. The claim is FOR UPDATE SKIP LOCKED,
// so this is the same mechanism a future N-worker dispatcher uses; the
// semaphore is what keeps a burst of queued steps from fanning into an
// unbounded number of agent subprocesses on one host.
func (s *Spawner) drainRunQueue(ctx context.Context) {
	// Capture the semaphore once and use it for both acquire and release so a
	// startup-time SetMaxConcurrentRuns can't strand a token on a replaced
	// channel.
	sem := s.semaphore()
	for {
		if ctx.Err() != nil {
			return
		}
		// Memory guardrail: when host MemAvailable is below the floor,
		// stop claiming — runs stay queued and the next scan tick (or a
		// wake) re-checks. Checked per iteration, not per drain, because
		// each claimed run consumes memory as it spawns. Deliberately
		// BEFORE the semaphore acquire so a gated host parks with no
		// slot held.
		if s.dispatchMemGated() {
			return
		}
		// Acquire a concurrency slot BEFORE claiming, so we never flip a run to
		// 'running' that then sits idle waiting for a slot. Blocks at capacity
		// until a finishing run releases its slot; a shutdown breaks the wait
		// (in-flight runs are ctx-cancelled, and the boot reconcile re-queues
		// anything left mid-flight).
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		executorID, bootEpoch := s.executorIdentity()
		run, err := s.runQueue.ClaimNextRun(ctx, executorID, bootEpoch)
		if err != nil {
			<-sem
			dispatchLog.Warn("claim next run failed; retrying on the next scan", "error", err)
			return
		}
		if run == nil {
			<-sem
			return // queue drained — release the slot we acquired but didn't use
		}
		// run is a fresh per-iteration `:=` binding (not a loop variable), so each
		// goroutine captures its own; the deferred receive hands the slot back on
		// terminal.
		go func() {
			defer func() { <-sem }()
			s.dispatchClaimedRun(ctx, run)
		}()
	}
}

// dispatchClaimedRun runs one claimed blueprint step: load its context,
// rehydrate the shared workspace, materialize the step skill, run the agent,
// then hand the terminal state to the reactor. Failures before the agent runs
// requeue the run (transient) or fail it out (poison) without wedging the queue.
//
// Context split: the pre-agent setup reads honor the dispatcher ctx so a
// shutdown (or a future timeout) cancels them cleanly — a ctx-cancelled read
// returns early and leaves the claimed run 'running' for the next boot's
// reconcile to re-queue, never failing the blueprint on a clean shutdown. The
// terminal/reactor writes below deliberately stay on a detached
// context.Background(): once the agent has run, the blueprint MUST be advanced
// or finalized to avoid stranding it, so those must not be abortable by a
// shutdown mid-finalize (the same detached-terminal-write convention the rest of
// the spawner follows).
func (s *Spawner) dispatchClaimedRun(ctx context.Context, run *domain.AgentRun) {
	orgID := run.OrgID
	startTime := time.Now()

	br, err := s.blueprints.GetRunSystem(ctx, orgID, run.BlueprintRunID)
	if err != nil || br == nil {
		if ctx.Err() != nil {
			return // dispatcher shutting down — leave the claimed run for boot reconcile
		}
		// The owning blueprint_run is gone — nothing to drive. Fail the orphaned
		// run so it leaves the queue rather than re-claiming forever.
		s.failClaimedRun(orgID, run, fmt.Sprintf("load blueprint_run: %v", err))
		return
	}
	task, err := s.tasks.GetSystem(ctx, orgID, run.TaskID)
	if err != nil || task == nil {
		if ctx.Err() != nil {
			return
		}
		s.terminateBlueprint(orgID, br.ID, run.TaskID, run.TriggerType, run.CreatorUserID, startTime,
			runConfig{orgID: orgID, teamID: run.TeamID}, domain.BlueprintRunStatusFailed, fmt.Sprintf("load task: %v", err), run.BlueprintStepIndex, true)
		return
	}

	// A cancel raced in between the claim and now (the claim filters
	// cancel_requested, but the window is non-zero), or the blueprint is already
	// terminal: don't run the agent. Mark this claimed step cancelled and
	// finalize the blueprint if it's still running. Detached writes — this is a
	// terminal disposition, not abortable by shutdown.
	if br.Status != domain.BlueprintRunStatusRunning || br.CancelRequested {
		if _, mErr := s.agentRuns.MarkCancelledIfActiveSystem(context.Background(), orgID, run.ID, "user_cancelled", "Blueprint cancelled by user"); mErr != nil {
			dispatchLog.Warn("mark raced-cancel step cancelled failed", "run", run.ID, "error", mErr)
		}
		s.broadcastRunUpdate(orgID, run.ID, "cancelled")
		if br.Status == domain.BlueprintRunStatusRunning {
			cfg := runConfig{orgID: orgID, teamID: run.TeamID, wtPath: br.WorktreePath, hasWT: br.WorktreePath != "" && task.EntitySource == "github"}
			s.terminateBlueprint(orgID, br.ID, task.ID, run.TriggerType, run.CreatorUserID, startTime, cfg,
				domain.BlueprintRunStatusCancelled, "cancelled", run.BlueprintStepIndex, false)
		}
		return
	}

	// Sequence off the plan frozen at mint (br.StepPlan), not the live
	// blueprint_steps/prompts — an edit to the blueprint mid-flight must not
	// change what this run executes. The step + prompt are reconstructed from
	// the snapshot, so nothing on this path re-reads blueprint_steps/prompts.
	plan := br.StepPlan
	if len(plan) == 0 {
		s.terminateBlueprint(orgID, br.ID, task.ID, run.TriggerType, run.CreatorUserID, startTime,
			runConfig{orgID: orgID, teamID: run.TeamID}, domain.BlueprintRunStatusFailed, "blueprint run has empty step plan", run.BlueprintStepIndex, true)
		return
	}
	stepIdx := 0
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	if stepIdx < 0 || stepIdx >= len(plan) {
		s.terminateBlueprint(orgID, br.ID, task.ID, run.TriggerType, run.CreatorUserID, startTime,
			runConfig{orgID: orgID, teamID: run.TeamID}, domain.BlueprintRunStatusFailed, fmt.Sprintf("step index %d out of range", stepIdx), run.BlueprintStepIndex, true)
		return
	}
	planStep := plan[stepIdx]
	step := planStep.Step(br.BlueprintID)
	stepPrompt := planStep.Prompt()

	// Resolve the run's GitHub client (per-org/owner seam). model is already on
	// the claimed row (captured at Delegate time, stable across the blueprint).
	gh := s.resolveGHClient(ctx, orgID, ownerForTask(*task))

	// The blueprint_run is live on this step → place the task in_progress before
	// any (possibly slow) workspace setup, so the board reflects the work
	// immediately. The aggregate bounces in_progress ↔ in_review as steps
	// park/resume.
	s.recomputeTaskBoardColumn(orgID, task.ID)

	// Build (step 0, first claim) or rehydrate (later steps / crash re-claim) the
	// shared workspace. A transient setup failure requeues; a persistent one
	// fails the blueprint.
	cfg, err := s.buildStepConfig(ctx, orgID, br, *task, *run, gh)
	if err != nil {
		s.handleStepSetupError(orgID, br, *run, err)
		return
	}
	cfg.orgID = orgID
	cfg.teamID = run.TeamID
	cfg.isBlueprintStep = true
	cfg.blueprintRunID = br.ID
	cfg.blueprintStep = stepIdx

	// Increment the step prompt's usage, routed per trigger type.
	if run.TriggerType == "manual" {
		if e := s.tx.SyntheticClaimsWithTx(ctx, orgID, run.CreatorUserID, func(ts db.TxStores) error {
			return ts.Prompts.IncrementUsage(ctx, orgID, stepPrompt.ID)
		}); e != nil {
			dispatchLog.Warn("increment usage for step prompt failed", "prompt", stepPrompt.ID, "error", e)
		}
	} else if e := s.prompts.IncrementUsageSystem(ctx, orgID, stepPrompt.ID); e != nil {
		dispatchLog.Warn("increment usage for step prompt failed", "prompt", stepPrompt.ID, "error", e)
	}

	// Materialize this step's skill, wiping any prior step's so step N only sees
	// its own SKILL.md.
	if err := skills.WipeBlueprintSkills(cfg.wtPath); err != nil {
		dispatchLog.Warn("wipe skills failed", "run", run.ID, "step", stepIdx, "error", err)
	}
	slug := skills.SlugForBlueprintStep(stepIdx, stepPrompt.Name)
	if err := skills.MaterializeStepSkill(cfg.wtPath, slug, stepPrompt, step.Brief); err != nil {
		s.terminateBlueprint(orgID, br.ID, task.ID, run.TriggerType, run.CreatorUserID, startTime, cfg,
			domain.BlueprintRunStatusFailed, fmt.Sprintf("materialize step %d skill: %s", stepIdx, err.Error()), &stepIdx, false)
		return
	}

	// Position bit + tool extensions, exactly as the old loop set them.
	cfg.appendSysPrompt = nonterminalStepSysPrompt(stepIdx, len(plan))
	cfg.extraAllowedTools = s.collectExtraTools(stepPrompt.AllowedTools)

	// Next-step name comes from the frozen plan too — no live prompt read; an
	// empty name (impossible from a well-formed plan) falls back to "step N".
	var nextStepName string
	if stepIdx+1 < len(plan) {
		nextStepName = plan[stepIdx+1].PromptName
	}
	mission := buildBlueprintStepWrapperPrompt(*task, step, stepPrompt, slug, len(plan), nextStepName)

	toast.Info(s.wsHub, orgID, fmt.Sprintf("Blueprint step %d/%d: %s (%s)",
		stepIdx+1, len(plan), truncateToastMsg(stepPrompt.Name, 60), shortRunID(run.ID)))

	// Per-step cancel handle so CancelBlueprint can SIGKILL the active subprocess.
	stepCtx, stepCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancels[run.ID] = stepCancel
	s.mu.Unlock()

	// run.SessionID is empty on a first claim and non-empty when this run was
	// re-claimed mid-flight by a crash — runAgent resumes it when the warm
	// session survived, else starts fresh.
	s.runAgent(stepCtx, run.ID, *task, mission, cfg, time.Now(), run.Model, run.TriggerType, run.CreatorUserID, run.SessionID)

	s.mu.Lock()
	delete(s.cancels, run.ID)
	s.mu.Unlock()
	stepCancel()

	// Re-read the step run for its terminal status, then react. Detached ctx on
	// purpose: the agent has run, so we must read its terminal and advance/finalize
	// the blueprint even if the dispatcher is shutting down — skipping the reactor
	// here would strand the blueprint 'running' with no queued next step.
	stepRun, err := s.agentRuns.GetSystem(context.Background(), orgID, run.ID)
	if err != nil || stepRun == nil {
		s.terminateBlueprint(orgID, br.ID, task.ID, run.TriggerType, run.CreatorUserID, startTime, cfg,
			domain.BlueprintRunStatusFailed, fmt.Sprintf("read step %d run after agent: %v", stepIdx, err), &stepIdx, false)
		return
	}
	// stepRun loses OrgID/Attempts through Get; carry the claim's identity fields
	// the reactor needs (trigger type, creator, org) and the authoritative model
	// (stable across the blueprint) so the next enqueued step inherits it.
	stepRun.OrgID = orgID
	stepRun.TriggerType = run.TriggerType
	stepRun.CreatorUserID = run.CreatorUserID
	stepRun.Model = run.Model
	s.reactToStepTerminal(orgID, br, *stepRun, cfg, startTime)
}

// reactToStepTerminal is the blueprint state-machine reactor: given a step run
// that has reached a terminal (or parked) state, advance the blueprint_run.
// This is the post-step switch lifted out of the old runBlueprint loop —
// continue→enqueue-next, finish→complete+close, abort→leave-open,
// open→leave parked — now driven by the DB rather than a
// goroutine stack. It calls recomputeTaskBoardColumn on every transition so the
// board stays live under the queue model.
func (s *Spawner) reactToStepTerminal(orgID string, br *domain.BlueprintRun, stepRun domain.AgentRun, cfg runConfig, startTime time.Time) {
	triggerType := stepRun.TriggerType
	creatorUserID := stepRun.CreatorUserID
	stepIdx := 0
	if stepRun.BlueprintStepIndex != nil {
		stepIdx = *stepRun.BlueprintStepIndex
	}

	// Parked mid-step: leave the blueprint running, the worktree on disk, and the
	// snapshot in the blob store for the resume path. The aggregate column lands
	// the task in_review. Only `open` parks now: a step that queued a
	// draft PR / pending review completes normally and the orchestrator advances —
	// the artifact is a sidecar, surfaced via the derived approval column below.
	if stepRun.Status == "open" {
		dispatchLog.Info("blueprint_run step paused; blueprint remains running", "blueprint_run", br.ID, "step", stepIdx, "status", stepRun.Status)
		s.recomputeTaskBoardColumn(orgID, br.TaskID)
		return
	}

	// Sequence-level cancel: a cancel requested while the step was running ends
	// the blueprint without enqueuing the next step. Re-read the signal — it may
	// have been raised after the claim. On a read error we proceed with the
	// pre-agent br: a cancel raised in this narrow window could then be missed and
	// enqueue one extra step (the documented claim-window trade-off), so log it.
	if fresh, err := s.blueprints.GetRunSystem(context.Background(), orgID, br.ID); err != nil {
		dispatchLog.Warn("reactor: refresh blueprint_run for cancel check failed; proceeding with pre-agent state (a cancel in this window may enqueue one extra step)", "blueprint_run", br.ID, "error", err)
	} else if fresh != nil {
		br = fresh
	}
	if br.Status != domain.BlueprintRunStatusRunning {
		return // already finalized by a racing cancel/terminate
	}
	if br.CancelRequested {
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusCancelled, "cancelled", &stepIdx, false)
		return
	}

	switch stepRun.Status {
	case "cancelled":
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusCancelled, "step cancelled", &stepIdx, false)
		return
	case "failed", "task_unsolvable":
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusFailed, "step "+stepRun.Status, &stepIdx, false)
		return
	case "completed":
		// fall through to the outcome decision below
	default:
		// Any unexpected non-terminal status: the blueprint can't
		// sensibly continue, so fail it.
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusFailed, "step ended with status "+stepRun.Status, &stepIdx, false)
		return
	}

	// Advance off the frozen plan (br was just refreshed via GetRunSystem, so its
	// StepPlan is the snapshot taken at mint), never the live blueprint_steps —
	// editing the blueprint mid-flight must not redirect a running execution.
	plan := br.StepPlan
	if len(plan) == 0 {
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusFailed, "blueprint run has empty step plan", &stepIdx, false)
		return
	}
	isFinal := stepIdx >= len(plan)-1
	decision, abortReason := decideBlueprintStep(stepRun.Outcome, isFinal)
	switch decision {
	case blueprintStepAdvance:
		next := stepIdx + 1
		task, err := s.tasks.GetSystem(context.Background(), orgID, br.TaskID)
		if err != nil || task == nil {
			s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
				domain.BlueprintRunStatusFailed, "load task for advance", &stepIdx, false)
			return
		}
		// Bump the durable sequencing pointer, then enqueue the next step. Order
		// matters: the pointer is set first so a crash between here and the
		// enqueue leaves current_step_index naming the step the boot reconcile
		// would re-drive.
		if err := s.blueprints.SetRunCurrentStepSystem(context.Background(), orgID, br.ID, next); err != nil {
			dispatchLog.Warn("set current_step_index for blueprint_run failed", "blueprint_run", br.ID, "error", err)
		}
		if err := s.enqueueBlueprintStep(context.Background(), orgID, br.ID, *task, plan[next].Step(br.BlueprintID), stepModelOrInherit(plan[next].Model, stepRun.Model), triggerType, br.TriggerID, creatorUserID, br.ActorAgentID); err != nil {
			s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
				domain.BlueprintRunStatusFailed, fmt.Sprintf("enqueue step %d: %v", next, err), &stepIdx, false)
			return
		}
		// The shared worktree stays on disk (cfg.isBlueprintStep kept runAgent
		// from cleaning it), so the next claim warm-reuses it. Nudge the
		// dispatcher to pick the step up now.
		s.recomputeTaskBoardColumn(orgID, br.TaskID)
		s.wakeDispatcher()
	case blueprintStepFinish:
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusCompleted, "", &stepIdx, false)
	case blueprintStepAbort:
		reason := abortReason
		if reason == "" {
			reason = stepRun.OutcomeReason
		}
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusAborted, reason, &stepIdx, false)
	}
}

// stepModelOrInherit resolves the model a blueprint step runs on from its
// per-step override (Prompt.Model, frozen into the plan as
// BlueprintPlanStep.Model) and the run's inherited model — the team default for
// step 0, the prior step's model on an advance. `inherited` has already been
// through the org max-tier cap (domain.EffectiveModel in resolveAIModelForTeam).
//
// The override is DOWNGRADE-ONLY: it applies only when it names a known, cheaper
// tier than `inherited`. A same-or-higher — or unrecognized — override is
// ignored. This lets a step pick a cheaper model (the shipped PR-review
// blueprint runs Opus reviewers, then drops its aggregator step to Haiku)
// while making it structurally impossible for a per-step value to escalate past
// the org cap baked into `inherited`. An empty override inherits unchanged,
// preserving the long-standing one-model-per-blueprint behavior. Honoring a
// within-cap escalation would require threading the org cap down to here; no
// shipped or known use needs it yet.
func stepModelOrInherit(stepModel, inherited string) string {
	if stepModel == "" {
		return inherited
	}
	stepTier := domain.ParseTier(stepModel)
	if stepTier == domain.TierUnknown {
		return inherited
	}
	if stepTier < domain.ParseTier(inherited) {
		return stepTier.String()
	}
	return inherited
}

// enqueueBlueprintStep mints a queued runs row for step stepIndex of a
// blueprint_run. Shared by Delegate (step 0) and the reactor (every advance).
// actorAgentID is the executing bot, frozen on the blueprint_run at mint and
// passed through here so every step inherits the same runs.actor_agent_id —
// resolved once at the delegation entry point, never re-derived from the task
// claim (which is empty at step 0 on the event path and cleared by a takeover).
// triggerID is likewise the blueprint_run's frozen firing event_handler,
// denormalized onto every step's runs.trigger_id (empty for manual → NULL):
// the JOIN-free llm_spend view reads autonomous spend attribution off the runs
// row alone (the usage by-rule breakdown, TFAC-478), so a step run without it
// would show as autonomous cost attributable to no rule. TriggeringEventID is
// deliberately NOT inherited — the replay fence relocated to blueprint_runs,
// and stamping it per step would collide a multi-step chain on the leftover
// runs_event_trigger_fence index.
func (s *Spawner) enqueueBlueprintStep(ctx context.Context, orgID, blueprintRunID string, task domain.Task, step domain.BlueprintStep, model, triggerType, triggerID, creatorUserID, actorAgentID string) error {
	stepIdx := step.StepIndex
	return s.runQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID:                 uuid.New().String(),
		TaskID:             task.ID,
		PromptID:           step.StepPromptID,
		Status:             "queued",
		Model:              model,
		TriggerType:        triggerType,
		TriggerID:          triggerID,
		CreatorUserID:      creatorUserID,
		ActorAgentID:       actorAgentID,
		BlueprintRunID:     blueprintRunID,
		BlueprintStepIndex: &stepIdx,
	})
}

// buildStepConfig produces the runConfig for a claimed step. On the first claim
// of a blueprint (br.WorktreePath empty) it builds the shared worktree via the
// source-specific setup and stamps the resolved path onto the blueprint_run. On
// every later claim it reconstructs the lightweight config from the task and
// guarantees the shared worktree is on disk (warm reuse, or cold rehydrate from
// the durable snapshot via ensureWorkspace).
func (s *Spawner) buildStepConfig(ctx context.Context, orgID string, br *domain.BlueprintRun, task domain.Task, run domain.AgentRun, gh *ghclient.Client) (runConfig, error) {
	if br.WorktreePath == "" {
		var (
			cfg runConfig
			err error
		)
		switch task.EntitySource {
		case "github":
			cfg, err = s.setupGitHub(ctx, orgID, run.ID, task, gh)
		case "jira":
			cfg, err = s.setupJira(ctx, orgID, run.ID, task, gh)
		case "slack":
			cfg, err = s.setupSlack(ctx, orgID, run.ID, task, gh)
		default:
			err = fmt.Errorf("unsupported task source: %s", task.EntitySource)
		}
		if err != nil {
			return runConfig{}, err
		}
		// Stamp the shared worktree path onto the blueprint_run so later steps
		// (and the resume/cancel cleanup) can reconstruct it.
		if e := s.blueprints.SetRunWorktreePathSystem(context.Background(), orgID, br.ID, cfg.wtPath); e != nil {
			dispatchLog.Warn("set worktree_path for blueprint_run failed", "blueprint_run", br.ID, "error", e)
		}
		return cfg, nil
	}

	// Later step (or crash re-claim): reconstruct config + ensure the shared
	// worktree exists. ensureWorkspace warm-returns the on-disk path or cold-
	// rebuilds it from the snapshot keyed by the blueprint_run id.
	runForWS := &domain.AgentRun{ID: run.ID, WorktreePath: br.WorktreePath, BlueprintRunID: br.ID}
	cfg := runConfig{orgID: orgID, projectID: lookupEntityProjectID(s.entities, orgID, task.EntityID)}
	switch task.EntitySource {
	case "github":
		owner, repo, prNumber := parseGitHubTask(task)
		cfg.owner, cfg.repo, cfg.prNumber = owner, repo, prNumber
		cfg.scope = fmt.Sprintf("Repository: %s/%s\nPR: #%d", owner, repo, prNumber)
		cfg.toolsRef = ai.GHToolsTemplate
		cfg.hasWT = true
		wt, err := s.ensureWorkspace(ctx, orgID, runForWS, owner, repo, "")
		if err != nil {
			return runConfig{}, err
		}
		cfg.wtPath, cfg.runRoot = wt, wt
	case "jira":
		cfg.scope = fmt.Sprintf("Jira issue: %s", task.EntitySourceID)
		cfg.toolsRef = ai.GHToolsTemplate + "\n\n" + ai.JiraToolsTemplate
		cfg.hasWT = false
		wt, err := s.ensureWorkspace(ctx, orgID, runForWS, "", "", "")
		if err != nil {
			return runConfig{}, err
		}
		cfg.wtPath, cfg.runRoot = wt, wt
	case "slack":
		cfg.scope = fmt.Sprintf("Slack thread: %s", task.EntitySourceID)
		toolsRef := ai.GHToolsTemplate
		if ref, ok := ai.ToolsReferenceFor("slack"); ok {
			toolsRef += "\n\n" + ref
		}
		cfg.toolsRef = toolsRef
		cfg.hasWT = false
		wt, err := s.ensureWorkspace(ctx, orgID, runForWS, "", "", "")
		if err != nil {
			return runConfig{}, err
		}
		cfg.wtPath, cfg.runRoot = wt, wt
	default:
		return runConfig{}, fmt.Errorf("unsupported task source: %s", task.EntitySource)
	}
	return cfg, nil
}

// parseGitHubTask splits a GitHub PR task's "owner/repo#N" entity source id into
// its parts. prNumber is 0 when absent/unparseable; callers surface that as a
// setup failure.
func parseGitHubTask(task domain.Task) (owner, repo string, prNumber int) {
	repoStr := task.EntitySourceID
	if idx := strings.LastIndex(task.EntitySourceID, "#"); idx >= 0 {
		repoStr = task.EntitySourceID[:idx]
		fmt.Sscanf(task.EntitySourceID[idx+1:], "%d", &prNumber)
	}
	owner, repo = parseOwnerRepo(repoStr)
	return owner, repo, prNumber
}

// handleStepSetupError requeues a claimed run whose workspace setup hit a
// transient error, or fails the blueprint out once the run exhausts its retry
// budget (poison pill). The shared worktree, if partially built, stays on disk.
func (s *Spawner) handleStepSetupError(orgID string, br *domain.BlueprintRun, run domain.AgentRun, setupErr error) {
	if run.Attempts >= maxRunAttempts {
		dispatchLog.Error("workspace setup failed after attempts; failing blueprint", "run", run.ID, "attempts", run.Attempts, "error", setupErr)
		s.terminateBlueprint(orgID, br.ID, run.TaskID, run.TriggerType, run.CreatorUserID, time.Now(),
			runConfig{orgID: orgID, teamID: run.TeamID, wtPath: br.WorktreePath, hasWT: br.WorktreePath != ""},
			domain.BlueprintRunStatusFailed, "workspace setup: "+setupErr.Error(), run.BlueprintStepIndex, false)
		return
	}
	dispatchLog.Warn("workspace setup failed, requeuing", "run", run.ID, "attempt", run.Attempts, "error", setupErr)
	if err := s.runQueue.RequeueRun(context.Background(), orgID, run.ID, "workspace setup: "+setupErr.Error()); err != nil {
		dispatchLog.Warn("requeue run after setup failure failed", "run", run.ID, "error", err)
	}
}

// failClaimedRun marks an orphaned claimed run failed (its blueprint_run
// vanished, so there is nothing to drive). Best-effort.
func (s *Spawner) failClaimedRun(orgID string, run *domain.AgentRun, reason string) {
	dispatchLog.Error("marking run failed", "run", run.ID, "reason", reason)
	if _, err := s.agentRuns.MarkFailedIfActiveSystem(context.Background(), orgID, run.ID, ""); err != nil {
		dispatchLog.Warn("mark orphaned run failed", "run", run.ID, "error", err)
	}
}
