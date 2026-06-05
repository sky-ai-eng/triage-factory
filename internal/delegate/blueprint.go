package delegate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "embed" // powers blueprintStepNonterminalPrompt

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// blueprintStepNonterminalPrompt is the addendum appended to a NON-terminal
// blueprint step's system prompt. It adds the `continue` outcome (framed as
// the default hand-off) plus the narrow `finish` exception to the base
// completion contract. The terminal step and the N=1 single-prompt case
// append nothing — normal completion there IS `finish`, identical to a
// single-prompt run, so they never see `continue`.
//
//go:embed prompts/blueprint-step-nonterminal.txt
var blueprintStepNonterminalPrompt string

// delegateBlueprint spawns the orchestrator goroutine for a blueprint_run the
// caller (Delegate) has already minted through the replay fence. It sets up the
// shared worktree, stamps the resolved worktree_path back onto the row, and
// walks the step list. The returned id is the blueprint_run id — the UI / API
// surfaces this as "the blueprint that was kicked off".
//
// Failures inside the goroutine (worktree setup failure, db write errors)
// terminate the blueprint with a matching abort_reason rather than surfacing an
// error to the caller — the caller already holds the blueprint_run id and the
// UI subscribes to the row by id, so a synchronous error wouldn't be reflected
// anywhere visible.
func (s *Spawner) delegateBlueprint(orgID string, task domain.Task, blueprint *domain.Blueprint, steps []domain.BlueprintStep, blueprintRunID, triggerType, triggerID, creatorUserID string, gh *ghclient.Client, model string) string {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[blueprintRunID] = cancel
	s.mu.Unlock()
	// Mark this id as a blueprint_run so the setup phase's status helpers
	// don't broadcast agent_run_update events for a non-existent runs row.
	s.markBlueprintRunID(blueprintRunID)

	// The blueprint_run is live → place the task in_progress immediately (a
	// no-op for non-bot-claimed tasks). The aggregate column then bounces
	// in_progress ↔ in_review as steps park and resume, and terminateBlueprint
	// owns the terminal column.
	s.recomputeTaskBoardColumn(orgID, task.ID)

	go func() {
		startTime := time.Now()
		defer func() {
			s.mu.Lock()
			delete(s.cancels, blueprintRunID)
			s.mu.Unlock()
			cancel()
			s.unmarkBlueprintRunID(blueprintRunID)
		}()

		// Build the shared worktree exactly once; every step reuses the result.
		var cfg runConfig
		var setupErr error
		switch task.EntitySource {
		case "github":
			cfg, setupErr = s.setupGitHub(ctx, orgID, blueprintRunID, task, gh)
		case "jira":
			cfg, setupErr = s.setupJira(ctx, orgID, blueprintRunID, task, gh)
		default:
			setupErr = fmt.Errorf("unsupported task source: %s", task.EntitySource)
		}
		cfg.orgID = orgID
		if setupErr != nil {
			// The blueprint_run row already exists (created in Delegate, still
			// 'running'); transition it to failed. skipCleanup — there is no
			// finished worktree to reclaim on a setup failure.
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg,
				domain.BlueprintRunStatusFailed, setupErr.Error(), nil, true)
			return
		}

		// Stamp the resolved worktree path onto the row now that setup ran (it
		// was created with an empty path so the fence could commit first).
		if err := s.blueprints.SetRunWorktreePathSystem(context.Background(), orgID, blueprintRunID, cfg.wtPath); err != nil {
			log.Printf("[blueprint] warning: set worktree_path for blueprint_run %s: %v", blueprintRunID, err)
		}

		verb := "Blueprint started"
		if triggerType == "event" {
			verb = "Auto-fired blueprint"
		}
		toast.Info(s.wsHub, orgID, fmt.Sprintf("%s: %s (%s)",
			verb, truncateToastMsg(blueprint.Name, 60), shortRunID(blueprintRunID)))

		s.runBlueprint(ctx, orgID, blueprintRunID, task, blueprint, steps, cfg, startTime, model, triggerType, creatorUserID)
	}()

	return blueprintRunID
}

