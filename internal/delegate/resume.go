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

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// ErrRunNotResumable is returned by ResumeOpenRun when the run can't be
// resumed in its current state — typically a concurrent cancel, approval, or a
// competing resume moved it between the caller's validation read and our status
// flip (MarkResuming only flips a resumable run: open / completed+abort).
// Callers map this to 409 Conflict so the client can refresh and see the actual
// state.
var ErrRunNotResumable = errors.New("resume: run not in a resumable state")

// ErrWorkspaceExpired is returned by ResumeOpenRun when a resumable run's
// workspace is gone for good: its warm worktree was swept AND its durable
// snapshot was reaped by the retention TTL. The run's status is left unchanged
// (no flip), so the user gets a clear "this workspace has expired" signal rather
// than seeing the run silently fail mid-resume. Callers map it to 410 Gone.
var ErrWorkspaceExpired = errors.New("resume: this run's workspace has expired and can no longer be resumed")

// ResumeOpenRun wakes a parked/terminal-but-resumable run (open or
// completed+abort — see resumableState) with a new
// message. This method:
//  1. validates the run is resumable (session id, worktree path, task)
//  2. registers a cancellation handle in s.cancels[runID]
//  3. flips the run to running (a (status, outcome) compare-and-swap race
//     guard), and — for a completed+abort run whose blueprint already
//     terminated aborted — re-opens that blueprint in the same tx so the
//     resumed step can re-finalize it
//  4. spawns the goroutine that re-invokes Claude with the message and runs
//     the resulting completion through the same processCompletion path the
//     initial run uses
//
// agentMessage is the plain-text message to feed the resumed session (in P3,
// the user's steering text).
//
// Cancel-during-resume is closed by ordering: the cancel handle is in
// place before the status flip, so any Cancel() arriving after the
// flip finds the registered ctx and calls cancel() rather than
// falling through to the DB-write path. The resume goroutine writes
// its own terminal cancelled status when it observes ctx.Err() —
// the registered-cancel path doesn't write to the DB itself, so
// without that we'd leak a "cancelled but row says running" state.
//
// userID identifies the resuming user — the actor whose action woke the run.
// All writes inside the resume goroutine route under this user's synthetic
// claims regardless of the run's original trigger type (an event-triggered run
// resumed by a teammate gets the teammate's identity on the resume writes,
// which is the audit-honest outcome). Local mode passes
// runmode.LocalDefaultUserID; multi-mode handlers extract it from JWT claims.
func (s *Spawner) ResumeOpenRun(orgID, runID, agentMessage, userID string) error {
	if userID == "" {
		return fmt.Errorf("resume: empty user id")
	}
	run, err := s.agentRuns.GetSystem(context.Background(), orgID, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run not found")
	}
	// Only a resumable run (open / completed+abort) can be
	// woken; a finish/failed/cancelled or actively-running run is rejected up
	// front so the workspace pre-flight below never mislabels a non-resumable run
	// as "expired". The MarkResuming compare-and-swap re-checks under the row
	// lock, so this early-out doesn't relax the race guard.
	if !resumableState(run.Status, run.Outcome) {
		return ErrRunNotResumable
	}
	if run.SessionID == "" {
		return fmt.Errorf("run has no session id; cannot resume")
	}
	if run.WorktreePath == "" {
		return fmt.Errorf("run has no worktree path; cannot resume")
	}
	// A resume must reuse the model the run started with. run.Model is set
	// at Delegate time (always non-empty for a real run); guard here so the
	// resume fails with a clear message rather than tripping ResumeWithMessage's
	// required-model check downstream.
	if run.Model == "" {
		return fmt.Errorf("run has no model; cannot resume")
	}
	task, err := s.tasks.GetSystem(context.Background(), orgID, run.TaskID)
	if err != nil {
		return fmt.Errorf("load task: %w", err)
	}
	if task == nil {
		return fmt.Errorf("task not found for run")
	}

	// Resolve owner/repo for repoEnv. Best-effort: Jira-only runs have
	// no resolvable repo and the resumed subprocess simply runs
	// without TRIAGE_FACTORY_REPO, the same way Jira-no-match runs do
	// today.
	owner, repo := "", ""
	entity, err := s.entities.GetSystem(context.Background(), orgID, task.EntityID)
	if err == nil && entity != nil {
		owner, repo = parseOwnerRepo(entity.SourceID)
	}

	// Resolve extra allowed tools from the prompt used for this run.
	var extraTools string
	if run.PromptID != "" {
		if p, err := s.prompts.GetSystem(context.Background(), orgID, run.PromptID); err == nil && p != nil {
			extraTools = s.collectExtraTools(p.AllowedTools)
		}
	}

	// Capture state needed inside the goroutine.
	sessionID := run.SessionID
	cwd := run.WorktreePath
	model := run.Model
	taskCopy := *task
	// The run's blueprint_run_id drives both the resumed env's memory namespace
	// and processCompletion's namespacing + task disposition. Capture the raw
	// value and derive the namespace (blueprint_run_id — every run is a blueprint
	// step) so the resumed agent reads/writes the same _scratch/entity-memory/
	// <namespace>/ folder as the initial invocation.
	blueprintRunID := run.BlueprintRunID
	namespace := memoryNamespace(blueprintRunID, runID)
	// A completed+abort run's blueprint already terminated (aborted) when the
	// step stopped. Re-open it to running in the same tx as the run flip (below)
	// so the resumed step's new conclusion re-finalizes the blueprint through the
	// normal post-resume disposition (ResumeBlueprintAfterResume). An
	// An open resume leaves its still-running blueprint alone —
	// ReopenRunForResume's CAS (status='aborted') is a no-op there.
	reopenAbortedBlueprint := run.Status == "completed" && domain.RunOutcome(run.Outcome) == domain.RunOutcomeAbort
	// trigger_type is non-null in the schema (the CHECK pairs it
	// with creator_user_id nullability), so this fallback only
	// defends against legacy / test fixture rows that left the
	// column unset. Keeping it cheap and explicit rather than
	// trusting the read.
	// Resume is always user-initiated — the userID arg is the actor
	// who woke the run. All writes inside the resume route under
	// that user's synthetic claims regardless of the run's original
	// trigger type: an event-triggered run resumed by a teammate
	// still attributes the resume's writes to the teammate. The
	// trigger_type captured here is only used for the drainer +
	// pollDrainer hook on terminal exit (event runs still drive the
	// per-entity firing queue).
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}

	// Pre-flight the workspace before any side effect: a resume rebuilds from the
	// warm worktree, or failing that the durable snapshot. If both are gone — the
	// worktree was swept AND the snapshot was reaped by the retention TTL — the
	// workspace has expired. Surface that as a clear error WITHOUT flipping the
	// run (its status is unchanged at expiry), so the user learns the WIP is gone
	// rather than watching the run flip to failed mid-resume.
	if !s.workspaceRecoverable(context.Background(), orgID, run) {
		return ErrWorkspaceExpired
	}

	// Step 1: register the cancel handle synchronously. Once this
	// runs, a concurrent Cancel(runID) finds the entry and calls
	// cancel() on the ctx instead of falling through to the
	// MarkAgentRunCancelledIfActive DB-write path. The goroutine
	// observes ctx.Err() and writes the terminal cancelled status
	// itself.
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if _, ok := s.cancels[runID]; ok {
		s.mu.Unlock()
		cancel()
		// Should not happen for an open run (the initial goroutine exited when
		// the run idle-closed); defend against a double-resume or a stale entry.
		return fmt.Errorf("run already has an active goroutine")
	}
	s.cancels[runID] = cancel
	s.mu.Unlock()

	// Step 2: flip the run (from its resumable state) to running. This must
	// happen AFTER cancel registration: if the order is reversed, a Cancel()
	// arriving in the gap sees no goroutine, falls through to the DB path, and
	// races the resume into the "row cancelled but goroutine still running"
	// state. With the cancel handle already in place, any Cancel() now hits
	// cancel(ctx) and the goroutine handles the terminal write. Routed under the
	// resuming user's synthetic claims (resume is always user-initiated
	// regardless of the run's original trigger).
	bgCtx := context.Background()
	var flipped bool
	err = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, userID, func(ts db.TxStores) error {
		f, fErr := ts.AgentRuns.MarkResuming(bgCtx, orgID, runID)
		if fErr != nil {
			return fErr
		}
		flipped = f
		if !f {
			return nil // lost the wake CAS — nothing to re-open
		}
		// Re-open the aborted blueprint atomically with the run flip. A failure
		// rolls back the run flip too, so run + blueprint never split across the
		// resumable/terminal boundary.
		if reopenAbortedBlueprint && blueprintRunID != "" {
			if _, rErr := ts.Blueprints.ReopenRunForResume(bgCtx, orgID, blueprintRunID); rErr != nil {
				return rErr
			}
		}
		return nil
	})
	if err != nil {
		s.mu.Lock()
		delete(s.cancels, runID)
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("flip status: %w", err)
	}
	if !flipped {
		s.mu.Lock()
		delete(s.cancels, runID)
		s.mu.Unlock()
		cancel()
		return ErrRunNotResumable
	}
	s.broadcastRunUpdate(orgID, runID, "running")
	// The parked step is running again → bounce the aggregate board column back
	// to in_progress (no-op if a sibling run is still parked).
	s.recomputeTaskBoardColumn(orgID, taskCopy.ID)

	go func() {
		// disposed is set once the body reaches its post-resume disposition
		// (processCompletion + the inline finalize/re-park below). It stays false
		// on an early failure/cancel exit, which is the only case the defer's
		// blueprint re-finalize has to cover.
		var disposed bool
		defer func() {
			// An early exit (failure / cancel before processCompletion) leaves the
			// step terminal but the blueprint un-finalized — for any resumable
			// state, not just the re-opened abort case. Without finalizing here:
			// the blueprint strands `running`, and its snapshot is orphaned (the
			// reaper skips it once the run is cancelled/failed, i.e. no longer in a
			// resumable state). So re-finalize for every blueprint step that didn't
			// dispose. ResumeBlueprintAfterResume is the safe single authority: a
			// cancelled/failed step maps to a terminal blueprint (terminateBlueprint
			// discards the snapshot), a still-parked (open) step or
			// an already-terminal blueprint early-returns. The success path set
			// disposed=true, so this never doubles up.
			if blueprintRunID != "" && !disposed {
				s.ResumeBlueprintAfterResume(orgID, runID, userID)
			}
			s.mu.Lock()
			delete(s.cancels, runID)
			s.mu.Unlock()
			cancel()
			// Drain the per-entity firing queue on terminal exit —
			// matches the initial-run defer in Delegate so a resume
			// that lands the run terminal still flushes any
			// queued auto-firings for the same entity.
			s.notifyDrainer(orgID, triggerType, taskCopy.EntityID)
		}()

		// markCancelled writes the terminal cancelled status iff the
		// run is still non-terminal. The registered-handle Cancel()
		// path doesn't touch the DB; this goroutine owns the
		// terminal write any time it observes ctx.Err().
		markCancelled := func() {
			cancelCtx := context.Background()
			var ok bool
			_ = s.tx.SyntheticClaimsWithTx(cancelCtx, orgID, userID, func(ts db.TxStores) error {
				f, mErr := ts.AgentRuns.MarkCancelledIfActive(cancelCtx, orgID, runID, "user_cancelled", "Run cancelled by user")
				ok = f
				return mErr
			})
			if ok {
				s.broadcastRunUpdate(orgID, runID, "cancelled")
				// Cancelled mid-resume: the run won't continue, so drop the
				// snapshot taken when it parked. Same key/no-op semantics as
				// failRun's discard (runID-keyed; harmless for a blueprint step,
				// which terminateBlueprint cleans by blueprint_run_id).
				s.discardWorkspaceSnapshot(cancelCtx, orgID, runID)
			}
		}

		// Cancel raced before the goroutine scheduled. Write the
		// cancelled status ourselves and exit without invoking
		// Claude.
		if ctx.Err() != nil {
			markCancelled()
			return
		}

		repoEnv := ""
		if owner != "" && repo != "" {
			repoEnv = owner + "/" + repo
		}

		// Ensure the worktree is on disk before re-invoking the agent. Warm
		// path: the parked worktree survived (the dormancy guards kept it) →
		// ensureWorkspace returns it and rehydrate is a no-op. Cold path: it was
		// swept / the host was lost / `/tmp` was wiped → rebuild it from the
		// durable snapshot. The returned cwd may differ from run.WorktreePath
		// after a cold rebuild, so use it for both the resume and the
		// completion. cloneURL is empty: the local reboot case reuses the
		// persistent bare, which needs no seeding.
		resumeCwd, werr := s.ensureWorkspace(ctx, orgID, run, owner, repo, "")
		if werr != nil {
			s.failRun(orgID, runID, taskCopy.ID, "manual", userID, "ensure workspace before resume failed: "+werr.Error(), domain.RunFailureUnclassified)
			return
		}
		cwd = resumeCwd

		// Resume routes every downstream write under the resuming
		// user's synthetic claims regardless of the run's original
		// trigger type: pass "manual" + userID so processCompletion /
		// failRun / ResumeWithMessage / sink each pick the synth-claims
		// arm. The captured triggerType local (above) stays in scope
		// for the goroutine's notifyDrainer defer — event-triggered
		// runs still drive the per-entity firing queue on terminal.
		// run.TeamID (runs.team_id, NOT NULL) is the canonical owning team —
		// no task hop, always populated — so it drives both the absent-auto-deny
		// resolve and the resumed run's agenthost.RunInfo.TeamID (TFAC-458).
		outcome, err := s.ResumeWithMessage(ctx, orgID, runID, sessionID, cwd, agentMessage, ResumeOptions{
			Model:             model,
			RepoEnv:           repoEnv,
			ExtraAllowedTools: extraTools,
			Namespace:         namespace,
			TeamID:            run.TeamID,
		}, "manual", userID)
		if ctx.Err() != nil {
			// User cancelled mid-resume. ResumeWithMessage SIGKILLed
			// the subprocess via its own ctx.Done() watcher; we own
			// the terminal status write.
			markCancelled()
			return
		}
		if err != nil {
			s.failRun(orgID, runID, taskCopy.ID, "manual", userID, "resume failed: "+err.Error(), classifyFailureKind(err))
			return
		}
		// ResumeWithMessage always returns a non-nil outcome (it returns the same
		// &ResumeOutcome on every path); Completion is the field that's nil when
		// the resume produced no terminal result.
		if outcome.Completion == nil {
			s.failRun(orgID, runID, taskCopy.ID, "manual", userID, "resume produced no completion", domain.RunFailureNoResult)
			return
		}

		parked := s.processCompletion(ctx, orgID, runID, blueprintRunID, taskCopy, outcome.Completion, cwd, sessionID, "manual", userID)
		// The resumed step reached a terminal state (it didn't go open again or
		// queue an approval) → hand back to the blueprint orchestrator to
		// finalize: for a 1-step / final step this terminates the blueprint
		// (finish→close, abort→leave open). A non-final step's mid-blueprint
		// advance is the epic's resume work and stays unimplemented (terminated
		// with a clear reason).
		if !parked {
			s.ResumeBlueprintAfterResume(orgID, runID, userID)
		}
		// The body owns the disposition now (re-parked, or finalized above), so
		// the defer's re-finalize must not fire on top of it.
		disposed = true
	}()
	return nil
}

