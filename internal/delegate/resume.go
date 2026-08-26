// The SDK resume machinery: what a woken conversation is refused for, what a
// lost wake means, and ResumeWithMessage — the re-invoke that hands a claimed
// resume's message to a prior headless claude session. The wake itself lives in
// followup.go; this is the cold-resume backstop beneath it.

package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// ErrConversationNotResumable is returned when a wake's compare-and-swap found the
// conversation no longer in a resumable state (MarkQueuedForResume only flips
// open / completed) — a concurrent failure or terminal moved it out from under
// the caller. Callers map this to 409 Conflict so the client can refresh and
// see the actual state.
//
// A competing RESUME is not in this set, and that is the whole distinction the
// lost-flip path has to make (see lostWakeOutcome). Two wakes race, one loses,
// and the loser's message is queued alongside the winner's for the winner's
// claim to deliver — nothing about that is a conflict to report. A flip lost
// to a conversation that went terminal instead IS one: nothing will claim it,
// so the queued message is never delivered.
var ErrConversationNotResumable = errors.New("resume: conversation not in a resumable state")

// ErrConversationConcluded is returned when a conversation's workspace is
// intact but its blueprint has moved past this step, so a wake would strand it
// mid-flight forever — claimed by nobody, counted as queue depth by every
// counter. The blueprint's steps share one worktree and one snapshot blob, so
// the tree an earlier step would resume into is no longer its own — while the
// blueprint is still running (a later step is in that tree right now) exactly
// as once it has stopped.
//
// Permanent, and about this conversation rather than about timing. Not a
// claim that the conversation ended of its own accord — a step that concluded
// cleanly is exactly what reaches here once the blueprint moves on. A
// conversation stopped by a user reaches neither this nor
// ErrBlueprintCancelled: a stop freezes its blueprint 'running' with the
// pointer on that step, precisely so the parked step stays claimable.
//
// Its own sentinel rather than ErrConversationNotResumable, because the two say
// different things to a person. "Not resumable" means the state moved under
// you: refresh and look again. This means refreshing will never change it.
var ErrConversationConcluded = errors.New("resume: this conversation can no longer be continued — its blueprint has moved past this step (only the step a blueprint is currently on takes follow-ups)")

// ErrBlueprintCancelled is returned when a conversation's workspace is intact
// but its blueprint was called off at its own layer — the task it rode was
// closed or dispositioned, its team archived, or the run cancelled outright —
// so nothing under it runs again.
//
// As permanent as ErrConversationConcluded, and its own sentinel for the same
// reason that one is split from ErrConversationNotResumable: the words a
// person needs are different. Nothing here moved past anything — the work
// itself was ended, and the stop note on the transcript names the lifecycle
// event that ended it.
var ErrBlueprintCancelled = errors.New("resume: this conversation can no longer be continued — the workflow it belonged to was cancelled")

// ErrStepHandedOff is returned when a conversation concluded as a step of a
// blueprint that has not reacted to that terminal yet.
//
// Its own sentinel because it says the opposite of ErrConversationConcluded to
// the person reading it: that one means refreshing will never change the
// answer, this one means it will, and shortly. Callers map it to 409 Conflict.
var ErrStepHandedOff = errors.New("resume: this step just handed off to the next one — follow up on the blueprint's latest step")

// ErrWorkspaceExpired is returned when a resumable conversation's workspace is
// gone for good: its warm worktree was swept, no snapshot is there or coming,
// and its runtime cannot continue without one. That last clause is an SDK
// answer — its continuity lived in the session transcript the blob carried,
// while a native conversation's lives in `messages` and survives a workspace
// built from nothing, so a native one falls back instead of reaching here.
//
// The conversation's status is left unchanged (no flip), so the user gets a
// clear "this workspace has expired" signal rather than seeing the run
// silently fail mid-resume. Callers map it to 410 Gone.
var ErrWorkspaceExpired = errors.New("resume: this conversation's workspace has expired and can no longer be resumed")

