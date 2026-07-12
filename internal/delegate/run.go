// Generic agent execution loop and the post-stream branching that turns a
// terminal completion into the right DB state — record the parsed outcome,
// finalize the run row, and snapshot a voluntarily-aborted run for resume. A
// queued draft PR / pending review is an async sidecar artifact that never parks
// the run — the step completes with its real outcome and the
// orchestrator advances. Shared between the initial Delegate path and the
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
	"path/filepath"
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
	orgName, orgEmail, ok := s.ghResolver.OrgIdentityFor(ctx, orgID)
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
			// The login is host-scoped (user_github_identities is keyed by host), so
			// a failed base-URL resolve must SKIP the lookup, not pass host="": on a
			// GHE org an empty host would query the wrong (default) host and silently
			// miss — or mis-resolve — the human's identity. Both failures are
			// non-fatal (the run proceeds without a trailer) and debug-logged so an
			// operator can diagnose a missing/wrong trailer without it surfacing as a
			// run error.
			if host, herr := s.ghResolver.BaseURLFor(ctx, orgID); herr != nil {
				delegateLog.Debug("resolve org github base for co-author lookup failed; manual run will omit the trailer",
					"org", orgID, "error", herr)
			} else if login, err := stores.Users.GetGitHubLoginSystem(ctx, creatorUserID, host); err != nil {
				delegateLog.Debug("resolve co-author github login failed; manual run will omit the trailer",
					"creator", creatorUserID, "error", err)
			} else {
				coLogin = login
			}
		}
	}
	id, _ := githooks.ResolveCommitIdentity(orgName, orgEmail, manual, coLogin)
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
	// idle hibernation flips it to `open` (runAgent, below). The per-run cleanup
	// defers below read it to KEEP the worktree and session JSONL on disk as the
	// warm resume cache — mirroring the isBlueprintStep skip. Captured by
	// reference by the deferred closures, so they observe its final value at
	// return. A completed run never parks anymore: a queued artifact is
	// a sidecar, not a reason to hold the run open.
	var parked bool
	if cfg.hasWT {
		// GitHub PR cleanup. Best-effort cleanup on return; the worktree ID is unique per run
		// so a failed remove just leaves a dangling directory under _worktrees.
		defer func() {
			if cfg.isBlueprintStep {
				return
			}
			if parked {
				// Dormant (idle-closed `open`): the worktree is the warm cache the
				// resume reuses (a snapshot was taken too, for the cold path).
				return
			}
			// Capture the RemoveAt error rather than discarding it. If the
			// worktree dir failed to remove, the worktree is still on disk and
			// still attached to the bare's branch tracking — stripping the
			// per-PR config out from under a surviving checkout would break its
			// push/pull, so that cleanup is skipped below; the next bootstrap
			// sweep reclaims the orphan once the dir is gone.
			rmErr := worktree.RemoveAt(cfg.wtPath, runID)
			if rmErr != nil {
				delegateLog.Warn("worktree remove failed; skipping per-PR config cleanup", "run", runID, "error", rmErr)
			}
			// Drop the eager worktree's run_worktrees row (recorded at setup with
			// ref=pr-<N> so the least-privilege gates could authorize the task
			// repo) — regardless of the RemoveAt outcome, since the row is
			// run-scoped metadata, not the worktree, and a lingering dir doesn't
			// need its ledger entry. Past the parked/blueprint early-returns
			// above, so a resume or chain step keeps the row; only a real
			// terminal teardown removes it. Best-effort: a leaked row is harmless
			// (run-scoped, never collides a future run).
			if s.runWorktrees != nil && cfg.owner != "" && cfg.repo != "" && cfg.prNumber > 0 {
				if delErr := s.runWorktrees.DeleteByRepoRefSystem(context.Background(), orgID, runID, cfg.owner+"/"+cfg.repo, worktree.PRRefSlug(cfg.prNumber)); delErr != nil {
					delegateLog.Warn("delete eager worktree run_worktrees row failed", "run", runID, "repo", cfg.owner+"/"+cfg.repo, "error", delErr)
				}
			}
			// Per-PR config cleanup only when the worktree is actually gone (see
			// above). Pass the creating run id (the worktree-dir basename, which
			// CreateForPR set to runID) so CleanupPRConfig reclaims THIS run's
			// per-run branch + push remote, never a sibling's. It uses a detached
			// internal context so cancellation of the agent's ctx (timeout,
			// server shutdown) doesn't short-circuit it.
			if rmErr != nil {
				return
			}
			if cfg.prNumber > 0 && cfg.owner != "" && cfg.repo != "" {
				worktree.CleanupPRConfig(cfg.owner, cfg.repo, cfg.prNumber, filepath.Base(cfg.wtPath))
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
					if rmErr := worktree.RemoveAt(w.Path, runID); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
						delegateLog.Warn("remove worktree failed", "run", runID, "path", w.Path, "error", rmErr)
						// Fall through to drop the DB row anyway — it's ephemeral
						// run-coordination state, and a lingering on-disk dir is
						// reclaimed by the startup sweep regardless. Mirrors the
						// blueprint teardown loop (runBlueprintWorktreeCleanup).
					} else {
						// Inline per-PR config reclaim (Decision D): a finishing
						// workspace-add'd PR run reclaims its own per-run branch +
						// push remote here, so the bootstrap sweep stays a pure crash
						// backstop. Gated on the worktree being gone — `git branch -D`
						// is refused while a checkout survives. w.RunID == runID
						// (created the worktree), so this targets this run's branch,
						// never a concurrent run's.
						reclaimWorkspaceAddPRConfig(w)
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
	materializePriorMemories(s.taskMemory, orgID, cfg.teamID, claudeCwd, task.EntityID, namespace)

	// Copy the entity's project knowledge-base into
	// ./_scratch/project-knowledge/ if the entity is assigned to a
	// project, so the agent has curated project context available
	// alongside prior memories.
	materializeProjectKnowledge(orgID, claudeCwd, cfg.projectID)

	selfBin, err := os.Executable()
	if err != nil {
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, "failed to resolve own binary path: "+err.Error(), domain.RunFailureUnclassified)
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
	// The team's branch-naming convention, ticket-id-resolved, surfaced to the
	// agent as envelope guidance (TFAC-498). Not enforced — the push gate
	// authorizes whatever branch the worktree lands on.
	branchTemplate := s.resolveBranchTemplate(context.Background(), task)
	runURL := s.runURLFor(orgID, runID)
	prompt := buildPrompt(task, metadataJSON, mission, cfg.scope, cfg.toolsRef, agentBin, runID, agentRunRoot, namespace, branchTemplate, runURL)

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

	// Git proxy (multi mode, org has a GitHub credential): the per-repo
	// scoped-token source + the live repo/ref gate, plus the receive-pack
	// backstop wired to the same record path the pre-push hook uses so a
	// `git push --no-verify` the hook can't see still records a branch artifact
	// (TFAC-467). nil in local mode / no GitHub credential — no proxy.
	gitProxy := s.gitProxyConfigFor(ctx, info, stores)
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
		Cwd:          claudeCwd,
		Model:        model,
		SessionID:    resumeSession,
		Message:      prompt,
		AllowedTools: agentproc.BuildAllowedToolsWithExtras(selfBin, cfg.extraAllowedTools),
		MaxTurns:     100,
		ExtraEnv:     extraEnv,
		TraceID:      runID,
		SystemPrompt: cfg.appendSysPrompt,
		OrgID:        orgID,
		Secrets:      s.getRunSecrets(),
		LLMResolver:  s.llmResolverForRun(orgID, runID),
		// Executor bundle path only (nil elsewhere): the SigV4 proxy re-reads
		// the newest sealed bundle's triple live so a re-minted role-mode STS
		// session credential reaches a mid-run sandbox (TFAC-616).
		LLMCredentialSource: s.bundleLLMSourceFor(ctx, info),
		GitProxy:            gitProxy,
		StartAgentHost:      startAgentHost,
		// Org commit identity (TFAC-452): empty when none resolved → ambient git
		// config inherited (today's behavior).
		GitUserName:  commitIdentity.Name,
		GitUserEmail: commitIdentity.Email,
	}
	sink := newRunSink(s, orgID, runID, triggerType, creatorUserID)

	// teamID nil-derefs to "" (resolveAbsentAutoDeny then falls back to
	// defaults); task.TeamID is set for any task that reaches a run.
	teamID := ""
	if task.TeamID != nil {
		teamID = *task.TeamID
	}

	// Off-allowlist tool calls route to one of two dispositions, chosen once
	// per run:
	//
	//   - gVisor-sandboxed (multi mode + Linux): auto-approve. Delegated runs
	//     are unattended by design, so the presence-gated round-trip below
	//     almost always resolves via its own absent-grace deny anyway — the
	//     sandbox + the static allowlist + the enumerated agenthost RPC
	//     surface are the actual boundary, not a prompt nobody is there to
	//     answer.
	//   - Otherwise (local mode, no gVisor): the presence-gated browser
	//     round-trip — the allowlist is the only boundary there, so an
	//     off-allowlist call still needs a live decision or the
	//     absent-grace/timeout deny.
	var perms agentproc.PermissionHandler
	if agentproc.WillSandbox() {
		perms = s.AutoApprovePermissionHandler(runID)
	} else {
		perms = s.BrowserPermissionHandler(orgID, runID, s.resolveAbsentAutoDeny(ctx, teamID))
	}

	// Execute as a long-lived LiveRun — both local direct runs and multi-mode
	// gVisor-sandboxed runs drive through the streaming-input path (the
	// sandbox's bidirectional stdio channel is validated end-to-end).
	// runOneShot is retained only as the seam InteractiveSupported forks on,
	// for a future host that can't support the streaming path.
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
			opts:        baseOpts,
			perms:       perms,
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
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, fmt.Sprintf("%v\nstderr: %s", out.err, out.stderr), classifyFailureKind(out.err))
		return
	}

	s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, "agent runtime exited cleanly without producing a result event", domain.RunFailureNoResult)
}

