// User-driven lifecycle interventions on a delegated run: Takeover hands
// the running headless session over to the user's terminal, abortTakeover
// rolls back a takeover that failed mid-flight, and Release tears down a
// takeover the user is done with. All three share the path-safety
// canonicalization helper at the bottom of the file.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TakeoverResult is what Takeover returns to the HTTP handler.
type TakeoverResult struct {
	TakeoverPath string `json:"takeover_path"`
	SessionID    string `json:"session_id"`
}

// Sentinel errors the takeover HTTP handler uses to pick an HTTP status
// class. Anything NOT matching one of these is treated as a server-
// side problem (filesystem, git subprocess, DB) and surfaced as 5xx.
var (
	// ErrTakeoverInvalidState — the run is not in a state that can be
	// taken over (no active goroutine, no session id yet, no worktree).
	// 400 Bad Request: the client asked for something that doesn't make
	// sense given the run's state, but nothing's broken.
	ErrTakeoverInvalidState = errors.New("takeover: run not in a takeoverable state")

	// ErrTakeoverInProgress — another takeover for the same run is
	// already running. 409 Conflict: a previous request owns the slot.
	ErrTakeoverInProgress = errors.New("takeover: already in progress")

	// ErrTakeoverRaceLost — the run reached a terminal state on its own
	// before our takeover could finalize. 409 Conflict: the resource
	// changed under us, the client should re-fetch.
	ErrTakeoverRaceLost = errors.New("takeover: run finished before takeover could finalize")

	// ErrReleaseNothingHeld — Release was called against a run that
	// isn't a held takeover (wrong status, or already released so
	// worktree_path is empty). 409 Conflict from the HTTP handler.
	ErrReleaseNothingHeld = errors.New("release: run is not a held takeover")
)