// lostWakeOutcome answers for a wake whose compare-and-swap found the
// conversation already moved. The message is queued either way — the only
// question left is whether anything is coming to drain it, and the CAS refuses
// two situations that answer it oppositely:
//
//   - another wake won the race, or an engagement has since claimed the row.
//     Something is driving the conversation and drains whatever is queued, so
//     this wake succeeded and there is nothing to report.
//   - the conversation moved to a terminal. No claim predicate spans it, so
//     the queued row is never delivered. Reporting success there is a lie the
//     caller cannot detect, so it gets the same "the state moved under you"
//     answer the up-front guard gives.
//
// The distinction is exactly the displayed status, which is derived: `queued`
// and the active statuses are "a claim will drive it" and "a claim is driving
// it". `open` cannot appear — this wake's own undelivered row derives a parked
// conversation to `queued` — so everything else is a terminal.
//
// The queued row is left where it is on the refusing path. It is what the user
// typed, it is inert while the conversation stays terminal (nothing claims it),
// and a wake that later revives the conversation should carry it rather than
// find it deleted.
//
// A re-read that fails answers like a terminal: unable to prove delivery is
// coming, and a false success is worse here than a conflict the client
// resolves by refreshing.
//
// One shape reaches the terminal arm without having moved at all: a concluded
// step the CAS's hand-off guard refused after the pre-check let it through. The
// conflict is right (nothing claims it while the reactor still owes it a
// decision); only the log line's "went terminal" overstates what happened.
func (s *Spawner) lostWakeOutcome(ctx context.Context, orgID, conversationID string) error {
	conv, err := s.conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("resume: lost the wake race and could not re-read the conversation; reporting a conflict",
			"conversation", conversationID, "org_id", orgID, "error", err)
		return ErrConversationNotResumable
	}
	if conv == nil {
		return ErrConversationNotResumable
	}
	if conv.Status == domain.StatusQueued || domain.IsActiveConversationStatus(conv.Status) {
		return nil
	}
	delegateLog.Warn("resume: lost the wake race to a conversation that went terminal; the queued message will not be delivered",
		"conversation", conversationID, "org_id", orgID, "status", conv.Status)
	return ErrConversationNotResumable
}

// recordResumeTaskEvent puts a follow-up on its task's timeline.
//
// A resume deliberately moves nothing a person can see: the conversation goes
// back to queued, the blueprint is untouched, and a done task stays done. So
// this row — an event plus its task_events link — is the only trace the card
// keeps that work continued after it closed.
//
// Recorded, never published. This is audit linkage, not a routing signal, and
// no consumer should have to filter it back out of the bus.
//
// Best-effort throughout, and detached from the caller's ctx: the resume has
// already committed by the time this runs, so failing it over a missing audit
// row would turn a follow-up that worked into an error nobody can act on.
// run carries the pre-flip state — the whole point of the row is which rest
// state the follow-up woke.
func (s *Spawner) recordResumeTaskEvent(ctx context.Context, orgID, userID string, conv domain.Conversation) {
	if s.events == nil || s.tasks == nil || conv.TaskID == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	meta, err := json.Marshal(events.SystemConversationResumedMetadata{
		ConversationID: conv.ID,
		TaskID:         conv.TaskID,
		UserID:         userID,
		BlueprintRunID: conv.BlueprintRunID,
		FromStatus:     conv.Status,
		FromOutcome:    conv.Outcome,
	})
	if err != nil {
		delegateLog.Warn("marshal resume event metadata failed; the follow-up is not on the task timeline", "conversation", conv.ID, "error", err)
		return
	}
	eventID, err := s.events.RecordSystem(ctx, orgID, domain.Event{
		OrgID:        orgID,
		EventType:    domain.EventSystemConversationResumed,
		MetadataJSON: string(meta),
		OccurredAt:   time.Now().UTC(),
	})
	if err != nil {
		delegateLog.Warn("record resume event failed; the follow-up is not on the task timeline", "conversation", conv.ID, "task", conv.TaskID, "error", err)
		return
	}
	if err := s.tasks.RecordEventSystem(ctx, orgID, conv.TaskID, eventID, "resumed"); err != nil {
		delegateLog.Warn("link resume event to task failed", "conversation", conv.ID, "task", conv.TaskID, "event", eventID, "error", err)
	}
}

