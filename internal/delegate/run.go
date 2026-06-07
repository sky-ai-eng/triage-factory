// Generic agent execution loop and the post-stream branching that turns
// either a yield (park in awaiting_input) or a terminal completion (run
// the memory gate, finalize the run row) into the right DB state. Shared
// between the initial Delegate path and the SKY-139 yield-resume flow.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// sessionTranscriptExists reports whether the Claude session transcript for
// sessionID is on disk for the agent's cwd — the cheap existence check the
// crash-reclaim resume gates on, so a `--resume` is only attempted when the
// session JSONL actually survived (a missing one would hard-fail the agent).
func sessionTranscriptExists(wtPath, sessionID string) bool {
	p, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(wtPath), sessionID)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// runAgent is the generic agent execution loop. Works for any task type.
//
// creatorUserID carries the user identity for manual runs; it's the
// synthetic-claims subject the goroutine's write batches run under so
// RLS policies on the writes pass under tf_app. Empty for event-
// triggered runs (those write through the admin-pool `...System`
// methods, no JWT-claims context).
// priorSessionID is the run row's existing session_id at claim time — empty
// for a first claim, non-empty when the dispatcher re-claims a run stranded
// mid-flight by a crash. When present and the session transcript survived
// alongside the warm worktree, the agent resumes that session instead of
// starting fresh, so a restart continues the run rather than re-running it
// from scratch.
func (s *Spawner) runAgent(ctx context.Context, runID string, task domain.Task, mission string, cfg runConfig, startTime time.Time, model string, triggerType string, creatorUserID string, priorSessionID string) {
	orgID := cfg.orgID
	// parked is set true by processCompletion when this run ends in a dormant
	// state (awaiting_input / pending_approval) rather than terminating. The
	// per-run cleanup defers below read it to KEEP the worktree and session
	// JSONL on disk as the warm resume cache — mirroring the wasTakenOver /
	// isBlueprintStep skips. Captured by reference by the deferred closures, so
	// they observe its final value at return.
	var parked bool
	if cfg.hasWT {
		// GitHub PR cleanup. Best-effort cleanup on return; the worktree ID is unique per run
		// so a failed remove just leaves a dangling directory under _worktrees.
		// Skipped when the run was taken over — Takeover() needs the worktree
		// to still exist for its copy and explicitly cleans up afterward.
		defer func() {
			if s.wasTakenOver(runID) {
				// Taken-over runs leave their worktree in place for the
				// user's interactive session; don't touch the per-PR
				// config either, since the takeover dir still uses
				// head-<n> for push. SweepStaleForkPRConfig reclaims
				// that config on the next bootstrap once the takeover
				// dir is gone.
				return
			}
			if cfg.isBlueprintStep {
				return
			}
			if parked {
				// Dormant yield/approval: the worktree is the warm cache the
				// resume reuses (a snapshot was taken too, for the cold path).
				return
			}
			// Capture the RemoveAt error rather than discarding it.
			// If the worktree dir failed to remove, the worktree is
			// still on disk and still attached to the bare's branch
			// tracking — stripping the per-PR config out from under a
			// surviving checkout would break its push/pull. Skip
			// cleanup in that case; the next bootstrap sweep will
			// reclaim the orphan once the worktree is gone.
			rmErr := worktree.RemoveAt(cfg.wtPath, runID)
			if rmErr != nil {
				log.Printf("[delegate] worktree remove failed for %s; skipping per-PR config cleanup: %v", runID, rmErr)
				return
			}
			// CleanupPRConfig uses a detached internal context so
			// cancellation of the agent's ctx (timeout, server
			// shutdown) doesn't short-circuit the cleanup.
			if cfg.prNumber > 0 && cfg.owner != "" && cfg.repo != "" {
				worktree.CleanupPRConfig(cfg.owner, cfg.repo, cfg.headRef, cfg.prNumber)
			}
		}()
	} else if cfg.runRoot != "" {
		// Jira lazy cleanup: the agent materialized zero or more worktrees
		// under cfg.runRoot via `workspace add`. Iterate run_worktrees,
		// nuke each, then remove the run-root parent. Same takeover gate
		// as the GitHub branch above — multi-worktree takeover isn't
		// implemented yet, but the Takeover() rejection on empty
		// runs.worktree_path catches Jira runs before they get here, so
		// the gate is defensive rather than load-bearing.
		defer func() {
			if s.wasTakenOver(runID) {
				return
			}
			if cfg.isBlueprintStep {
				return
			}
			if parked {
				return
			}
			rows, err := s.runWorktrees.ListSystem(context.Background(), orgID, runID)
			if err != nil {
				log.Printf("[delegate] run %s: list run_worktrees for cleanup: %v", runID, err)
			} else {
				// Use a detached context so cleanup is not skipped if the
				// agent ctx has already been canceled.
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				for _, w := range rows {
					rmErr := worktree.RemoveAt(w.Path, runID)
					if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
						log.Printf("[delegate] run %s: remove worktree %s: %v", runID, w.Path, rmErr)
						continue
					}
					if delErr := s.runWorktrees.DeleteByPathSystem(cleanupCtx, orgID, runID, w.Path); delErr != nil {
						log.Printf("[delegate] run %s: delete run_worktrees row for %s: %v", runID, w.Path, delErr)
					}
				}
			}
			worktree.RemoveRunRoot(runID)
		}()
	}

	// Initial cwd for the child claude. Always the run-root: the worktree
	// itself for GitHub PR runs, or the throwaway parent for Jira lazy runs
	// (the agent cd's into a per-repo subdir after `workspace add`).
	claudeCwd := cfg.wtPath
	// Nuke the ghost ~/.claude/projects/<encoded-cwd> that claude auto-creates
	// for this cwd. Safety-railed to only touch entries under $TMPDIR.
	// Skipped when the run was taken over by the user — the JSONL inside
	// is the conversation state the resumed `claude --resume` reads.
	defer func() {
		if s.wasTakenOver(runID) {
			return
		}
		if cfg.isBlueprintStep {
			return
		}
		if parked {
			// Keep the session JSONL: it's the conversation state the resumed
			// `claude --resume` reads (and what the cold-path snapshot carries).
			return
		}
		worktree.RemoveClaudeProjectDir(claudeCwd)
	}()

	// The memory namespace groups this run's memory file with its siblings:
	// the blueprint_run_id when this run is a blueprint step (so step N+1 reads
	// step N's memory as its handoff), else the run's own id. It names the
	// folder the agent reads from and writes into, and it's exported below so
	// the contract can reference it deterministically.
	namespace := memoryNamespace(cfg.blueprintRunID, runID)

	// Materialize any prior task memories into ./_scratch/entity-memory/, one
	// folder per workflow run, and create this run's own namespace folder, so
	// the agent sees what previous iterations on this task have already tried.
	// The directory is git-excluded by writeLocalExcludes
	// (managedExcludePatterns in internal/worktree/worktree.go) so
	// nothing leaks into the PR.
	materializePriorMemories(s.taskMemory, orgID, claudeCwd, task.EntityID, namespace)

	// SKY-219: copy the entity's project knowledge-base into
	// ./_scratch/project-knowledge/ if the entity is assigned to a
	// project, so the agent has curated project context available
	// alongside prior memories.
	materializeProjectKnowledge(orgID, claudeCwd, cfg.projectID)

	selfBin, err := os.Executable()
	if err != nil {
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, "failed to resolve own binary path: "+err.Error())
		return
	}

	// Load the primary event's metadata so buildPrompt can flatten its
	// fields into named placeholders (WORKFLOW_RUN_ID, HEAD_SHA, etc.) —
	// see placeholders.go. A DB failure here is non-fatal: the replacer
	// just leaves event-derived placeholders empty. FKs guarantee the
	// event exists, so a real miss would be a DB-level problem we want
	// to log and continue through rather than aborting the run.
	metadataJSON, err := s.events.GetMetadataSystem(context.Background(), orgID, task.PrimaryEventID)
	if err != nil {
		log.Printf("[delegate] warning: failed to load event metadata for task %s (event %s): %v — event placeholders will render empty", task.ID, task.PrimaryEventID, err)
		metadataJSON = ""
	}

	prompt := buildPrompt(task, metadataJSON, mission, cfg.scope, cfg.toolsRef, selfBin, runID)

	s.updateStatus(orgID, runID, "agent_starting")
	if ctx.Err() != nil {
		s.handleCancelled(orgID, runID, startTime, cfg.wtPath, triggerType, creatorUserID)
		return
	}

	extraEnv := []string{
		"TRIAGE_FACTORY_RUN_ID=" + runID,
		"TRIAGE_FACTORY_REVIEW_PREVIEW=1",
		"TRIAGE_FACTORY_RUN_ROOT=" + cfg.runRoot, // Set for both sources so the completion-gate retry message can reference an absolute _scratch/entity-memory path that resolves regardless of which worktree the agent has cd'd into.
		// The memory namespace folder: blueprint_run_id for a blueprint step
		// (its siblings' handoff), else the run's own id. Non-absolute, so it
		// passes through translateEnvForSandbox unchanged. The <entity_memory>
		// contract names this folder via the env var.
		"TRIAGE_FACTORY_BLUEPRINT_RUN_ID=" + namespace,
	}
	// Set TRIAGE_FACTORY_REPO when the run has a resolved GitHub repo context
	// (GitHub PR runs only) so gh subcommands can default to the right target
	// without the agent needing to pass --repo. Jira lazy runs leave it unset:
	// after the agent cd's into a worktree materialized by `workspace add`,
	// cmd/exec/gh/repo.go:resolveRepo falls through to .git/config, which is
	// the correct per-repo answer.
	if cfg.owner != "" && cfg.repo != "" {
		extraEnv = append(extraEnv, "TRIAGE_FACTORY_REPO="+cfg.owner+"/"+cfg.repo)
	}

	s.updateStatus(orgID, runID, "running")

	// StartAgentHost is invoked from inside agentproc.Run's sandbox
	// branch; the closure brings the run identity along so the
	// daemon's per-socket LocalClient routes writes through the right
	// (orgID, userID) pair. Local-mode + non-sandbox calls never
	// invoke this closure (agentproc gates on shouldSandbox).
	stores, storesSet := s.getStores()
	var startAgentHost func() (sandbox.Mount, io.Closer, error)
	if storesSet {
		startAgentHost = func() (sandbox.Mount, io.Closer, error) {
			info := agenthost.RunInfo{
				OrgID:            orgID,
				UserID:           creatorUserID,
				RunID:            runID,
				IsEventTriggered: triggerType == domain.TriggerTypeEvent,
			}
			hd, mount, err := agenthost.Start(stores, info)
			if err != nil {
				return sandbox.Mount{}, nil, err
			}
			return mount, hd, nil
		}
	}

	// Crash re-claim: if the run already carries a session from a prior
	// (crashed) invocation AND that session's transcript survived next to the
	// warm worktree, resume it rather than starting fresh. buildStepConfig has
	// already guaranteed the worktree is on disk by now (a host-loss re-claim
	// fails in ensureWorkspace before reaching here), so the transcript is
	// present exactly when it's safe to resume; otherwise we fall back to a
	// fresh session, which is correct, just re-does the step's work.
	resumeSession := ""
	if priorSessionID != "" && sessionTranscriptExists(claudeCwd, priorSessionID) {
		resumeSession = priorSessionID
		log.Printf("[delegate] run %s re-claimed mid-flight; resuming session %s", runID, priorSessionID)
	}

	log.Printf("[delegate] claude starting for run %s (cwd: %s)", runID, claudeCwd)
	baseOpts := agentproc.RunOptions{
		Cwd:            claudeCwd,
		Model:          model,
		SessionID:      resumeSession,
		Message:        prompt,
		AllowedTools:   agentproc.BuildAllowedToolsWithExtras(selfBin, cfg.extraAllowedTools),
		MaxTurns:       100,
		ExtraEnv:       extraEnv,
		TraceID:        runID,
		SystemPrompt:   cfg.appendSysPrompt,
		OrgID:          orgID,
		Secrets:        s.getRunSecrets(),
		StartAgentHost: startAgentHost,
	}
	sink := newRunSink(s, orgID, runID, triggerType, creatorUserID)

	// Execute as a long-lived LiveRun off the dispatcher where supported
	// (local), falling back to the one-shot sandbox path otherwise (multi —
	// streaming-input isn't wired through gVisor yet). Both produce the same
	// liveOutcome shape for the shared branching below.
	var out liveOutcome
	if agentproc.InteractiveSupported() {
		out = s.runLiveAndDrive(ctx, liveRunSpec{
			park: liveParkContext{
				orgID:         orgID,
				runID:         runID,
				taskID:        task.ID,
				namespace:     namespace,
				claudeCwd:     claudeCwd,
				triggerType:   triggerType,
				creatorUserID: creatorUserID,
			},
			opts: baseOpts,
			// Autonomous run: a nil permission handler makes the wrapper omit
			// canUseTool, so the allowlist is the sole gate (off-allowlist
			// tools auto-deny, no prompt) — byte-identical to the headless
			// one-shot path. P3 wires the browser pass-through handler.
			perms:       nil,
			sink:        sink,
			idleTimeout: s.idleTimeout(),
		})
	} else {
		out = s.runOneShot(ctx, baseOpts, sink)
	}

	// If Takeover() flipped the takenOver flag while we were streaming,
	// every code path below — completion ingestion, status updates, fail
	// paths, toasts — would step on the takeover lifecycle. Bail out
	// silently: Takeover owns the DB row and the worktree from here on.
	if s.wasTakenOver(runID) {
		return
	}

	// Idle hibernation parked the run (awaiting_input, snapshot written) — the
	// same dormant disposition as a yield, so keep the warm worktree.
	if out.hibernated {
		parked = true
		return
	}

	if out.result != nil {
		parked = s.processCompletion(ctx, orgID, runID, cfg.blueprintRunID, task, out.result, claudeCwd, out.sessionID, model, cfg.owner, cfg.repo, triggerType, creatorUserID, cfg.extraAllowedTools)
		return
	}

	if out.err != nil {
		if ctx.Err() != nil {
			s.handleCancelled(orgID, runID, startTime, cfg.wtPath, triggerType, creatorUserID)
			return
		}
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, fmt.Sprintf("%v\nstderr: %s", out.err, out.stderr))
		return
	}

	s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, "agent runtime exited cleanly without producing a result event")
}