// runBlueprint orchestrates a blueprint prompt against one task. It owns the
// shared worktree (built once via setupGitHub / setupJira) and walks
// the ordered step list, creating one runs row per step. After each
// step terminates, it reads the step run's terminal `runs.outcome` and
// decides whether to advance, finish, or abort (see decideBlueprintStep).
//
// Yield mid-blueprint and pending_approval mid-blueprint are handled
// separately via ResumeBlueprintAfterYield / ResumeBlueprintAfterApproval —
// the orchestrator returns early when the step lands in awaiting_input
// or pending_approval, leaving blueprint_runs.status='running' and the
// shared worktree on disk for the eventual resume.
func (s *Spawner) runBlueprint(
	ctx context.Context,
	orgID, blueprintRunID string,
	task domain.Task,
	blueprint *domain.Blueprint,
	steps []domain.BlueprintStep,
	cfg runConfig,
	startTime time.Time,
	model string,
	triggerType string,
	creatorUserID string,
) {
	if len(steps) == 0 {
		s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusFailed,
			"blueprint has no steps", nil, false)
		return
	}

	for i, step := range steps {
		if ctx.Err() != nil {
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusCancelled,
				"cancelled", &step.StepIndex, false)
			return
		}

		stepPrompt, err := s.prompts.GetSystem(ctx, orgID, step.StepPromptID)
		if err != nil || stepPrompt == nil {
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusFailed,
				fmt.Sprintf("step %d prompt fetch failed", i), &step.StepIndex, false)
			return
		}

		// Wipe any prior step's materialized skill so step N+1 only
		// sees its own SKILL.md.
		if err := skills.WipeBlueprintSkills(cfg.wtPath); err != nil {
			log.Printf("[blueprint] run %s step %d: wipe skills: %v", blueprintRunID, i, err)
		}
		slug := skills.SlugForBlueprintStep(i, stepPrompt.Name)
		if err := skills.MaterializeStepSkill(cfg.wtPath, slug, stepPrompt, step.Brief); err != nil {
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusFailed,
				fmt.Sprintf("materialize step %d skill: %s", i, err.Error()), &step.StepIndex, false)
			return
		}

		// Create the per-step run row. prompt_id points at the leaf
		// step prompt (so per-step stats stay accurate); blueprint_run_id
		// + blueprint_step_index thread it back to the blueprint instance.
		stepRunID := uuid.New().String()
		stepIdxCopy := i
		// Step runs inherit the blueprint's creator: manual blueprint steps
		// attribute to the user who initiated the blueprint (not the org
		// owner the createManual COALESCE would otherwise fall back to);
		// event-triggered blueprint steps stay creator-less (the
		// trigger_type='event' + creator_user_id IS NULL CHECK is what
		// the createEventTriggered routing satisfies).
		//
		// Routing mirrors Delegate's run insert: manual goes through
		// SyntheticClaimsWithTx so runs_insert's RLS check sees the
		// step's creator; event-triggered stays on the admin pool.
		stepRow := domain.AgentRun{
			ID:                 stepRunID,
			TaskID:             task.ID,
			PromptID:           stepPrompt.ID,
			Status:             "initializing",
			Model:              model,
			TriggerType:        triggerType,
			CreatorUserID:      creatorUserID,
			BlueprintRunID:     blueprintRunID,
			BlueprintStepIndex: &stepIdxCopy,
			WorktreePath:       cfg.wtPath,
		}
		var stepCreateErr error
		if triggerType == "manual" {
			stepCreateErr = s.tx.SyntheticClaimsWithTx(context.Background(), orgID, creatorUserID, func(ts db.TxStores) error {
				return ts.AgentRuns.Create(context.Background(), orgID, stepRow)
			})
		} else {
			stepCreateErr = s.agentRuns.Create(context.Background(), orgID, stepRow)
		}
		if stepCreateErr != nil {
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusFailed,
				fmt.Sprintf("create step %d run: %s", i, stepCreateErr.Error()), &step.StepIndex, false)
			return
		}
		s.broadcastRunUpdate(orgID, stepRunID, "initializing")
		// A step is starting → the blueprint's board column is in_progress
		// (unless a prior step is parked, which the aggregate also handles).
		s.recomputeTaskBoardColumn(orgID, task.ID)
		var incErr error
		if triggerType == "manual" {
			incErr = s.tx.SyntheticClaimsWithTx(ctx, orgID, creatorUserID, func(ts db.TxStores) error {
				return ts.Prompts.IncrementUsage(ctx, orgID, stepPrompt.ID)
			})
		} else {
			incErr = s.prompts.IncrementUsageSystem(ctx, orgID, stepPrompt.ID)
		}
		if incErr != nil {
			log.Printf("[blueprint] warning: failed to increment usage for step prompt %s: %v", stepPrompt.ID, incErr)
		}

		// Per-step cancel handle so Cancel(stepRunID) cancels just the
		// active step. The blueprint ctx itself stays alive across steps.
		stepCtx, stepCancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.cancels[stepRunID] = stepCancel
		s.mu.Unlock()

		stepCfg := cfg
		stepCfg.isBlueprintStep = true
		stepCfg.blueprintRunID = blueprintRunID
		stepCfg.blueprintStep = i
		// Position is one bit, system-injected: only non-terminal steps
		// get the contract fragment that offers `continue`. The terminal
		// step (and the N=1 single-prompt case) append nothing and complete
		// via the default `finish`, exactly like a single-prompt run. No
		// numeric index reaches the agent.
		stepCfg.appendSysPrompt = nonterminalStepSysPrompt(i, len(steps))
		stepCfg.extraAllowedTools = s.collectExtraTools(stepPrompt.AllowedTools)

		var nextStepName string
		if i+1 < len(steps) {
			if np, err := s.prompts.GetSystem(ctx, orgID, steps[i+1].StepPromptID); err == nil && np != nil {
				nextStepName = np.Name
			}
		}
		mission := buildBlueprintStepWrapperPrompt(task, step, stepPrompt, slug, len(steps), nextStepName)

		toast.Info(s.wsHub, orgID, fmt.Sprintf("Blueprint step %d/%d: %s (%s)",
			i+1, len(steps), truncateToastMsg(stepPrompt.Name, 60), shortRunID(stepRunID)))

		s.runAgent(stepCtx, stepRunID, task, mission, stepCfg, time.Now(), model, triggerType, creatorUserID)

		// Clear the cancel handle now that the step has returned.
		s.mu.Lock()
		delete(s.cancels, stepRunID)
		s.mu.Unlock()
		stepCancel()

		// Re-read the run row to learn its terminal status. runAgent's
		// return is unconditional — completion / failure / cancellation
		// / pending_approval / yield all come back through here.
		stepRun, err := s.agentRuns.GetSystem(context.Background(), orgID, stepRunID)
		if err != nil || stepRun == nil {
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusFailed,
				fmt.Sprintf("read step %d run after agent: %v", i, err), &step.StepIndex, false)
			return
		}

		// Yield / pending_approval mid-blueprint: leave the blueprint in
		// 'running' and the worktree on disk. The corresponding resume
		// hook (ResumeBlueprintAfterYield / ResumeBlueprintAfterApproval) will
		// pick up where we left off.
		if stepRun.Status == "awaiting_input" || stepRun.Status == "pending_approval" {
			log.Printf("[blueprint] run %s step %d paused at status=%s; blueprint remains running", blueprintRunID, i, stepRun.Status)
			// Parked step → the aggregate column is in_review ("needs 👀").
			s.recomputeTaskBoardColumn(orgID, task.ID)
			return
		}

		if stepRun.Status == "cancelled" {
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusCancelled,
				"step cancelled", &step.StepIndex, false)
			return
		}
		if stepRun.Status == "failed" || stepRun.Status == "task_unsolvable" {
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusFailed,
				"step "+stepRun.Status, &step.StepIndex, false)
			return
		}
		if stepRun.Status != "completed" {
			// Defensive: any unexpected non-terminal status (taken_over
			// is the most likely candidate) ends the blueprint in failed
			// state. taken_over runs are owned by the user from here on,
			// so the blueprint can't sensibly continue.
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusFailed,
				"step ended with status "+stepRun.Status, &step.StepIndex, false)
			return
		}

		// The step completed; interpret its terminal envelope outcome
		// (runs.outcome, persisted by processCompletion) to decide the
		// next move. continue advances; finish / last-step continue end
		// the blueprint completed and close the task; abort leaves it
		// open; a missing outcome on a non-final step is the floor after
		// the outcome gate exhausted its retries → abort no-outcome.
		isFinal := i == len(steps)-1
		decision, abortReason := decideBlueprintStep(stepRun.Outcome, isFinal)
		switch decision {
		case blueprintStepAdvance:
			// Fall through to the next loop iteration.
		case blueprintStepFinish:
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusCompleted,
				"", &step.StepIndex, false)
			return
		case blueprintStepAbort:
			reason := abortReason
			if reason == "" {
				// Explicit abort: copy the agent's "why I stopped" from
				// runs.outcome_reason into blueprint_runs.abort_reason.
				reason = stepRun.OutcomeReason
			}
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusAborted,
				reason, &step.StepIndex, false)
			return
		}
	}

	// Unreachable in practice — decideBlueprintStep never returns
	// blueprintStepAdvance for the final step, so the last iteration always
	// terminates above. Kept as a defensive net so a future edit that adds a
	// loop-continue path can't strand the blueprint in 'running'.
	s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusCompleted,
		"", nil, false)
}