// workspaceRecoverable reports whether a parked run can still be resumed. Four
// rungs, most-certain-first, and only the last is a judgement call:
//
//   - the warm worktree survives on disk;
//   - the durable snapshot blob is present to cold-rehydrate from;
//   - neither, but the record says a persist is in flight. That is the gap a
//     park deliberately opens — the status flips before the capture runs — and
//     the claim path's own wait resolves it (see ensureWorkspace);
//   - neither, and no persist is coming (the record says failed, names a write
//     that never finished, or does not exist). The answer splits on the
//     runtime here, because the engines keep their continuity in different
//     places: native wakes into a workspace built from nothing, SDK is expired
//     without the session transcript the blob carried.
//
// A check we can't complete (no storage wired, a blob hiccup) counts as
// recoverable — a transient inability to verify must never strand a resumable
// run as expired, and the claim path re-checks for real.
func (s *Spawner) workspaceRecoverable(ctx context.Context, orgID string, conv *domain.Conversation) bool {
	if conv.WorktreePath != "" {
		if _, err := os.Stat(conv.WorktreePath); err == nil {
			return true
		} else if !os.IsNotExist(err) {
			// A stat we couldn't complete (permission, I/O) is not proof the
			// worktree is gone — count it as recoverable rather than emit a
			// false 410 on a transient error.
			delegateLog.Warn("resume: worktree stat inconclusive; treating as recoverable", "conversation", conv.ID, "path", conv.WorktreePath, "error", err)
			return true
		}
	}
	blobs := s.Storage()
	if blobs == nil {
		return true
	}
	keyID := memoryNamespace(conv.BlueprintRunID)
	ok, err := blobs.Exists(ctx, snapshotKey(orgID, keyID))
	if err != nil {
		delegateLog.Warn("resume: snapshot existence check failed", "conversation", conv.ID, "error", err)
		return true
	}
	if ok {
		return true
	}
	state, sErr := s.snapshotStateFor(ctx, orgID, keyID)
	if sErr != nil {
		// Inconclusive, so recoverable: a store that cannot answer is not
		// evidence either way.
		delegateLog.Warn("resume: workspace snapshot state read failed; treating as recoverable", "conversation", conv.ID, "error", sErr)
		return true
	}
	if state != nil && state.State == domain.WorkspaceSnapshotPending {
		return true
	}
	return conv.Runtime == domain.ConversationRuntimeNative
}

