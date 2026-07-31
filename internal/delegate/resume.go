// Open-run resume flow: a run that went `open` (a turn ended without a
// conclusion and the process then idle-closed) is woken by ResumeOpenRun,
// which flips it back to running and re-invokes the session with a new message.
// ResumeWithMessage is the lower-level re-invoke helper. The P3 steering
// endpoints (message/interrupt) are the production caller; this is the durable
// cold-resume backstop beneath the warm-process steering path.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// ErrRunNotResumable is returned by ResumeOpenRun when the run can't be
// resumed in its current state — typically a concurrent cancel, approval, or a
// competing resume moved it between the caller's validation read and our status
// flip (MarkQueuedForResume only flips a resumable run: open / completed+abort).
// Callers map this to 409 Conflict so the client can refresh and see the actual
// state.
var ErrRunNotResumable = errors.New("resume: run not in a resumable state")

// ErrWorkspaceExpired is returned by ResumeOpenRun when a resumable run's
// workspace is gone for good: its warm worktree was swept AND its durable
// snapshot was reaped by the retention TTL. The run's status is left unchanged
// (no flip), so the user gets a clear "this workspace has expired" signal rather
// than seeing the run silently fail mid-resume. Callers map it to 410 Gone.
var ErrWorkspaceExpired = errors.New("resume: this run's workspace has expired and can no longer be resumed")

