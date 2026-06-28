package delegate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "embed" // powers blueprintStepNonterminalPrompt

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
// status='completed'; the non-terminal statuses (open, cancelled, failed) are
// handled by the caller before this is consulted.
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
		// holds continue/finish/abort, or NULL). Never close a task on a value
		// we don't understand: abort and leave it open for a human, regardless
		// of position. Mirrors the old "unknown verdict → abort" floor.
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
//
// The cfg here is a cleanup-scoped config (worktree fields + orgID), not the
// full run-execution config. The run-bearing callers (dispatchClaimedRun /
// handleStepSetupError) stamp cfg.teamID off the claimed run, but the
// CancelBlueprint / paused-cleanup callers have only a task (no claimed run)
// and leave it empty — so cfg.teamID is NOT reliably set here. Any future
// run-attributed work on the terminal path (e.g. recording a failed run's
// artifacts, TFAC-454) must thread the team/identity explicitly rather than
// trust cfg.
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
		blueprintLog.Error("mark blueprint_run status failed; skipping cleanup to keep blueprint row consistent", "blueprint_run", blueprintRunID, "status", status, "error", markErr)
		return
	}

	if status == domain.BlueprintRunStatusCompleted {
		// Completion and task-closure are independent now. A clean
		// finalization closes the task ONLY when no artifact remains unresolved; a
		// blueprint that completed with an open draft PR / ready review leaves the
		// task open, surfaced in the derived approval column, until the last
		// artifact is resolved (the last resolution closes it).
		if s.blueprintHasUnresolvedArtifacts(bgCtx, orgID, blueprintRunID) {
			// Leave the task open and place it in the approval column. Don't close.
			s.placeTaskInApprovalColumn(bgCtx, orgID, taskID)
		} else {
			// Mirror single-run behavior: a clean blueprint finalization with no
			// unresolved artifact closes the task.
			var closeErr error
			if triggerType == "manual" {
				closeErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
					return ts.Tasks.Close(bgCtx, orgID, taskID, "run_completed", "")
				})
			} else {
				closeErr = s.tasks.CloseSystem(bgCtx, orgID, taskID, "run_completed", "")
			}
			if closeErr != nil {
				blueprintLog.Warn("close task failed", "task", taskID, "error", closeErr)
			}
		}

		// TFAC-442: a clean completion means the agent opened its PR and the work is now
		// awaiting human review + merge — "in progress" to a Jira watcher, NOT
		// done. Re-assert the InProgress bucket (idempotent; usually a no-op
		// since the dispatch-time mirror already moved the ticket) rather than
		// transitioning to Done. PR-opened ≠ ticket-done: the ticket only reaches
		// Done when its PR merges, via a separate entity-driven mirror.
		// mirrorJiraInProgressForTask re-checks bot ownership, so a mid-run user
		// takeover (claim flipped to the user) leaves the terminal Jira write to
		// the user path. Close above does not clear the claim, so the ownership
		// re-read still sees the bot.
		s.mirrorJiraInProgressForTask(bgCtx, orgID, taskID)
	}
	// Aborted / failed / cancelled blueprints intentionally do NOT mark
	// the task done — leave it in the queue so a human can inspect the
	// steps' run memory (durable in run_memory) and decide what to do next.

	if !skipCleanup {
		s.runBlueprintWorktreeCleanup(blueprintRunID, cfg)
	}

	// Drop the durable workspace snapshot now the blueprint is terminal so the
	// blob store doesn't orphan it — EXCEPT an aborted terminal, whose
	// completed+abort step run stays message-resumable. An abort snapshot is
	// retained for the resume path (the worktree above was cleaned, so a resume
	// cold-rehydrates from this blob) and reaped by the retention TTL sweep once
	// it ages out; discarding it here would defeat resume. Keyed by
	// blueprint_run_id (the shared workspace's key); idempotent, so the discard
	// is a no-op for a blueprint that never snapshotted. Covers the non-abort
	// terminals: clean finish, fail, cancel.
	if status != domain.BlueprintRunStatusAborted {
		s.discardWorkspaceSnapshot(bgCtx, orgID, blueprintRunID)
	}

	// Drain the per-entity queue exactly once for the blueprint (independent
	// of how many steps ran).
	if cfgEntity := taskEntityID(s.tasks, orgID, taskID); cfgEntity != "" {
		s.notifyDrainer(orgID, triggerType, cfgEntity)
	}

	dur := time.Since(startTime)
	blueprintLog.Info("blueprint_run terminated",
		"blueprint_run", blueprintRunID, "status", status, "reason", abortReason, "duration", dur)
}

