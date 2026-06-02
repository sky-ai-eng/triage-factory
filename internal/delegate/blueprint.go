package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "embed" // powers blueprintStepSystemPrompt

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

//go:embed prompts/chain-step-system.txt
var blueprintStepSystemPrompt string

// delegateBlueprint is the multi-step analog of Delegate's single-prompt
// body. The caller supplies the resolved blueprint + its ordered (len > 1)
// step list; this sets up the shared worktree, creates the blueprint_runs
// row, and spawns the orchestrator goroutine. The returned id is the
// blueprint_run id (not a step run id) — the UI / API surfaces this as "the
// blueprint that was kicked off".
//
// Failures inside this function (empty step list, worktree setup failure, db
// write errors) terminate the blueprint immediately with a matching
// abort_reason rather than returning an error to the caller — the caller
// already has the blueprint_run id and the UI subscribes to the row by id, so
// a synchronous error wouldn't be reflected anywhere visible.
func (s *Spawner) delegateBlueprint(orgID string, task domain.Task, blueprint *domain.Blueprint, steps []domain.BlueprintStep, triggerType, triggerID, creatorUserID string, gh *ghclient.Client, model string) (string, error) {
	if len(steps) == 0 {
		return "", fmt.Errorf("blueprint %q has no steps", blueprint.Name)
	}

	// Allocate the blueprint-run id up front so the goroutine and the caller
	// both reference the same row — we want callers to be able to
	// subscribe to blueprint_runs/{id} immediately, not wait for a setup
	// round-trip.
	blueprintRunID := uuid.New().String()

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[blueprintRunID] = cancel
	s.mu.Unlock()
	// Mark this id as a blueprint_run so the setup phase's status helpers
	// don't broadcast agent_run_update events for a non-existent runs row.
	s.markBlueprintRunID(blueprintRunID)

	go func() {
		startTime := time.Now()
		defer func() {
			s.mu.Lock()
			delete(s.cancels, blueprintRunID)
			s.mu.Unlock()
			cancel()
			s.unmarkBlueprintRunID(blueprintRunID)
		}()

		// Build the shared worktree exactly once. The same setupGitHub /
		// setupJira used by single runs — blueprint steps reuse the result.
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
			// Persist a blueprint_runs row anyway so the UI has something to
			// show; write abort_reason and completed_at directly in the
			// insert — MarkRunStatus won't match a row that isn't 'running'.
			now := time.Now().UTC()
			_, _ = s.blueprints.CreateRun(ctx, orgID, domain.BlueprintRun{
				ID:           blueprintRunID,
				BlueprintID:  blueprint.ID,
				TaskID:       task.ID,
				TriggerType:  domain.BlueprintTriggerType(triggerType),
				TriggerID:    triggerID,
				Status:       domain.BlueprintRunStatusFailed,
				AbortReason:  setupErr.Error(),
				CompletedAt:  &now,
				WorktreePath: "",
			})
			if cfgEntity := taskEntityID(s.tasks, orgID, task.ID); cfgEntity != "" {
				s.notifyDrainer(orgID, triggerType, cfgEntity)
			}
			return
		}

		if _, err := s.blueprints.CreateRun(ctx, orgID, domain.BlueprintRun{
			ID:           blueprintRunID,
			BlueprintID:  blueprint.ID,
			TaskID:       task.ID,
			TriggerType:  domain.BlueprintTriggerType(triggerType),
			TriggerID:    triggerID,
			Status:       domain.BlueprintRunStatusRunning,
			WorktreePath: cfg.wtPath,
		}); err != nil {
			log.Printf("[blueprint] failed to persist blueprint_run %s: %v", blueprintRunID, err)
			s.runBlueprintWorktreeCleanup(blueprintRunID, cfg)
			if cfgEntity := taskEntityID(s.tasks, orgID, task.ID); cfgEntity != "" {
				s.notifyDrainer(orgID, triggerType, cfgEntity)
			}
			return
		}

		verb := "Blueprint started"
		if triggerType == "event" {
			verb = "Auto-fired blueprint"
		}
		toast.Info(s.wsHub, orgID, fmt.Sprintf("%s: %s (%s)",
			verb, truncateToastMsg(blueprint.Name, 60), shortRunID(blueprintRunID)))

		s.runBlueprint(ctx, orgID, blueprintRunID, task, blueprint, steps, cfg, startTime, model, triggerType, creatorUserID)
	}()

	return blueprintRunID, nil
}