// processCompletion handles the post-stream branching for any Claude
// invocation (initial run or yield-resume): if the parsed envelope is
// a yield, park the run in awaiting_input; otherwise run the completion
// gate and finalize the run as terminal. Shared between runAgent and
// ResumeAfterYield so a yield-then-resume run lands in identical
// terminal state to a run that completed in one shot — same gate, same
// toast, same task-done bookkeeping. SKY-139.
//
// blueprintRunID is the run's blueprint run (cfg.blueprintRunID for an initial
// run, the resumed run's blueprint_run_id for a yield-resume); empty for a
// standalone run. It's the authoritative source for the memory namespace and
// for whether this is a blueprint step — threaded in by the caller, which
// already holds it, rather than re-fetched, so a DB hiccup can't silently
// mis-namespace the memory or mis-route the task close.
//
// The caller is responsible for draining any subprocess state
// (the agentproc.Run path waits internally); this helper only
// operates on the parsed completion.
//
// Returns parked: true when the run ended dormant (awaiting_input or
// pending_approval) rather than terminal, so runAgent's cleanup defers keep
// the worktree + session JSONL on disk as the warm resume cache.
func (s *Spawner) processCompletion(
	ctx context.Context,
	orgID, runID, blueprintRunID string,
	task domain.Task,
	completion *agentproc.Result,
	claudeCwd, sessionID, model, owner, repo, triggerType, creatorUserID, extraAllowedTools string,
) (parked bool) {
	// Yield branch (SKY-139): the agent emitted outcome:"yield" to pause
	// the run for user input rather than terminating. Park it in
	// awaiting_input and skip BOTH gates and CompleteAgentRun — a pause
	// isn't a termination, so we don't force a memory write or an outcome.
	// The respond endpoint reopens the session via ResumeAfterYield when the
	// user answers. routeYield's IsError guard keeps a Claude-side error
	// (e.g. max-turns hit) that happens to carry yield-shaped JSON a failure,
	// not a pause.
	if s.routeYield(orgID, runID, task, completion, claudeCwd, blueprintRunID, sessionID, triggerType, creatorUserID) {
		return true
	}

	repoEnv := ""
	if owner != "" && repo != "" {
		repoEnv = owner + "/" + repo
	}

	// The memory namespace is the folder grouping this run's memory file with
	// its blueprint siblings (so step N+1 reads step N's as its handoff), else
	// the run's own id for a standalone run. Derived from the caller-supplied
	// blueprint_run_id — no DB fetch, so it can't silently fall back to the
	// wrong namespace on a transient read error.
	namespace := memoryNamespace(blueprintRunID, runID)

	// One completion gate (consolidates the former memory-write + outcome
	// gates): if the agent terminated without writing its namespaced memory
	// file ./_scratch/entity-memory/<namespace>/<runID>.md OR without a valid
	// terminal outcome, resume the session with a correction naming exactly
	// what's missing, up to maxCompletionRetries. Skipped on an infra IsError
	// completion — that's failing regardless and resuming a crashed session is
	// futile. Retries that produce new completions are merged into the totals
	// so cost/duration accounting reflects the full invocation, not just the
	// initial call.
	//
	// Pass model + repoEnv explicitly rather than letting the gate read live
	// spawner state, so a concurrent UpdateCredentials can't silently switch
	// models or drop repo context mid-run.
	if !completion.IsError {
		completion = s.completionGate(ctx, orgID, runID, claudeCwd, namespace, completion, sessionID, model, repoEnv, extraAllowedTools, triggerType, creatorUserID)
	}

	// Unconditional upsert of the run_memory row at termination
	// (SKY-204): row presence === "termination passed through the
	// gate", agent_content NULL === "agent didn't comply with the gate
	// after retries" (UpsertAgentMemory normalizes empty/whitespace
	// input to NULL on the way in). blueprint_run_id is denormalized
	// onto the row so the next run's materializer folders this file
	// under the right namespace.
	agentContent, fileState := readAgentMemoryFile(claudeCwd, namespace, runID)
	if err := s.taskMemory.UpsertAgentMemorySystem(context.Background(), orgID, runID, task.EntityID, blueprintRunID, agentContent); err != nil {
		log.Printf("[delegate] warning: failed to upsert memory for run %s: %v", runID, err)
	}
	switch fileState {
	case memoryFileMissing:
		log.Printf("[delegate] run %s: memory file missing after gate retries (agent_content NULL)", runID)
	case memoryFileEmpty:
		log.Printf("[delegate] run %s: memory file present but empty after gate retries (agent_content NULL)", runID)
	case memoryFileReadErr:
		log.Printf("[delegate] run %s: memory file unreadable after gate retries (agent_content NULL)", runID)
	}

	// A gate resume above can itself end in a yield (the agent decided it
	// needs the user before it can write memory or a valid outcome). Honor
	// that yield exactly as an initial one — park in awaiting_input — rather
	// than letting it fall through to the terminal-completion path below,
	// which would record status=completed and (for a single run) close the
	// task. The memory upsert just ran, but it's idempotent and gets
	// rewritten when the run truly terminates after the user responds.
	if s.routeYield(orgID, runID, task, completion, claudeCwd, blueprintRunID, sessionID, triggerType, creatorUserID) {
		return true
	}

	// Every run is a step of a blueprint_run now (a single prompt is a 1-step
	// blueprint), so this helper never owns task disposition: it persists
	// outcome/status only and leaves advancement + task close to the orchestrator
	// (runBlueprint / terminateBlueprint). blueprintRunID is always non-empty here.

	resultSummary := ""
	status := "completed"
	if completion.IsError {
		status = "failed"
	}
	var outcome, outcomeReason string
	if parsed := parseAgentResult(completion.Result); parsed != nil {
		resultSummary = parsed.Summary
		switch {
		case parsed.hasValidOutcome():
			outcome = parsed.Outcome
			if domain.RunOutcome(parsed.Outcome) == domain.RunOutcomeAbort {
				outcomeReason = parsed.Reason
			}
		case domain.RunOutcome(parsed.Outcome) == domain.RunOutcomeAbort:
			// The agent deliberately chose to abort but never supplied a
			// reason, even after the outcome gate's retries. Honor the abort
			// (leaving the task open) rather than letting the finish fallback
			// below close a task the agent explicitly declined to complete;
			// the reason stays empty.
			outcome = string(domain.RunOutcomeAbort)
		}
	}
	// No single-run finish fallback here: a run that completes with no
	// recognizable outcome keeps a NULL outcome and is left for the orchestrator.
	// decideBlueprintStep maps a NULL outcome on the final (or only) step to
	// finish — the same close-on-clean-completion behavior — and a NULL on a
	// non-final step to no-outcome→abort.

	// Detached context: the run's ctx may have been cancelled (user
	// cancel mid-stream) but the terminal write still needs to record.
	// Manual runs wrap in synthetic claims so the UPDATE passes RLS
	// under tf_app with the creator's identity; event-triggered runs
	// bypass via the admin pool.
	bgCtx := context.Background()

	// Does this completed run carry a queued external action awaiting human
	// approval? Two side-tables park a completed run in pending_approval:
	//   - pending_reviews: agent ran `pr submit-review` under
	//     TRIAGE_FACTORY_REVIEW_PREVIEW=1, queued the review for approval.
	//   - pending_prs: agent ran `pr create` under the same flag.
	// Detected once here — before the terminal write — so a blueprint step's
	// outcome can be coerced (below) before it's persisted, and reused for
	// the pending_approval flip after the write. Side-table lookups use
	// admin-pool System variants (no JWT claims in scope) and run on bgCtx so
	// a racing cancel doesn't silently strand a queued review outside the
	// approval queue.
	hasPending := false
	if status == "completed" {
		// A lookup error leaves hasPending=false, which would let the run
		// finish 'completed' while a queued review/PR strands outside the
		// approval queue (and skips the blueprint-step coercion below) — log
		// it so that failure mode is observable rather than silent.
		pendingReview, rErr := s.reviews.ByRunIDSystem(bgCtx, orgID, runID)
		if rErr != nil {
			log.Printf("[delegate] warning: pending-review lookup for run %s failed; a queued review may strand outside the approval queue: %v", runID, rErr)
		}
		if pendingReview != nil {
			hasPending = true
		} else {
			pendingPR, pErr := s.pendingPRs.ByRunIDSystem(bgCtx, orgID, runID)
			if pErr != nil {
				log.Printf("[delegate] warning: pending-PR lookup for run %s failed; a queued PR may strand outside the approval queue: %v", runID, pErr)
			}
			if pendingPR != nil {
				hasPending = true
			}
		}
	}

	// External-action coercion: a step that took a terminal external action
	// (queued a review or PR for human approval) ends the blueprint — there's
	// nothing for a follow-on step to do once the work is awaiting a human, and
	// the post-approval resume (ResumeBlueprintAfterApproval) finalizes only on a
	// finish outcome. Coerce anything-but-abort → finish before the terminal
	// write: continue (hand-off is moot), a missing outcome (gate exhausted —
	// the old single-run finish fallback that's now gone), and finish itself
	// (no-op) all resolve to finish. An explicit abort is the one exception — the
	// agent deliberately stopped, so the task stays open even with a queued
	// action. Replaces the synthetic --final verdict the chain path inserted.
	if hasPending && domain.RunOutcome(outcome) != domain.RunOutcomeAbort {
		outcome = string(domain.RunOutcomeFinish)
	}

	if triggerType == "manual" {
		if err := s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			return ts.AgentRuns.Complete(bgCtx, orgID, runID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, completion.StopReason, resultSummary, outcome, outcomeReason)
		}); err != nil {
			log.Printf("[delegate] warning: failed to record completion for run %s: %v", runID, err)
		}
	} else if err := s.agentRuns.CompleteSystem(bgCtx, orgID, runID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, completion.StopReason, resultSummary, outcome, outcomeReason); err != nil {
		log.Printf("[delegate] warning: failed to record completion for run %s: %v", runID, err)
	}

	s.updateBreakerCounter(task.ID, triggerType, status)

	if status == "completed" {
		// A queued external action (detected above) parks the run in
		// pending_approval: the user approves via the UI and the server
		// flips it back to completed. Frontend distinguishes by which
		// side-table has a row (the /api/agent/runs/{id} response carries
		// pending_kind). A single run stays pending_approval; a blueprint
		// step's outcome was already coerced to finish above so the
		// post-approval resume terminates the blueprint.
		if hasPending {
			// Snapshot the workspace to durable storage BEFORE parking in
			// pending_approval: once the run is dormant a resume could land
			// without the warm worktree, so the blob has to exist by the time
			// the flip commits. Best-effort — the warm worktree (kept on disk
			// below via the parked guard) is the primary path; the snapshot is
			// the cold-path backstop. Keyed by namespace (blueprint_run_id for a
			// step, else run_id).
			if err := s.snapshotWorkspace(ctx, orgID, namespace, claudeCwd, sessionID); err != nil {
				log.Printf("[delegate] warning: failed to snapshot workspace for run %s before pending_approval: %v", runID, err)
			}
			// Guarded transition: only flip to pending_approval if the
			// row is still 'completed'. A racing cancel/takeover after
			// agentRuns.Complete would otherwise be silently clobbered
			// by an unconditional UPDATE.
			var flippedToPending bool
			var flipErr error
			if triggerType == "manual" {
				flipErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
					f, ferr := ts.AgentRuns.MarkPendingApprovalIfCompleted(bgCtx, orgID, runID)
					flippedToPending = f
					return ferr
				})
			} else {
				flippedToPending, flipErr = s.agentRuns.MarkPendingApprovalIfCompletedSystem(bgCtx, orgID, runID)
			}
			if flipErr != nil {
				log.Printf("[delegate] warning: failed to set pending_approval for run %s: %v", runID, flipErr)
			} else if flippedToPending {
				status = "pending_approval"
				// Dormant: keep the worktree as the warm cache. The snapshot is
				// discarded by the approval/terminate path, not here.
				parked = true
			}
		}
	}

	// Task disposition (close on finish, leave-open on abort) is the
	// orchestrator's job now, not the step's: runBlueprint reads this run's
	// terminal runs.outcome and routes through terminateBlueprint, which owns the
	// terminal column. A step completion must never close the task here — the
	// next step may be about to run.
	s.broadcastRunUpdate(orgID, runID, status)
	// Recompute the aggregate board column. A pending_approval flip lands the
	// task in in_review here; a plain completion keeps it in_progress until the
	// orchestrator advances (next step) or terminates (done / leave-open).
	s.recomputeTaskBoardColumn(orgID, task.ID)

	// Toast the terminal state. Success cases auto-hide; failed/unsolvable
	// show as an error toast so the user notices even if they've clicked
	// away from the runs page.
	switch status {
	case "completed", "pending_approval":
		toast.Success(s.wsHub, orgID, fmt.Sprintf("Run %s completed", shortRunID(runID)))
	case "failed":
		toast.Error(s.wsHub, orgID, fmt.Sprintf("Run %s failed: %s", shortRunID(runID), truncateToastMsg(resultSummary, 160)))
	case "task_unsolvable":
		toast.Warning(s.wsHub, orgID, fmt.Sprintf("Run %s — task unsolvable: %s", shortRunID(runID), truncateToastMsg(resultSummary, 140)))
	}

	// Workspace-snapshot cleanup is owned by terminateBlueprint, keyed by
	// blueprint_run_id (the shared workspace's key) — every run is a blueprint
	// step now, so there is no standalone run_id-keyed snapshot to drop here. A
	// parked run keeps its snapshot for the eventual resume.
	return parked
}