// blueprintStepOutcome is the orchestrator's decision after a completed
// blueprint step, derived from the step's runs.outcome and its position.
type blueprintStepOutcome int

const (
	// blueprintStepAdvance moves to the next step (a non-final `continue`).
	blueprintStepAdvance blueprintStepOutcome = iota
	// blueprintStepFinish terminates the blueprint completed and closes the
	// task — an explicit `finish`, a final-step `continue` (structural
	// finish), or an unambiguous missing outcome on the final step.
	blueprintStepFinish
	// blueprintStepAbort terminates the blueprint aborted and leaves the task
	// open — an explicit `abort`, or a missing outcome on a non-final step.
	blueprintStepAbort
)

// decideBlueprintStep maps a completed step's terminal outcome + position to
// the orchestrator's next move. Only valid for a step whose run reached
// status='completed'; the non-terminal statuses (awaiting_input,
// pending_approval, cancelled, failed) are handled by the caller before this
// is consulted, and yield never reaches here (it parks the run in
// awaiting_input rather than completing).
//
// abortReason is non-empty only for the missing-outcome-on-a-non-final-step
// case ("no-outcome"); for an explicit abort it is empty and the caller
// copies runs.outcome_reason into blueprint_runs.abort_reason.
func decideBlueprintStep(outcome string, isFinal bool) (decision blueprintStepOutcome, abortReason string) {
	switch domain.RunOutcome(outcome) {
	case domain.RunOutcomeContinue:
		// continue hands off to the next step — except on the final step,
		// where there is no next step and continue resolves to a structural
		// finish.
		if isFinal {
			return blueprintStepFinish, ""
		}
		return blueprintStepAdvance, ""
	case domain.RunOutcomeFinish:
		return blueprintStepFinish, ""
	case domain.RunOutcomeAbort:
		return blueprintStepAbort, ""
	case "":
		// Missing outcome (empty string === SQL NULL). On the final step
		// (and therefore N=1) a missing outcome is unambiguous → finish,
		// exactly like a single-prompt run. On a non-final step it means
		// the outcome gate exhausted its retries without a usable hand-off;
		// abort with "no-outcome" rather than guess at advancement.
		if isFinal {
			return blueprintStepFinish, ""
		}
		return blueprintStepAbort, "no-outcome"
	default:
		// An unrecognized, non-empty outcome — a future/buggy value the
		// orchestrator can't interpret (a valid completed step only ever
		// holds continue/finish/abort, or NULL; yield never completes).
		// Never close a task on a value we don't understand: abort and leave
		// it open for a human, regardless of position. Mirrors the old
		// "unknown verdict → abort" floor.
		return blueprintStepAbort, "unknown-outcome: " + outcome
	}
}