// runBlueprint orchestrates a blueprint prompt against one task. It owns the
// shared worktree (built once via setupGitHub / setupJira) and walks
// the ordered step list, creating one runs row per step. After each
// step terminates, it reads the latest chain:verdict artifact and
// decides whether to advance, abort, or fail.
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
		if err := skills.WipeChainSkills(cfg.wtPath); err != nil {
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
		stepCfg.appendSysPrompt = blueprintStepSystemPrompt
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

		verdict, err := s.blueprints.GetLatestVerdictSystem(ctx, orgID, stepRunID)
		if err != nil {
			log.Printf("[blueprint] run %s step %d: read verdict: %v", blueprintRunID, i, err)
		}
		if verdict == nil {
			// Synthetic abort — record so the UI shows the same shape
			// as a real verdict, then halt.
			synthetic := domain.ChainVerdict{
				Outcome:   domain.ChainVerdictAbort,
				Reason:    "no-verdict",
				Synthetic: true,
			}
			if payload, err := json.Marshal(synthetic); err == nil {
				payloadStr := string(payload)
				var insertErr error
				if triggerType == "manual" {
					insertErr = s.tx.SyntheticClaimsWithTx(ctx, orgID, creatorUserID, func(ts db.TxStores) error {
						return ts.Blueprints.InsertVerdict(ctx, orgID, stepRunID, payloadStr)
					})
				} else {
					insertErr = s.blueprints.InsertVerdictSystem(ctx, orgID, stepRunID, payloadStr)
				}
				if insertErr != nil {
					log.Printf("[blueprint] run %s step %d: insert synthetic verdict artifact: %v", blueprintRunID, i, insertErr)
				}
			}
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusAborted,
				"no-verdict", &step.StepIndex, false)
			return
		}
		switch verdict.Outcome {
		case domain.ChainVerdictFinal:
			// Step decided the blueprint's intended outcome is reached here.
			// Terminate as completed (closes the task) and record the
			// step index so the UI can show "exited early at step N".
			reason := verdict.Reason
			if reason == "" {
				reason = "step recorded --final"
			}
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusCompleted,
				reason, &step.StepIndex, false)
			return
		case domain.ChainVerdictAbort:
			reason := verdict.Reason
			if reason == "" {
				reason = "step recorded --abort"
			}
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusAborted,
				reason, &step.StepIndex, false)
			return
		case domain.ChainVerdictAdvance:
		default:
			// Unknown outcome — treat as abort.
			s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusAborted,
				"unknown verdict outcome: "+string(verdict.Outcome), &step.StepIndex, false)
			return
		}
	}

	s.terminateBlueprint(orgID, blueprintRunID, task.ID, triggerType, creatorUserID, startTime, cfg, domain.BlueprintRunStatusCompleted,
		"", nil, false)
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
	// the task done — leave it in the queue so a human can inspect
	// _scratch/handoff.md and decide what to do next.

	if !skipCleanup {
		s.runBlueprintWorktreeCleanup(blueprintRunID, cfg)
	}

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

