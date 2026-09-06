// Generic agent execution loop and the post-stream branching that turns a
// terminal completion into the right DB state — record the parsed outcome,
// finalize the conversation row, and snapshot a voluntarily-aborted
// conversation for resume. A queued draft PR / pending review is an async
// sidecar artifact that never parks the conversation — the step completes with
// its real outcome and the orchestrator advances. Shared between the initial
// Delegate path and the resume-with-message flow. The concluded-vs-open turn
// classification and the
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
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// noopCloser is an io.Closer that closes nothing. The executor path returns it
// from startAgentHost because the credential sidecar owns the exec-verb socket's
// lifecycle — the orchestrator hosts no daemon there and so has nothing to shut
// down. It is deliberately not an io.ReadCloser: there is no reader to read.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }

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

// errorResultSummary turns an agent runtime error result into the human-facing
// failure summary persisted on the run. result is the SDK's error text (the
// meaningful "why", e.g. "No conversation found with session ID: …"); subtype
// is its classification ("error_during_execution", …). An error result with no
// text still needs a non-empty stand-in — the frontend gates its failure detail
// on a non-empty summary, so returning "" would reproduce the very
// failed-with-no-reason gap this exists to close.
func errorResultSummary(result, subtype string) string {
	if s := strings.TrimSpace(result); s != "" {
		return s
	}
	if subtype != "" {
		return "agent runtime error: " + subtype
	}
	return "agent runtime reported an error result with no detail"
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
			// non-fatal (the conversation proceeds without a trailer) and debug-logged
			// so an operator can diagnose a missing/wrong trailer without it surfacing
			// as a conversation error.
			if host, herr := s.ghResolver.BaseURLFor(ctx, orgID); herr != nil {
				delegateLog.Debug("resolve org github base for co-author lookup failed; manual conversation will omit the trailer",
					"org", orgID, "error", herr)
			} else if login, err := stores.Users.GetGitHubLoginSystem(ctx, creatorUserID, host); err != nil {
				delegateLog.Debug("resolve co-author github login failed; manual conversation will omit the trailer",
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
// creatorUserID carries the user identity for manual conversations; it's the
// synthetic-claims subject the goroutine's write batches run under so
// RLS policies on the writes pass under tf_app. Empty for event-
// triggered conversations (those write through the admin-pool `...System`
// methods, no JWT-claims context).
// priorSessionID is the conversation row's existing session_id at claim time
// — empty for a first claim, non-empty when the dispatcher re-claims a
// conversation stranded mid-flight by a crash. When present and the session
// transcript survived alongside the warm worktree, the agent resumes that
// session instead of starting fresh, so a restart continues the conversation
// rather than re-running it from scratch.
//
// Returns fenced: true when the DB refused this engagement's writes because
// its claim was released out from under it. Nothing was recorded and nothing
// may be — the caller must stop too, rather than reacting to a terminal that
// belongs to whoever owns the conversation now.
func (s *Spawner) runAgent(ctx context.Context, conversationID string, task domain.Task, mission string, cfg runConfig, startTime time.Time, model string, triggerType string, creatorUserID string, priorSessionID string) (fenced bool) {
	orgID := cfg.orgID

	// The config is built, so whatever this engagement was going to write about
	// where it runs and on what has been written. Hold the row to it now, while
	// the claim path that wrote it is still the thing to fix — see
	// resume_contract.go.
	s.assertResumeCoordinates(ctx, orgID, conversationID, resumeCheckClaim)

	// This engagement's questions die with it: once it lets go, any prompt
	// still open was asked by a process that no longer exists. Best-effort
	// tidy for audit reads only — ListPending derives the same answer from the
	// claim, so a crash that never reaches this defer costs nothing.
	defer s.ExpirePermissionsForClaim(orgID, cfg.claimID)

	// parked is set true when this run ends dormant rather than terminating:
	// a park flips it to `open` (runAgent, below). The per-run cleanup
	// defers below read it to KEEP the worktree and session JSONL on disk as the
	// warm resume cache — mirroring the isBlueprintStep skip. Captured by
	// reference by the deferred closures, so they observe its final value at
	// return. A completed run never parks anymore: a queued artifact is
	// a sidecar, not a reason to hold the run open.
	var parked bool

	// fail records this run's infra-failure terminal and folds a fence trip
	// into the same disposition every other fenced exit takes: nothing
	// written, and the workspace left on disk for whoever owns the
	// conversation now (see the mid-stream trip below for why deleting it is
	// the worse mistake). Returns what runAgent returns.
	fail := func(msg string, kind domain.ConversationFailureKind) bool {
		if !s.failConversation(orgID, conversationID, task.ID, cfg.claimID, triggerType, creatorUserID, msg, kind) {
			return false
		}
		parked = true
		return true
	}

	// cancelled parks this run and folds a fence trip the same way. This is
	// the path a partition self-fence trip drives every live run down, so a
	// late self-fence lands here first. parked is set unconditionally, not
	// just on a fence trip: a cancel is a park now, so the worktree stays as
	// the warm resume cache exactly like an idle hibernation's does.
	//
	// sessionID rides in from the caller because it is only known once the
	// agent has actually started — the pre-launch cancel below passes "" and
	// snapshots a workspace with no transcript to carry.
	cancelled := func(sessionID string) bool {
		fenced := s.parkConversationOpen(ctx, liveParkContext{
			orgID:          orgID,
			conversationID: conversationID,
			taskID:         task.ID,
			namespace:      memoryNamespace(cfg.blueprintRunID),
			claudeCwd:      cfg.wtPath,
			triggerType:    triggerType,
			creatorUserID:  creatorUserID,
			claimID:        cfg.claimID,
			reason:         db.ParkStopped(domain.ParkReasonUserCancelled, ""),
			runtime:        domain.ConversationRuntimeSDK,
		}, sessionID)
		parked = true
		return fenced
	}

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
			rmErr := worktree.RemoveAt(cfg.wtPath, conversationID)
			if rmErr != nil {
				delegateLog.Warn("worktree remove failed; skipping per-PR config cleanup", "conversation", conversationID, "error", rmErr)
			}
			// Drop the eager worktree's conversation_worktrees row (recorded at setup with
			// ref=pr-<N> so the least-privilege gates could authorize the task
			// repo) — regardless of the RemoveAt outcome, since the row is
			// run-scoped metadata, not the worktree, and a lingering dir doesn't
			// need its ledger entry. Past the parked/blueprint early-returns
			// above, so a resume or chain step keeps the row; only a real
			// terminal teardown removes it. Best-effort: a leaked row is harmless
			// (run-scoped, never collides a future run).
			if s.conversationWorktrees != nil && cfg.owner != "" && cfg.repo != "" && cfg.prNumber > 0 {
				if delErr := s.conversationWorktrees.DeleteByRepoRefSystem(context.WithoutCancel(ctx), orgID, conversationID, cfg.owner+"/"+cfg.repo, worktree.PRRefSlug(cfg.prNumber)); delErr != nil {
					delegateLog.Warn("delete eager worktree conversation_worktrees row failed", "conversation", conversationID, "repo", cfg.owner+"/"+cfg.repo, "error", delErr)
				}
			}
			// Per-PR config cleanup only when the worktree is actually gone (see
			// above). Pass the creating run id (the worktree-dir basename, which
			// CreateForPR set to conversationID) so CleanupPRConfig reclaims THIS run's
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
		// under cfg.runRoot via `workspace add`. Iterate conversation_worktrees,
		// nuke each, then remove the run-root parent.
		defer func() {
			if cfg.isBlueprintStep {
				return
			}
			if parked {
				return
			}
			rows, err := s.conversationWorktrees.ListSystem(context.WithoutCancel(ctx), orgID, conversationID)
			if err != nil {
				delegateLog.Warn("list conversation_worktrees for cleanup failed", "conversation", conversationID, "error", err)
			} else {
				// Use a detached context so cleanup is not skipped if the
				// agent ctx has already been canceled.
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				for _, w := range rows {
					if rmErr := worktree.RemoveAt(w.Path, conversationID); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
						delegateLog.Warn("remove worktree failed", "conversation", conversationID, "path", w.Path, "error", rmErr)
						// Fall through to drop the DB row anyway — it's ephemeral
						// run-coordination state, and a lingering on-disk dir is
						// reclaimed by the startup sweep regardless. Mirrors the
						// blueprint teardown loop (runBlueprintWorktreeCleanup).
					} else {
						// Inline per-PR config reclaim (Decision D): a finishing
						// workspace-add'd PR run reclaims its own per-run branch +
						// push remote here, so the bootstrap sweep stays a pure crash
						// backstop. Gated on the worktree being gone — `git branch -D`
						// is refused while a checkout survives. w.ConversationID == conversationID
						// (created the worktree), so this targets this run's branch,
						// never a concurrent run's.
						reclaimWorkspaceAddPRConfig(w)
					}
					if delErr := s.conversationWorktrees.DeleteByPathSystem(cleanupCtx, orgID, conversationID, w.Path); delErr != nil {
						delegateLog.Warn("delete conversation_worktrees row failed", "conversation", conversationID, "path", w.Path, "error", delErr)
					}
				}
			}
			worktree.RemoveRunRoot(conversationID)
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

	// The namespace groups this run with its blueprint siblings — the run tree,
	// the workspace snapshot, and which prior memories count as this run's own
	// handoff rather than history. Exported below so per-run scratch paths the
	// prompts name (the review passes' shared drop point) resolve deterministically.
	namespace := memoryNamespace(cfg.blueprintRunID)

	// Whether this tree is still ours decides what MAY be written in it, not what
	// the agent gets. Once a launch has handed the tree to the sandbox uid — a warm
	// blueprint step N>0, or a warm resume — every create and every O_TRUNC rewrite
	// under it is EACCES for the capability-less orchestrator, so anything that
	// must reach a warm step travels as a read-only mount off a directory the
	// orchestrator owns outright instead.
	handedOff := sandbox.RunTreeHandedOff(claudeCwd)

	// What the REPO tracks under the scratch dir, for the writes that do land in
	// the tree. The dir is git-excluded by writeLocalExcludes
	// (managedExcludePatterns in internal/worktree/worktree.go), but an exclude is
	// powerless over an already-tracked path — and for a GitHub PR run this tree
	// IS the repo checkout, so an infrastructure write landing on one would ride
	// the agent's next commit into the PR.
	var owned repoFiles
	if !handedOff {
		// A tree an older binary built holds its scratch under the previous name;
		// take it over before anything reads or writes there, so files an earlier
		// step of this same workflow left behind are where this binary's prompts
		// say they are.
		worktree.AdoptLegacyScratchDir(ctx, claudeCwd)
		owned = scanRepoFiles(ctx, claudeCwd)

		// The symlink that stands in for the mounted memory tree, planted while
		// the tree is still writable — once, up front, so no later step needs a
		// write here at all. No-op in local mode, where the directory is real.
		if err := worktree.EnsureSandboxMemoryLink(ctx, claudeCwd); err != nil {
			delegateLog.Warn("plant sandbox memory symlink failed; this conversation reads no prior memory", "conversation", conversationID, "cwd", claudeCwd, "error", err)
		}
	}

	// Prior memory for THIS launch: this workflow run's earlier steps under
	// this-run/, prior separate runs under history/. Rendered on every launch,
	// warm tree or cold — only the target moves, into a staging dir the launch
	// mounts read-only when the agent is jailed. This is the blueprint handoff, so
	// a step that cannot get it starts from nothing.
	//
	// It is a DB read and a tree copy that can run large, and the last thing
	// between a rehydrated workspace and the agent starting — so one span
	// covers the on-disk context the agent will read.
	stagingCtx, stagingSpan := tracer.Start(ctx, "engagement.stage_context")
	memoryDir, memoryOwned := entityMemoryTarget(&cfg, conversationID, claudeCwd, owned)
	materializePriorMemories(s.taskMemory, orgID, cfg.teamID, memoryDir, task.EntityID, cfg.blueprintRunID, memoryOwned)

	// The team knowledge base, copied into ./_tfac/knowledge/: the task team's
	// own two roots plus every other team's published one, resolved from
	// cfg.teamID alone. Still an in-tree write, so a warm step keeps whatever
	// the first step's copy left there — a knowledge base edited mid-blueprint
	// does not reach later steps, and the manifest describes what THIS launch
	// staged rather than what the tree happens to hold.
	knowledge := ""
	if handedOff {
		delegateLog.Debug("run tree already handed to the sandbox identity; team knowledge not refreshed for this step", "conversation", conversationID, "cwd", claudeCwd)
	} else {
		knowledge = s.stageTeamKnowledge(stagingCtx, orgID, cfg.teamID, claudeCwd, owned)
	}

	// A blueprint's steps share one tree and all write the same memory filename,
	// so a step starting a fresh conversation must not be credited with whatever
	// its predecessor left at that path. Delete it where that is possible; where it
	// is not, digest it, and let termination refuse to ingest content the agent
	// never wrote. A run continuing a session (a mid-flight re-claim) keeps what it
	// already wrote and needs neither.
	var priorMemory *memoryFingerprint
	if priorSessionID == "" {
		if !handedOff {
			clearAgentMemoryFile(claudeCwd, owned)
		}
		// After the clear, so a file it removed leaves no fingerprint — and a file
		// it could not remove (repo-owned, or a failure) still cannot be misread as
		// this run's work.
		priorMemory = fingerprintAgentMemoryFile(claudeCwd)
	}

	stagingSpan.End()

	selfBin, err := os.Executable()
	if err != nil {
		return fail("failed to resolve own binary path: "+err.Error(), domain.ConversationFailureUnclassified)
	}

	// Load the primary event's metadata so the task context can carry the
	// event's own fields (workflow run id, head sha, check name, ...). A DB
	// failure here is non-fatal: the block just renders without them. FKs
	// guarantee the event exists, so a real miss would be a DB-level problem we
	// want to log and continue through rather than aborting the run.
	metadataJSON, err := s.events.GetMetadataSystem(context.WithoutCancel(ctx), orgID, task.PrimaryEventID)
	if err != nil {
		delegateLog.Warn("load event metadata for task failed; the task context will carry no event fields", "task", task.ID, "event", task.PrimaryEventID, "error", err)
		metadataJSON = ""
	}

	// Resolve the paths the agent will actually observe, which differ from the
	// host paths under the sandbox: the run-root is bind-mounted at "/work" and
	// the TF binary at sandboxTFBinary. The prompt states both concretely, which
	// is what makes the agent's file tools (no shell expansion) and its
	// `triagefactory exec ...` invocations resolve regardless of sandbox mode.
	// selfBin itself stays the host path below for BuildAllowedToolsWithExtras —
	// agentproc.Run rewrites the allowlist's binary path for the sandbox on its
	// own (rewriteAllowedToolsForSandbox).
	agentRunRoot := agentproc.AgentVisibleRoot(cfg.runRoot)
	agentBin := agentproc.AgentVisibleBinary(selfBin)
	// The team's branch-naming convention, ticket-id-resolved, surfaced to the
	// agent as run-context guidance (TFAC-498). Not enforced — the push gate
	// authorizes whatever branch the worktree lands on.
	branchTemplate := s.resolveBranchTemplate(context.WithoutCancel(ctx), task)
	runURL := s.runURLFor(orgID, conversationID)
	prompt := buildPrompt(task, metadataJSON, cfg.prSkeleton, mission, cfg.scope, cfg.toolsRef, agentBin, agentRunRoot, branchTemplate, runURL, knowledge)

	// The stop is read before the phase write, so a run stopped during
	// bring-up parks here without ever asking the fence — the refusal below is
	// then reserved for a claim that went away with no cancel behind it.
	if ctx.Err() != nil {
		return cancelled("")
	}
	if s.updatePhase(ctx, orgID, conversationID, cfg.claimID, domain.ClaimPhaseAgentStarting) {
		// Fenced out before the runtime came up: nothing to write, no process
		// to kill. The workspace stays for whoever owns the conversation now.
		parked = true
		return true
	}

	extraEnv := []string{
		"TRIAGE_FACTORY_CONVERSATION_ID=" + conversationID,
		"TRIAGE_FACTORY_CONVERSATION_ROOT=" + cfg.runRoot, // Set for both sources so the completion-gate retry message can reference the absolute _tfac/memory.md path that resolves regardless of which worktree the agent has cd'd into.
		// The workflow run this step belongs to. Non-absolute, so it passes
		// through translateEnvForSandbox unchanged. Prompts that share a drop
		// point across the steps of one run (the parallel review passes) name
		// their directory through this.
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

	s.updatePhase(ctx, orgID, conversationID, cfg.claimID, "")

	// StartAgentHost is invoked from inside agentproc.Run's sandbox branch —
	// which only a multi-mode dispatch reaches, and every multi dispatch
	// stood up the per-run credential sidecar first. The sidecar HOSTS the
	// exec-verb socket server (the relocation) and already created the
	// socket during bring-up; the orchestrator only supplies the bind mount,
	// keeping the hostile-input parser (with its all-orgs stores) out of its
	// own address space entirely. Local (no sandbox) never invokes the
	// closure, and the former in-process socket server over live stores is
	// gone with the fused single-process shape.
	var startAgentHost func() (sandbox.Mount, io.Closer, error)
	if cfg.sidecar != nil {
		startAgentHost = func() (sandbox.Mount, io.Closer, error) {
			return agenthost.SocketMountFor(conversationID), noopCloser{}, nil
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
		delegateLog.Info("conversation re-claimed mid-flight; resuming session", "conversation", conversationID, "session", priorSessionID)
	}

	// teamID nil-derefs to "" (resolveAbsentAutoDeny then falls back to
	// defaults); task.TeamID is set for any task that reaches a run.
	teamID := ""
	if task.TeamID != nil {
		teamID = *task.TeamID
	}

	// Local mode's half of the real-gh channel: a loopback injector holding the
	// org's live credential, fronted by the TF-owned pinned binary. Multi gets
	// the same channel from the sidecar (cfg.sidecar below), so exactly one
	// of the two is ever non-nil. A host that can't provision it degrades to the
	// scoped exec verbs rather than failing the run.
	localGH, localGHCloser := s.startLocalGHChannel(ctx, orgID, conversationID, cfg.owner, agenthost.ConversationInfo{
		OrgID:            orgID,
		UserID:           creatorUserID,
		ConversationID:   conversationID,
		TeamID:           teamID,
		IsEventTriggered: triggerType == domain.TriggerTypeEvent,
	}, cfg.localGit.handler())
	defer func() { _ = localGHCloser.Close() }()
	ghChannel := cfg.sidecar.ghChannel(conversationID)
	if ghChannel == nil {
		ghChannel = localGH
	}
	if cfg.localGit != nil {
		extraEnv = append(extraEnv, githooks.PushCaptureEnvVar+"="+githooks.PushCaptureProxy)
		// The stand-down above is only correct while every push really does
		// transit the proxy, so the SSH transport joins it here rather than
		// reaching the org host on its own.
		extraEnv = append(extraEnv, cfg.localGit.sshBridgeEnv()...)
	}

	// Local-mode courtesy isolation: the mount-namespace plan plus the
	// agenthost daemon its exec verbs dial. nil (and free) in multi mode and
	// whenever the operator opted out; a failure fails the run rather than
	// dropping the agent onto the operator's filesystem unannounced.
	localSbx, err := s.startLocalSandbox(conversationID, namespace, ghChannel, agenthost.ConversationInfo{
		OrgID:            orgID,
		UserID:           creatorUserID,
		ConversationID:   conversationID,
		TeamID:           teamID,
		IsEventTriggered: triggerType == domain.TriggerTypeEvent,
	})
	if err != nil {
		return fail(err.Error(), domain.ConversationFailureUnclassified)
	}
	defer func() { _ = localSbx.Close() }()

	delegateLog.Info("claude starting for conversation", "conversation", conversationID, "cwd", claudeCwd)
	baseOpts := agentproc.RunOptions{
		Cwd:            claudeCwd,
		Model:          model,
		PermissionMode: s.resolveSDKPermissionMode(ctx, teamID),
		SessionID:      resumeSession,
		Message:        prompt,
		// The gh subcommands are granted only when the channel is actually
		// live: without one, `gh` on a local host would resolve to the user's
		// own installation under the user's own auth, so the allowlist must
		// not name it at all.
		AllowedTools: agentproc.BuildAllowedToolsFor(agentproc.AllowedToolsOptions{
			SelfBin: selfBin,
			Extras:  cfg.extraAllowedTools,
			GH:      ghChannel != nil,
		}),
		ExtraEnv:        extraEnv,
		TraceID:         conversationID,
		MemoryNamespace: namespace,
		SystemPrompt:    cfg.appendSysPrompt,
		OrgID:           orgID,
		Secrets:         s.getRunSecrets(),
		LLMResolver:     s.llmResolverForConversation(orgID, conversationID),
		// Measured sandbox cost lands on the engagement that paid for it, at
		// teardown. Empty claim id (a path with no claimed run in scope)
		// records nothing.
		ClaimID:              cfg.claimID,
		RecordSandboxActuals: s.recordSandboxActuals,
		// Multi mode: hand agentproc the prebuilt run network and the
		// sidecar's proxy env so it launches into them and holds no
		// credential — the sidecar owns the LLM/git/egress proxies + the
		// live SigV4 refresh. nil in local (no sandbox).
		PrebuiltNetwork:  cfg.sidecar.runNetwork(),
		PrebuiltProxyEnv: cfg.sidecar.jailEnv(),
		StartAgentHost:   startAgentHost,
		// Real-gh channel: the sidecar's injector + mounted binary in multi, the
		// loopback injector + fetched binary in local. nil when neither could be
		// provisioned, which keeps the run on the scoped exec verbs.
		GHChannel: ghChannel,
		// This step's staged skill, bind-mounted read-only. Empty for every
		// non-blueprint run and in local mode (where the skill lives in the
		// worktree instead).
		SkillsSourcePath: cfg.skillsSourcePath,
		// This launch's materialized prior-memory tree, bind-mounted read-only at
		// the path the run tree's `_tfac/entity-memory` symlink points to. Empty in
		// local mode, where that path is the real directory.
		MemorySourcePath: cfg.memorySourcePath,
		// Org commit identity (TFAC-452): empty when none resolved → ambient git
		// config inherited (today's behavior).
		GitUserName:    commitIdentity.Name,
		GitUserEmail:   commitIdentity.Email,
		GitConfigPairs: cfg.localGit.configPairs(ghChannel),
		// Local mount namespace. nil in multi mode (the gVisor jail is the
		// isolation there) and whenever the operator opted out.
		LocalSandbox: localSbx.runSpec(),
	}
	sink := newConversationSink(s, orgID, conversationID, cfg.claimID, triggerType, creatorUserID)

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
		perms = s.AutoApprovePermissionHandler(conversationID)
	} else {
		perms = s.BrowserPermissionHandler(orgID, conversationID, cfg.claimID, s.resolveAbsentAutoDeny(ctx, teamID))
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
				orgID:          orgID,
				conversationID: conversationID,
				taskID:         task.ID,
				namespace:      namespace,
				claudeCwd:      claudeCwd,
				triggerType:    triggerType,
				creatorUserID:  creatorUserID,
				claimID:        cfg.claimID,
				runtime:        domain.ConversationRuntimeSDK,
			},
			opts:        baseOpts,
			perms:       perms,
			sink:        sink,
			idleTimeout: s.idleTimeout(),
		})
	} else {
		out = s.runOneShot(ctx, baseOpts, sink)
	}

	// Fenced out mid-stream: the claim this engagement holds was released and
	// a successor is driving the conversation. Every branch below records
	// something about the run — a terminal, a cancellation, a failure — and
	// none of it is this engagement's to record anymore. The agent is already
	// dead (the sink killed it, or the driver closed it on a refused park), so
	// the only thing left is to leave.
	//
	// Two ways to learn it, and either is conclusive: a refused transcript
	// write (the sink) or a refused park (the driver). They are separate
	// signals because they come from different writes — a run whose turn ended
	// quietly may never attempt another insert.
	//
	// parked=true is what keeps the workspace on disk. Ownership is gone, so
	// whether the successor is rehydrating into this very worktree is no
	// longer knowable from here; a leaked directory is reclaimed by the next
	// startup sweep, a deleted one that turned out to be in use is not.
	if sink.fenceTripped() || out.fenced {
		parked = true
		return true
	}

	// The stream is over and the row is about to come to rest — parked, or
	// terminal on a step someone may still pick up. Whatever is on it now is
	// what a follow-up finds, so this is where the session id is expected: it
	// does not exist before the stream's first system/init, which is why the
	// claim-time check above leaves it alone. Detached ctx — a shutdown that
	// cancelled the run must not skip the diagnostic about the row it left.
	//
	// An engagement that produced neither a park nor a result never reached a
	// session at all; its failure is already recorded, and naming a coordinate
	// it was never going to have would just be noise on top.
	if out.hibernated || out.result != nil {
		s.assertResumeCoordinates(context.WithoutCancel(ctx), orgID, conversationID, resumeCheckRest)
	}

	// The driver parked the run itself (status `open`, snapshot written) — a
	// paused turn, or the idle backstop — a dormant disposition, so keep the
	// warm worktree as the fast resume path.
	if out.hibernated {
		parked = true
		return
	}

	if out.result != nil {
		parked, fenced = s.processCompletion(ctx, orgID, conversationID, cfg.blueprintRunID, cfg.claimID, task, out.result, claudeCwd, priorMemory, out.sessionID, triggerType, creatorUserID)
		return fenced
	}

	if out.err != nil {
		if ctx.Err() != nil {
			return cancelled(out.sessionID)
		}
		return fail(fmt.Sprintf("%v\nstderr: %s", out.err, out.stderr), classifyFailureKind(out.err))
	}

	return fail("agent runtime exited cleanly without producing a result event", domain.ConversationFailureNoResult)
}

// processCompletion is the single disposition authority for a result, whatever
// backend produced it (the live driver, the one-shot sandbox fallback, or a
// resume). It classifies the turn-end and acts uniformly:
//
//   - valid conclusion → record the outcome (finalize / let the orchestrator
//     advance or close). A queued review/PR is an async sidecar artifact and
//     never parks the run; the step completes with its real outcome.
//   - no conclusion (prose / nothing) → the run is open, not a termination:
//     park it open (snapshot + flip + keep the warm worktree) and return.
//     Every backend hands its no-conclusion turn here with the process
//     already closed — the live driver included, which is what lets the flip
//     land after the process is gone rather than under it.
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
// priorMemory digests the memory file this invocation INHERITED at the fixed
// write path, so content the agent never wrote is not ingested as its work. nil
// on a resume (whose own file is its work) and wherever there was nothing to
// inherit, which is the ordinary case.
//
// Returns parked: true when the run ended dormant (open) rather than terminal,
// so runAgent's cleanup defers keep the worktree + session JSONL on disk as the
// warm resume cache. A terminal completion (including one that produced a draft
// PR / pending review) returns false — the artifact is a resolvable sidecar, not
// a reason to park.
//
// claimID names the engagement that produced this result, so its terminal
// write goes through the claim fence. Empty on paths with no claimed run in
// scope, which keeps the unfenced write.
//
// Returns fenced: true when that terminal was refused because the claim is
// released. Nothing was recorded, and the caller must not react to the run's
// state either — a successor owns its disposition. parked comes back true
// alongside it so the workspace survives (see runAgent).
func (s *Spawner) processCompletion(
	ctx context.Context,
	orgID, conversationID, blueprintRunID, claimID string,
	task domain.Task,
	completion *agentproc.Result,
	claudeCwd string,
	priorMemory *memoryFingerprint,
	sessionID, triggerType, creatorUserID string,
) (parked, fenced bool) {
	// The engagement's terminal, as a punctual span linked back to the setup
	// that started it. This is where a run's own accounting already exists —
	// cost, wall time, turn count — so it is the one place those figures can
	// ride into the trace without being recomputed; the span's own duration
	// is the bookkeeping, not the agent's work, which is exactly why the
	// agent's numbers have to be attributes rather than inferred.
	ctx, span := s.startPunctual(ctx, conversationID, "engagement.complete",
		telemetry.OrgID(orgID),
		telemetry.TaskID(task.ID),
		telemetry.AgentCostUSD(completion.CostUSD),
		telemetry.AgentDuration(int64(completion.DurationMs)),
		telemetry.Count(completion.NumTurns))
	// Defaults to the park below, which is the one exit that returns before
	// the terminal classification runs.
	terminal := "open"
	defer func() {
		span.SetAttributes(telemetry.Outcome(terminal))
		span.End()
	}()

	// The namespace keys this run's workspace (tree + snapshot) among its
	// blueprint siblings. Derived from the caller-supplied blueprint_run_id —
	// no DB fetch, so it can't silently fall back to the wrong key on a
	// transient read error.
	namespace := memoryNamespace(blueprintRunID)

	// Classify the turn-end up front. A no-conclusion turn (prose / nothing) is
	// NOT a termination — the run is open. Park it open (snapshot for the
	// cold-resume backstop, flip the status, keep the warm worktree) and skip the
	// terminal bookkeeping below. An IsError result is always a termination, so
	// it never takes this branch however envelope-shaped its text.
	//
	// A refused park is reported as fenced, not swallowed: the row belongs to
	// a successor, so nothing here — not the terminal, not the blueprint
	// advance the caller would run on a plain park — is this engagement's to
	// decide. parked stays true either way, because that is what keeps the
	// workspace on disk for whoever holds the conversation now.
	class, parsed := classifyAgentResult(completion.Result)
	if !completion.IsError && class == turnNone {
		fencedOut := s.parkConversationOpen(ctx, liveParkContext{
			orgID:          orgID,
			conversationID: conversationID,
			taskID:         task.ID,
			namespace:      namespace,
			claudeCwd:      claudeCwd,
			triggerType:    triggerType,
			creatorUserID:  creatorUserID,
			claimID:        claimID,
			reason:         db.ParkIdle(),
			runtime:        domain.ConversationRuntimeSDK,
		}, sessionID)
		return true, fencedOut
	}

	// Unconditional upsert of the conversation_memory row at termination: row presence
	// === "the run terminated", agent_content NULL === "the agent didn't write a
	// usable memory file" (UpsertAgentMemory normalizes empty/whitespace input
	// to NULL on the way in). blueprint_run_id is denormalized onto the row so
	// the next run's materializer can tell this workflow run's own steps from
	// history.
	agentContent, fileState := readConversationMemory(claudeCwd, priorMemory)
	if _, err := s.taskMemory.UpsertAgentMemorySystem(context.WithoutCancel(ctx), orgID, conversationID, task.EntityID, blueprintRunID, agentContent); err != nil {
		delegateLog.Warn("upsert memory for conversation failed", "conversation", conversationID, "error", err)
	}
	switch fileState {
	case memoryFileMissing:
		delegateLog.Debug("memory file missing at termination (agent_content NULL)", "conversation", conversationID)
	case memoryFileEmpty:
		delegateLog.Debug("memory file present but empty at termination (agent_content NULL)", "conversation", conversationID)
	case memoryFileReadErr:
		delegateLog.Debug("memory file unreadable at termination (agent_content NULL)", "conversation", conversationID)
	case memoryFileStale:
		delegateLog.Debug("memory file holds exactly the content this conversation inherited; it belongs to the previous step (agent_content NULL)", "conversation", conversationID)
	}

	// Attach the conversation's memory to every entity it materially engaged —
	// the primary (task) entity plus everything it produced — so the narrative
	// is reachable from all of them, not just the denormalized primary on
	// conversation_memory.entity_id. Cancellation-detached for the same reason
	// the upsert above is: a cancelled turn still owns its terminal
	// bookkeeping.
	s.attachConversationMemoryEntities(context.WithoutCancel(ctx), orgID, conversationID, task.EntityID)

	// Every run is a step of a blueprint_run now (a single prompt is a 1-step
	// blueprint), so this helper never owns task disposition: it persists
	// outcome/status only and leaves advancement + task close to the orchestrator
	// (reactToStepTerminal / terminateBlueprint). blueprintRunID is always non-empty here.

	resultSummary := ""
	status := "completed"
	var outcome, outcomeReason string
	failureKind := domain.ConversationFailureUnclassified
	switch {
	case completion.IsError:
		// Process crash / runtime error — an always-knowable terminal. Carry the
		// runtime's own error text as the summary: without it the run persists as
		// a bare agent_error and the only copy of "why" is the executor's stderr,
		// so the UI shows a failure with no reason.
		status = "failed"
		failureKind = domain.ConversationFailureAgentError
		resultSummary = errorResultSummary(completion.Result, completion.Subtype)
	case class == turnValid:
		resultSummary = parsed.Summary
		outcome = parsed.Outcome
		if domain.ConversationOutcome(parsed.Outcome) == domain.ConversationOutcomeAbort {
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
		failureKind = domain.ConversationFailureNoResult
		resultSummary = "agent did not return a valid completion envelope"
	}

	// The classification is settled; carry it onto the terminal span. A
	// failed run additionally gets the failure kind, which is the closed
	// domain.ConversationFailure* vocabulary rather than the free-text summary.
	terminal = status
	if status == "failed" {
		terminal = status + "_" + string(failureKind)
	} else if outcome != "" {
		terminal = status + "_" + outcome
	}

	// Detached context: the run's ctx may have been cancelled (user
	// cancel mid-stream) but the terminal write still needs to record.
	// WithoutCancel rather than Background: identical detachment (values
	// kept, cancellation dropped) while the writes stay inside this
	// engagement's trace instead of orphaning.
	// Manual runs wrap in synthetic claims so the UPDATE passes RLS
	// under tf_app with the creator's identity; event-triggered runs
	// bypass via the admin pool.
	bgCtx := context.WithoutCancel(ctx)

	// A queued draft PR / pending review NO LONGER parks the run. The
	// artifact was already recorded by the exec choke point and is an async
	// sidecar: a human resolves it independently and it never blocks step
	// progression. So there's no external-action coercion and no approval park
	// here — the step completes with its real outcome (continue advances,
	// finish terminates) per blueprintDecisionForStepConversation, and the approval state is
	// derived downstream from the unresolved-artifact set (has_unresolved_artifacts).

	// Every non-failed terminal snapshots its workspace — while the worktree and
	// session transcript are still on disk — then lets the cleanup defers tear
	// the worktree down (parked stays false: keeping a concluded run's worktree
	// warm is not acceptable, so cold rehydrate from this blob is the resume
	// path). `completed` is the whole non-failed terminal set, whatever the
	// outcome: an abort is picked back up by a human, and a finish is the case a
	// follow-up on concluded work needs — a workspace that only exists for the
	// runs that went badly is a workspace nobody can follow up on. A failed run
	// is excluded: the infrastructure under it died, so there is nothing coherent
	// to rehydrate, and failConversation drops whatever blob it had.
	//
	// Every step of a blueprint writes to the one key the blueprint shares, so a
	// multi-step run overwrites its own blob per step and the last writer — the
	// step a resume would land on — wins. terminateBlueprint retains it for every
	// terminal but `failed`; the TTL sweep is what collects it.
	//
	// BEFORE the terminal write, and that ordering is the contract: the terminal
	// is also the claim release, so the instant it commits a follow-up can be
	// accepted and a claim minted — for a workspace that, the other way round, is
	// still being written.
	if status == "completed" {
		if err := s.snapshotWorkspace(ctx, orgID, conversationID, namespace, claimID, claudeCwd, sessionID, domain.ConversationRuntimeSDK); err != nil {
			delegateLog.Warn("snapshot workspace for completed conversation failed", "conversation", conversationID, "outcome", outcome, "error", err)
		}
	}

	var completeErr error
	var completedRow *domain.Conversation
	switch {
	case claimID != "":
		completedRow, completeErr = s.conversations.CompleteForClaimSystem(bgCtx, orgID, conversationID, claimID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, resultSummary, outcome, outcomeReason, string(failureKind))
	case triggerType == "manual":
		completeErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			r, err := ts.Conversations.Complete(bgCtx, orgID, conversationID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, resultSummary, outcome, outcomeReason, string(failureKind))
			completedRow = r
			return err
		})
	default:
		completedRow, completeErr = s.conversations.CompleteSystem(bgCtx, orgID, conversationID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, resultSummary, outcome, outcomeReason, string(failureKind))
	}
	if errors.Is(completeErr, db.ErrClaimReleased) {
		// A successor owns the conversation, so this result is not the run's
		// disposition — it is the output of an engagement that lost its
		// claim. Recording it would overwrite live work with a stale
		// terminal, and every step below (breaker, board, toast, broadcast)
		// reports on that same overwritten row. Stop here, and park so the
		// successor's workspace is left alone. The snapshot above already
		// landed, harmlessly: the successor holds the claim, so nothing can
		// resume from the blob before its own conclusion overwrites it.
		delegateLog.Error("claim fence refused the completion terminal — a successor owns this conversation; recording nothing",
			"conversation", conversationID, "claim_id", claimID, "org_id", orgID, "error", completeErr)
		return true, true
	}
	if completeErr != nil {
		delegateLog.Warn("record completion for conversation failed", "conversation", conversationID, "error", completeErr)
	}

	s.updateBreakerCounter(task.ID, triggerType, status)

	// broadcastStatus prefers the row the write actually persisted over the
	// caller's own status variable — the same value here (Complete writes
	// status verbatim, no COALESCE), but sourced from what landed rather than
	// what was asked for, per the returned-row standard.
	broadcastStatus := status
	if completedRow != nil {
		broadcastStatus = completedRow.Status
	}

	// Task disposition (close on finish, leave-open on abort) is the
	// orchestrator's job now, not the step's: reactToStepTerminal reads this run's
	// terminal conversations.outcome and routes through terminateBlueprint,
	// which owns the terminal column. A step completion must never close the
	// task here — the next step may be about to run.
	if status == "failed" {
		s.broadcastConversationFailed(orgID, conversationID, failureKind)
	} else {
		s.broadcastConversationUpdate(orgID, conversationID, broadcastStatus)
	}
	// Recompute the aggregate board column. A completed step that left an
	// unresolved artifact (draft PR / ready review) lands the task in_review (the
	// derived approval column); otherwise it stays in_progress until the
	// orchestrator advances (next step) or terminates (done / leave-open).
	s.recomputeTaskBoardColumn(orgID, task.ID)

	// Toast the terminal state. Success cases auto-hide; a failure shows as an
	// error toast so the user notices even if they've clicked away from the
	// runs page.
	switch status {
	case "completed":
		toast.Success(s.wsHub, orgID, fmt.Sprintf("Run %s completed", shortConversationID(conversationID)))
	case "failed":
		toast.Error(s.wsHub, orgID, fmt.Sprintf("Run %s failed: %s", shortConversationID(conversationID), truncateToastMsg(resultSummary, 160)))
	}

	// Workspace-snapshot cleanup is owned by terminateBlueprint, keyed by
	// blueprint_run_id (the shared workspace's key) — every run is a blueprint
	// step now, so there is no standalone conversation-id-keyed snapshot to drop here. A
	// parked run keeps its snapshot for the eventual resume.
	return parked, false
}