// processCompletion is the single disposition authority for a result, whatever
// backend produced it (the live driver, the one-shot sandbox fallback, or a
// resume). It classifies the turn-end and acts uniformly:
//
//   - valid conclusion → record the outcome (finalize / let the orchestrator
//     advance or close). A queued review/PR is an async sidecar artifact and
//     never parks the run; the step completes with its real outcome.
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
// Returns parked: true when the run ended dormant (open) rather than terminal,
// so runAgent's cleanup defers keep the worktree + session JSONL on disk as the
// warm resume cache. A terminal completion (including one that produced a draft
// PR / pending review) returns false — the artifact is a resolvable sidecar, not
// a reason to park.
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
	failureKind := domain.RunFailureUnclassified
	switch {
	case completion.IsError:
		// Process crash / runtime error — an always-knowable terminal.
		status = "failed"
		failureKind = domain.RunFailureAgentError
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
		// the orchestrator would read as a clean finish on a final step. Same
		// no-usable-result kind as the never-produced-a-result-event failure.
		status = "failed"
		failureKind = domain.RunFailureNoResult
		resultSummary = "agent did not return a valid completion envelope"
	}

	// Detached context: the run's ctx may have been cancelled (user
	// cancel mid-stream) but the terminal write still needs to record.
	// Manual runs wrap in synthetic claims so the UPDATE passes RLS
	// under tf_app with the creator's identity; event-triggered runs
	// bypass via the admin pool.
	bgCtx := context.Background()

	// A queued draft PR / pending review NO LONGER parks the run. The
	// artifact was already recorded by the exec choke point and is an async
	// sidecar: a human resolves it independently and it never blocks step
	// progression. So there's no external-action coercion and no pending_approval
	// flip here — the step completes with its real outcome (continue advances,
	// finish terminates) per decideBlueprintStep, and the approval state is
	// derived downstream from the unresolved-artifact set (has_unresolved_artifacts).

	if triggerType == "manual" {
		if err := s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			return ts.AgentRuns.Complete(bgCtx, orgID, runID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, completion.StopReason, resultSummary, outcome, outcomeReason, string(failureKind))
		}); err != nil {
			delegateLog.Warn("record completion for run failed", "run", runID, "error", err)
		}
	} else if err := s.agentRuns.CompleteSystem(bgCtx, orgID, runID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, completion.StopReason, resultSummary, outcome, outcomeReason, string(failureKind)); err != nil {
		delegateLog.Warn("record completion for run failed", "run", runID, "error", err)
	}

	s.updateBreakerCounter(task.ID, triggerType, status)

	// A completed+abort run (the agent deliberately stopped) is message-resumable,
	// so snapshot its workspace now — while the worktree and session transcript
	// are still on disk — then let the per-run cleanup defers tear the worktree
	// down (parked stays false: keeping every aborted run's worktree warm is not
	// acceptable, so cold rehydrate from this blob is the resume path).
	// terminateBlueprint retains the blob for an aborted terminal; the TTL sweep
	// reaps it. A completed run that produced a draft PR / pending review does NOT
	// park or snapshot for that reason — the artifact is a sidecar
	// resolved asynchronously, the step completed with its real outcome, and the
	// approval state is derived from the unresolved-artifact set downstream.
	if status == "completed" && domain.RunOutcome(outcome) == domain.RunOutcomeAbort {
		if err := s.snapshotWorkspace(ctx, orgID, namespace, claudeCwd, sessionID); err != nil {
			delegateLog.Warn("snapshot workspace for aborted run failed", "run", runID, "error", err)
		}
	}

	// Task disposition (close on finish, leave-open on abort) is the
	// orchestrator's job now, not the step's: runBlueprint reads this run's
	// terminal runs.outcome and routes through terminateBlueprint, which owns the
	// terminal column. A step completion must never close the task here — the
	// next step may be about to run.
	if status == "failed" {
		s.broadcastRunFailed(orgID, runID, failureKind)
	} else {
		s.broadcastRunUpdate(orgID, runID, status)
	}
	// Recompute the aggregate board column. A completed step that left an
	// unresolved artifact (draft PR / ready review) lands the task in_review (the
	// derived approval column); otherwise it stays in_progress until the
	// orchestrator advances (next step) or terminates (done / leave-open).
	s.recomputeTaskBoardColumn(orgID, task.ID)

	// Toast the terminal state. Success cases auto-hide; failed/unsolvable
	// show as an error toast so the user notices even if they've clicked
	// away from the runs page.
	switch status {
	case "completed":
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