// Takeover hands a running headless session over to the user for
// interactive resume.
//
// Order of operations is load-bearing:
//
//  1. Atomically validate the run + flip the takenOver flag while still
//     holding the spawner lock. Setting the flag BEFORE anything else
//     ensures every cleanup path in runAgent (worktree.Remove,
//     RemoveRunCwd, RemoveClaudeProjectDir, db.CompleteAgentRun, the
//     pending_approval flip, the toasts) checks wasTakenOver() and
//     short-circuits. Without that, the previous race-prone version
//     could see runAgent's defers nuke the worktree mid-copy.
//
//  2. Cancel the agent's context. The kill-watcher goroutine wakes
//     and SIGKILLs the headless process group within milliseconds, so
//     the agent stops mutating files. We DON'T wait for the runAgent
//     goroutine to finish — its defers are no-ops because of the flag,
//     and the worktree files are stable from the moment the SIGKILL
//     lands.
//
//  3. Hand over the worktree into the takeover destination via
//     worktree.CopyForTakeover (atomic `git worktree move` on the same
//     filesystem; add+overlay fallback otherwise).
//
//  4. Mark the run terminal as taken_over and broadcast the status so
//     the frontend re-renders the card.
//
// Preconditions: the run must be active (registered cancel func) and
// have produced a session_id. Without a session id `claude --resume`
// has nothing to attach to, so we refuse early.
//
// Takeover does NOT take a request context. Once we set the flag and
// SIGKILL the agent (step 2), the operation MUST complete cleanly —
// either succeed or run abortTakeover to roll back filesystem and DB
// state. Tying the work to an HTTP request's context would let a
// client disconnect (closed tab, network blip, browser navigate)
// trigger the rollback mid-takeover, irreversibly destroying the
// agent, the worktree, and the session JSONL — for an action the user
// didn't ask to abort. We use context.Background() for the commit-
// phase git operations so the work is insulated from the request
// lifecycle. Pre-commit DB lookups are fast and don't need
// cancellation either.
//
// The session JSONL under ~/.claude/projects survives by virtue of:
// (a) the gated RemoveClaudeProjectDir defer in runAgent, and
// (b) startup Cleanup honoring ListTakenOverRunIDs.
//
// userID identifies the taking-over user for audit + the task-claim
// flip. Local mode handlers pass runmode.LocalDefaultUserID; multi-
// mode handlers extract from JWT claims.
func (s *Spawner) Takeover(orgID, runID, baseDir, userID string) (*TakeoverResult, error) {
	if baseDir == "" {
		return nil, fmt.Errorf("takeover: empty destination base dir")
	}
	if userID == "" {
		return nil, fmt.Errorf("takeover: empty user id")
	}

	// Background ctx so client disconnect during the commit can't abort
	// us mid-rename. See the function-level comment.
	ctx := context.Background()

	// Preflight reads go through the app pool under the requesting
	// user's synthetic claims so RLS filters out runs/tasks the user
	// can't see. Without this, a user with a guessed runID could trip
	// the side effects below (cancel handle drain, agent SIGKILL,
	// worktree copy) before the eventual MarkTakenOver write fails
	// RLS — destroying another user's run while learning nothing
	// themselves. Two reads in one tx so the task-source gate sees
	// the same isolation boundary as the run lookup.
	var (
		run  *domain.AgentRun
		task *domain.Task
	)
	if err := s.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		r, rErr := ts.AgentRuns.Get(ctx, orgID, runID)
		if rErr != nil {
			return fmt.Errorf("load run: %w", rErr)
		}
		run = r
		if r == nil {
			return nil
		}
		t, tErr := ts.Tasks.Get(ctx, orgID, r.TaskID)
		if tErr != nil {
			return fmt.Errorf("load task for takeover gate: %w", tErr)
		}
		task = t
		return nil
	}); err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: run %s not found", ErrTakeoverInvalidState, runID)
	}
	if run.SessionID == "" {
		return nil, fmt.Errorf("%w: run %s has no session id yet — wait until the agent has started", ErrTakeoverInvalidState, runID)
	}
	if run.WorktreePath == "" {
		return nil, fmt.Errorf("%w: run %s has no worktree path; cannot take over", ErrTakeoverInvalidState, runID)
	}
	// Jira lazy runs DO populate worktree_path (with the run-root, so a
	// resume can reuse it as the resume cwd). Takeover for those
	// would require copying the full run-root tree including all
	// per-repo worktrees as subdirs, plus rewriting any absolute paths
	// the agent recorded in its session — out of scope for now. Reject
	// explicitly via a task-source check rather than papering over with
	// an empty-path heuristic.
	if task != nil && task.EntitySource == "jira" {
		return nil, fmt.Errorf("%w: run %s is a Jira lazy delegation; multi-worktree takeover is not yet supported", ErrTakeoverInvalidState, runID)
	}

	// Atomically: confirm the run is still active, confirm we haven't
	// already taken it over, flip the flag, and grab the cancel func.
	// Doing all four under the lock prevents a concurrent natural
	// completion from draining cancels[runID] between our check and
	// our set.
	s.mu.Lock()
	cancel, active := s.cancels[runID]
	already := s.takenOver[runID]
	if !active {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: run %s is no longer active — it may have just finished", ErrTakeoverInvalidState, runID)
	}
	if already {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: run %s", ErrTakeoverInProgress, runID)
	}
	s.takenOver[runID] = true
	s.mu.Unlock()

	// Capture the source cwd's symlink-resolved path BEFORE
	// CopyForTakeover removes/renames it. We need this later to find
	// the agent's session JSONL under ~/.claude/projects/<encoded-cwd>;
	// Claude Code keys session storage by cwd-encoding, so without
	// MaterializeSessionForTakeover the user's `claude --resume <id>`
	// from the takeover dir fails with "No conversation found." The
	// resolution has to happen here (path still exists) rather than
	// after the move (path is gone).
	resolvedOldCwd := worktree.ResolveClaudeProjectCwd(run.WorktreePath)

	// Stop the agent. Fire-and-forget — the kill-watcher SIGKILLs the
	// process group, the goroutine's gated defers skip cleanup, the
	// worktree stays on disk for our copy.
	if cancel != nil {
		cancel()
	}

	destPath, err := worktree.CopyForTakeover(ctx, runID, run.WorktreePath, baseDir)
	if err != nil {
		// Copy failed — the agent has been cancelled but the gated
		// defers in runAgent suppressed all the normal cleanup. Roll
		// back: clear the flag, do the cleanup ourselves, and mark the
		// row terminal so it doesn't sit in 'running' forever and so a
		// retry isn't permanently blocked by a stale takenOver entry.
		s.abortTakeover(orgID, runID, run.WorktreePath, "", userID)
		return nil, fmt.Errorf("copy worktree: %w", err)
	}

	// CopyForTakeover removes the source: the move path renames it out
	// of /tmp atomically, the overlay fallback explicitly Removes after
	// the copy. Either way no further cleanup of the original is needed.

	// Materialize the session JSONL under the takeover dir's project
	// entry so `claude --resume` finds it. Done before the DB mark so
	// that a copy failure rolls the whole takeover back rather than
	// leaving the row in 'taken_over' state pointing at a destination
	// the user can't actually resume from.
	if err := worktree.MaterializeSessionForTakeover(resolvedOldCwd, destPath, run.SessionID); err != nil {
		s.abortTakeover(orgID, runID, run.WorktreePath, destPath, userID)
		return nil, fmt.Errorf("copy session jsonl: %w", err)
	}

	// SKY-261 B+: takeover is one semantic event — run.status='taken_over'
	// AND the task's claim flips bot→user. MarkAgentRunTakenOver does
	// both in a single transaction so a partial failure can't leave
	// the system inconsistent (e.g., run reads taken_over but task
	// still shows bot claim). The userID arg engages the claim-flip
	// arm of the atomic UPDATE; the run's actor_agent_id stays
	// stamped at the bot (immutable audit). Wrapped in synthetic
	// claims so the multi-pool RLS policies see a user-attributed
	// transition (takeover is always user-initiated).
	bgCtx := context.Background()
	var ok bool
	err = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, userID, func(ts db.TxStores) error {
		o, mErr := ts.AgentRuns.MarkTakenOver(bgCtx, orgID, runID, destPath, userID)
		ok = o
		return mErr
	})
	if err != nil {
		// Transaction failed and rolled back — both run and task are
		// unchanged. Same FS cleanup as the copy-failure path.
		s.abortTakeover(orgID, runID, run.WorktreePath, destPath, userID)
		return nil, fmt.Errorf("mark taken_over: %w", err)
	}
	if !ok {
		// Race-loss: the goroutine wrote a real terminal status before
		// our flag was set. The tx rolled back, leaving everything
		// unchanged. Its natural defers ran (the gates evaluated false
		// at that point), but ours skipped because the flag was set
		// by the time they fired — abortTakeover catches them up.
		// abortTakeover's guarded UPDATE will no-op since the row is
		// already terminal, so the agent's actual outcome is preserved.
		s.abortTakeover(orgID, runID, run.WorktreePath, destPath, userID)
		return nil, fmt.Errorf("%w: run %s", ErrTakeoverRaceLost, runID)
	}

	// Broadcast on the claim axis so the Board can move the card out
	// of the bot's lane into the user's lane. The run status update
	// (broadcastRunUpdate below) is separate — that's run lifecycle,
	// not task responsibility. The two channels stay split (SKY-261 B+).
	if s.wsHub != nil {
		s.wsHub.Broadcast(websocket.Event{
			Type:  "task_claimed",
			OrgID: orgID,
			Data: map[string]any{
				"task_id":             run.TaskID,
				"claimed_by_agent_id": "",
				"claimed_by_user_id":  userID,
			},
		})
	}

	s.broadcastRunUpdate(orgID, runID, "taken_over")
	toast.Info(s.wsHub, orgID, fmt.Sprintf("Taken over: run %s — resume in your terminal", shortRunID(runID)))

	return &TakeoverResult{TakeoverPath: destPath, SessionID: run.SessionID}, nil
}

