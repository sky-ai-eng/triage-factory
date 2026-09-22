package delegate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// blueprintStepDecision is the orchestrator's decision after a completed
// blueprint step, derived from the step conversation's conversations.outcome
// and the step's position. Named a decision, not an outcome: an outcome is what
// the agent reported (domain.ConversationOutcome, on the conversation), and
// this is what the orchestrator does about it — the two appear in the same
// scope below.
type blueprintStepDecision int

const (
	// blueprintStepAdvance moves to the next step (a non-final `continue`).
	blueprintStepAdvance blueprintStepDecision = iota
	// blueprintStepFinish terminates the blueprint completed and closes the
	// task — an explicit `finish`, a final-step `continue` (structural
	// finish), or an unambiguous missing outcome on the final step.
	blueprintStepFinish
	// blueprintStepAbort terminates the blueprint aborted and leaves the task
	// open — an explicit `abort`, or a missing outcome on a non-final step.
	blueprintStepAbort
)

// blueprintDecisionForStepConversation maps a completed step RUN's terminal outcome +
// the step's position to the orchestrator's next move. runOutcome is the step
// run's conversations.outcome — the step itself (a blueprint_steps row) carries
// no outcome, which is why this reads the run and is named for it. Only valid
// for a step whose run reached status='completed'; the non-terminal statuses
// (open, cancelled, failed) are handled by the caller before this is consulted.
//
// abortReason is non-empty only for the missing-outcome-on-a-non-final-step
// case ("no-outcome"); for an explicit abort it is empty and the caller
// copies conversations.outcome_reason into blueprint_runs.abort_reason.
func blueprintDecisionForStepConversation(runOutcome string, isFinal bool) (decision blueprintStepDecision, abortReason string) {
	switch domain.ConversationOutcome(runOutcome) {
	case domain.ConversationOutcomeContinue:
		// continue hands off to the next step — except on the final step,
		// where there is no next step and continue resolves to a structural
		// finish.
		if isFinal {
			return blueprintStepFinish, ""
		}
		return blueprintStepAdvance, ""
	case domain.ConversationOutcomeFinish:
		return blueprintStepFinish, ""
	case domain.ConversationOutcomeAbort:
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
		return blueprintStepAbort, "unknown-outcome: " + runOutcome
	}
}