// runBlueprintWorktreeCleanup performs the cleanup runAgent would have done
// per-step, except now once for the whole blueprint.
func (s *Spawner) runBlueprintWorktreeCleanup(blueprintRunID string, cfg runConfig) {
	if cfg.hasWT {
		if err := worktree.RemoveAt(cfg.wtPath, blueprintRunID); err != nil {
			blueprintLog.Warn("worktree remove failed", "blueprint_run", blueprintRunID, "error", err)
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
			blueprintLog.Warn("list step runs for cleanup failed", "blueprint_run", blueprintRunID, "error", err)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, sr := range stepRuns {
			rows, err := s.runWorktrees.ListSystem(context.Background(), cfg.orgID, sr.ID)
			if err != nil {
				blueprintLog.Warn("list run_worktrees for step failed", "blueprint_run", blueprintRunID, "step_run", sr.ID, "error", err)
				// Log but continue to attempt DB row deletion below.
				rows = nil
			}
			for _, w := range rows {
				if err := worktree.RemoveAt(w.Path, sr.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
					blueprintLog.Warn("remove worktree failed", "blueprint_run", blueprintRunID, "path", w.Path, "error", err)
					// Still attempt the DB row deletion even if the worktree remove failed.
				}
				if err := s.runWorktrees.DeleteByPathSystem(cleanupCtx, cfg.orgID, sr.ID, w.Path); err != nil {
					blueprintLog.Warn("delete run_worktrees row failed", "blueprint_run", blueprintRunID, "path", w.Path, "error", err)
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
		blueprintLog.Warn("resolve entity for task failed", "task", taskID, "error", err)
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

	// Raise the DB sequence-cancel signal first (decision #3). From here the
	// claim stops handing out this blueprint's queued steps, and the dispatcher's
	// reactor finalizes the blueprint 'cancelled' instead of enqueuing the next
	// step. This is the durable half of the cancel; the in-memory subprocess kill
	// below is the active half.
	if _, err := s.blueprints.RequestRunCancelSystem(context.Background(), orgID, blueprintRunID); err != nil {
		blueprintLog.Warn("raise cancel signal failed", "blueprint_run", blueprintRunID, "error", err)
	}

	// Kill the active step's subprocess, if one is running. The dispatcher
	// registers a per-step cancel under the step run_id; sweep every active step
	// run and cancel its handle.
	//
	// anyActive tracks whether at least one live subprocess got killed. If
	// nothing was active — the blueprint is paused on an open/approval step, or a
	// queued step that was never claimed — no dispatcher goroutine will run the
	// reactor, so we drive terminateBlueprint ourselves below.
	var anyActive bool
	stepIDs, err := s.blueprints.ActiveStepRunIDsSystem(context.Background(), orgID, blueprintRunID)
	if err != nil {
		// Couldn't enumerate the active step runs, so we can't target their
		// subprocess kills — the cancel_requested signal above still stops the
		// queue from advancing and we finalize the blueprint below, but a
		// currently-running subprocess will run to completion (then short-circuit
		// in the reactor on the non-running blueprint). Log so that wasteful run
		// is diagnosable.
		blueprintLog.Warn("list active step runs failed; a live subprocess may run to completion", "blueprint_run", blueprintRunID, "error", err)
	} else {
		s.mu.Lock()
		for _, runID := range stepIDs {
			if cancel, ok := s.cancels[runID]; ok {
				cancel()
				anyActive = true
			}
		}
		s.mu.Unlock()
	}

	if anyActive {
		// A step subprocess is being killed — its runAgent will return cancelled
		// and the reactor (seeing cancel_requested) will terminateBlueprint. Avoid
		// double-marking here so the reactor's MarkRunStatus wins.
		return nil
	}

	// No live subprocess: a queued-not-started step (cancels with zero work) or a
	// parked step. Mark every still-active step run cancelled so nothing lingers
	// in the queue, then finalize the blueprint ourselves.
	for _, runID := range stepIDs {
		if _, mErr := s.agentRuns.MarkCancelledIfActiveSystem(context.Background(), orgID, runID, "user_cancelled", "Blueprint cancelled by user"); mErr != nil {
			blueprintLog.Warn("mark step run cancelled failed", "step_run", runID, "error", mErr)
		}
	}

	// Paused blueprint: rebuild just enough cfg for terminateBlueprint's worktree
	// cleanup (mirrors ResumeBlueprintAfterApproval — owner/repo/prNumber
	// aren't persisted on blueprint_runs, so CleanupPRConfig is skipped).
	task, err := s.tasks.GetSystem(context.Background(), orgID, cr.TaskID)
	if err != nil || task == nil {
		blueprintLog.Warn("load task for paused blueprint_run failed", "blueprint_run", blueprintRunID, "error", err)
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

// finalizeParkedBlueprintOnCancel finalizes the owning blueprint_run when a step
// run is cancelled through the spawner's DB-only path (Cancel with no live
// orchestrator goroutine — the step had parked, so the orchestrator already
// returned and nothing else will mark the blueprint_run terminal). Without this,
// cancelling an open/approval-parked step would mark only the run cancelled and
// strand the blueprint_run in 'running' (and its shared-workspace snapshot in
// the blob store).
//
// It does NOT drain the per-entity queue: the drainer's manual short-circuit
// keys off the run's trigger type, which the caller (Cancel) already passes to
// notifyDrainer — folding the drain in here (via terminateBlueprint) would
// couple it to the write-pool routing and drain a manual run that must not.
// So this finalizes the blueprint row + worktree + snapshot only:
//
//   - marks the blueprint_run cancelled, routed by userID exactly like
//     CancelBlueprint — a user cancel (non-empty) under the user's synthetic
//     claims, a system cancel (empty — router cleanup / drain sweep) through the
//     admin pool;
//   - runs the shared-worktree cleanup;
//   - discards the blueprint_run-keyed snapshot (idempotent — a no-op when
//     terminateBlueprint already dropped it, or when none was taken).
//
// This path only runs with no live goroutine, so the blueprint is sequentially
// paused (no other step is executing) and finalizing the whole blueprint_run on
// the single cancelled step is correct.
func (s *Spawner) finalizeParkedBlueprintOnCancel(ctx context.Context, orgID string, run *domain.AgentRun, userID string) {
	if s.blueprints == nil || run.BlueprintRunID == "" {
		return
	}
	if cr, err := s.blueprints.GetRunSystem(ctx, orgID, run.BlueprintRunID); err == nil && cr != nil &&
		cr.Status == domain.BlueprintRunStatusRunning {
		reason := "user_cancelled"
		if userID == "" {
			reason = "system_cancelled"
		}
		if userID != "" {
			_, _ = s.markBlueprintRunStatusAsUser(ctx, orgID, userID, cr.ID, domain.BlueprintRunStatusCancelled, reason, run.BlueprintStepIndex)
		} else {
			_, _ = s.blueprints.MarkRunStatusSystem(ctx, orgID, cr.ID, domain.BlueprintRunStatusCancelled, reason, run.BlueprintStepIndex)
		}
		// Reconstruct just enough cfg for the worktree cleanup (mirrors
		// CancelBlueprint / ResumeBlueprintAfterApproval — owner/repo/prNumber
		// aren't persisted on blueprint_runs, so CleanupPRConfig is skipped).
		cfg := runConfig{orgID: orgID, wtPath: cr.WorktreePath}
		if task, _ := s.tasks.GetSystem(ctx, orgID, cr.TaskID); task != nil && task.EntitySource == "github" {
			cfg.hasWT = true
		}
		s.runBlueprintWorktreeCleanup(cr.ID, cfg)
	}
	s.discardWorkspaceSnapshot(ctx, orgID, run.BlueprintRunID)
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

// ResumeBlueprintAfterResume finalizes a blueprint after one of its step runs
// was resumed (via ResumeOpenRun) and reached a terminal state — once
// processCompletion reports the step is no longer parked. It reads the resumed
// step's terminal runs.outcome + position and routes through terminateBlueprint,
// so a 1-step (or final-step) resume closes the task on finish and leaves it
// open on abort.
//
// A non-final step that wants to advance mid-blueprint (continue) is the epic's
// resume work and is not built here: the blueprint is terminated with a clear
// reason rather than silently stalling in 'running'.
//
// userID identifies the actor for audit (the user whose action resumed the
// run). Local mode passes runmode.LocalDefaultUserID; multi-mode handlers
// extract it from JWT claims.
func (s *Spawner) ResumeBlueprintAfterResume(orgID, stepRunID, userID string) {
	cr, stepIdx, err := s.blueprints.GetRunForRunSystem(context.Background(), orgID, stepRunID)
	if err != nil || cr == nil {
		return
	}
	if cr.Status != domain.BlueprintRunStatusRunning {
		return
	}

	stepRun, err := s.agentRuns.GetSystem(context.Background(), orgID, stepRunID)
	if err != nil || stepRun == nil {
		blueprintLog.Warn("read step run failed", "step_run", stepRunID, "error", err)
		return
	}
	// Still dormant after the resume (went open again) → the blueprint stays
	// running; the next resume drives finalization.
	if stepRun.Status == "open" {
		return
	}

	task, err := s.tasks.GetSystem(context.Background(), orgID, cr.TaskID)
	if err != nil || task == nil {
		blueprintLog.Warn("load task for blueprint_run failed", "blueprint_run", cr.ID, "error", err)
		_, _ = s.markBlueprintRunStatusAsUser(context.Background(), orgID, userID, cr.ID, domain.BlueprintRunStatusFailed, "resume_task_load_failed", stepIdx)
		return
	}
	cfg := runConfig{orgID: orgID, wtPath: cr.WorktreePath}
	if task.EntitySource == "github" {
		cfg.hasWT = true
	}

	// isFinal = this is the last step (and therefore the only step for N=1).
	// Read off the plan frozen at mint (cr.StepPlan), not the live steps, so a
	// mid-flight edit can't change a resumed run's terminal disposition.
	// Defaults to true when the index or plan can't be resolved, so an unknown
	// position never advances into the unbuilt mid-blueprint resume.
	isFinal := true
	if stepIdx != nil && len(cr.StepPlan) > 0 {
		isFinal = *stepIdx >= len(cr.StepPlan)-1
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
			return domain.BlueprintRunStatusAborted, "multi_step_resume_not_implemented"
		}
	case "failed", "task_unsolvable":
		return domain.BlueprintRunStatusFailed, "step " + stepRun.Status
	case "cancelled":
		return domain.BlueprintRunStatusCancelled, "step cancelled"
	default:
		return domain.BlueprintRunStatusFailed, "step ended with status " + stepRun.Status
	}
}

// blueprintHasUnresolvedArtifacts reports whether any step run of the blueprint
// produced an artifact still awaiting human resolution (a draft PR or a ready
// pending review — domain.HasUnresolvedArtifacts). This is the derived signal
// that decides whether a completed blueprint closes its task (none unresolved)
// or leaves it open in the approval column (≥1 unresolved). It loads the
// blueprint's step runs, then delegates to runsHaveUnresolvedArtifacts. Runs from
// a detached goroutine with no request claims, so it reads via the admin-pool
// `...System` readers.
//
// Fails OPEN: any read error returns true. Closing a task that still has an
// unresolved artifact would silently drop the approval workflow (and is hard to
// recover); leaving a task open spuriously is self-correcting on the next
// derivation and recoverable by a human. So on error we assume "may be
// unresolved" rather than risk the destructive direction.
//
// The unwired-store / empty-id guard below is NOT a read error and does not
// trigger fail-open: a nil artifact store (a test fixture without artifact
// tracking) or an empty blueprint id means there are no artifacts to be
// unresolved, so it returns false.
func (s *Spawner) blueprintHasUnresolvedArtifacts(ctx context.Context, orgID, blueprintRunID string) bool {
	if s.artifacts == nil || s.blueprints == nil || blueprintRunID == "" {
		return false
	}
	runs, err := s.blueprints.RunsForBlueprintSystem(ctx, orgID, blueprintRunID)
	if err != nil {
		blueprintLog.Warn("list step runs for unresolved-artifact check failed; treating as unresolved (fail open)", "blueprint_run", blueprintRunID, "error", err)
		return true
	}
	return s.runsHaveUnresolvedArtifacts(ctx, orgID, runs)
}

// runsHaveUnresolvedArtifacts reports whether any of the given runs produced an
// unresolved artifact, reading each run's artifacts via the admin-pool reader.
// Split out so a caller that already holds the run set (recomputeTaskBoardColumn)
// reuses it without re-loading. Fails OPEN like blueprintHasUnresolvedArtifacts:
// a per-run read error returns true rather than risk under-reporting. The read is
// one query per run; the run set is a single blueprint_run's steps, so it is
// bounded by step count, not blueprint history.
func (s *Spawner) runsHaveUnresolvedArtifacts(ctx context.Context, orgID string, runs []domain.AgentRun) bool {
	if s.artifacts == nil {
		return false
	}
	for _, r := range runs {
		arts, err := s.artifacts.ListByRunSystem(ctx, orgID, r.ID)
		if err != nil {
			blueprintLog.Warn("list artifacts for unresolved check failed; treating as unresolved (fail open)", "step_run", r.ID, "error", err)
			return true
		}
		if domain.HasUnresolvedArtifacts(arts) {
			return true
		}
	}
	return false
}