// terminateBlueprint finalizes the blueprint run row and runs the shared
// worktree cleanup that runAgent's per-step defers skipped. taskDone
// distinguishes "all steps green, mark task done like a single run
// would" (status=completed) from "stopped early — leave the task open
// for human review" (any other terminal). skipCleanup short-circuits
// when the worktree itself is already gone (worktree_lost path).
//
// triggerType + creatorUserID route the terminal writes. Manual blueprints
// (and user-initiated CancelBlueprint / Resume* that pass "manual" + the
// requesting user's ID) write under synthetic claims; event-triggered
// blueprints write through the admin pool.
func (s *Spawner) terminateBlueprint(
	orgID, blueprintRunID, taskID, triggerType, creatorUserID string,
	startTime time.Time,
	cfg runConfig,
	status domain.BlueprintRunStatus,
	abortReason string,
	abortedAtStep *int,
	skipCleanup bool,
) {
	bgCtx := context.Background()
	var markErr error
	if triggerType == "manual" {
		markErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			_, mErr := ts.Blueprints.MarkRunStatus(bgCtx, orgID, blueprintRunID, status, abortReason, abortedAtStep)
			return mErr
		})
	} else {
		_, markErr = s.blueprints.MarkRunStatusSystem(bgCtx, orgID, blueprintRunID, status, abortReason, abortedAtStep)
	}
	if markErr != nil {
		log.Printf("[blueprint] FATAL: mark blueprint_run %s status=%s: %v — skipping cleanup to keep blueprint row consistent", blueprintRunID, status, markErr)
		return
	}

	if status == domain.BlueprintRunStatusCompleted {
		// Mirror single-run behavior: a clean blueprint finalization closes the task.
		var closeErr error
		if triggerType == "manual" {
			closeErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
				return ts.Tasks.Close(bgCtx, orgID, taskID, "run_completed", "")
			})
		} else {
			closeErr = s.tasks.CloseSystem(bgCtx, orgID, taskID, "run_completed", "")
		}
		if closeErr != nil {
			log.Printf("[blueprint] close task %s: %v", taskID, closeErr)
		}
	}
	// Aborted / failed / cancelled blueprints intentionally do NOT mark
	// the task done — leave it in the queue so a human can inspect the
	// steps' run memory (durable in run_memory) and decide what to do next.

	if !skipCleanup {
		s.runBlueprintWorktreeCleanup(blueprintRunID, cfg)
	}

	// Drop the durable workspace snapshot now the blueprint is terminal so the
	// blob store doesn't orphan it. Keyed by blueprint_run_id (the shared
	// workspace's key); idempotent, so this is a no-op for a blueprint that
	// never parked and never snapshotted. Covers every terminal path through
	// here: clean finish, abort/fail, cancel, and the approval-resume finalize.
	s.discardWorkspaceSnapshot(bgCtx, orgID, blueprintRunID)

	// Drain the per-entity queue exactly once for the blueprint (independent
	// of how many steps ran).
	if cfgEntity := taskEntityID(s.tasks, orgID, taskID); cfgEntity != "" {
		s.notifyDrainer(orgID, triggerType, cfgEntity)
	}

	dur := time.Since(startTime)
	log.Printf("[blueprint] blueprint_run %s terminated status=%s reason=%q duration=%s",
		blueprintRunID, status, abortReason, dur)
}