// terminateBlueprint finalizes the blueprint run row and runs the shared
// worktree cleanup that runAgent's per-step defers skipped. status
// distinguishes "all steps green, mark task done like a single run
// would" (status=completed) from "stopped early — leave the task open
// for human review" (any other terminal). skipCleanup short-circuits
// when the worktree itself is already gone (worktree_lost path).
//
// triggerType + creatorUserID route the terminal writes. Manual blueprints
// (and user-initiated CancelBlueprintRun / Resume* that pass "manual" + the
// requesting user's ID) write under synthetic claims; event-triggered
// blueprints write through the admin pool.
//
// The cfg here is a cleanup-scoped config (worktree fields + orgID), not the
// full run-execution config. The run-bearing callers (dispatchClaimedConversation /
// handlePreAgentFailure) stamp cfg.teamID off the claimed run, but the
// CancelBlueprintRun / paused-cleanup callers have only a task (no claimed run)
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
		// Completion and task-closure are independent. A clean finalization
		// closes the task ONLY when no artifact remains unresolved; a blueprint
		// that completed with an open draft PR / ready review leaves the task
		// exactly as it is — bot-claimed and in_progress — until the last
		// artifact is resolved (the last resolution closes it). The signal that
		// a human is owed something rides the card frame and the attention
		// order, not the lane the card sits in, so there is no column to move
		// it to and nothing to write here. Unresolved is asked of the TASK, not
		// of this blueprint: a draft PR carried in from an earlier engagement is
		// unresolved work whichever run just finished.
		if !s.taskHasUnresolvedArtifacts(bgCtx, orgID, taskID) {
			// Mirror single-run behavior: a clean blueprint finalization with no
			// unresolved artifact closes the task.
			var closeErr error
			if triggerType == "manual" {
				closeErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
					_, err := ts.Tasks.Close(bgCtx, orgID, taskID, "run_completed", "")
					return err
				})
			} else {
				_, closeErr = s.tasks.CloseSystem(bgCtx, orgID, taskID, "run_completed", "")
			}
			// ErrNoSuchTask here means a concurrent close already landed —
			// harmless (Close's WHERE clause is state-guarded, so this branch
			// only reaches a task that's still non-terminal or missing); any
			// other error is worth a warning.
			if closeErr != nil && !errors.Is(closeErr, db.ErrNoSuchTask) {
				blueprintLog.Warn("close task failed", "task", taskID, "error", closeErr)
			}
		}

		// A clean completion means the agent opened its PR and the work is now
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
	// steps' run memory (durable in conversation_memory) and decide what to do next.

	// The warm tree goes, and the durable copy stays. That is deliberate and it
	// is what makes the task's next conversation cheap rather than free: the
	// snapshot blob is keyed by the task and survives every terminal, so the
	// next delegation rehydrates the tree at the same path — one cold
	// rehydrate, the cost a resume on another executor already pays. Keeping
	// the directory instead would mean a checked-out branch on disk belonging
	// to a blueprint that has stopped, which the boot reconcile then orphans.
	if !skipCleanup {
		s.runBlueprintWorktreeCleanup(blueprintRunID, workspaceKey(taskID), cfg)
	}

	// Reclaim any staging dir still held for this blueprint — the step skill and
	// the materialized memory tree alike. Each step's own dispatch drops its dirs
	// on a terminal disposition, so what reaches here is the parked case — a step
	// left `open` as the warm resume point, whose blueprint then terminated (a
	// cancel, or an abort) and will never resume. Best-effort, and the startup
	// sweep is the backstop.
	s.reclaimBlueprintStepStaging(bgCtx, orgID, blueprintRunID)

	// Nothing discards the workspace snapshot here, whatever the terminal. The
	// blob is keyed by the TASK, and a terminal is the blueprint's end rather
	// than the task's: a completed blueprint's work is what a follow-up
	// continues from, a cancelled one is work somebody stopped and is most
	// likely to want back, and a failed one is a task whose next attempt has
	// to start somewhere. A failure especially — the infrastructure under the
	// run died, which says nothing about the tree it died in.
	//
	// Retention is what collects them instead, on the task's idleness rather
	// than on any conversation's outcome — which is what makes keeping a
	// failed step's blob safe rather than a leak. See workspace_snapshot.go's
	// write-policy note for the pair.

	// Wake the firing worker exactly once for the blueprint (independent of
	// how many steps ran): the task's gate is open.
	s.wakeFirings()

	dur := time.Since(startTime)
	blueprintLog.Info("blueprint_run terminated",
		"blueprint_run", blueprintRunID, "status", status, "reason", abortReason, "duration", dur)
}

// reclaimBlueprintStepStaging removes the orchestrator-owned staging dirs of
// every run in a terminated blueprint — the step skill and the materialized
// memory tree. Keyed per step run (that is the staging key for both), and
// idempotent — a step that already dropped its own dirs at its terminal is a
// no-op here. A read failure just forgoes the reclaim; the dirs are small and
// the startup sweep collects them.
func (s *Spawner) reclaimBlueprintStepStaging(ctx context.Context, orgID, blueprintRunID string) {
	if !agentproc.WillSandbox() {
		return // local mode never stages outside the worktree
	}
	stepConversations, err := s.blueprints.ConversationsForBlueprintSystem(ctx, orgID, blueprintRunID)
	if err != nil {
		blueprintLog.Warn("list step conversations for staging reclaim failed", "blueprint_run", blueprintRunID, "error", err)
		return
	}
	for _, sr := range stepConversations {
		if err := skills.RemoveStagedSkills(sandbox.TrustedSkillsSourcePath(sr.ID)); err != nil {
			blueprintLog.Warn("remove staged step skill failed", "blueprint_run", blueprintRunID, "step_conversation", sr.ID, "error", err)
		}
		removeStagedMemory(sandbox.TrustedMemorySourcePath(sr.ID))
	}
}

