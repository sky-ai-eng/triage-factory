// Yield-resume flow (SKY-139): when an agent emits status:"yield" the
// run parks in awaiting_input; ResumeAfterYield is the entry point used
// by the respond endpoint to wake the session back up with the user's
// answer. ResumeWithMessage is the lower-level helper shared with the
// memory-gate retry loop.

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

// ErrYieldNotResumable is returned by ResumeAfterYield when the run
// can't be resumed in its current state — typically a concurrent
// cancel or takeover flipped it terminal between the handler's
// validation read and our status flip. The respond endpoint maps
// this to 409 Conflict so the client can refresh and see the actual
// state. SKY-139.
var ErrYieldNotResumable = errors.New("yield: run not in awaiting_input")

// ResumeAfterYield is the entry point used by the respond endpoint
// after the user records an answer to a yield. This method:
//  1. validates the run is resumable (session id, worktree path, task)
//  2. registers a cancellation handle in s.cancels[runID]
//  3. flips status awaiting_input → running (with race guard)
//  4. spawns the goroutine that re-invokes Claude with the user's
//     plain-text response and runs the resulting completion through
//     the same processCompletion path the initial run uses
//
// agentMessage is the plain-text rendering of the user's response
// shaped by domain.RenderYieldResponseForAgent.
//
// Cancel-during-resume is closed by ordering: the cancel handle is in
// place before the status flip, so any Cancel() arriving after the
// flip finds the registered ctx and calls cancel() rather than
// falling through to the DB-write path. The resume goroutine writes
// its own terminal cancelled status when it observes ctx.Err() —
// the registered-cancel path doesn't write to the DB itself, so
// without that we'd leak a "cancelled but row says running" state.
//
// userID identifies the responding user — the actor whose action
// resumed the yielded run. All writes inside the resume goroutine
// route under this user's synthetic claims regardless of the run's
// original trigger type (an event-triggered run that yielded and was
// answered by a teammate gets the teammate's identity on the resume
// writes, which is the audit-honest outcome). Local mode passes
// runmode.LocalDefaultUserID; multi-mode handlers extract it from JWT
// claims.
//
// SKY-139.
func (s *Spawner) ResumeAfterYield(orgID, runID, agentMessage, userID string) error {
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
	if run.SessionID == "" {
		return fmt.Errorf("run has no session id; cannot resume")
	}
	if run.WorktreePath == "" {
		return fmt.Errorf("run has no worktree path; cannot resume")
	}
	// A resume must reuse the model the run started with. run.Model is set
	// at Delegate time (always non-empty for a real run); guard here so the
	// yield path fails with a clear message rather than tripping
	// ResumeWithMessage's required-model check downstream. SKY-389 review #1.
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
	// Memory namespace for the resumed env: the run's blueprint_run_id, else
	// its own id. Keeps the resumed agent reading/writing the same
	// _scratch/entity-memory/<namespace>/ folder as the initial invocation.
	namespace := memoryNamespace(run.BlueprintRunID, runID)
	// trigger_type is non-null in the schema (the CHECK pairs it
	// with creator_user_id nullability), so this fallback only
	// defends against legacy / test fixture rows that left the
	// column unset. Keeping it cheap and explicit rather than
	// trusting the read.
	// Resume is always user-initiated — the userID arg is the actor
	// who clicked respond. All writes inside the resume route under
	// that user's synthetic claims regardless of the run's original
	// trigger type: an event-triggered run answered by a teammate
	// still attributes the resume's writes to the teammate. The
	// trigger_type captured here is only used for the drainer +
	// pollDrainer hook on terminal exit (event runs still drive the
	// per-entity firing queue).
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
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
		// Should not happen for awaiting_input (the initial
		// goroutine exited when it parked the run); defend against
		// a double-respond or a stale entry.
		return fmt.Errorf("run already has an active goroutine")
	}
	s.cancels[runID] = cancel
	s.mu.Unlock()

	// Step 2: flip status awaiting_input → running. This must happen
	// AFTER cancel registration: if the order is reversed, a Cancel()
	// arriving in the gap sees no goroutine, falls through to the DB
	// path, and races the resume into the "row cancelled but
	// goroutine still running" state the review bot flagged. With
	// the cancel handle already in place, any Cancel() now hits
	// cancel(ctx) and the goroutine handles the terminal write.
	// Routed under the responding user's synthetic claims (resume is
	// always user-initiated regardless of the run's original trigger).
	bgCtx := context.Background()
	var flipped bool
	err = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, userID, func(ts db.TxStores) error {
		f, fErr := ts.AgentRuns.MarkResuming(bgCtx, orgID, runID)
		flipped = f
		return fErr
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
		return ErrYieldNotResumable
	}
	s.broadcastRunUpdate(orgID, runID, "running")

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.cancels, runID)
			s.mu.Unlock()
			cancel()
			// Drain the per-entity firing queue on terminal exit —
			// matches the initial-run defer in Delegate so a yield
			// resume that lands the run terminal still flushes any
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

		// Resume routes every downstream write under the responding
		// user's synthetic claims regardless of the run's original
		// trigger type: pass "manual" + userID so processCompletion /
		// failRun / ResumeWithMessage / sink each pick the synth-claims
		// arm. The captured triggerType local (above) stays in scope
		// for the goroutine's notifyDrainer defer — event-triggered
		// runs still drive the per-entity firing queue on terminal.
		outcome, err := s.ResumeWithMessage(ctx, orgID, runID, sessionID, cwd, agentMessage, ResumeOptions{
			Model:             model,
			RepoEnv:           repoEnv,
			ExtraAllowedTools: extraTools,
			Namespace:         namespace,
		}, "manual", userID)
		if ctx.Err() != nil {
			// User cancelled mid-resume. ResumeWithMessage SIGKILLed
			// the subprocess via its own ctx.Done() watcher; we own
			// the terminal status write.
			markCancelled()
			return
		}
		if err != nil {
			s.failRun(orgID, runID, taskCopy.ID, "manual", userID, "resume after yield failed: "+err.Error())
			return
		}
		if outcome == nil || outcome.Completion == nil {
			s.failRun(orgID, runID, taskCopy.ID, "manual", userID, "resume after yield produced no completion")
			return
		}

		s.processCompletion(ctx, orgID, runID, taskCopy, outcome.Completion, cwd, sessionID, model, owner, repo, "manual", userID, extraTools)
	}()
	return nil
}