// runBlueprintWorktreeCleanup performs the cleanup runAgent would have done
// per-step, except now once for the whole blueprint.
func (s *Spawner) runBlueprintWorktreeCleanup(blueprintRunID string, cfg runConfig) {
	if cfg.hasWT {
		if err := worktree.RemoveAt(cfg.wtPath, blueprintRunID); err != nil {
			log.Printf("[blueprint] worktree remove failed for blueprint %s: %v", blueprintRunID, err)
			return
		}
		if cfg.prNumber > 0 && cfg.owner != "" && cfg.repo != "" {
			worktree.CleanupPRConfig(cfg.owner, cfg.repo, cfg.headRef, cfg.prNumber)
		}
	} else if cfg.runRoot != "" {
		// Jira blueprints materialize worktrees lazily via `workspace add`,
		// which keys run_worktrees rows by each *step's* run_id (the
		// agent's TRIAGE_FACTORY_RUN_ID), not by the blueprint_run_id.
		// Iterate every step run in the blueprint so we actually find and
		// remove their reservations.
		stepRuns, err := s.blueprints.RunsForBlueprintSystem(context.Background(), cfg.orgID, blueprintRunID)
		if err != nil {
			log.Printf("[blueprint] run %s: list step runs for cleanup: %v", blueprintRunID, err)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, sr := range stepRuns {
			rows, err := s.runWorktrees.ListSystem(context.Background(), cfg.orgID, sr.ID)
			if err != nil {
				log.Printf("[blueprint] run %s: list run_worktrees for step %s: %v", blueprintRunID, sr.ID, err)
				// Log but continue to attempt DB row deletion below.
				rows = nil
			}
			for _, w := range rows {
				if err := worktree.RemoveAt(w.Path, sr.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
					log.Printf("[blueprint] run %s: remove worktree %s: %v", blueprintRunID, w.Path, err)
					// Still attempt the DB row deletion even if the worktree remove failed.
				}
				if err := s.runWorktrees.DeleteByPathSystem(cleanupCtx, cfg.orgID, sr.ID, w.Path); err != nil {
					log.Printf("[blueprint] run %s: delete run_worktrees row for %s: %v", blueprintRunID, w.Path, err)
				}
			}
		}
		worktree.RemoveRunRoot(blueprintRunID)
	}
	worktree.RemoveClaudeProjectDir(cfg.wtPath)
}

// taskEntityID resolves the entity_id for a task. Used to drain the
// per-entity firing queue at blueprint terminal.
func taskEntityID(tasks db.TaskStore, orgID, taskID string) string {
	t, err := tasks.GetSystem(context.Background(), orgID, taskID)
	if err != nil {
		log.Printf("[blueprint] taskEntityID: failed to resolve entity for task %s: %v", taskID, err)
		return ""
	}
	if t == nil {
		return ""
	}
	return t.EntityID
}

// nonterminalStepSysPrompt returns the system-prompt addendum for step i of
// total steps: the non-terminal contract fragment (which offers `continue`)
// for every step before the last, and the empty string for the terminal step
// — and therefore the empty string for the whole N=1 single-step case, where
// i==0 is also the last step. This is the one position bit the agent's
// contract depends on; nothing else about ordering reaches the agent.
func nonterminalStepSysPrompt(i, total int) string {
	if i < total-1 {
		return blueprintStepNonterminalPrompt
	}
	return ""
}

// buildBlueprintStepWrapperPrompt produces the per-step user prompt carrying
// step-specific data. The base completion contract (envelope.txt) plus the
// non-terminal addendum (blueprint-step-nonterminal.txt, injected only for
// non-final steps) own the protocol; this wrapper supplies only the step's
// context.
func buildBlueprintStepWrapperPrompt(task domain.Task, step domain.BlueprintStep, stepPrompt *domain.Prompt, slug string, total int, nextStepName string) string {
	mission := strings.TrimSpace(step.Brief)
	if mission == "" {
		mission = stepPrompt.Name
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are step %d of %d in a blueprint firing on this task.\n\n", step.StepIndex+1, total)
	fmt.Fprintf(&b, "Task: %s\n", strings.TrimSpace(task.Title))
	fmt.Fprintf(&b, "Mission for this step: %s\n\n", mission)
	fmt.Fprintf(&b, "Skill slug: %q (materialized at ./.claude/skills/%s/SKILL.md)\n", slug, slug)
	isFinal := step.StepIndex+1 == total
	fmt.Fprintf(&b, "Is final step: %v\n", isFinal)
	if !isFinal {
		nextLabel := nextStepName
		if nextLabel == "" {
			nextLabel = fmt.Sprintf("step %d", step.StepIndex+2)
		}
		fmt.Fprintf(&b, "Next step: %q\n", nextLabel)
	}
	// Prior steps' findings are their memory files in this blueprint run's
	// namespace folder under _scratch/entity-memory/ — the <entity_memory>
	// contract tells the agent to read them first as its handoff. No separate
	// handoff file.
	return b.String()
}

// CancelBlueprint cancels every step inside a blueprint run, marks the blueprint
// row cancelled, and lets the active step's runAgent return naturally.
// Safe to call when the blueprint is already terminal.
//
// userID identifies the actor for audit. The cancel is always a
// user-initiated action (user clicked Cancel), so writes route under
// the user's synthetic claims regardless of the underlying blueprint's
// original trigger_type. In local mode callers pass
// runmode.LocalDefaultUserID; multi-mode handlers extract the user
// from JWT claims.
func (s *Spawner) CancelBlueprint(orgID, blueprintRunID, userID string) error {
	cr, err := s.blueprints.GetRunSystem(context.Background(), orgID, blueprintRunID)
	if err != nil {
		return fmt.Errorf("load blueprint run: %w", err)
	}
	if cr == nil {
		return fmt.Errorf("blueprint run %s not found", blueprintRunID)
	}
	if cr.Status != domain.BlueprintRunStatusRunning {
		return nil
	}

	// Cancel any cancel handles registered by the orchestrator for this
	// blueprint. The orchestrator stores per-step cancels under the step
	// run_id; we sweep all active step runs and cancel them. We also
	// cancel the blueprint's own ctx so the setup phase and inter-step
	// checks see the cancellation.
	//
	// anyActive tracks whether at least one orchestrator-owned context
	// got canceled. If nothing was active — the blueprint is paused on a
	// pending_approval or awaiting_input step — the orchestrator
	// goroutine has already exited, so no later path will run cleanup.
	// In that case we drive terminateBlueprint ourselves below.
	var anyActive bool
	stepIDs, err := s.blueprints.ActiveStepRunIDsSystem(context.Background(), orgID, blueprintRunID)
	if err == nil {
		s.mu.Lock()
		for _, runID := range stepIDs {
			if cancel, ok := s.cancels[runID]; ok {
				cancel()
				anyActive = true
			}
		}
		// Also cancel the blueprint-level context registered at delegateBlueprint.
		if blueprintCancel, ok := s.cancels[blueprintRunID]; ok {
			blueprintCancel()
			anyActive = true
		}
		s.mu.Unlock()
	}

	if anyActive {
		// Orchestrator goroutine is still alive — it will observe the
		// cancellation, the step's runAgent will return, and the loop
		// will call terminateBlueprint (which marks the blueprint cancelled and
		// runs cleanup). Avoid double-marking here so terminateBlueprint's
		// MarkRunStatus succeeds.
		return nil
	}

	// Paused blueprint: rebuild just enough cfg for terminateBlueprint's worktree
	// cleanup (mirrors ResumeBlueprintAfterApproval — owner/repo/prNumber
	// aren't persisted on blueprint_runs, so CleanupPRConfig is skipped).
	task, err := s.tasks.GetSystem(context.Background(), orgID, cr.TaskID)
	if err != nil || task == nil {
		log.Printf("[blueprint] CancelBlueprint: load task for paused blueprint_run %s: %v", blueprintRunID, err)
		// User-initiated cancel — write under the cancelling user's
		// synthetic claims rather than the blueprint's original trigger
		// identity. Audit shows "user X cancelled this blueprint".
		var markErr error
		_, markErr = s.markBlueprintRunStatusAsUser(context.Background(), orgID, userID, blueprintRunID, domain.BlueprintRunStatusCancelled, "user_cancelled", nil)
		return markErr
	}
	cfg := runConfig{orgID: orgID, wtPath: cr.WorktreePath}
	if task.EntitySource == "github" {
		cfg.hasWT = true
	}
	// User-initiated cancel uses "manual" routing with the cancelling
	// user's identity regardless of the blueprint's original trigger type.
	s.terminateBlueprint(orgID, cr.ID, cr.TaskID, "manual", userID, cr.StartedAt, cfg,
		domain.BlueprintRunStatusCancelled, "user_cancelled", nil, false)
	return nil
}

// markBlueprintRunStatusAsUser writes a blueprint_run status transition under
// the given user's synthetic claims. Used by user-initiated CancelBlueprint
// / Resume* paths that need to attribute the write to the requesting
// user even though the blueprint's original trigger_type may have been
// 'event'.
func (s *Spawner) markBlueprintRunStatusAsUser(ctx context.Context, orgID, userID, blueprintRunID string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	var changed bool
	err := s.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		c, mErr := ts.Blueprints.MarkRunStatus(ctx, orgID, blueprintRunID, status, abortReason, abortedAtStep)
		changed = c
		return mErr
	})
	return changed, err
}