// abortTakeover unwinds the in-progress takeover state when CopyForTakeover
// or the final DB mark fails. Two things have to happen: the runAgent
// goroutine's gated cleanup (worktree.Remove, RemoveClaudeProjectDir,
// the natural-completion DB write) was suppressed because the
// takenOver flag is set, so we have to do it ourselves; and the run
// row needs to reach a terminal state so the UI and the active-run
// gate don't see a phantom running run forever.
//
// Notably we DO NOT delete s.takenOver[runID]. The flag must stay
// sticky-on for the rest of the goroutine's lifetime — its gates
// (handleCancelled, failRun, the deferred RemoveClaudeProjectDir, the
// natural-completion block) re-read wasTakenOver at unpredictable
// times relative to abortTakeover. Clearing the flag would let any
// late-firing gate proceed with normal cleanup, which races our own
// rollback: the goroutine's unconditional db.CompleteAgentRun would
// overwrite our 'takeover_failed' stop_reason with 'cancelled', and
// its RemoveClaudeProjectDir would run alongside ours. Leaving the
// flag on keeps every gate closed and abortTakeover the sole writer.
//
// The retry-unblocking concern that motivated the original delete is
// moot anyway: once we've SIGKILLed the agent and reached this code
// path, there's no live run left to take over. A user retry creates
// a new delegated run, not a new takeover of the same run.
//
// destPath may be empty (copy never produced one) or may point at a
// partial directory; either way we RemoveAll it.
//
// userID is the taking-over user, passed through from Takeover.
// Takeover is always user-initiated, so the rollback's status write
// routes under the user's synthetic claims rather than the admin pool.
func (s *Spawner) abortTakeover(orgID, runID, claudeCwd, destPath, userID string) {
	if destPath != "" {
		// Use RemoveAt rather than os.RemoveAll so the bare's
		// worktree registration is pruned. By the time we reach
		// abortTakeover after a successful CopyForTakeover, the
		// bare has a worktrees/<runID>/ entry whose gitdir points at
		// destPath; just removing the directory leaves that entry
		// dangling and breaks the next `git worktree add` or `move`
		// against the same runID.
		if err := worktree.RemoveAt(destPath, runID); err != nil {
			log.Printf("[delegate] warning: abort takeover for %s: remove dest %s: %v", runID, destPath, err)
		}
	}
	if claudeCwd != "" {
		worktree.RemoveClaudeProjectDir(claudeCwd)
		// Remove the actual worktree path (which equals claudeCwd for
		// runs with a worktree, the only kind takeover supports). Using
		// claudeCwd rather than runID-derived runDir() guards against
		// the source worktree ever living outside /tmp/triagefactory-
		// runs/<runID> — Remove(runID) would silently target the
		// canonical path and miss the actual one.
		if err := worktree.RemoveAt(claudeCwd, runID); err != nil {
			log.Printf("[delegate] warning: abort takeover for %s: remove worktree: %v", runID, err)
		}
	}

	// If the row is still non-terminal (copy/DB-error path: the
	// goroutine's gated handleCancelled didn't write anything), mark it
	// cancelled so the UI and the active-run gate don't see a phantom
	// running run forever. If the row is already terminal (race-loss
	// path), this no-ops and we leave the agent's real outcome alone.
	// Routes through the takeover user's synthetic claims (takeover
	// is always user-initiated; the rollback is part of that same
	// action).
	bgCtx := context.Background()
	var ok bool
	err := s.tx.SyntheticClaimsWithTx(bgCtx, orgID, userID, func(ts db.TxStores) error {
		f, mErr := ts.AgentRuns.MarkCancelledIfActive(bgCtx, orgID, runID, "takeover_failed", "Takeover failed; run was cancelled")
		ok = f
		return mErr
	})
	if err != nil {
		log.Printf("[delegate] warning: abort takeover for %s: mark cancelled: %v", runID, err)
		return
	}
	if ok {
		s.broadcastRunUpdate(orgID, runID, "cancelled")
	}
}