// ResumeOpenRun is resume-by-enqueue (TFAC-585, decision log #7): a
// message to a parked/terminal-but-resumable run (open or completed+abort
// — see resumableState) records the message durably, then re-queues the
// SAME runs row as ordinary claimable work — no in-process resume
// goroutine spawns, in ANY mode (the previous in-process implementation
// is retired entirely, TF_ROLE=all included). This method:
//  1. validates the run is resumable (session id, worktree path, model)
//     and that its workspace can still be recovered — a warm worktree or a
//     durable snapshot (else ErrWorkspaceExpired → 410, with no side effect)
//  2. in ONE tx: flips the run's status back to `queued` (a (status,
//     outcome) compare-and-swap race guard, MarkQueuedForResume), records
//     agentMessage on run_pending_input, and — for a completed+abort run
//     whose blueprint already terminated aborted — re-opens that blueprint.
//     Binding the input write to the winning flip is what keeps concurrent
//     wakes from persisting a loser's message for a later claim to deliver
//  3. nudges the dispatcher (a ~ms same-process wake at TF_ROLE=all; a
//     cross-pod wake is out of scope here — the claiming executor's own
//     scan-interval backstop picks the row up, per the ticket's accepted
//     "wake latency traverses the queue" decision)
//
// The actual delivery — rehydrating the workspace and re-invoking Claude
// with the recorded message — happens later, off the ordinary claim path
// (Spawner.dispatchResumeClaim in dispatch.go), on whichever executor
// claims the row. This is the split the ticket calls for: "operations on
// live processes are routed; creation of work is queued" — a resume mints
// work, so it goes through the same admission gates (memory floor,
// concurrency cap, fair ordering) every other queued run does.
//
// agentMessage is the plain-text message to feed the resumed session (in
// P3, the user's steering text).
//
// userID identifies the resuming user — the actor whose action woke the
// run. Every write here (and, later, every write inside the delivering
// claim) routes under this user's synthetic claims regardless of the
// run's original trigger type (an event-triggered run resumed by a
// teammate gets the teammate's identity on the resume writes, which is
// the audit-honest outcome). Local mode passes runmode.LocalDefaultUserID;
// multi-mode handlers extract it from JWT claims.
func (s *Spawner) ResumeOpenRun(ctx context.Context, orgID, runID, agentMessage, userID string) error {
	if userID == "" {
		return fmt.Errorf("resume: empty user id")
	}
	if s.pendingInput == nil {
		return fmt.Errorf("resume: no pending-input store wired")
	}
	run, err := s.agentRuns.GetSystem(ctx, orgID, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run not found")
	}
	// Only a resumable run (open / completed+abort) can be woken. The
	// MarkQueuedForResume compare-and-swap re-checks under the row lock,
	// so this early-out doesn't relax the race guard — it just avoids
	// recording an input row for a run that's obviously not resumable.
	if !resumableState(run.Status, run.Outcome) {
		return ErrRunNotResumable
	}
	if run.SessionID == "" {
		return fmt.Errorf("run has no session id; cannot resume")
	}
	if run.WorktreePath == "" {
		return fmt.Errorf("run has no worktree path; cannot resume")
	}
	if run.Model == "" {
		return fmt.Errorf("run has no model; cannot resume")
	}
	// Refuse a wake whose workspace is gone for good with no side effect, so
	// the caller sees 410 Gone. Without this the flip below succeeds and the
	// run dies at claim time (a generic failRun) instead — turning an
	// expected, recoverable "this workspace expired" into a destroyed run.
	if !s.workspaceRecoverable(ctx, orgID, run) {
		return ErrWorkspaceExpired
	}

	// A completed+abort run's blueprint already terminated (aborted) when
	// the step stopped. Re-open it to running in the same tx as the run
	// flip (below) so the eventual resumed step's new conclusion re-
	// finalizes the blueprint through the normal post-resume disposition
	// (ResumeBlueprintAfterResume, called from dispatchResumeClaim once
	// delivery completes). An open resume leaves its still-running
	// blueprint alone — ReopenRunForResume's CAS (status='aborted') is a
	// no-op there. Reopening now (not at claim time) is required:
	// ClaimNextRun only claims rows whose blueprint_run is 'running', so a
	// still-'aborted' blueprint would make this row permanently
	// unclaimable.
	blueprintRunID := run.BlueprintRunID
	reopenAbortedBlueprint := run.Status == "completed" && domain.RunOutcome(run.Outcome) == domain.RunOutcomeAbort

	// Step 2: flip the run (from its resumable state) back to `queued` —
	// resume-by-enqueue re-queues the SAME row as ordinary claimable work
	// rather than flipping straight to `running` the way the retired
	// in-process implementation did. Routed under the resuming user's
	// synthetic claims (resume is always user-initiated regardless of the
	// run's original trigger).
	var flipped bool
	err = s.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		f, fErr := ts.Conversations.MarkQueuedForResume(ctx, orgID, runID)
		if fErr != nil {
			return fErr
		}
		flipped = f
		if !f {
			return nil // lost the wake CAS — no winner, so no input to record
		}
		// Record the input in the SAME tx as the winning flip. Binding the
		// write to the CAS is what makes concurrent wakes safe: a loser rolls
		// this back, so only the winner's message ever persists and no orphan
		// row survives a lost race for a later claim to mis-deliver.
		if sErr := ts.RunPendingInput.Store(ctx, orgID, runID, userID, agentMessage); sErr != nil {
			return fmt.Errorf("record pending input: %w", sErr)
		}
		// Re-open the aborted blueprint atomically with the run flip. A failure
		// rolls back the run flip too, so run + blueprint never split across the
		// resumable/terminal boundary.
		if reopenAbortedBlueprint && blueprintRunID != "" {
			if _, rErr := ts.Blueprints.ReopenRunForResume(ctx, orgID, blueprintRunID); rErr != nil {
				return rErr
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("flip status: %w", err)
	}
	if !flipped {
		return ErrRunNotResumable
	}
	s.broadcastRunUpdate(orgID, runID, "queued")
	// The parked step is queued for resume again → bounce the aggregate
	// board column back to in_progress (no-op if a sibling run is still
	// parked).
	s.recomputeTaskBoardColumn(orgID, run.TaskID)
	// Same-process fast path (TF_ROLE=all): the wake is a ~ms in-process
	// dispatcher nudge. Cross-pod, there is no wake nudge here by design —
	// the claiming executor's own scan-interval backstop
	// (DefaultRunScanInterval) picks the row up; a cross-pod wake nudge
	// (tf_wake) is TFAC-586's fleet-reaper scope, not this seam's.
	s.wakeDispatcher()
	return nil
}

// workspaceRecoverable reports whether a parked run can still be resumed:
// its warm worktree survives on disk, or its durable snapshot is present to
// cold-rehydrate from. A check we can't complete (no storage wired, a blob
// hiccup) counts as recoverable — a transient inability to verify must never
// strand a resumable run as expired, and the claim path re-checks for real.
func (s *Spawner) workspaceRecoverable(ctx context.Context, orgID string, run *domain.Conversation) bool {
	if run.WorktreePath != "" {
		if _, err := os.Stat(run.WorktreePath); err == nil {
			return true
		} else if !os.IsNotExist(err) {
			// A stat we couldn't complete (permission, I/O) is not proof the
			// worktree is gone — count it as recoverable rather than emit a
			// false 410 on a transient error.
			delegateLog.Warn("resume: worktree stat inconclusive; treating as recoverable", "run", run.ID, "path", run.WorktreePath, "error", err)
			return true
		}
	}
	blobs := s.Storage()
	if blobs == nil {
		return true
	}
	ok, err := blobs.Exists(ctx, snapshotKey(orgID, memoryNamespace(run.BlueprintRunID, run.ID)))
	if err != nil {
		delegateLog.Warn("resume: snapshot existence check failed", "run", run.ID, "error", err)
		return true
	}
	return ok
}

// markCancelledAfterResume writes the terminal cancelled status for a
// run being resumed off the claim path (Spawner.dispatchResumeClaim) iff
// the run is still non-terminal — the resume-by-enqueue counterpart of
// the retired in-process goroutine's own markCancelled closure. A
// registered s.cancels handle's Cancel() doesn't touch the DB itself; the
// delivering claim owns the terminal write any time it observes its ctx
// cancelled.
// claimID names the delivering engagement, so the terminal goes through the
// claim fence: a resume whose executor was reaped mid-turn must not cancel
// the conversation its successor has picked up. Empty keeps the unfenced
// write. Returns fenced: true when the terminal was refused, in which case
// nothing was written, broadcast, or discarded.
func (s *Spawner) markCancelledAfterResume(orgID, runID, claimID, userID string) (fenced bool) {
	cancelCtx := context.Background()
	var ok bool
	var mErr error
	if claimID != "" {
		ok, mErr = s.agentRuns.MarkCancelledIfActiveForClaimSystem(cancelCtx, orgID, runID, claimID, "user_cancelled", "Run cancelled by user")
	} else {
		mErr = s.tx.SyntheticClaimsWithTx(cancelCtx, orgID, userID, func(ts db.TxStores) error {
			f, err := ts.Conversations.MarkCancelledIfActive(cancelCtx, orgID, runID, "user_cancelled", "Run cancelled by user")
			ok = f
			return err
		})
	}
	if errors.Is(mErr, db.ErrClaimReleased) {
		// A successor owns the conversation. Its snapshot is the workspace
		// that successor resumes from, so the discard below is skipped along
		// with the terminal itself.
		delegateLog.Error("claim fence refused the resume cancellation — a successor owns this conversation; recording nothing",
			"run", runID, "claim_id", claimID, "org_id", orgID, "error", mErr)
		return true
	}
	if ok {
		s.broadcastRunUpdate(orgID, runID, "cancelled")
		// Cancelled mid-resume: the run won't continue, so drop the
		// snapshot taken when it parked. Same key/no-op semantics as
		// failRun's discard (runID-keyed; harmless for a blueprint step,
		// which terminateBlueprint cleans by blueprint_run_id).
		s.discardWorkspaceSnapshot(cancelCtx, orgID, runID)
	}
	return false
}

// ResumeOptions configures a ResumeWithMessage invocation. Callers
// populate these from the values they captured at the original run's
// start, so a resume reuses the exact model / repo / tool context the run
// began with rather than re-resolving live state mid-run.
type ResumeOptions struct {
	// Model is the model the run started with. **Required** — a resume
	// must reuse the model captured at run start (run.Model, captured by
	// ResumeOpenRun), never a freshly-resolved one, or a config change between
	// the initial invocation and the resume would silently switch models
	// mid-run. ResumeWithMessage rejects an empty Model with an error
	// rather than falling back to a live per-(org, team) resolve, which
	// would reintroduce exactly that mid-run drift.
	Model string

	// RepoEnv, if non-empty, is passed to the resumed subprocess as
	// TRIAGE_FACTORY_REPO=<value>. Preserves the GitHub repo context that
	// the initial runAgent invocation set up for gh subcommands so
	// resumes don't lose the implicit --repo default. Format is
	// "owner/name" — composed by the caller from cfg.owner and cfg.repo.
	//
	// Left empty for Jira-no-match runs that never had repo context in
	// the first place.
	RepoEnv string

	// ExtraAllowedTools carries the prompt/agent-derived tool extensions
	// so a resumed session has the same --allowedTools as the initial
	// invocation. Without this, MCP tools allowed on the first run
	// would be rejected on resume.
	ExtraAllowedTools string

	// Namespace is the run's workspace namespace — its blueprint_run_id (see
	// memoryNamespace). Exported to the resumed subprocess as
	// TRIAGE_FACTORY_BLUEPRINT_RUN_ID so per-run scratch paths resolve to the
	// same place they did on the initial invocation. Required for the resume to
	// stay consistent with the initial env; callers capture it from the run
	// (ResumeOpenRun → run.BlueprintRunID).
	Namespace string

	// TeamID is the run's owning team. Resolves the presence-gated
	// absent-auto-deny policy for the resumed run's permission prompts
	// (TFAC-392), and stamps the resumed run's agenthost.RunInfo.TeamID so
	// the capture writers can attribute artifacts (TFAC-458). ResumeOpenRun
	// captures it from the run row (run.TeamID, NOT NULL); empty falls back
	// to the schema defaults for the absent-auto-deny resolve.
	TeamID string

	// execSandbox, when non-nil (TF_ROLE=executor), is the run network +
	// credential sidecar the dispatcher stood up for this resume turn.
	// ResumeWithMessage threads it into agentproc.RunOptions and the agenthost;
	// the dispatcher owns its teardown. nil on all/local.
	execSandbox *executorSandbox

	// claimID is the engagement driving this resume — its own claims row, not
	// the one the run was originally claimed under (a resume is a fresh
	// engagement, and its cost belongs to it). Threaded so teardown can stamp
	// the turn's measured sandbox cost by id; empty records nothing.
	claimID string
}

// ResumeOutcome bundles what ResumeWithMessage returns: the raw
// completion event from the resumed stream (nil if none was observed),
// the parsed agent result JSON (nil if the completion text didn't
// contain a parseable envelope), and captured stderr for diagnostics.
//
// ResumeWithMessage always returns a non-nil *ResumeOutcome (the same struct on
// every path, error or not), so callers guard on Completion == nil, not on a
// nil outcome. Callers decide how to interpret a nil Completion — ResumeOpenRun
// treats it as a session-level failure and surfaces an error.
type ResumeOutcome struct {
	Completion *agentproc.Result
	Result     *agentResult
	StderrText string
}

// ResumeWithMessage resumes a prior headless claude session with a new
// user message and streams the result through the same message-
// persistence path as the initial invocation. The cold-resume backstop for an
// open run (ResumeOpenRun) when the warm process is gone.
//
// Callers pass the sessionID captured during the initial run (read
// from runs.session_id, populated on the runSink during the original
// invocation), the cwd the original run used so the resumed
// subprocess sees the same worktree, and the user message to append
// to the conversation. The runID is reused so resumed messages append
// to the existing messages stream — the UI sees one coherent
// conversation.
//
// This helper does NOT update runs status. The caller manages
// lifecycle: the memory-gate retry loop keeps the run in its current
// state during retries and only finalizes once the gate passes or
// gives up. Mirroring the initial invocation's status updates here
// would produce double CompleteConversation writes with stale
// cost/duration fields overwriting the real totals.
func (s *Spawner) ResumeWithMessage(ctx context.Context, orgID, runID, sessionID, cwd, message string, opts ResumeOptions, triggerType, creatorUserID string) (*ResumeOutcome, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("resume: missing session id")
	}
	if cwd == "" {
		return nil, fmt.Errorf("resume: missing cwd")
	}
	// A resume MUST reuse the model the run started with. Requiring it
	// here — rather than falling back to a live per-(org,
	// team) resolve — is what closes the mid-run model-drift gap: a config
	// change between the initial invocation and this resume must never
	// switch models underneath a single logical run. ResumeOpenRun captures
	// and passes it (run.Model); an empty value is a wiring bug, surfaced loudly.
	if opts.Model == "" {
		return nil, fmt.Errorf("resume: missing model (caller must pass the model captured at run start)")
	}
	model := opts.Model

	selfBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own binary path: %w", err)
	}

	extraEnv := []string{
		"TRIAGE_FACTORY_CONVERSATION_ID=" + runID,
		// Mirror runAgent's TRIAGE_FACTORY_CONVERSATION_ROOT setting. The resume
		// cwd IS the original run-root (runAgent passed runRoot as the
		// agentproc Cwd; for GitHub PR runs the worktree IS the run-root,
		// for Jira lazy runs the run-root is the throwaway parent of
		// per-repo worktrees). Without this, the memory-gate retry
		// message — which references
		// $TRIAGE_FACTORY_CONVERSATION_ROOT/_tfac/memory.md for
		// absolute-path resilience across `cd`s — would resolve to
		// an empty string in the resumed shell and the agent couldn't
		// follow the retry instructions. Same env shape as the initial
		// invocation so the agent sees a consistent environment across
		// every prompt of the conversation.
		"TRIAGE_FACTORY_CONVERSATION_ROOT=" + cwd,
	}
	// Mirror runAgent's namespace export so a resumed agent resolves the per-run
	// scratch paths its prompts name to the same place the initial invocation did.
	if opts.Namespace == "" {
		return nil, fmt.Errorf("resume: missing namespace (caller must pass the workspace namespace captured at run start)")
	}
	extraEnv = append(extraEnv, "TRIAGE_FACTORY_BLUEPRINT_RUN_ID="+opts.Namespace)
	// Preserve the initial run's GitHub repo context so gh subcommands
	// in the resumed session keep their implicit --repo default. Without
	// this, a resumed run on a GitHub task could suddenly fail any gh
	// invocation that relied on the env var set in runAgent.
	if opts.RepoEnv != "" {
		extraEnv = append(extraEnv, "TRIAGE_FACTORY_REPO="+opts.RepoEnv)
	}

	// Resume runs follow the same sandbox-branch path as the initial
	// invocation, so wire the same per-run agenthost socket mount. The
	// credential sidecar hosts the exec-verb socket server (the relocation)
	// and already created the socket at bring-up; the orchestrator only
	// supplies the bind mount. Local (no sandbox) never invokes the closure,
	// so none is built — the former in-process socket server over live
	// stores is gone with the fused single-process shape.
	var startAgentHost func() (sandbox.Mount, io.Closer, error)
	if opts.execSandbox != nil {
		startAgentHost = func() (sandbox.Mount, io.Closer, error) {
			return agenthost.SocketMountFor(runID), noopCloser{}, nil
		}
	}

	// A resumed run gets the same gh channel shape the initial invocation had:
	// the sidecar's in multi, a fresh loopback injector in local (the channel is
	// per-invocation, not per-run — a cold resume starts a new subprocess, so it
	// needs its own listener, placeholder and trust file).
	ownerForGH, _, _ := strings.Cut(opts.RepoEnv, "/")
	localGH, localGHCloser := s.startLocalGHChannel(ctx, orgID, runID, ownerForGH, agenthost.RunInfo{
		OrgID:            orgID,
		UserID:           creatorUserID,
		RunID:            runID,
		TeamID:           opts.TeamID,
		IsEventTriggered: triggerType == domain.TriggerTypeEvent,
	})
	defer func() { _ = localGHCloser.Close() }()
	ghChannel := opts.execSandbox.ghChannel(runID)
	if ghChannel == nil {
		ghChannel = localGH
	}

	baseOpts := agentproc.RunOptions{
		Cwd:       cwd,
		Model:     model,
		SessionID: sessionID,
		Message:   message,
		// gh is granted only alongside a live channel — see runAgent's note.
		AllowedTools: agentproc.BuildAllowedToolsFor(agentproc.AllowedToolsOptions{
			SelfBin: selfBin,
			Extras:  opts.ExtraAllowedTools,
			GH:      ghChannel != nil,
		}),
		MaxTurns:        100,
		ExtraEnv:        extraEnv,
		TraceID:         runID,
		MemoryNamespace: opts.Namespace,
		OrgID:           orgID,
		Secrets:         s.getRunSecrets(),
		LLMResolver:     s.llmResolverForRun(orgID, runID),
		// This turn's own engagement pays for this turn's jail.
		ClaimID:              opts.claimID,
		RecordSandboxActuals: s.recordSandboxActuals,
		// Multi mode: launch into the prebuilt network + the sidecar's proxy
		// env; the sidecar holds the credentials. nil in local (no sandbox).
		PrebuiltNetwork:  opts.execSandbox.runNetwork(),
		PrebuiltProxyEnv: opts.execSandbox.proxyEnv(),
		StartAgentHost:   startAgentHost,
		GHChannel:        ghChannel,
		// Re-mount the step skill this run's original claim staged, when it's
		// still on disk — a resume continues the same step, so it should see the
		// same skill it started with.
		SkillsSourcePath: stagedStepSkillsSource(runID),
		// Same for the prior-memory tree: a resume continues the same step, so it
		// reads the same handoff it started with rather than a freshly rendered one.
		MemorySourcePath: stagedEntityMemorySource(runID),
	}
	sink := newRunSink(s, orgID, runID, opts.claimID, triggerType, creatorUserID)

	// Off-allowlist tool calls route the same way the initial run does:
	// gVisor-sandboxed delegated runs auto-approve (the sandbox + the static
	// allowlist + the enumerated agenthost RPC surface are the actual
	// boundary, not a prompt nobody is there to answer); local-mode resumes
	// keep the presence-gated browser round-trip since the allowlist is
	// their only boundary. opts.TeamID falls back to defaults when empty.
	var perms agentproc.PermissionHandler
	if agentproc.WillSandbox() {
		perms = s.AutoApprovePermissionHandler(runID)
	} else {
		perms = s.BrowserPermissionHandler(orgID, runID, s.resolveAbsentAutoDeny(ctx, opts.TeamID))
	}

	// Resume executes as a LiveRun (re-registered in procs, so a resumed run
	// is interruptible/steerable), falling back to the one-shot sandbox path
	// in multi mode. idleTimeout 0 disables hibernation AND the driver's
	// multi-turn loop: a resume is a bounded re-invoke that drives to one
	// terminal result, not a fresh long-lived autonomous run.
	var out liveOutcome
	if agentproc.InteractiveSupported() {
		out = s.runLiveAndDrive(ctx, liveRunSpec{
			// taskID is intentionally omitted: hibernation is disabled on
			// resumes (idleTimeout 0 below), so hibernatePark — the only
			// taskID consumer — is unreachable here. (It also degrades safely
			// to a no-op board recompute if a future change re-enables idle.)
			park: liveParkContext{
				orgID:         orgID,
				runID:         runID,
				namespace:     opts.Namespace,
				claudeCwd:     cwd,
				triggerType:   triggerType,
				creatorUserID: creatorUserID,
			},
			opts:        baseOpts,
			perms:       perms,
			sink:        sink,
			idleTimeout: 0,
		})
	} else {
		out = s.runOneShot(ctx, baseOpts, sink)
	}

	outcome := &ResumeOutcome{}
	outcome.Completion = out.result
	outcome.StderrText = out.stderr
	if out.result != nil {
		outcome.Result = parseAgentResult(out.result.Result)
	}

	if out.err != nil && out.result == nil {
		// The driver returns ctx.Err() directly when ctx triggered the kill
		// before any completion was captured; preserve that shape so the
		// resume goroutine's ctx.Err() check still routes through markCancelled.
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		return outcome, fmt.Errorf("agent runtime resume failed: %w (stderr: %s)", out.err, outcome.StderrText)
	}

	return outcome, nil
}