// ResumeBlueprintAfterYield finalizes a blueprint after one of its step runs
// resumed from a yield and reached a terminal state (the respond endpoint drives
// it via ResumeAfterYield once processCompletion reports the step is no longer
// parked). It reads the resumed step's terminal runs.outcome + position and
// routes through terminateBlueprint, so a 1-step (or final-step) yield-resume
// closes the task on finish and leaves it open on abort — parity with the
// pre-collapse single-prompt yield-resume.
//
// A non-final step that wants to advance mid-blueprint (continue) is the epic's
// resume work and is not built here: the blueprint is terminated with a clear
// reason rather than silently stalling in 'running'.
//
// userID identifies the actor for audit (the user whose response resumed the
// yielded run). Local mode passes runmode.LocalDefaultUserID; multi-mode handlers
// extract it from JWT claims.
func (s *Spawner) ResumeBlueprintAfterYield(orgID, stepRunID, userID string) {
	cr, stepIdx, err := s.blueprints.GetRunForRunSystem(context.Background(), orgID, stepRunID)
	if err != nil || cr == nil {
		return
	}
	if cr.Status != domain.BlueprintRunStatusRunning {
		return
	}

	stepRun, err := s.agentRuns.GetSystem(context.Background(), orgID, stepRunID)
	if err != nil || stepRun == nil {
		log.Printf("[blueprint] yield-resume run %s: read step run: %v", stepRunID, err)
		return
	}
	// Still dormant after the resume (yielded again / queued an approval) → the
	// blueprint stays running; the next respond/approval drives finalization.
	if stepRun.Status == "awaiting_input" || stepRun.Status == "pending_approval" {
		return
	}

	task, err := s.tasks.GetSystem(context.Background(), orgID, cr.TaskID)
	if err != nil || task == nil {
		log.Printf("[blueprint] yield-resume: load task for blueprint_run %s: %v", cr.ID, err)
		_, _ = s.markBlueprintRunStatusAsUser(context.Background(), orgID, userID, cr.ID, domain.BlueprintRunStatusFailed, "yield_resume_task_load_failed", stepIdx)
		return
	}
	cfg := runConfig{orgID: orgID, wtPath: cr.WorktreePath}
	if task.EntitySource == "github" {
		cfg.hasWT = true
	}

	// isFinal = this is the last step (and therefore the only step for N=1).
	// Defaults to true when the index or step list can't be resolved, so an
	// unknown position never advances into the unbuilt mid-blueprint resume.
	isFinal := true
	if stepIdx != nil {
		if steps, err := s.blueprints.ListStepsSystem(context.Background(), orgID, cr.BlueprintID); err == nil && len(steps) > 0 {
			isFinal = *stepIdx >= len(steps)-1
		}
	}

	status, reason := blueprintTerminalForResumedStep(stepRun, isFinal)
	s.terminateBlueprint(orgID, cr.ID, cr.TaskID, "manual", userID, cr.StartedAt, cfg,
		status, reason, stepIdx, false)
}