// Release tears down a held takeover: removes the takeover worktree dir,
// reclaims per-PR config in the bare repo, drops the takeover dir's
// ~/.claude/projects entry, and flips the run row from "held" to
// "released" (status stays 'taken_over' for audit; worktree_path is
// cleared so the resume picker, banner and startup-cleanup-preserve
// queries all drop the row).
//
// Caller signals: pre-release validation failures return ErrReleaseNothingHeld
// (HTTP 409). Filesystem failures during teardown are surfaced as 5xx;
// the row stays held so a retry can finish the job rather than ending
// up half-cleaned with the row marked released.
//
// Order matters:
//
//  1. Resolve config (takeover base) and validate the worktree_path
//     lives under it — defense-in-depth against a poisoned DB row
//     pointing at an arbitrary directory.
//
//  2. Snapshot identity that's only available while the worktree
//     exists: branch name (via WorktreeBranch) and the symlink-
//     resolved cwd (via EvalSymlinks, captured at step 1's safety
//     check). Both are gone after RemoveAt.
//
//  3. Remove the worktree dir + deregister from the bare. From this
//     moment forward, the next delegated run on the same PR can
//     fetch into the branch ref — the load-bearing step. If this
//     fails, return early and leave EVERYTHING ELSE intact (DB row
//     stays held, projects entry stays put) so a retry can resume
//     with the user's resume command still functional.
//
//  4. CleanupPRConfig — reclaim the bare's per-PR remote + branch
//     tracking. Best-effort; SweepStaleForkPRConfig is the next
//     bootstrap's backstop if this fails.
//
//  5. Drop the ~/.claude/projects entry. Done AFTER RemoveAt so a
//     RemoveAt failure doesn't leave the user with neither a
//     working takeover dir nor the resume JSONL needed to retry.
//     Uses the resolved-path variant (RemoveClaudeProjectDirForResolved)
//     since the cwd no longer exists on disk by this point —
//     EvalSymlinks-based variants would silently no-op.
//
//  6. Flip the DB row. Done LAST so a teardown failure leaves the row
//     held and the action retryable.
//
// We deliberately don't clear s.takenOver[runID] (sticky-by-design;
// see Takeover's comment) — a released run never spawns another
// goroutine, so the flag is harmless once set.
//
// userID identifies the releasing user for audit. Local mode handlers
// pass runmode.LocalDefaultUserID; multi-mode handlers extract from
// JWT claims.
func (s *Spawner) Release(orgID, runID, userID string) error {
	if userID == "" {
		return fmt.Errorf("release: empty user id")
	}
	// Preflight read under the requesting user's synthetic claims so
	// RLS filters out runs the user can't see. A guessed runID by
	// another user would otherwise reach the path-safety + cleanup
	// machinery below; with this gate it surfaces as
	// ErrReleaseNothingHeld before any side effect.
	var run *domain.AgentRun
	if err := s.tx.SyntheticClaimsWithTx(context.Background(), orgID, userID, func(ts db.TxStores) error {
		r, rErr := ts.AgentRuns.Get(context.Background(), orgID, runID)
		if rErr != nil {
			return fmt.Errorf("load run: %w", rErr)
		}
		run = r
		return nil
	}); err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("%w: run %s not found", ErrReleaseNothingHeld, runID)
	}
	if run.Status != "taken_over" || run.WorktreePath == "" {
		return fmt.Errorf("%w: run %s (status=%q, worktree_path=%q)", ErrReleaseNothingHeld, runID, run.Status, run.WorktreePath)
	}

	takeoverPath := run.WorktreePath

	// (1) Resolve the takeover base early. Two reasons:
	//
	//   - Path-safety check: a poisoned worktree_path (DB row tampered
	//     with, or a bug elsewhere wrote an arbitrary path into the
	//     column) would otherwise let RemoveAt nuke a directory we
	//     don't own. Refuse the release if takeoverPath isn't under
	//     the configured takeover base.
	//
	//   - Projects-dir cleanup needs the base for its own safety rail.
	//
	// s.takeoverDir is the raw instance_config.server_takeover_dir
	// read at boot; ResolveTakeoverDir applies the empty → default
	// substitution and "~/" expansion to produce the actual base
	// path. handleAgentTakeover calls the same helper on the same
	// stored value when creating the takeover dir, so the release
	// safety check reads against an identical baseline.
	takeoverBase, err := ResolveTakeoverDir(s.takeoverDir)
	if err != nil {
		return fmt.Errorf("resolve takeover base: %w", err)
	}
	// Tolerate a missing takeover dir: the user may have manually
	// deleted it, or a prior release may have removed the dir but
	// crashed before the DB flip. We still need to be able to clear
	// worktree_path so the run can stop appearing in the banner. Fall
	// back to filepath.Abs(filepath.Clean(...)) when EvalSymlinks
	// fails on either side; canonicalizeForSafetyCheck applies the
	// same fallback to both inputs so the prefix comparison stays
	// consistent.
	canonPath, err := canonicalizeForSafetyCheck(takeoverPath)
	if err != nil {
		return fmt.Errorf("canonicalize takeover path %s: %w", takeoverPath, err)
	}
	canonBase, err := canonicalizeForSafetyCheck(takeoverBase)
	if err != nil {
		return fmt.Errorf("canonicalize takeover base %s: %w", takeoverBase, err)
	}
	rel, err := filepath.Rel(canonBase, canonPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("release: takeover path %s is not under takeover base %s; refusing teardown", canonPath, canonBase)
	}
	// resolvedPath is used by step (5) for the projects-dir cleanup and
	// should match the symlink-resolved cwd Claude Code would have used
	// when creating the ~/.claude/projects entry. Compute it best-effort
	// before removing the takeover dir, with an Abs+Clean fallback when
	// the path cannot be fully resolved (for example, if it no longer
	// exists).
	resolvedPath := ""
	if evalPath, evalErr := filepath.EvalSymlinks(takeoverPath); evalErr == nil {
		resolvedPath, err = filepath.Abs(evalPath)
		if err != nil {
			return fmt.Errorf("resolve absolute symlink-evaluated takeover path %s: %w", evalPath, err)
		}
		resolvedPath = filepath.Clean(resolvedPath)
	} else {
		resolvedPath, err = filepath.Abs(takeoverPath)
		if err != nil {
			return fmt.Errorf("resolve absolute takeover path %s: %w", takeoverPath, err)
		}
		resolvedPath = filepath.Clean(resolvedPath)
	}

	// (2) Capture branch name from the takeover dir. Best-effort: empty
	// branch is acceptable (CleanupPRConfig handles "" — it just skips
	// the own-repo branch.<headRef>.* and `branch -D headRef` blocks,
	// while still reclaiming the synthetic triagefactory/pr-<n> remote
	// + branch). Detached HEAD also returns "" via WorktreeBranch.
	headBranch, _ := worktree.WorktreeBranch(takeoverPath)

	// Look up task → entity to derive owner/repo/PR for CleanupPRConfig.
	task, err := s.tasks.GetSystem(context.Background(), orgID, run.TaskID)
	if err != nil {
		return fmt.Errorf("load task for release: %w", err)
	}
	var owner, repo string
	var prNumber int
	if task != nil && task.EntitySource == "github" {
		repoStr := task.EntitySourceID
		if idx := strings.LastIndex(repoStr, "#"); idx >= 0 {
			repoStr = repoStr[:idx]
		}
		owner, repo = parseOwnerRepo(repoStr)
		if idx := strings.LastIndex(task.EntitySourceID, "#"); idx >= 0 {
			fmt.Sscanf(task.EntitySourceID[idx+1:], "%d", &prNumber)
		}
	}

	// (3) Remove the worktree dir + bare-side registration FIRST. If
	// this fails, return early with everything else intact: the
	// projects-dir entry (and the JSONL inside it) is still on disk,
	// so the user's `claude --resume` keeps working and they can
	// retry the release. If we'd cleaned the projects entry first
	// and then failed RemoveAt, we'd be in a half-released state
	// where the run is "held" for retry but the resume history is
	// already gone.
	if err := worktree.RemoveAt(canonPath, runID); err != nil {
		return fmt.Errorf("remove takeover worktree %s: %w", canonPath, err)
	}

	// (4) Per-PR config cleanup. Best-effort — failures here leak a
	// stale remote + branch into the bare's config, which the next
	// bootstrap's SweepStaleForkPRConfig reclaims. Don't block the
	// release on it.
	if owner != "" && repo != "" && prNumber > 0 {
		worktree.CleanupPRConfig(owner, repo, headBranch, prNumber)
	}

	// (5) Drop the ~/.claude/projects entry now that the worktree dir
	// is gone. Uses the resolved-path variant because the cwd no
	// longer exists on disk — RemoveClaudeProjectDir's internal
	// EvalSymlinks would fail and silently no-op. resolvedPath was
	// captured at step (1) before RemoveAt destroyed the path.
	worktree.RemoveClaudeProjectDirForResolved(resolvedPath)

	// (6) DB flip. The status='taken_over' AND worktree_path != ''
	// guard inside MarkAgentRunReleased makes a concurrent double-call
	// idempotent: the second one returns ok=false and we return a
	// 409-equivalent. Release is user-initiated; write under the
	// releasing user's synthetic claims.
	var ok bool
	releaseCtx := context.Background()
	err = s.tx.SyntheticClaimsWithTx(releaseCtx, orgID, userID, func(ts db.TxStores) error {
		o, mErr := ts.AgentRuns.MarkReleased(releaseCtx, orgID, runID)
		ok = o
		return mErr
	})
	if err != nil {
		return fmt.Errorf("mark released: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: run %s (DB row no longer matches release preconditions)", ErrReleaseNothingHeld, runID)
	}

	s.broadcastRunUpdate(orgID, runID, "taken_over")
	toast.Info(s.wsHub, orgID, fmt.Sprintf("Released takeover: run %s", shortRunID(runID)))

	return nil
}

// canonicalizeForSafetyCheck returns filepath.Clean(filepath.Abs(p))
// for the takeover-base prefix check in Release. Deliberately doesn't
// EvalSymlinks: the manual-delete case (user rm -rf'd the takeover
// dir) makes EvalSymlinks fail on the path side, and mixing
// EvalSymlinks-resolved (existing base) with abs-only (missing path)
// produces inconsistent results — on macOS, e.g., the base resolves
// to /private/var/... while the missing path stays /var/..., wrongly
// failing the prefix check.
//
// Abs+Clean alone is sufficient because the takeover_path stored in
// the DB comes from worktree.CopyForTakeover, which itself uses
// filepath.Abs(baseDir); the path and base were canonicalised the
// same way at takeover time. The threat model (poisoned worktree_path
// pointing at, say, /etc) is still defended because Abs+Clean turns
// arbitrary input into an unambiguous absolute form before the rel
// check.
//
// Without this fix Release wedged on its up-front EvalSymlinks when
// the takeover dir was already gone, leaving the run permanently held
// in the UI with no way to flip the DB row.
func canonicalizeForSafetyCheck(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