// buildBlueprintStepWrapperPrompt produces the per-step user prompt carrying
// step-specific data. The system prompt (chain-step-system.txt) owns the
// protocol contract; this wrapper supplies only the step's context.
func buildBlueprintStepWrapperPrompt(task domain.Task, step domain.BlueprintStep, stepPrompt *domain.Prompt, slug string, total int, nextStepName string) string {
	mission := strings.TrimSpace(step.Brief)
	if mission == "" {
		mission = stepPrompt.Name
	}
	binaryPath, _ := os.Executable()
	if binaryPath == "" {
		binaryPath = "triagefactory"
	}
	binaryPath = filepath.Clean(binaryPath)

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
	fmt.Fprintf(&b, "Binary path for verdict commands: %s\n", binaryPath)
	b.WriteString("Handoff notes from prior steps: ./_scratch/handoff.md\n")
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

// ResumeBlueprintAfterYield re-enters the orchestrator loop for the
// remaining steps after a yield-resume completes successfully.
// Currently not fully implemented: marks the blueprint aborted so it
// doesn't silently stall in 'running'.
//
// userID identifies the actor for audit (the user whose response
// resumed the yielded run). Local mode passes
// runmode.LocalDefaultUserID; multi-mode handlers extract it from
// JWT claims.
func (s *Spawner) ResumeBlueprintAfterYield(orgID, stepRunID, userID string) {
	cr, stepIdx, err := s.blueprints.GetRunForRunSystem(context.Background(), orgID, stepRunID)
	if err != nil || cr == nil {
		return
	}
	if cr.Status != domain.BlueprintRunStatusRunning {
		return
	}
	log.Printf("[blueprint] yield-resume not yet implemented for blueprint_run %s step run %s; aborting blueprint", cr.ID, stepRunID)
	task, err := s.tasks.GetSystem(context.Background(), orgID, cr.TaskID)
	if err != nil || task == nil {
		log.Printf("[blueprint] yield-resume: load task for blueprint_run %s: %v", cr.ID, err)
		// Fall back to a bare MarkBlueprintRunStatus without full cleanup.
		// User-initiated — attribute to the resuming user.
		_, _ = s.markBlueprintRunStatusAsUser(context.Background(), orgID, userID, cr.ID, domain.BlueprintRunStatusAborted, "yield_resume_not_implemented", stepIdx)
		return
	}
	cfg := runConfig{orgID: orgID, wtPath: cr.WorktreePath}
	if task.EntitySource == "github" {
		cfg.hasWT = true
	}
	s.terminateBlueprint(orgID, cr.ID, cr.TaskID, "manual", userID, cr.StartedAt, cfg,
		domain.BlueprintRunStatusAborted, "yield_resume_not_implemented", stepIdx, false)
}

// ResumeBlueprintAfterApproval is invoked by the reviews / pending-PR
// approval handlers after they flip a step run from pending_approval
// back to completed. It only handles the --final verdict case (the only
// shape under which a blueprint step is allowed to land in pending_approval
// — see the guard in spawner.processCompletion): terminate the blueprint
// as completed, close the task, and clean the shared worktree.
//
// If the verdict is missing or is not Final, the blueprint stays in
// 'running' on the assumption that something raced or the agent
// recorded the wrong verdict; a human can inspect blueprint_runs and
// resolve manually rather than have us guess.
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

	verdict, err := s.blueprints.GetLatestVerdictSystem(context.Background(), orgID, stepRunID)
	if err != nil {
		log.Printf("[blueprint] approval-resume run %s: read verdict: %v", stepRunID, err)
		return
	}
	if verdict == nil || verdict.Outcome != domain.ChainVerdictFinal {
		log.Printf("[blueprint] approval-resume blueprint_run %s step run %s: verdict not --final (%+v); blueprint left running", cr.ID, stepRunID, verdict)
		return
	}

	task, err := s.tasks.GetSystem(context.Background(), orgID, cr.TaskID)
	if err != nil || task == nil {
		log.Printf("[blueprint] approval-resume blueprint_run %s: load task: %v", cr.ID, err)
		return
	}

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

	reason := verdict.Reason
	if reason == "" {
		reason = "step recorded --final"
	}
	s.terminateBlueprint(orgID, cr.ID, cr.TaskID, "manual", userID, cr.StartedAt, cfg,
		domain.BlueprintRunStatusCompleted, reason, stepIdx, false)
}

// isNonFinalBlueprintStep returns true when the run is a blueprint step that
// is not the last step in the blueprint. Used as a guard in
// processCompletion to prevent mid-blueprint approval stalls.
//
// Returns true (safe default) on DB error: treating an unknown step as
// non-final ensures the pending-approval guard still engages even when
// the DB is flaky, preventing unintended mid-blueprint external actions.
func (s *Spawner) isNonFinalBlueprintStep(orgID, runID string) bool {
	run, err := s.agentRuns.GetSystem(context.Background(), orgID, runID)
	if err != nil {
		log.Printf("[blueprint] isNonFinalBlueprintStep: query run %s: %v", runID, err)
		return true
	}
	if run == nil || run.BlueprintRunID == "" || run.BlueprintStepIndex == nil {
		return false
	}
	blueprintRun, err := s.blueprints.GetRunSystem(context.Background(), orgID, run.BlueprintRunID)
	if err != nil {
		log.Printf("[blueprint] isNonFinalBlueprintStep: query blueprint_run %s for run %s: %v", run.BlueprintRunID, runID, err)
		return true
	}
	if blueprintRun == nil {
		return false
	}
	steps, err := s.blueprints.ListStepsSystem(context.Background(), orgID, blueprintRun.BlueprintID)
	if err != nil {
		log.Printf("[blueprint] isNonFinalBlueprintStep: list steps for blueprint %s run %s: %v", run.BlueprintRunID, runID, err)
		return true
	}
	return *run.BlueprintStepIndex+1 < len(steps)
}