// blueprintTerminalForResumedStep maps a resumed step run's terminal state +
// position to the blueprint's terminal status. Mirrors runBlueprint's in-loop
// disposition for the resume path: a clean completion routes through
// decideBlueprintStep (finish/advance/abort), and the non-terminal-completed
// statuses map to the matching blueprint terminal.
func blueprintTerminalForResumedStep(stepRun *domain.AgentRun, isFinal bool) (domain.BlueprintRunStatus, string) {
	switch stepRun.Status {
	case "completed":
		decision, abortReason := decideBlueprintStep(stepRun.Outcome, isFinal)
		switch decision {
		case blueprintStepFinish:
			return domain.BlueprintRunStatusCompleted, ""
		case blueprintStepAbort:
			reason := abortReason
			if reason == "" {
				reason = stepRun.OutcomeReason
			}
			return domain.BlueprintRunStatusAborted, reason
		default: // blueprintStepAdvance — mid-blueprint resume not implemented
			return domain.BlueprintRunStatusAborted, "multi_step_yield_resume_not_implemented"
		}
	case "failed", "task_unsolvable":
		return domain.BlueprintRunStatusFailed, "step " + stepRun.Status
	case "cancelled":
		return domain.BlueprintRunStatusCancelled, "step cancelled"
	default:
		return domain.BlueprintRunStatusFailed, "step ended with status " + stepRun.Status
	}
}