// routeYield parks the run in awaiting_input when completion is a well-formed
// yield envelope, returning true if it handled the completion (the caller must
// then return without running the terminal-completion path). Used in two
// places in processCompletion: before the memory/outcome gates (an initial
// yield is a pause, not a termination, so it skips both gates) and again after
// them (a gate resume can itself end in a yield, which must still park rather
// than be treated as a completion that closes the task).
//
// A malformed/payload-less "yield" is deliberately NOT routed here (isYield
// gates on a valid payload) — it falls through so the outcome gate can
// re-prompt for a usable envelope instead of parking a modal the user can't
// act on. An IsError completion is never a yield, however yield-shaped its
// JSON.
//
// claudeCwd / blueprintRunID / sessionID are threaded through to persistYield
// so it can snapshot the workspace before parking — the run is about to go
// dormant and a resume could land without the warm worktree.
func (s *Spawner) routeYield(orgID, runID string, task domain.Task, completion *agentproc.Result, claudeCwd, blueprintRunID, sessionID, triggerType, creatorUserID string) bool {
	if completion.IsError {
		return false
	}
	parsed := parseAgentResult(completion.Result)
	if parsed == nil || !parsed.isYield() {
		return false
	}
	if err := s.persistYield(orgID, runID, parsed.Yield, completion, claudeCwd, blueprintRunID, sessionID, triggerType, creatorUserID); err != nil {
		log.Printf("[delegate] failed to persist yield for run %s: %v", runID, err)
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, "failed to record yield: "+err.Error())
	}
	return true
}