// ResumeOptions configures a ResumeWithMessage invocation. Callers
// populate these from the values they captured at the original run's
// start, so a resume reuses the exact model / repo / tool context the run
// began with rather than re-resolving live state mid-run.
type ResumeOptions struct {
	// Model is the model the run started with. **Required** — a resume
	// must reuse the model captured at run start (conv.Model, captured by
	// dispatchResumeClaim), never a freshly-resolved one, or a config change
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
	// (dispatchResumeClaim → conv.BlueprintRunID).
	Namespace string

	// TeamID is the conversation's owning team. Resolves the presence-gated
	// absent-auto-deny policy for the resumed conversation's permission
	// prompts (TFAC-392), and stamps the resumed conversation's
	// agenthost.ConversationInfo.TeamID so the capture writers can attribute
	// artifacts (TFAC-458). The claim captures it from the conversation row
	// (conv.TeamID, NOT NULL); empty falls back to the schema defaults for
	// the absent-auto-deny resolve.
	TeamID string

	// sidecar, when non-nil (TF_ROLE=executor), is the run network +
	// credential sidecar the dispatcher stood up for this resume turn.
	// ResumeWithMessage threads it into agentproc.RunOptions and the agenthost;
	// the dispatcher owns its teardown. nil on all/local.
	sidecar *runSidecar

	// localGit is the local-mode per-engagement Git proxy started before a
	// cold rehydrate. The dispatcher owns its teardown.
	localGit *localGitChannel

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
// nil outcome. Callers decide how to interpret a nil Completion —
// dispatchResumeClaim treats it as a session-level failure and surfaces an
// error.
type ResumeOutcome struct {
	Completion *agentproc.Result
	Result     *agentResult
	StderrText string
}

// ResumeWithMessage resumes a prior headless claude session with a new
// user message and streams the result through the same message-
// persistence path as the initial invocation. The cold-resume backstop for an
// open run when the warm process is gone.
//
// Callers pass the sessionID captured during the initial run (read
// from conversations.sdk_session_id, populated on the conversationSink during the original
// invocation), the cwd the original run used so the resumed
// subprocess sees the same worktree, and the user message to append
// to the conversation. The conversationID is reused so resumed messages append
// to the existing messages stream — the UI sees one coherent
// conversation.
//
// This helper does NOT update conversations status. The caller manages
// lifecycle: the memory-gate retry loop keeps the run in its current
// state during retries and only finalizes once the gate passes or
// gives up. Mirroring the initial invocation's status updates here
// would produce double terminal completion writes with stale
// cost/duration fields overwriting the real totals.
func (s *Spawner) ResumeWithMessage(ctx context.Context, orgID, conversationID, sessionID, cwd, message string, opts ResumeOptions, triggerType, creatorUserID string) (*ResumeOutcome, error) {
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
	// switch models underneath a single logical run. The resume claim captures
	// and passes it (conv.Model); an empty value is a wiring bug, surfaced loudly.
	if opts.Model == "" {
		return nil, fmt.Errorf("resume: missing model (caller must pass the model captured at run start)")
	}
	model := opts.Model

	// This engagement's questions die with it: once it lets go, any prompt
	// still open was asked by a process that no longer exists. Best-effort
	// tidy for audit reads only — ListPending derives the same answer from the
	// claim, so a crash that never reaches this defer costs nothing.
	defer s.ExpirePermissionsForClaim(orgID, opts.claimID)

	selfBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own binary path: %w", err)
	}

	extraEnv := []string{
		"TRIAGE_FACTORY_CONVERSATION_ID=" + conversationID,
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
	if opts.sidecar != nil {
		startAgentHost = func() (sandbox.Mount, io.Closer, error) {
			return agenthost.SocketMountFor(conversationID), noopCloser{}, nil
		}
	}

	// A resumed run gets the same gh channel shape the initial invocation had:
	// the sidecar's in multi, a fresh loopback injector in local (the channel is
	// per-invocation, not per-run — a cold resume starts a new subprocess, so it
	// needs its own listener, placeholder and trust file).
	ownerForGH, _, _ := strings.Cut(opts.RepoEnv, "/")
	localGH, localGHCloser := s.startLocalGHChannel(ctx, orgID, conversationID, ownerForGH, agenthost.ConversationInfo{
		OrgID:            orgID,
		UserID:           creatorUserID,
		ConversationID:   conversationID,
		TeamID:           opts.TeamID,
		IsEventTriggered: triggerType == domain.TriggerTypeEvent,
	}, opts.localGit.handler())
	defer func() { _ = localGHCloser.Close() }()
	ghChannel := opts.sidecar.ghChannel(conversationID)
	if ghChannel == nil {
		ghChannel = localGH
	}
	if opts.localGit != nil {
		extraEnv = append(extraEnv, githooks.PushCaptureEnvVar+"="+githooks.PushCaptureProxy)
		// The stand-down above is only correct while every push really does
		// transit the proxy, so the SSH transport joins it here rather than
		// reaching the org host on its own.
		extraEnv = append(extraEnv, opts.localGit.sshBridgeEnv()...)
	}

	// A resume gets the same isolation the initial invocation had — the
	// namespace is per-invocation, like the gh channel above, so a cold resume
	// builds its own plan and its own agenthost daemon over the same run tree.
	localSbx, err := s.startLocalSandbox(conversationID, opts.Namespace, ghChannel, agenthost.ConversationInfo{
		OrgID:            orgID,
		UserID:           creatorUserID,
		ConversationID:   conversationID,
		TeamID:           opts.TeamID,
		IsEventTriggered: triggerType == domain.TriggerTypeEvent,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = localSbx.Close() }()

	baseOpts := agentproc.RunOptions{
		Cwd:            cwd,
		Model:          model,
		PermissionMode: s.resolveSDKPermissionMode(ctx, opts.TeamID),
		SessionID:      sessionID,
		Message:        message,
		// gh is granted only alongside a live channel — see runAgent's note.
		AllowedTools: agentproc.BuildAllowedToolsFor(agentproc.AllowedToolsOptions{
			SelfBin: selfBin,
			Extras:  opts.ExtraAllowedTools,
			GH:      ghChannel != nil,
		}),
		MaxTurns:        100,
		ExtraEnv:        extraEnv,
		TraceID:         conversationID,
		MemoryNamespace: opts.Namespace,
		OrgID:           orgID,
		Secrets:         s.getRunSecrets(),
		LLMResolver:     s.llmResolverForConversation(orgID, conversationID),
		// This turn's own engagement pays for this turn's jail.
		ClaimID:              opts.claimID,
		RecordSandboxActuals: s.recordSandboxActuals,
		// Multi mode: launch into the prebuilt network + the sidecar's proxy
		// env; the sidecar holds the credentials. nil in local (no sandbox).
		PrebuiltNetwork:  opts.sidecar.runNetwork(),
		PrebuiltProxyEnv: opts.sidecar.jailEnv(),
		StartAgentHost:   startAgentHost,
		GHChannel:        ghChannel,
		GitConfigPairs:   opts.localGit.configPairs(ghChannel),
		LocalSandbox:     localSbx.runSpec(),
		// Re-mount the step skill this run's original claim staged, when it's
		// still on disk — a resume continues the same step, so it should see the
		// same skill it started with.
		SkillsSourcePath: stagedStepSkillsSource(conversationID),
		// Same for the prior-memory tree: a resume continues the same step, so it
		// reads the same handoff it started with rather than a freshly rendered one.
		MemorySourcePath: stagedEntityMemorySource(conversationID),
	}
	sink := newConversationSink(s, orgID, conversationID, opts.claimID, triggerType, creatorUserID)

	// Off-allowlist tool calls route the same way the initial run does: a
	// gVisor-jailed SDK run auto-approves (the sandbox + the static allowlist +
	// the enumerated agenthost RPC surface are the actual boundary, not a
	// prompt nobody is there to answer); an unjailed one keeps the
	// presence-gated browser round-trip since the allowlist is its only
	// boundary. opts.TeamID falls back to defaults when empty.
	var perms agentproc.PermissionHandler
	if agentproc.WillSandbox() {
		perms = s.AutoApprovePermissionHandler(conversationID)
	} else {
		perms = s.BrowserPermissionHandler(orgID, conversationID, opts.claimID, s.resolveAbsentAutoDeny(ctx, opts.TeamID))
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
			// resumes (idleTimeout 0 below), so markConversationOpen — the only
			// taskID consumer — is unreachable here. (It also degrades safely
			// to a no-op board recompute if a future change re-enables idle.)
			park: liveParkContext{
				orgID:          orgID,
				conversationID: conversationID,
				namespace:      opts.Namespace,
				claudeCwd:      cwd,
				triggerType:    triggerType,
				creatorUserID:  creatorUserID,
				claimID:        opts.claimID,
				runtime:        domain.ConversationRuntimeSDK,
			},
			opts:        baseOpts,
			perms:       perms,
			sink:        sink,
			idleTimeout: 0,
		})
	} else {
		out = s.runOneShot(ctx, baseOpts, sink)
	}

	// A fence trip on this path — the sink's refused insert, or the driver's
	// refused park — surfaces as an outcome with no completion, and the caller
	// routes that through failConversation. That is the correct disposition and it is
	// safe to reach: failConversation's terminal is itself claim-fenced, so the refusal
	// repeats there and nothing lands on the successor's row. No dedicated
	// fenced field on ResumeOutcome, because there is no action it would
	// unlock that isn't already refused one layer down.
	outcome := &ResumeOutcome{}
	outcome.Completion = out.result
	outcome.StderrText = out.stderr
	if out.result != nil {
		outcome.Result = parseAgentResult(out.result.Result)
	}

	if out.err != nil && out.result == nil {
		// The driver returns ctx.Err() directly when ctx triggered the kill
		// before any completion was captured; preserve that shape so the
		// resume goroutine's ctx.Err() check still routes through the cancel park.
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		return outcome, fmt.Errorf("agent runtime resume failed: %w (stderr: %s)", out.err, outcome.StderrText)
	}

	return outcome, nil
}