// ResumeBlueprintAfterApproval is invoked by the reviews / pending-PR
// approval handlers after they flip a step run from pending_approval
// back to completed. It only handles the finish-outcome case (the only
// shape under which a blueprint step is allowed to land in pending_approval
// — see the multi-step coercion in spawner.processCompletion, which forces a
// step that queued a terminal external action to runs.outcome='finish'):
// terminate the blueprint as completed, close the task, and clean the shared
// worktree.
//
// If the step run's outcome is missing or is not finish, the blueprint stays
// in 'running' on the assumption that something raced or the step recorded
// the wrong outcome; a human can inspect blueprint_runs and resolve manually
// rather than have us guess.
//
// userID identifies the approving user for audit. Local mode passes
// runmode.LocalDefaultUserID; multi-mode handlers extract it from JWT
// claims.
func (s *Spawner) ResumeBlueprintAfterApproval(orgID, stepRunID, userID string) {
	cr, stepIdx, err := s.blueprints.GetRunForRunSystem(context.Background(), orgID, stepRunID)
	if err != nil || cr == nil {
		return
	}
	if cr.Status != domain.BlueprintRunStatusRunning {
		return
	}

	stepRun, err := s.agentRuns.GetSystem(context.Background(), orgID, stepRunID)
	if err != nil || stepRun == nil {
		log.Printf("[blueprint] approval-resume run %s: read step run: %v", stepRunID, err)
		return
	}
	if domain.RunOutcome(stepRun.Outcome) != domain.RunOutcomeFinish {
		log.Printf("[blueprint] approval-resume blueprint_run %s step run %s: outcome %q is not finish; blueprint left running", cr.ID, stepRunID, stepRun.Outcome)
		return
	}

	task, err := s.tasks.GetSystem(context.Background(), orgID, cr.TaskID)
	if err != nil || task == nil {
		log.Printf("[blueprint] approval-resume blueprint_run %s: load task: %v", cr.ID, err)
		return
	}

	// No workspace rehydrate here: approval finalizes the blueprint (it does
	// not re-invoke the agent), so there is no resumed run to hand a warm
	// worktree to. A cold-swept worktree is fine — terminateBlueprint's cleanup
	// RemoveAt/RemoveClaudeProjectDir no-op on a missing dir, and it drops the
	// durable snapshot regardless.
	//
	// Reconstruct just enough runConfig for terminateBlueprint's worktree
	// cleanup. The original orchestrator goroutine (which held the full
	// cfg) returned when the step landed in pending_approval, so we
	// rebuild from durable state. owner/repo/prNumber/headRef are not
	// stored on blueprint_runs; CleanupPRConfig is best-effort and skipped
	// here — leaves a few stale git config entries but no user-visible
	// effect.
	cfg := runConfig{orgID: orgID, wtPath: cr.WorktreePath}
	if task.EntitySource == "github" {
		cfg.hasWT = true
	}

	s.terminateBlueprint(orgID, cr.ID, cr.TaskID, "manual", userID, cr.StartedAt, cfg,
		domain.BlueprintRunStatusCompleted, "", stepIdx, false)
}