// persistYield records an agent yield request, accumulates the partial
// invocation totals onto the run row, and parks the run in
// awaiting_input. SKY-139.
//
// The status flip is guarded against concurrent terminal flips
// (cancellation, takeover) by MarkAgentRunAwaitingInput's
// status-NOT-IN filter. If the run already reached a terminal state
// while the agent was emitting the yield envelope (rare but possible
// — a user cancel raced the stream's last line), we still record the
// yield_request message for transcript completeness but skip the
// status flip and the toast. The terminal status the racing path set
// stands.
func (s *Spawner) persistYield(orgID, runID string, req *domain.YieldRequest, completion *agentproc.Result, claudeCwd, blueprintRunID, sessionID, triggerType, creatorUserID string) error {
	// Detached context — yield bookkeeping must survive ctx cancel
	// (a user cancel mid-yield-emit still needs the transcript
	// row recorded for audit).
	bgCtx := context.Background()

	if triggerType == "manual" {
		if err := s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			return ts.AgentRuns.AddPartialTotals(bgCtx, orgID, runID, completion.CostUSD, completion.DurationMs, completion.NumTurns)
		}); err != nil {
			log.Printf("[delegate] warning: failed to record partial totals for run %s: %v", runID, err)
		}
	} else if err := s.agentRuns.AddPartialTotalsSystem(bgCtx, orgID, runID, completion.CostUSD, completion.DurationMs, completion.NumTurns); err != nil {
		log.Printf("[delegate] warning: failed to record partial totals for run %s: %v", runID, err)
	}

	var msg *domain.AgentMessage
	if triggerType == "manual" {
		if err := s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			m, ierr := ts.AgentRuns.InsertYieldRequest(bgCtx, orgID, runID, req)
			if ierr != nil {
				return ierr
			}
			msg = m
			return nil
		}); err != nil {
			return fmt.Errorf("insert yield request: %w", err)
		}
	} else {
		m, ierr := s.agentRuns.InsertYieldRequestSystem(bgCtx, orgID, runID, req)
		if ierr != nil {
			return fmt.Errorf("insert yield request: %w", ierr)
		}
		msg = m
	}
	s.broadcastMessage(orgID, runID, msg)

	// Snapshot the workspace to durable storage BEFORE parking in
	// awaiting_input. Parking makes the run resumable on a host that may not
	// have the warm worktree, so the snapshot must exist by the time the flip
	// commits. Best-effort: the warm worktree (kept on disk by runAgent's
	// parked guard) is the fast resume path; this blob is the cold-path
	// backstop ensureWorkspace reads when the cache is gone. Keyed by namespace
	// (blueprint_run_id for a step, else run_id) — the same key the worktree dir
	// is named after.
	if err := s.snapshotWorkspace(bgCtx, orgID, memoryNamespace(blueprintRunID, runID), claudeCwd, sessionID); err != nil {
		log.Printf("[delegate] warning: failed to snapshot workspace for run %s before yield: %v", runID, err)
	}

	var flipped bool
	if triggerType == "manual" {
		if err := s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			f, ferr := ts.AgentRuns.MarkAwaitingInput(bgCtx, orgID, runID)
			if ferr != nil {
				return ferr
			}
			flipped = f
			return nil
		}); err != nil {
			return fmt.Errorf("mark awaiting_input: %w", err)
		}
	} else {
		f, ferr := s.agentRuns.MarkAwaitingInputSystem(bgCtx, orgID, runID)
		if ferr != nil {
			return fmt.Errorf("mark awaiting_input: %w", ferr)
		}
		flipped = f
	}
	if !flipped {
		// Terminal status was already set by a racing path (cancel, takeover).
		// The yield_request message is recorded for transcript completeness but
		// the run ends in whatever terminal state the racing path chose; no toast
		// or broadcast needed (the racing path already broadcast). The snapshot
		// taken just above won't be read (the run won't resume), but it's keyed by
		// blueprint_run_id and dropped by the blueprint-level terminal path — the
		// orchestrator sees the racing terminal status and calls terminateBlueprint,
		// or the DB-only cancel runs finalizeParkedBlueprintOnCancel — so there's
		// nothing to discard here.
		return nil
	}
	s.broadcastRunUpdate(orgID, runID, "awaiting_input")
	toast.Info(s.wsHub, orgID, fmt.Sprintf("Run %s waiting for response", shortRunID(runID)))
	return nil
}
