// Generic agent execution loop and the post-stream branching that turns a
// terminal completion into the right DB state — record the parsed outcome,
// finalize the run row, and (when the agent queued a review/PR) park it in
// pending_approval. Shared between the initial Delegate path and the
// resume-with-message flow. The concluded-vs-open turn classification and the
// invalid-envelope re-prompt live on the live driver (live.go); by the time a
// result reaches processCompletion it is a conclusion (or an IsError / crash
// result), never an open turn-end.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
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

// resolveCommitIdentity resolves the org's GitHub commit-author identity for
// this run — and, on a manual run whose delegating human differs from the org
// login, the Co-authored-by trailer crediting them (TFAC-452). This is the
// single seam: one resolution feeds both run modes (local direct + sandbox) and
// both the GitHub-PR and Jira-branch paths, via RunOptions.GitUserName/Email
// (the author/committer) and the prepare-commit-msg hook env var (the trailer).
//
// Returns the zero CommitIdentity (stamp nothing) when the GitHub resolver isn't
// wired (unit tests leave s.ghResolver nil), no org identity resolves
// (ok=false — no App, no stored PAT login), or the stores aren't set for the
// co-author lookup. The caller then leaves git identity unset and the agent
// inherits ambient config — never a fabricated identity; it self-heals once the
// org's PAT login is (re)persisted.
//
// This is also the single seam for a future per-team commit-author NAME: a
// team_agents name override would replace id.Name here only (the email stays the
// org-account noreply form so commits still link to the org identity).
func (s *Spawner) resolveCommitIdentity(ctx context.Context, orgID, triggerType, creatorUserID string) githooks.CommitIdentity {
	if s.ghResolver == nil {
		return githooks.CommitIdentity{}
	}
	orgLogin, ok := s.ghResolver.OrgIdentityFor(ctx, orgID)
	if !ok {
		return githooks.CommitIdentity{}
	}
	// Co-author only on manual runs (creatorUserID is NULL for event runs).
	// Resolve the delegating human's GitHub login on the org's current host;
	// ResolveCommitIdentity then emits the trailer only when it differs from the
	// org login (case-insensitive) — the N=1 same-PAT org gets none.
	manual := triggerType == "manual"
	var coLogin string
	if manual && creatorUserID != "" {
		if stores, set := s.getStores(); set {
			host, _ := s.ghResolver.BaseURLFor(ctx, orgID)
			coLogin, _ = stores.Users.GetGitHubLoginSystem(ctx, creatorUserID, host)
		}
	}
	id, _ := githooks.ResolveCommitIdentity(orgLogin, manual, coLogin)
	return id
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
	// parked is set true when this run ends dormant rather than terminating:
	// idle hibernation flips it to `open` (runAgent, below), or processCompletion
	// parks it in `pending_approval`. The per-run cleanup defers below read it to
	// KEEP the worktree and session JSONL on disk as the warm resume cache —
	// mirroring the isBlueprintStep skip. Captured by reference
	// by the deferred closures, so they observe its final value at return.
	var parked bool
	if cfg.hasWT {
		// GitHub PR cleanup. Best-effort cleanup on return; the worktree ID is unique per run
		// so a failed remove just leaves a dangling directory under _worktrees.
		defer func() {
			if cfg.isBlueprintStep {
				return
			}
			if parked {
				// Dormant (idle-closed `open`, or `pending_approval`): the worktree
				// is the warm cache the resume reuses (a snapshot was taken too, for
				// the cold path).
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
				delegateLog.Warn("worktree remove failed; skipping per-PR config cleanup", "run", runID, "error", rmErr)
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
		// nuke each, then remove the run-root parent.
		defer func() {
			if cfg.isBlueprintStep {
				return
			}
			if parked {
				return
			}
			rows, err := s.runWorktrees.ListSystem(context.Background(), orgID, runID)
			if err != nil {
				delegateLog.Warn("list run_worktrees for cleanup failed", "run", runID, "error", err)
			} else {
				// Use a detached context so cleanup is not skipped if the
				// agent ctx has already been canceled.
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				for _, w := range rows {
					rmErr := worktree.RemoveAt(w.Path, runID)
					if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
						delegateLog.Warn("remove worktree failed", "run", runID, "path", w.Path, "error", rmErr)
						continue
					}
					if delErr := s.runWorktrees.DeleteByPathSystem(cleanupCtx, orgID, runID, w.Path); delErr != nil {
						delegateLog.Warn("delete run_worktrees row failed", "run", runID, "path", w.Path, "error", delErr)
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
	defer func() {
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
		delegateLog.Warn("load event metadata for task failed; event placeholders will render empty", "task", task.ID, "event", task.PrimaryEventID, "error", err)
		metadataJSON = ""
	}

	// Resolve the paths the agent will actually observe, which differ from the
	// host paths under the sandbox: the run-root is bind-mounted at "/work" and
	// the TF binary at sandboxTFBinary. Pre-expanding these into the prompt is
	// what makes the agent's file tools (no shell expansion) and its
	// `{{BINARY_PATH}} exec ...` invocations resolve regardless of sandbox mode.
	// selfBin itself stays the host path below for BuildAllowedToolsWithExtras —
	// agentproc.Run rewrites the allowlist's binary path for the sandbox on its
	// own (rewriteAllowedToolsForSandbox).
	agentRunRoot := agentproc.AgentVisibleRoot(cfg.runRoot)
	agentBin := agentproc.AgentVisibleBinary(selfBin)
	prompt := buildPrompt(task, metadataJSON, mission, cfg.scope, cfg.toolsRef, agentBin, runID, agentRunRoot, namespace)

	s.updateStatus(orgID, runID, "agent_starting")
	if ctx.Err() != nil {
		s.handleCancelled(orgID, runID, startTime, cfg.wtPath, triggerType, creatorUserID)
		return
	}

	extraEnv := []string{
		"TRIAGE_FACTORY_RUN_ID=" + runID,
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

	// Resolve the org's GitHub commit identity once for this run (TFAC-452). The
	// org identity authors + commits every agent commit (injected as
	// user.name/user.email via baseOpts below, both run modes); a manual run
	// whose delegating human differs additionally co-attributes them through the
	// prepare-commit-msg hook, fed by this namespaced env var. A zero identity
	// (resolver/stores unwired, or no org identity resolves) leaves git config
	// ambient — no fabricated identity. One resolution feeds both modes and both
	// the GitHub-PR and Jira-branch paths; it carries across blueprint steps via
	// the copied creator/trigger. The env var is namespaced (not GIT_*) so it's
	// never ambient git identity and passes translateEnvForSandbox unchanged.
	commitIdentity := s.resolveCommitIdentity(ctx, orgID, triggerType, creatorUserID)
	if commitIdentity.CoAuthorTrailer != "" {
		extraEnv = append(extraEnv, "TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER="+commitIdentity.CoAuthorTrailer)
	}

	s.updateStatus(orgID, runID, "running")

	// StartAgentHost is invoked from inside agentproc.Run's sandbox
	// branch; the closure brings the run identity along so the
	// daemon's per-socket LocalClient routes writes through the right
	// (orgID, userID) pair. Local-mode + non-sandbox calls never
	// invoke this closure (agentproc gates on shouldSandbox).
	stores, storesSet := s.getStores()
	var (
		info           agenthost.RunInfo
		startAgentHost func() (sandbox.Mount, io.Closer, error)
	)
	if storesSet {
		info = agenthost.RunInfo{
			OrgID:            orgID,
			UserID:           creatorUserID,
			RunID:            runID,
			TeamID:           cfg.teamID,
			IsEventTriggered: triggerType == domain.TriggerTypeEvent,
		}
		startAgentHost = func() (sandbox.Mount, io.Closer, error) {
			hd, mount, err := agenthost.Start(stores, info)
			if err != nil {
				return sandbox.Mount{}, nil, err
			}
			return mount, hd, nil
		}
	}

	// Git proxy (multi mode, repo in scope): wire its receive-pack backstop to
	// the same record path the pre-push hook uses, so a `git push --no-verify`
	// the hook can't see still records a branch artifact (TFAC-467). nil in
	// local mode / no repo / no stores — no proxy, accepted gap.
	gitProxy := s.gitProxyConfigFor(ctx, orgID, cfg.owner)
	if gitProxy != nil && storesSet {
		gitProxy.RecordPush = gitPushRecorder(stores, info)
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
		delegateLog.Info("run re-claimed mid-flight; resuming session", "run", runID, "session", priorSessionID)
	}

	delegateLog.Info("claude starting for run", "run", runID, "cwd", claudeCwd)
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
		GitProxy:       gitProxy,
		StartAgentHost: startAgentHost,
		// Org commit identity (TFAC-452): empty when none resolved → ambient git
		// config inherited (today's behavior).
		GitUserName:  commitIdentity.Name,
		GitUserEmail: commitIdentity.Email,
	}
	sink := newRunSink(s, orgID, runID, triggerType, creatorUserID)

	// Resolve the presence-gated absent-auto-deny policy once for this run's
	// team (TFAC-392) and capture it in the permission handler closure — no
	// per-prompt DB read. teamID nil-derefs to "" (resolveAbsentAutoDeny then
	// falls back to defaults); task.TeamID is set for any task that reaches a run.
	teamID := ""
	if task.TeamID != nil {
		teamID = *task.TeamID
	}
	absentDeny := s.resolveAbsentAutoDeny(ctx, teamID)

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
			// Interactive runs surface off-allowlist tools to the browser as a
			// permission_request and park the turn until the user answers (Allow/Deny)
			// or permTimeout() denies it — kept below idleTimeout so an unwatched run
			// degrades to a bounded deny, never a hang. With absent-auto-deny on, an
			// unattended prompt denies after the team's grace instead. A generous
			// allowlist keeps prompts rare.
			perms:       s.BrowserPermissionHandler(orgID, runID, absentDeny),
			sink:        sink,
			idleTimeout: s.idleTimeout(),
		})
	} else {
		out = s.runOneShot(ctx, baseOpts, sink)
	}

	// Idle hibernation parked the run (status `open`, snapshot written) — a
	// dormant disposition, so keep the warm worktree as the fast resume path.
	if out.hibernated {
		parked = true
		return
	}

	if out.result != nil {
		parked = s.processCompletion(ctx, orgID, runID, cfg.blueprintRunID, task, out.result, claudeCwd, out.sessionID, triggerType, creatorUserID)
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

// processCompletion is the single disposition authority for a result, whatever
// backend produced it (the live driver, the one-shot sandbox fallback, or a
// resume). It classifies the turn-end and acts uniformly:
//
//   - valid conclusion → record the outcome (finalize / let the orchestrator
//     advance or close); a queued review/PR parks it in pending_approval.
//   - no conclusion (prose / nothing) → the run is open, not a termination:
//     park it open (snapshot + flip + keep the warm worktree) and return. The
//     live driver normally consumes this in its loop; it reaches here only from
//     the one-shot/resume backends (which hold no warm process) or a crash that
//     left a complete-but-open last turn.
//   - invalid attempt / IsError → record failed (a knowable error) with the
//     totals already folded onto the result.
//
// Keeping the disposition here — rather than in the live driver — is what makes
// the non-live backends honest: a one-shot/resume turn that ends open or with a
// malformed envelope is no longer mis-recorded as a clean NULL-outcome
// completion (which would wrongly advance/close a final step).
//
// blueprintRunID is the run's blueprint run (cfg.blueprintRunID for an initial
// run, the resumed run's blueprint_run_id for a resume). It's the authoritative
// source for the memory namespace and for whether this is a blueprint step —
// threaded in by the caller, which already holds it, rather than re-fetched, so
// a DB hiccup can't silently mis-namespace the memory or mis-route the task
// close.
//
// Returns parked: true when the run ended dormant (open, or pending_approval)
// rather than terminal, so runAgent's cleanup defers keep the worktree +
// session JSONL on disk as the warm resume cache.
func (s *Spawner) processCompletion(
	ctx context.Context,
	orgID, runID, blueprintRunID string,
	task domain.Task,
	completion *agentproc.Result,
	claudeCwd, sessionID, triggerType, creatorUserID string,
) (parked bool) {
	// The memory namespace is the folder grouping this run's memory file with
	// its blueprint siblings (so step N+1 reads step N's as its handoff).
	// Derived from the caller-supplied blueprint_run_id — no DB fetch, so it
	// can't silently fall back to the wrong namespace on a transient read error.
	namespace := memoryNamespace(blueprintRunID, runID)

	// Classify the turn-end up front. A no-conclusion turn (prose / nothing) is
	// NOT a termination — the run is open. Park it open (snapshot for the
	// cold-resume backstop, flip the status, keep the warm worktree) and skip the
	// terminal bookkeeping below. An IsError result is always a termination, so
	// it never takes this branch however envelope-shaped its text.
	class, parsed := classifyAgentResult(completion.Result)
	if !completion.IsError && class == turnNone {
		s.parkRunOpen(liveParkContext{
			orgID:         orgID,
			runID:         runID,
			taskID:        task.ID,
			namespace:     namespace,
			claudeCwd:     claudeCwd,
			triggerType:   triggerType,
			creatorUserID: creatorUserID,
		}, sessionID)
		return true
	}

	// Unconditional upsert of the run_memory row at termination: row presence
	// === "the run terminated", agent_content NULL === "the agent didn't write a
	// usable memory file" (UpsertAgentMemory normalizes empty/whitespace input
	// to NULL on the way in). blueprint_run_id is denormalized onto the row so
	// the next run's materializer folders this file under the right namespace.
	agentContent, fileState := readAgentMemoryFile(claudeCwd, namespace, runID)
	if err := s.taskMemory.UpsertAgentMemorySystem(context.Background(), orgID, runID, task.EntityID, blueprintRunID, agentContent); err != nil {
		delegateLog.Warn("upsert memory for run failed", "run", runID, "error", err)
	}
	switch fileState {
	case memoryFileMissing:
		delegateLog.Debug("memory file missing at termination (agent_content NULL)", "run", runID)
	case memoryFileEmpty:
		delegateLog.Debug("memory file present but empty at termination (agent_content NULL)", "run", runID)
	case memoryFileReadErr:
		delegateLog.Debug("memory file unreadable at termination (agent_content NULL)", "run", runID)
	}

	// Every run is a step of a blueprint_run now (a single prompt is a 1-step
	// blueprint), so this helper never owns task disposition: it persists
	// outcome/status only and leaves advancement + task close to the orchestrator
	// (runBlueprint / terminateBlueprint). blueprintRunID is always non-empty here.

	resultSummary := ""
	status := "completed"
	var outcome, outcomeReason string
	switch {
	case completion.IsError:
		// Process crash / runtime error — an always-knowable terminal.
		status = "failed"
	case class == turnValid:
		resultSummary = parsed.Summary
		outcome = parsed.Outcome
		if domain.RunOutcome(parsed.Outcome) == domain.RunOutcomeAbort {
			outcomeReason = parsed.Reason
		}
	default: // turnInvalid
		// An envelope attempt that never validated. The live driver re-prompts
		// this in place; a backend that can't (one-shot) or that exhausted the
		// bound lands here, where it's a knowable error → failed, recorded with
		// the totals folded onto the result. NOT a NULL-outcome completion, which
		// the orchestrator would read as a clean finish on a final step.
		status = "failed"
		resultSummary = "agent did not return a valid completion envelope"
	}

	// Detached context: the run's ctx may have been cancelled (user
	// cancel mid-stream) but the terminal write still needs to record.
	// Manual runs wrap in synthetic claims so the UPDATE passes RLS
	// under tf_app with the creator's identity; event-triggered runs
	// bypass via the admin pool.
	bgCtx := context.Background()

	// Does this completed run carry a queued external action awaiting human
	// approval? Two artifact kinds park a completed run in pending_approval:
	//   - a review artifact whose ready sentinel is set: agent ran `pr
	//     submit-review`, which finalized the GitHub pending review for approval.
	//     A start-review that never reached submit-review has a pending review
	//     artifact with NO ready sentinel and must NOT park (fixes the spurious
	//     start-but-not-submit park) — FirstReadyReview enforces that.
	//   - a draft PR artifact: agent ran `pr create`, which opens a real draft
	//     PR and records a pull_request artifact in state=draft.
	// Detected once here — before the terminal write — so a blueprint step's
	// outcome can be coerced (below) before it's persisted, and reused for the
	// pending_approval flip after the write. One admin-pool by-run read (no JWT
	// claims in scope), on bgCtx so a racing cancel doesn't silently strand a
	// queued action outside the approval queue.
	hasPending := false
	if status == "completed" {
		// A lookup error leaves hasPending=false, which would let the run finish
		// 'completed' while a queued review/PR strands outside the approval queue
		// (and skips the blueprint-step coercion below) — log it so that failure
		// mode is observable rather than silent.
		arts, aErr := s.artifacts.ListByRunSystem(bgCtx, orgID, runID)
		if aErr != nil {
			delegateLog.Warn("artifact lookup for run failed; a queued review/PR may strand outside the approval queue", "run", runID, "error", aErr)
		}
		if domain.FirstReadyReview(arts) != nil || domain.FirstDraftPullRequest(arts) != nil {
			hasPending = true
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
			delegateLog.Warn("record completion for run failed", "run", runID, "error", err)
		}
	} else if err := s.agentRuns.CompleteSystem(bgCtx, orgID, runID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, completion.StopReason, resultSummary, outcome, outcomeReason); err != nil {
		delegateLog.Warn("record completion for run failed", "run", runID, "error", err)
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
			// Guarded transition: only flip to pending_approval if the
			// row is still 'completed'. A racing cancel/takeover after
			// agentRuns.Complete would otherwise be silently clobbered
			// by an unconditional UPDATE. The flip comes FIRST — the
			// review/PR approval card keys off this status, so nothing
			// user-visible waits on workspace I/O.
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
				delegateLog.Warn("set pending_approval for run failed", "run", runID, "error", flipErr)
			} else if flippedToPending {
				status = "pending_approval"
				// Dormant: keep the worktree as the warm cache.
				parked = true
				// Snapshot every pending_approval flip, whatever the outcome: a
				// run parked in any non-finish state is message-resumable, and a
				// user can message it (resuming the session) before approving the
				// queued artifact — a resume that lands without the warm worktree
				// (host loss / sandbox reclaim) must rebuild from this blob.
				// Written AFTER the flip: the snapshot only matters once the warm
				// worktree is gone, never in the flip-to-visible window, so
				// nothing user-visible waits on the git-bundle cost. Keyed by
				// namespace (blueprint_run_id). A finish-coerced flip's blob is
				// dropped by terminateBlueprint on approval if never resumed;
				// retention is otherwise bounded by the TTL sweep.
				if err := s.snapshotWorkspace(ctx, orgID, namespace, claudeCwd, sessionID); err != nil {
					delegateLog.Warn("snapshot workspace for run after pending_approval failed", "run", runID, "error", err)
				}
			}
		} else if domain.RunOutcome(outcome) == domain.RunOutcomeAbort {
			// A plain abort (completed + outcome=abort, no queued artifact) is
			// message-resumable, so snapshot its workspace now — while the
			// worktree and session transcript are still on disk — then let the
			// per-run cleanup defers tear the worktree down (parked stays false:
			// keeping every aborted run's worktree warm is not acceptable, so
			// cold rehydrate from this blob is the resume path). terminateBlueprint
			// retains the blob for an aborted terminal; the TTL sweep reaps it.
			if err := s.snapshotWorkspace(ctx, orgID, namespace, claudeCwd, sessionID); err != nil {
				delegateLog.Warn("snapshot workspace for aborted run failed", "run", runID, "error", err)
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