// ResumeOptions configures a ResumeWithMessage invocation. Callers
// populate these from the values they captured at the original run's
// start, so a resume reuses the exact model / repo / tool context the run
// began with rather than re-resolving live state mid-run.
type ResumeOptions struct {
	// Model is the model the run started with. **Required** — a resume
	// must reuse the model captured at run start (run.Model for the
	// yield path, the gate's captured model for the memory-gate retry),
	// never a freshly-resolved one, or a config change between the
	// initial invocation and the resume would silently switch models
	// mid-run. ResumeWithMessage rejects an empty Model with an error
	// rather than falling back to a live per-(org, team) resolve, which
	// would reintroduce exactly that mid-run drift (SKY-389 review #1).
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

	// Namespace is the run's memory namespace — its blueprint_run_id, else the
	// run's own id (see memoryNamespace). Exported to the resumed subprocess as
	// TRIAGE_FACTORY_BLUEPRINT_RUN_ID so the agent's <entity_memory> contract
	// names the same folder it read and wrote on the initial invocation.
	// Required for the resume to stay consistent with the initial env; callers
	// capture it from the run (ResumeAfterYield → run.BlueprintRunID, the
	// completion gate → the namespace it computed).
	Namespace string
}

// ResumeOutcome bundles what ResumeWithMessage returns: the raw
// completion event from the resumed stream (nil if none was observed),
// the parsed agent result JSON (nil if the completion text didn't
// contain a parseable envelope), and captured stderr for diagnostics.
//
// Callers decide how to interpret a nil Completion — the memory-gate
// retry loop treats it as "retry again if attempts remain, else flag
// memory_missing," while a yield-resume flow might treat it as a
// session-level failure and surface an error.
type ResumeOutcome struct {
	Completion *agentproc.Result
	Result     *agentResult
	StderrText string
}

// ResumeWithMessage resumes a prior headless claude session with a new
// user message and streams the result through the same message-
// persistence path as the initial invocation. Used by the SKY-141
// task-memory write-gate retry loop and the SKY-139 yield-to-user flow.
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
	// switch models underneath a single logical run. Both real callers
	// capture and pass it (ResumeAfterYield → run.Model, the completion
	// gate → the captured model); an empty value is a wiring bug, surfaced loudly.
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
		"TRIAGE_FACTORY_REVIEW_PREVIEW=1",
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
	// initial invocation (the completion-gate retry path depends on this, and a
	// yield-resume continuing the work must land its memory in the same place).
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

	apOutcome, runErr := agentproc.Run(ctx, agentproc.RunOptions{
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
		StartAgentHost: startAgentHost,
	}, newRunSink(s, orgID, runID, triggerType, creatorUserID))

	outcome := &ResumeOutcome{}
	if apOutcome != nil {
		outcome.Completion = apOutcome.Result
		outcome.StderrText = apOutcome.Stderr
		if apOutcome.Result != nil {
			outcome.Result = parseAgentResult(apOutcome.Result.Result)
		}
	}

	if runErr != nil && (apOutcome == nil || apOutcome.Result == nil) {
		// agentproc.Run returns ctx.Err() directly when ctx triggered
		// the kill before any completion was captured; preserve that
		// shape so the SKY-139 yield-resume goroutine's ctx.Err()
		// check still routes through markCancelled.
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		return outcome, fmt.Errorf("agent runtime resume failed: %w (stderr: %s)", runErr, outcome.StderrText)
	}

	return outcome, nil
}