// runBlueprintWorktreeCleanup performs the cleanup runAgent would have done
// per-step, except now once for the whole blueprint.
//
// Two ids, because two different things are named: blueprintRunID is the
// blueprint whose step conversations are enumerated (and what the logs say),
// while wsKey is the workspace key — the task's, shared by every conversation
// that ever ran in the tree. They are not interchangeable: RemoveRunRoot
// derives the directory from the key it is handed, so the blueprint's id there
// would sweep a path nothing ever built.
func (s *Spawner) runBlueprintWorktreeCleanup(blueprintRunID, wsKey string, cfg runConfig) {
	if cfg.hasWT {
		if err := worktree.RemoveAt(cfg.wtPath, wsKey); err != nil {
			blueprintLog.Warn("worktree remove failed", "blueprint_run", blueprintRunID, "error", err)
			return
		}
		if cfg.prNumber > 0 && cfg.owner != "" && cfg.repo != "" {
			// The eager PR worktree's per-run branch is namespaced by the id
			// CreateForPR ran under — the worktree-dir basename, which is the
			// run-root's key. filepath.Base derives it from the path so this
			// stays correct regardless of the key.
			worktree.CleanupPRConfig(cfg.owner, cfg.repo, cfg.prNumber, filepath.Base(cfg.wtPath))
		}
	} else if cfg.runRoot != "" {
		// Jira blueprints materialize worktrees lazily via `workspace add`,
		// which records a conversation_worktrees row per *step* conversation
		// (the agent's TRIAGE_FACTORY_CONVERSATION_ID) under the task's one
		// run root. Iterate every step conversation so we find and remove the
		// checkouts each of them made; the root itself comes off below, once.
		stepConversations, err := s.blueprints.ConversationsForBlueprintSystem(context.Background(), cfg.orgID, blueprintRunID)
		if err != nil {
			blueprintLog.Warn("list step conversations for cleanup failed", "blueprint_run", blueprintRunID, "error", err)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, sr := range stepConversations {
			rows, err := s.conversationWorktrees.ListSystem(context.Background(), cfg.orgID, sr.ID)
			if err != nil {
				blueprintLog.Warn("list conversation_worktrees for step failed", "blueprint_run", blueprintRunID, "step_conversation", sr.ID, "error", err)
				// Log but continue to attempt DB row deletion below.
				rows = nil
			}
			for _, w := range rows {
				if err := worktree.RemoveAt(w.Path, sr.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
					blueprintLog.Warn("remove worktree failed", "blueprint_run", blueprintRunID, "path", w.Path, "error", err)
					// Still attempt the DB row deletion even if the worktree remove failed.
				} else {
					// Worktree gone — reclaim its per-run PR branch + push remote
					// inline (Decision D), so the bootstrap sweep stays a pure
					// crash backstop. w.ConversationID == sr.ID (created the worktree).
					reclaimWorkspaceAddPRConfig(w)
				}
				if err := s.conversationWorktrees.DeleteByPathSystem(cleanupCtx, cfg.orgID, sr.ID, w.Path); err != nil {
					blueprintLog.Warn("delete conversation_worktrees row failed", "blueprint_run", blueprintRunID, "path", w.Path, "error", err)
				}
			}
		}
		// Clean the agent's ghost ~/.claude/projects entry (keyed on the session
		// cwd = cfg.wtPath, the blueprint run-root) BEFORE removing the run-root
		// dir — RemoveClaudeProjectDir resolves the cwd via EvalSymlinks and
		// silently no-ops once the dir is gone.
		worktree.RemoveClaudeProjectDir(cfg.wtPath)
		// The run-root is keyed by the task (setup and the cold rehydrate both
		// build it there), and every `workspace add` checkout nests under it, so
		// one removal reclaims the whole tree — the per-step
		// conversation_worktrees rows and their PR config were already reclaimed above.
		worktree.RemoveRunRoot(wsKey)
		return
	}
	worktree.RemoveClaudeProjectDir(cfg.wtPath)
}

// reclaimWorkspaceAddPRConfig reclaims the per-run PR branch + push remote a
// `workspace add --pr N` worktree left in the shared bare, keyed off the
// conversation_worktrees row's ref (pr-<N>) and conversation_id (the conversation that created it, so the
// per-run branch namespace matches). A no-op for non-PR refs (default, branch
// slugs) — those leave detached checkouts with no per-PR config. The blueprint
// teardown above is its only caller: reclaiming inline there is what keeps the
// bootstrap sweep a pure crash backstop rather than the ordinary path.
func reclaimWorkspaceAddPRConfig(w domain.ConversationWorktree) {
	prNum, ok := prNumberFromRef(w.Ref)
	if !ok {
		return
	}
	owner, repo := parseOwnerRepo(w.RepoID)
	if owner == "" || repo == "" {
		return
	}
	worktree.CleanupPRConfig(owner, repo, prNum, w.ConversationID)
}

// prNumberFromRef extracts N from a "pr-<N>" conversation_worktrees ref. ok=false for any
// non-PR ref ("default", a branch slug), which carries no per-PR config.
func prNumberFromRef(ref string) (int, bool) {
	rest, ok := strings.CutPrefix(ref, "pr-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// nonterminalStepSysPrompt returns the system-prompt addendum for step i of
// total steps: the non-terminal contract fragment (which offers `continue`)
// for every step before the last, and the empty string for the terminal step
// — and therefore the empty string for the whole N=1 single-step case, where
// i==0 is also the last step. This is the one position bit the agent's
// contract depends on; nothing else about ordering reaches the agent.
//
// The fragment itself is a block of internal/agentprompt, resolved per
// runtime: it extends the base completion contract, so the two must stay in
// step, and they only do that if one package owns both.
func nonterminalStepSysPrompt(i, total int) string {
	if i < total-1 {
		return agentprompt.NonTerminalCompletion(machinistSpec())
	}
	return ""
}

// buildBlueprintStepWrapperPrompt produces the per-step user prompt carrying
// step-specific data. The base completion contract (the composed framework
// prompt) plus the non-terminal addendum (agentprompt.NonTerminalCompletion,
// injected only for non-final steps) own the protocol; this wrapper supplies
// only the step's context.
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
	// Prior steps' findings are their memory files under
	// _tfac/entity-memory/this-task/, the newest of them also injected into the
	// conversation's opening turn — the <entity_memory> contract tells the agent
	// where each is. No separate handoff file.
	return b.String()
}

// raiseBlueprintCancel writes the blueprint layer's durable cancel signal for a
// run's owning blueprint, and reports whether it landed. It is the one place
// cancellation is spelled: from here the claim gate stops handing out this
// blueprint's steps, and whichever path disposes of the running step finalizes
// the blueprint 'cancelled' (reactToStepTerminal for a live step,
// ResumeBlueprintAfterResume for a resumed one, the dispatcher's stop
// settlement for one nobody holds).
//
// It returns the error rather than swallowing it because what follows a failed
// signal differs by caller: a blueprint whose signal never committed is one
// the claim gate will keep advancing, so a caller whose next move depends on
// the sequence being stopped has to know. requestBlueprintCancel is the
// best-effort spelling for the callers that don't.
//
// Only the blueprint's own cancel verbs and the lifecycle teardown one layer up
// (a closed or swiped task, an archived team) reach this. Stopping a
// conversation must not: the signal is what turns a parked step into a
// permanently unresumable one.
func (s *Spawner) raiseBlueprintCancel(ctx context.Context, orgID, blueprintRunID string) error {
	if s.blueprints == nil || blueprintRunID == "" {
		return nil
	}
	_, err := s.blueprints.RequestRunCancelSystem(ctx, orgID, blueprintRunID)
	return err
}

// requestBlueprintCancel raises the signal best-effort, for the callers that
// finish their teardown either way: a failure leaves the blueprint running
// with a parked step, which the boot reconcile and the retention sweep both
// tolerate, so it logs rather than failing the caller.
func (s *Spawner) requestBlueprintCancel(ctx context.Context, orgID, blueprintRunID string) {
	if err := s.raiseBlueprintCancel(ctx, orgID, blueprintRunID); err != nil {
		blueprintLog.Warn("raise cancel signal failed", "blueprint_run", blueprintRunID, "error", err)
	}
}

// CancelBlueprintRun asks a blueprint run to end: it raises the run's cancel
// signal, then records a stop intent on every step conversation that has not
// concluded. It writes no conversation status and no run status. The step's
// holder settles its stop through its own fenced park and the reactor behind
// it finalizes the run 'cancelled'; a step nobody holds — queued, or parked
// `open` — is settled by the dispatcher, which cancels the run in the same
// transaction. Safe to call when the run is already terminal.
//
// userID is the acting user and becomes each intent's actor, so the settlement
// records the cancellation as theirs. In local mode callers pass
// runmode.LocalDefaultUserID; multi-mode handlers extract the user from JWT
// claims.
func (s *Spawner) CancelBlueprintRun(orgID, blueprintRunID, userID string) error {
	ctx := context.Background()
	cr, err := s.blueprints.GetRunSystem(ctx, orgID, blueprintRunID)
	if err != nil {
		return fmt.Errorf("load blueprint run: %w", err)
	}
	if cr == nil {
		return fmt.Errorf("blueprint run %s not found", blueprintRunID)
	}
	if cr.Status != domain.BlueprintRunStatusRunning {
		return nil
	}

	// The run's own intent first. From here the claim gate stops handing out
	// this blueprint's queued steps, and whichever writer settles a step's
	// stop reads it and finalizes the run 'cancelled' rather than advancing.
	s.requestBlueprintCancel(ctx, orgID, blueprintRunID)

	stepIDs, err := s.blueprints.ActiveStepConversationIDsSystem(ctx, orgID, blueprintRunID)
	if err != nil {
		// The cancel signal above still keeps the run from advancing, but with
		// no step carrying an intent nothing settles it, so the caller hears
		// that the cancel did not take.
		return fmt.Errorf("list active step conversations: %w", err)
	}
	var errs []error
	for _, id := range stepIDs {
		// ErrNoActiveConversation is a step that concluded between the list
		// and the intent write; the reactor reading its terminal sees the
		// cancel signal and finalizes the run.
		if err := s.requestStop(ctx, orgID, id, userID); err != nil && !errors.Is(err, ErrNoActiveConversation) {
			errs = append(errs, fmt.Errorf("request stop for step conversation %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// finalizeCancelledBlueprintRun writes a running blueprint_run's cancelled
// terminal, as a system cancel, and cleans the shared worktree behind it. Its
// caller is StopBlueprintRun on a run with no step conversation: nothing else
// is going to reach that terminal, because there is no conversation to carry
// an intent.
//
// abortedAtStep is the step the cancellation landed on, when the caller has
// one; nil when the run had no step to name.
func (s *Spawner) finalizeCancelledBlueprintRun(ctx context.Context, orgID string, cr *domain.BlueprintRun, abortedAtStep *int) {
	_, _ = s.blueprints.MarkRunStatusSystem(ctx, orgID, cr.ID, domain.BlueprintRunStatusCancelled, "system_cancelled", abortedAtStep)
	s.cleanupCancelledBlueprintWorktree(ctx, orgID, cr.ID, cr.TaskID, cr.WorktreePath)
}

// cleanupCancelledBlueprintWorktree reclaims a cancelled run's shared
// worktree. The cfg is reconstructed rather than carried, because
// owner/repo/prNumber aren't persisted on blueprint_runs — so CleanupPRConfig
// is skipped and only the worktree is reclaimed. The snapshot survives: it is
// the parked workspace the cancel retained, and the retention TTL collects it.
// Best-effort; a pod that never held the tree finds nothing to remove.
func (s *Spawner) cleanupCancelledBlueprintWorktree(ctx context.Context, orgID, blueprintRunID, taskID, worktreePath string) {
	cfg := runConfig{orgID: orgID, wtPath: worktreePath}
	if task, _ := s.tasks.GetSystem(ctx, orgID, taskID); task != nil && task.EntitySource == "github" {
		cfg.hasWT = true
	}
	s.runBlueprintWorktreeCleanup(blueprintRunID, workspaceKey(taskID), cfg)
}

// markBlueprintRunStatusAsUser writes a blueprint_run status transition under
// the given user's synthetic claims. Used by user-initiated CancelBlueprintRun
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

// ResumeBlueprintAfterResume finalizes a blueprint after one of its step
// runs was resumed (via a user follow-up) and reached a terminal state —
// once processCompletion reports the step is no longer parked. It reads the
// resumed step's terminal conversations.outcome + position and routes
// through terminateBlueprint, so a 1-step (or final-step) resume closes the
// task on finish and leaves it open on abort.
//
// A non-final step that wants to advance mid-blueprint (continue) is the epic's
// resume work and is not built here: the blueprint is terminated with a clear
// reason rather than silently stalling in 'running'.
//
// userID identifies the actor for audit (the user whose action resumed the
// run). Local mode passes runmode.LocalDefaultUserID; multi-mode handlers
// extract it from JWT claims.
func (s *Spawner) ResumeBlueprintAfterResume(orgID, stepConversationID, userID string) {
	cr, stepIdx, err := s.blueprints.GetRunForConversationSystem(context.Background(), orgID, stepConversationID)
	if err != nil || cr == nil {
		return
	}
	if cr.Status != domain.BlueprintRunStatusRunning {
		return
	}
	// reactToStepTerminal's far-side re-check, on the path a resumed step
	// finalizes through instead. Same reason it has one: this terminates the
	// blueprint, and doing that on a step the sequence has moved past kills a
	// run whose current step is still executing.
	if !isCurrentBlueprintStep(cr, stepIdx) {
		blueprintLog.Error("resume finalize: terminal from a step the blueprint has moved past; ignoring it (no transition)",
			"blueprint_run", cr.ID, "step_conversation", stepConversationID, "step", stepIdx, "current_step", cr.CurrentStepIndex)
		return
	}

	stepConversation, err := s.conversations.GetSystem(context.Background(), orgID, stepConversationID)
	if err != nil || stepConversation == nil {
		blueprintLog.Warn("read step conversation failed", "step_conversation", stepConversationID, "error", err)
		return
	}
	// Still dormant after the resume (went open again) → the blueprint stays
	// running; the next resume drives finalization. Unless a cancel is behind
	// the park: a cancelled resume parks `open` rather than writing a terminal
	// of its own, so cancel_requested is what tells the two apart — the same
	// ordering reactToStepTerminal uses, and for the same reason.
	if stepConversation.Status == "open" && !cr.CancelRequested {
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

	// A cancel raised against this blueprint decides its terminal regardless of
	// how the resumed step ended — including the `open` park a cancelled resume
	// leaves behind, which the mapping below would otherwise read as an
	// unexpected status and fail the blueprint over.
	if cr.CancelRequested {
		s.terminateBlueprint(orgID, cr.ID, cr.TaskID, "manual", userID, cr.StartedAt, cfg,
			domain.BlueprintRunStatusCancelled, "cancelled", stepIdx, false)
		return
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

	status, reason := blueprintTerminalForResumedStepConversation(stepConversation, isFinal)
	s.terminateBlueprint(orgID, cr.ID, cr.TaskID, "manual", userID, cr.StartedAt, cfg,
		status, reason, stepIdx, false)
}

// blueprintTerminalForResumedStepConversation maps a resumed step
// conversation's terminal state + position to the blueprint's terminal status.
// Mirrors reactToStepTerminal's
// disposition, for the resume path: a clean completion routes through
// blueprintDecisionForStepConversation (finish/advance/abort), and the
// non-terminal-completed statuses map to the matching blueprint terminal.
func blueprintTerminalForResumedStepConversation(stepConversation *domain.Conversation, isFinal bool) (domain.BlueprintRunStatus, string) {
	switch stepConversation.Status {
	case "completed":
		decision, abortReason := blueprintDecisionForStepConversation(stepConversation.Outcome, isFinal)
		switch decision {
		case blueprintStepFinish:
			return domain.BlueprintRunStatusCompleted, ""
		case blueprintStepAbort:
			reason := abortReason
			if reason == "" {
				reason = stepConversation.OutcomeReason
			}
			return domain.BlueprintRunStatusAborted, reason
		default: // blueprintStepAdvance — mid-blueprint resume not implemented
			return domain.BlueprintRunStatusAborted, "multi_step_resume_not_implemented"
		}
	case "failed":
		return domain.BlueprintRunStatusFailed, "step " + stepConversation.Status
	default:
		return domain.BlueprintRunStatusFailed, "step ended with status " + stepConversation.Status
	}
}

// taskHasUnresolvedArtifacts reports whether any conversation on the task holds
// an artifact still awaiting human resolution (a draft PR or a ready pending
// review — domain.HasUnresolvedArtifacts). This is the derived signal that
// decides whether a completed blueprint closes its task (none unresolved) or
// leaves it open for a human (≥1 unresolved). Runs from a detached goroutine
// with no request claims, so it reads via the admin-pool `...System` readers.
//
// The scope is the task rather than the run that just finished, because
// artifacts outlive the engagement that produced them: a task requeued or
// re-delegated with a draft PR still open carries that PR into its next run,
// and asking only the finishing blueprint's own steps would call the task
// resolved with the draft still sitting on GitHub. One artifact query per
// conversation, so the cost is the task's own history, never org-wide.
//
// Fails OPEN: any read error returns true. Closing a task that still has an
// unresolved artifact would silently drop the approval workflow (and is hard to
// recover); leaving a task open spuriously is self-correcting on the next
// derivation and recoverable by a human. So on error we assume "may be
// unresolved" rather than risk the destructive direction.
//
// The unwired-store / empty-id guard below is NOT a read error and does not
// trigger fail-open: a nil artifact store (a test fixture without artifact
// tracking) or an empty task id means there are no artifacts to be unresolved,
// so it returns false.
func (s *Spawner) taskHasUnresolvedArtifacts(ctx context.Context, orgID, taskID string) bool {
	if s.artifacts == nil || s.conversations == nil || taskID == "" {
		return false
	}
	convs, err := s.conversations.ListForTaskSystem(ctx, orgID, taskID)
	if err != nil {
		blueprintLog.Warn("list task conversations for unresolved-artifact check failed; treating as unresolved (fail open)", "task", taskID, "error", err)
		return true
	}
	for _, conv := range convs {
		arts, err := s.artifacts.ListByConversationSystem(ctx, orgID, conv.ID)
		if err != nil {
			blueprintLog.Warn("list artifacts for unresolved check failed; treating as unresolved (fail open)", "conversation", conv.ID, "error", err)
			return true
		}
		if domain.HasUnresolvedArtifacts(arts) {
			return true
		}
	}
	return false
}

// CloseTaskIfTerminalAndResolved is the terminal-on-last task closure for a
// pull request resolved somewhere other than the approval click: a human
// marking it ready on GitHub, or an agent doing so because its mission asked.
// The click's own handler closes the task the same way; this is the same rule
// for a resolution nobody clicked, called by the artifact reconciler when it
// observes the transition. Closes the conversation's task iff its blueprint
// run finished cleanly and no conversation on the task — any attempt, not just
// this blueprint's steps — still holds an unresolved artifact. Anything else is
// a no-op: an aborted or failed blueprint keeps its task open for a human, and
// so does a sibling draft nobody has answered yet.
//
// It also stands down on a task that has moved past this run: one no agent
// holds any more, or one whose newest blueprint run is not this one. An
// artifact outlives the engagement that produced it, so a resolution can land
// on a task somebody has since self-claimed or re-delegated, and closing on it
// would take the task to done under whoever holds it now. Both conditions
// earn their place — a re-delegation leaves the task bot-claimed and only the
// run id separates the engagements, while a self-claim clears the agent id
// without minting a run at all.
//
// Fails CLOSED on a read error, like the rest of this family: leaving a task
// open spuriously is recoverable by a human, closing one with a draft still
// pending silently drops the approval workflow. Runs with no request claims,
// so every read is an admin-pool `...System` variant. The Jira mirror is not
// re-asserted here for the same reason the approval click does not: the
// blueprint's own completion already did.
func (s *Spawner) CloseTaskIfTerminalAndResolved(ctx context.Context, orgID, conversationID string) {
	if s.blueprints == nil || s.conversations == nil || s.tasks == nil || conversationID == "" {
		return
	}
	br, _, err := s.blueprints.GetRunForConversationSystem(ctx, orgID, conversationID)
	if err != nil {
		blueprintLog.Warn("terminal-on-last close: blueprint lookup failed; leaving task open (fail closed)", "conversation", conversationID, "error", err)
		return
	}
	if br == nil || br.TaskID == "" || br.Status != domain.BlueprintRunStatusCompleted {
		return
	}
	task, err := s.tasks.GetSystem(ctx, orgID, br.TaskID)
	if err != nil {
		blueprintLog.Warn("terminal-on-last close: task read failed; leaving task open (fail closed)", "task", br.TaskID, "error", err)
		return
	}
	if task == nil || task.ClaimedByAgentID == "" {
		return
	}
	isNewest, err := s.blueprints.IsNewestRunForTaskSystem(ctx, orgID, br.TaskID, br.ID)
	if err != nil {
		blueprintLog.Warn("terminal-on-last close: newest-run check failed; leaving task open (fail closed)", "task", br.TaskID, "error", err)
		return
	}
	if !isNewest {
		return
	}
	if s.taskHasUnresolvedArtifacts(ctx, orgID, br.TaskID) {
		return
	}
	// CloseSystem's WHERE is state-guarded, so a task the approval click (or a
	// concurrent cycle) already closed answers ErrNoSuchTask — not a failure.
	if _, err := s.tasks.CloseSystem(ctx, orgID, br.TaskID, "run_completed", ""); err != nil {
		if !errors.Is(err, db.ErrNoSuchTask) {
			blueprintLog.Warn("terminal-on-last close: close task failed", "task", br.TaskID, "conversation", conversationID, "error", err)
		}
		return
	}
	s.broadcastTaskUpdate(orgID, br.TaskID, "done")
}