// workspaceRecoverable reports whether a parked run can still be resumed: its
// warm worktree survives on disk, or its durable snapshot is present to
// cold-rehydrate from. False means the workspace expired — the retention reaper
// dropped the snapshot after the warm worktree had already been swept. A
// blob-store read error is treated as recoverable (logged, not fatal): a
// transient storage hiccup must not strand a resumable run as "expired".
func (s *Spawner) workspaceRecoverable(ctx context.Context, orgID string, run *domain.AgentRun) bool {
	if run.WorktreePath != "" {
		if _, err := os.Stat(run.WorktreePath); err == nil {
			return true // warm worktree on disk
		}
	}
	blobs := s.Storage()
	if blobs == nil {
		return false
	}
	ok, err := blobs.Exists(ctx, snapshotKey(orgID, memoryNamespace(run.BlueprintRunID, run.ID)))
	if err != nil {
		delegateLog.Warn("resume: snapshot existence check failed", "run", run.ID, "error", err)
		return true
	}
	return ok
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

	// Namespace is the run's memory namespace — its blueprint_run_id (see
	// memoryNamespace). Exported to the resumed subprocess as
	// TRIAGE_FACTORY_BLUEPRINT_RUN_ID so the agent's <entity_memory> contract
	// names the same folder it read and wrote on the initial invocation.
	// Required for the resume to stay consistent with the initial env; callers
	// capture it from the run (ResumeOpenRun → run.BlueprintRunID).
	Namespace string

	// TeamID is the run's owning team. Resolves the presence-gated
	// absent-auto-deny policy for the resumed run's permission prompts
	// (TFAC-392), and stamps the resumed run's agenthost.RunInfo.TeamID so
	// the capture writers can attribute artifacts (TFAC-458). ResumeOpenRun
	// captures it from the run row (run.TeamID, NOT NULL); empty falls back
	// to the schema defaults for the absent-auto-deny resolve.
	TeamID string
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
// to the existing run_messages stream — the UI sees one coherent
// conversation.
//
// This helper does NOT update runs status. The caller manages
// lifecycle: the memory-gate retry loop keeps the run in its current
// state during retries and only finalizes once the gate passes or
// gives up. Mirroring the initial invocation's status updates here
// would produce double CompleteAgentRun writes with stale
// cost/duration fields overwriting the real totals.
func (s *Spawner) ResumeWithMessage(ctx context.Context, orgID, runID, sessionID, cwd, message string, opts ResumeOptions, triggerType, creatorUserID string) (*ResumeOutcome, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("resume: missing session id")
	}
	if cwd == "" {
		return nil, fmt.Errorf("resume: missing cwd")
	}
	// A resume MUST reuse the model the run started with (SKY-389 review
	// #1). Requiring it here — rather than falling back to a live per-(org,
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
		"TRIAGE_FACTORY_RUN_ID=" + runID,
		// Mirror runAgent's TRIAGE_FACTORY_RUN_ROOT setting. The resume
		// cwd IS the original run-root (runAgent passed runRoot as the
		// agentproc Cwd; for GitHub PR runs the worktree IS the run-root,
		// for Jira lazy runs the run-root is the throwaway parent of
		// per-repo worktrees). Without this, the memory-gate retry
		// message — which now references
		// $TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/ for
		// absolute-path resilience across `cd`s — would resolve to
		// an empty string in the resumed shell and the agent couldn't
		// follow the retry instructions. Same env shape as the initial
		// invocation so the agent sees a consistent environment across
		// every prompt of the conversation.
		"TRIAGE_FACTORY_RUN_ROOT=" + cwd,
	}
	// Mirror runAgent's memory-namespace export so the resumed agent writes
	// into the same _scratch/entity-memory/<namespace>/ folder it used on the
	// initial invocation (a resume continuing the work must land its memory in
	// the same place).
	if opts.Namespace == "" {
		return nil, fmt.Errorf("resume: missing namespace (caller must pass the memory namespace captured at run start)")
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
	// invocation, so wire the same per-run agenthost daemon. The run
	// identity is unchanged across resumes — only the prompt and the
	// SessionID differ.
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
			TeamID:           opts.TeamID,
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

	// Resume runs follow the sandbox branch in multi mode, so wire the same
	// per-run git proxy the initial invocation got. Authority is re-derived
	// live (per-repo lazy mint + the run_worktrees-backed gate), so no head-ref
	// or owner needs threading through resume — the durable rows carry it. Also
	// wire the receive-pack backstop to the same record path the pre-push hook
	// uses (TFAC-467). nil config (local mode / no GitHub credential) leaves the
	// --no-verify gap accepted.
	gitProxy := s.gitProxyConfigFor(ctx, info, stores)
	if gitProxy != nil && storesSet {
		gitProxy.RecordPush = gitPushRecorder(stores, info)
	}

	baseOpts := agentproc.RunOptions{
		Cwd:            cwd,
		Model:          model,
		SessionID:      sessionID,
		Message:        message,
		AllowedTools:   agentproc.BuildAllowedToolsWithExtras(selfBin, opts.ExtraAllowedTools),
		MaxTurns:       100,
		ExtraEnv:       extraEnv,
		TraceID:        runID,
		OrgID:          orgID,
		Secrets:        s.getRunSecrets(),
		GitProxy:       gitProxy,
		StartAgentHost: startAgentHost,
	}
	sink := newRunSink(s, orgID, runID, triggerType, creatorUserID)

	// Off-allowlist tool calls route the same way the initial run does
	// (TFAC-557): gVisor-sandboxed delegated runs auto-approve (the sandbox +
	// the static allowlist + the enumerated agenthost RPC surface are the
	// actual boundary, not a prompt nobody is there to answer);
	// local-mode resumes keep the presence-gated browser round-trip
	// (TFAC-392) since the allowlist is their only boundary. opts.TeamID
	// falls back to defaults when empty.
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
