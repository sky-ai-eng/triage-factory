// The conversation-queue dispatcher + the blueprint state-machine reactor. A
// blueprint step is enqueued as a conversations row with no status at all — needing a
// driver is derived, not stored; the dispatcher claims it,
// rehydrates the shared workspace, runs the agent, and on terminal the reactor
// advances the blueprint_run (enqueue the next step / finalize / leave parked).
// Sequencing lives on blueprint_runs (current_step_index + cancel_requested), so
// no goroutine holds it and a mid-flight blueprint is crash-recoverable.
//
// Single-process, single worker today (one dispatcher; horizontal executors are
// a separate concern). The same claim mechanism scales to N workers — the
// Postgres claim already uses FOR UPDATE SKIP LOCKED.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
)

// maxClaimAttempts caps how many times the dispatcher re-claims one queue
// episode before giving up on it. A healthy engagement starts on attempt 1;
// consecutive failures with nothing ever started mean a deterministic fault,
// and stopping there is what keeps the dispatcher from spinning one row while
// the rest of the queue waits. What "giving up" means then is not one thing —
// see disposeOfExhaustedConversation.
//
// At the scan interval this is about ten seconds of automatic retry, which is
// the shape of the transient infrastructure faults it exists for. A fault that
// outlasts it is not one more retry away.
const maxClaimAttempts = 5

// Default cadences for RunDispatcher, exported so main can tune them and tests
// can drive the loop fast. The scan is the correctness backstop (a dropped wake
// only defers a claim to the next tick); the wake channel is the latency nudge.
const DefaultDispatchScanInterval = 2 * time.Second

// WakeDispatcher is the exported form of wakeDispatcher, for callers outside
// this package — the tf_wake NOTIFY listener (internal/app, TFAC-586) nudges
// the dispatcher the moment a cross-process EnqueueConversation/resume-enqueue lands,
// instead of waiting out the scan interval backstop.
func (s *Spawner) WakeDispatcher() {
	s.wakeDispatcher()
}

// wakeDispatcher nudges the dispatcher to drain now rather than wait for the
// next scan tick. Best-effort, non-blocking: a full buffer means a wake is
// already pending, so dropping this one loses nothing.
func (s *Spawner) wakeDispatcher() {
	if s.dispatchWake == nil {
		return
	}
	select {
	case s.dispatchWake <- struct{}{}:
	default:
	}
}

// RunDispatcher is the conversation-queue drain loop — the queue-driven
// orchestrator's worker. On boot it reconciles conversations/blueprint_runs
// stranded by a crash, then claims queued steps and drives each through
// runAgent + the reactor until ctx is cancelled. A nil ConversationQueueStore
// makes this a logged no-op.
func (s *Spawner) RunDispatcher(ctx context.Context, scanInterval time.Duration) {
	if s.conversationQueue == nil {
		dispatchLog.Warn("conversation-queue dispatcher not started: no ConversationQueueStore wired")
		return
	}

	// Mark the dispatcher live for the executor healthz probe
	// (dispatcher_alive) for as long as this loop runs; clear it on return
	// (ctx cancel / shutdown).
	s.dispatcherRunning.Store(true)
	defer s.dispatcherRunning.Store(false)

	s.reconcileConversationQueue(ctx)

	scan := time.NewTicker(scanInterval)
	defer scan.Stop()

	s.drainConversationQueue(ctx) // drain whatever survived the restart / boot reconcile

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.dispatchWake:
			s.drainConversationQueue(ctx)
		case <-scan.C:
			s.drainConversationQueue(ctx)
		}
	}
}

// reconcileConversationQueue is the boot crash-recovery sweep (decision #4). Runs left
// mid-flight by a crash (claimed/running/setup statuses) are re-queued so the
// dispatcher re-claims and re-runs them; a mid-flight blueprint thus resumes by
// re-running its current step. Dormant `open` runs are left parked — they resume
// through their own paths, not the queue.
func (s *Spawner) reconcileConversationQueue(ctx context.Context) {
	// No step-plan handling needed: a re-queued mid-flight run is re-claimed by
	// dispatchClaimedConversation, which reads the plan frozen on its blueprint_run (off
	// br.StepPlan), so the resumed step runs the same program it was minted with.
	//
	// Ownership-scoped (TFAC-578): this only sweeps rows this instance itself
	// claimed during an earlier boot (executorIdentity()), never a live
	// sibling's claimed/running work — see ConversationQueueStore.ResetProcessingConversations.
	executorID, bootEpoch := s.executorIdentity()
	n, err := s.conversationQueue.ResetProcessingConversations(ctx, executorID, bootEpoch)
	if err != nil {
		dispatchLog.Error("boot reconcile: reset in-flight conversations failed", "error", err)
		return
	}
	if n > 0 {
		dispatchLog.Info("boot reconcile: re-queued in-flight conversations stranded by a crash", "count", n)
	}

	// Mirror sweep for the opposite desync — child conversations left
	// non-terminal under an already-terminal blueprint_run. ResetProcessingConversations
	// above only requeues under a *running* parent, so these orphans are
	// invisible to it; left alone, an orphan still reading as claimable keeps
	// the dispatcher on phantom work and pins its feature branch in a worktree, requeuing any
	// sibling fetch forever. Cancel them so the row stops looking live (the
	// worktree.Cleanup sweep already reclaimed the on-disk dir for non-parked
	// conversations). The atomic cancel in MarkRunStatus prevents new desyncs; this heals
	// rows broken before that landed.
	c, err := s.conversationQueue.ReconcileOrphanedConversations(ctx)
	if err != nil {
		dispatchLog.Error("boot reconcile: cancel orphaned child conversations failed", "error", err)
		return
	}
	if c > 0 {
		dispatchLog.Info("boot reconcile: healed orphaned child conversations and conversation↔claim desyncs", "count", c)
	}
}

// drainConversationQueue claims queued runs and hands each to a goroutine, bounded by
// the process-wide concurrency semaphore, until the queue is empty (or ctx is
// cancelled). It acquires a slot BEFORE each claim, so at capacity it blocks on
// a free slot rather than reserving a run it can't yet run (no claim held over
// idle work). Each claimed run executes — setup, agent, reactor — off the
// dispatcher, so the loop keeps claiming the next run (up to the cap) without
// waiting for the previous one to finish. The claim is FOR UPDATE SKIP LOCKED,
// so this is the same mechanism a future N-worker dispatcher uses; the
// semaphore is what keeps a burst of queued steps from fanning into an
// unbounded number of agent subprocesses on one host.
func (s *Spawner) drainConversationQueue(ctx context.Context) {
	// Capture the semaphore once and use it for both acquire and release so a
	// startup-time SetMaxConcurrentRuns can't strand a token on a replaced
	// channel.
	sem := s.semaphore()
	for {
		if ctx.Err() != nil {
			return
		}
		// Memory guardrail: when available memory (cgroup-aware) is below the floor,
		// stop claiming — runs stay queued and the next scan tick (or a
		// wake) re-checks. Checked per iteration, not per drain, because
		// each claimed run consumes memory as it spawns. Deliberately
		// BEFORE the semaphore acquire so a gated host parks with no
		// slot held.
		if s.dispatchMemGated() {
			return
		}
		// Identity fence: a superseded instance identity (another process
		// re-registered this id — see fenceIdentity) must not stamp new
		// claims. Sticky until restart; in-flight runs finish untouched.
		if s.IdentityFenced() {
			return
		}
		// Partition self-fence: a heartbeat WRITE failure past the
		// self-fence deadline (checkPartitionSelfFence) — reversible, un-
		// fences on the next successful write; in-flight runs at the
		// moment of fencing were already killed by killAllLiveSandboxes.
		if s.PartitionFenced() {
			return
		}
		// Drain: an operator asked this instance to quiesce (the CLI drain
		// verb flips instances.draining; the next heartbeat reads it back —
		// see heartbeatOnce). Live runs finish or hibernate-on-idle; no new
		// claims start.
		if s.Draining() {
			return
		}
		// Acquire a concurrency slot BEFORE claiming, so we never flip a run to
		// 'running' that then sits idle waiting for a slot. Blocks at capacity
		// until a finishing run releases its slot; a shutdown breaks the wait
		// (in-flight runs are ctx-cancelled, and the boot reconcile re-queues
		// anything left mid-flight). The try-then-block split exists only to
		// observe which of the two happened: saturation episodes are
		// transition-logged so a backlog of queued runs is diagnosable from
		// the log rather than reading as a silent hang.
		select {
		case sem <- struct{}{}:
			s.noteCapAcquireImmediate(cap(sem))
		default:
			s.noteCapAcquireBlocked(ctx, cap(sem))
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		executorID, bootEpoch := s.executorIdentity()
		conv, err := s.conversationQueue.ClaimNextConversation(ctx, executorID, bootEpoch, s.claimPlacement())
		if err != nil {
			<-sem
			dispatchLog.Warn("claim next conversation failed; retrying on the next scan", "error", err)
			return
		}
		if conv == nil {
			<-sem
			return // queue drained — release the slot we acquired but didn't use
		}
		// Fleet telemetry: count the claim for this sampler interval (TFAC-589).
		s.claimCount.Add(1)
		// conv is a fresh per-iteration `:=` binding (not a loop variable), so each
		// goroutine captures its own; the deferred receive hands the slot back on
		// terminal.
		go func() {
			defer func() { <-sem }()
			s.dispatchClaimedConversation(ctx, conv)
		}()
	}
}

// dispatchClaimedConversation runs one claimed blueprint step: load its context,
// rehydrate the shared workspace, materialize the step skill, run the agent,
// then hand the terminal state to the reactor. Nothing that fails before the
// agent's first turn ends the conversation — it goes back on the queue for
// another attempt, and only an exhausted budget decides anything permanent
// (handlePreAgentFailure).
//
// Context split: the pre-agent setup reads honor the dispatcher ctx so a
// shutdown (or a future timeout) cancels them cleanly — a ctx-cancelled read
// returns early and leaves the claimed run 'running' for the next boot's
// reconcile to re-queue, never failing the blueprint on a clean shutdown. The
// terminal/reactor writes below deliberately stay on a detached
// context.Background(): once the agent has run, the blueprint MUST be advanced
// or finalized to avoid stranding it, so those must not be abortable by a
// shutdown mid-finalize (the same detached-terminal-write convention the rest of
// the spawner follows).
func (s *Spawner) dispatchClaimedConversation(ctx context.Context, conv *domain.Conversation) {
	orgID := conv.OrgID
	startTime := time.Now()

	// The engagement's trace root. It ends at agent-live rather than here, so
	// the deferred call is the deregister plus the backstop for a claim that
	// never reached the agent at all.
	ctx, endEngagement := s.beginEngagement(ctx, conv)
	defer endEngagement()

	// The claim-validity gates: two DB round trips and a queue peek, every one
	// of which can be the reason a claim took seconds to reach the sandbox, and
	// none of which the phase ladder can show (the first phase write is still
	// several steps away).
	gateCtx, gateSpan := tracer.Start(ctx, "engagement.claim_gates")
	gateOpen := true
	closeGate := func(outcome string, err error) {
		if !gateOpen {
			return
		}
		gateOpen = false
		gateSpan.SetAttributes(telemetry.Outcome(outcome))
		recordSpanError(gateSpan, err)
		gateSpan.End()
	}
	// Backstop only: every exit below closes the gate span with its own answer,
	// and this fires solely if a later edit adds one that forgets to.
	defer closeGate(engagementNotStarted, nil)

	br, err := s.blueprints.GetRunSystem(gateCtx, orgID, conv.BlueprintRunID)
	if err != nil || br == nil {
		if ctx.Err() != nil {
			closeGate(engagementShutdown, nil)
			s.endEngagement(conv.ID, engagementShutdown)
			return // dispatcher shutting down — leave the claimed run for boot reconcile
		}
		// The owning blueprint_run is gone — nothing to drive. Fail the orphaned
		// run so it leaves the queue rather than re-claiming forever.
		closeGate("blueprint_missing", err)
		s.failEngagement(conv.ID, fmt.Errorf("load blueprint_run: %v", err))
		s.failClaimedConversation(orgID, conv, fmt.Sprintf("load blueprint_run: %v", err))
		return
	}
	task, err := s.tasks.GetSystem(gateCtx, orgID, conv.TaskID)
	if err != nil || task == nil {
		if ctx.Err() != nil {
			closeGate(engagementShutdown, nil)
			s.endEngagement(conv.ID, engagementShutdown)
			return
		}
		closeGate("task_missing", err)
		s.failEngagement(conv.ID, fmt.Errorf("load task: %v", err))
		s.terminateBlueprint(orgID, br.ID, conv.TaskID, conv.TriggerType, conv.CreatorUserID, startTime,
			runConfig{orgID: orgID, teamID: conv.TeamID}, domain.BlueprintRunStatusFailed, fmt.Sprintf("load task: %v", err), conv.BlueprintStepIndex, true)
		return
	}

	// A claim the blueprint no longer authorizes: a cancel raced in between the
	// claim and now (the claim filters cancel_requested, but the window is
	// non-zero), or this conversation is not the one a finished blueprint left
	// drivable. Don't run the agent — park this claimed step and finalize the
	// blueprint if it's still running. Detached writes — this is a disposition,
	// not abortable by shutdown. Nothing ran under this claim, so there is no
	// workspace state to snapshot on the way down.
	//
	// The predicate is the claim gate's, re-applied on this side of the claim.
	// It has to be: reading it as "the blueprint is still running" is what would
	// park a follow-up on concluded work the instant it was claimed — the exact
	// silent, permanent failure that widening the gate is meant to end.
	if !blueprintDrivableForClaim(br, conv.BlueprintStepIndex) {
		closeGate(engagementCancelled, nil)
		s.endEngagement(conv.ID, engagementCancelled)
		if _, mErr := s.conversations.ParkOpenForClaimSystem(context.WithoutCancel(ctx), orgID, conv.ID, conv.ClaimID, db.ParkStopped(domain.ParkReasonBlueprintCancelled, "Blueprint cancelled by user")); errors.Is(mErr, db.ErrClaimReleased) {
			// This claim was released before it disposed of its own step, so
			// a successor holds the run now. Everything below acts on it —
			// consuming its pending input, broadcasting its state, finalizing
			// its blueprint — so none of it runs.
			dispatchLog.Error("claim fence refused the raced-cancel park — a successor owns this conversation; recording nothing",
				"conversation", conv.ID, "claim_id", conv.ClaimID, "org_id", orgID, "error", mErr)
			return
		} else if mErr != nil {
			dispatchLog.Warn("park raced-cancel step failed", "conversation", conv.ID, "error", mErr)
		}
		// This claim won't run the agent, so whatever was queued for it must
		// not survive to be mis-delivered on a later reclaim — discard it.
		// Skipped for a native conversation for the same reason the resume
		// routing below is: its undelivered rows are its ordinary input queue,
		// not staged resume messages, and flipping them delivered here would
		// silently drop turns from a cancelled-then-restarted conversation.
		if s.pendingInput != nil && undeliveredRowsFor(conv.Runtime) == inputRoleStagedResume {
			if _, _, _, cErr := s.pendingInput.Consume(context.WithoutCancel(ctx), orgID, conv.ID); cErr != nil {
				dispatchLog.Warn("clear pending input for raced-cancel step failed", "conversation", conv.ID, "error", cErr)
			}
		}
		s.broadcastConversationUpdate(orgID, conv.ID, "open")
		if br.Status == domain.BlueprintRunStatusRunning {
			cfg := runConfig{orgID: orgID, teamID: conv.TeamID, wtPath: br.WorktreePath, hasWT: br.WorktreePath != "" && task.EntitySource == "github"}
			s.terminateBlueprint(orgID, br.ID, task.ID, conv.TriggerType, conv.CreatorUserID, startTime, cfg,
				domain.BlueprintRunStatusCancelled, "cancelled", conv.BlueprintStepIndex, false)
		}
		return
	}

	// Past the authorization gate, so this claim will really run — decide the
	// model it runs on before either arm below picks it up. A refusal here is a
	// team whose default its own enable-set no longer includes: nothing has run
	// under this claim, so it takes the ordinary pre-agent failure path and the
	// message names the model.
	model, err := s.modelForClaim(ctx, orgID, br, *conv)
	if err != nil {
		s.failEngagement(conv.ID, err)
		// An enable-set refusal is settled, not transient: every retry re-reads
		// the same rows and refuses again, so the retry ladder would spend the
		// claim budget to arrive here anyway — and then say the runtime failed
		// to start, which is not what happened and points at the wrong fix. It
		// takes the exhausted disposition directly instead, which parks a
		// conversation with a transcript and fails a step that never ran.
		if errors.Is(err, domain.ErrModelNotEnabled) {
			s.disposeOfModelRefusal(orgID, br, *conv, err)
			return
		}
		s.handlePreAgentFailure(orgID, br, *conv, err)
		return
	}
	conv.Model = model

	// Resume-by-enqueue: queued input means this claim is NOT a
	// fresh/crash-reclaimed blueprint step — it's a parked/terminal-resumable
	// run woken by a user message and re-queued onto its own row
	// (SendMessage's follow-up path). Peek (not Consume) routes the claim to the
	// resume path — dispatchResumeClaim flushes the rows only once it is about
	// to deliver, so a crash during the intervening workspace rehydrate leaves
	// them for the next claim rather than losing the message.
	//
	// Native conversations never take this branch. Undelivered rows are how
	// ALL input reaches a native loop — the delegation's opening turn, a
	// user follow-up, the loop's own repair notice — so their presence says
	// nothing about whether this claim is a resume, and the loop drains them
	// itself on its first iteration. Routing one here would hand a native
	// conversation to a path that resumes a Claude session it never had.
	if s.pendingInput != nil && undeliveredRowsFor(conv.Runtime) == inputRoleStagedResume {
		if msg, userID, ok, perr := s.pendingInput.Peek(gateCtx, orgID, conv.ID); perr != nil {
			dispatchLog.Warn("peek pending input failed; falling through to the blueprint-step path", "conversation", conv.ID, "error", perr)
		} else if ok {
			// The resume path is this same engagement continuing, so it keeps
			// the root: its own bring-up and rehydrate become children of it.
			closeGate("resume", nil)
			s.dispatchResumeClaim(ctx, conv, task, msg, userID)
			return
		}
		// An SDK claim on a finished blueprint that carries no message to
		// deliver. Only a crash reaches this: the staged message was consumed
		// by an earlier claim that then died before concluding, or the peek
		// above failed outright. The step machinery below would re-run the
		// final step's mission from the top on work that is already concluded,
		// so park the conversation back and leave its workspace alone — the
		// next follow-up re-queues it with a message of its own.
		//
		// A native conversation never needs this: its input lives in the
		// transcript, so a re-claim with nothing new to say is just the loop
		// continuing, which is what it should do.
		if br.Status != domain.BlueprintRunStatusRunning {
			closeGate(engagementNoMessage, nil)
			s.endEngagement(conv.ID, engagementNoMessage)
			if _, mErr := s.conversations.ParkOpenForClaimSystem(context.WithoutCancel(ctx), orgID, conv.ID, conv.ClaimID, db.ParkIdle()); mErr != nil {
				dispatchLog.Warn("park message-less follow-up claim failed", "conversation", conv.ID, "error", mErr)
			}
			dispatchLog.Warn("follow-up claim on a finished blueprint carried no message; parked without running the step",
				"conversation", conv.ID, "blueprint_run", br.ID, "org_id", orgID)
			s.broadcastConversationUpdate(orgID, conv.ID, "open")
			return
		}
	}

	// TF_ROLE=executor: stand up the run network + credential sidecar + proxies
	// BEFORE workspace setup, so the pre-sandbox clone/GetPR and the agenthost
	// route through the sidecar (holding placeholders) while the orchestrator
	// holds no credential. nil on every other role (in-process proxy path) and
	// on an unwired fixture. A bring-up failure (brain not provisioning) is a
	// transient setup failure — requeue like any other. Torn down after the run.
	closeGate("passed", nil)

	// The per-conversation cancel handle, registered HERE — before the
	// sidecar, the git channel, the fetch and the clone — rather than at the
	// runtime call. A stop arriving during bring-up resolves this handle and
	// cancels stepCtx, which every setup call below runs under, so the clone
	// it interrupts returns and the exits below read the stop. Registered
	// after bring-up, a stop during it found no handle and took the DB-only
	// path: the row parked and the claim released while the setup goroutine,
	// never told, finished the clone and launched an agent into a conversation
	// its user had already stopped. The window that remains — the claim gate,
	// the model resolve and the queue peek above — is the same narrow one the
	// resume path accepts.
	//
	// The deferred deregister is the backstop for the exits between here and
	// the runtime call; the explicit one after the runtime keeps the handle's
	// lifetime around the agent exactly as it was.
	stepCtx, stepCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancels[conv.ID] = stepCancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.cancels, conv.ID)
		s.mu.Unlock()
		stepCancel()
	}()

	// stoppedDuringBringUp is the first question every bring-up exit asks of
	// its error: a setup call that returned because stepCtx was cancelled did
	// not fail, it was stopped, and the stop already decided the disposition.
	// The park here is the engagement's own half of it, through the fence — on
	// a user stop the verb has usually parked and released first, and the
	// refusal is the design working (see markConversationOpen). Without this
	// the exit would read the cancelled clone as a transient setup failure and
	// requeue (a no-op against a released claim, but the wrong story in the
	// trace) or, out of attempts, fail the blueprint behind a conversation the
	// user merely stopped. No snapshot: nothing this engagement built is a
	// workspace worth capturing yet, and a cold resume rebuilds from scratch.
	//
	// The dispatcher's own shutdown is not a stop and is read first by the
	// arms that distinguish it; here a cancelled parent means "not ours to
	// dispose of", so the answer is no.
	stoppedDuringBringUp := func() bool {
		if ctx.Err() != nil || stepCtx.Err() == nil {
			return false
		}
		s.endEngagement(conv.ID, engagementCancelled)
		s.markConversationOpen(stepCtx, liveParkContext{
			orgID:          orgID,
			conversationID: conv.ID,
			taskID:         task.ID,
			triggerType:    conv.TriggerType,
			creatorUserID:  conv.CreatorUserID,
			claimID:        conv.ClaimID,
			reason:         db.ParkStopped(domain.ParkReasonUserCancelled, ""),
			runtime:        conv.Runtime,
		})
		return true
	}

	sidecar, err := s.bringUpRunSidecar(stepCtx, orgID, conv, *task)
	if err != nil {
		if ctx.Err() != nil {
			s.endEngagement(conv.ID, engagementShutdown)
			return // dispatcher shutting down — leave the claimed run for boot reconcile
		}
		if stoppedDuringBringUp() {
			return
		}
		s.failEngagement(conv.ID, err)
		s.handlePreAgentFailure(orgID, br, *conv, err)
		return
	}
	defer sidecar.Close()
	localGit, err := s.startLocalGitChannel(stepCtx, orgID, *task, agenthost.ConversationInfo{
		OrgID:            orgID,
		UserID:           conv.CreatorUserID,
		ConversationID:   conv.ID,
		TeamID:           conv.TeamID,
		IsEventTriggered: conv.TriggerType == domain.TriggerTypeEvent,
	})
	if err != nil {
		if stoppedDuringBringUp() {
			return
		}
		s.failEngagement(conv.ID, err)
		s.handlePreAgentFailure(orgID, br, *conv, err)
		return
	}
	defer func() { _ = localGit.Close() }()

	// Sequence off the plan frozen at mint (br.StepPlan), not the live
	// blueprint_steps/prompts — an edit to the blueprint mid-flight must not
	// change what this run executes. The step + prompt are reconstructed from
	// the snapshot, so nothing on this path re-reads blueprint_steps/prompts.
	plan := br.StepPlan
	if len(plan) == 0 {
		s.failEngagement(conv.ID, errors.New("blueprint run has empty step plan"))
		s.terminateBlueprint(orgID, br.ID, task.ID, conv.TriggerType, conv.CreatorUserID, startTime,
			runConfig{orgID: orgID, teamID: conv.TeamID}, domain.BlueprintRunStatusFailed, "blueprint run has empty step plan", conv.BlueprintStepIndex, true)
		return
	}
	stepIdx := 0
	if conv.BlueprintStepIndex != nil {
		stepIdx = *conv.BlueprintStepIndex
	}
	if stepIdx < 0 || stepIdx >= len(plan) {
		s.failEngagement(conv.ID, fmt.Errorf("step index %d out of range", stepIdx))
		s.terminateBlueprint(orgID, br.ID, task.ID, conv.TriggerType, conv.CreatorUserID, startTime,
			runConfig{orgID: orgID, teamID: conv.TeamID}, domain.BlueprintRunStatusFailed, fmt.Sprintf("step index %d out of range", stepIdx), conv.BlueprintStepIndex, true)
		return
	}
	planStep := plan[stepIdx]
	step := planStep.Step(br.BlueprintID)
	stepPrompt := planStep.Prompt()

	// Resolve the run's GitHub client for the self-contained (all/local) path.
	// On the executor path sidecar is non-nil and setupGitHub routes GetPR
	// through the sidecar's GitHub-REST proxy instead — this client is unused
	// there (the executor's secret store is disabled).
	owner, repo := ownerRepoForTask(*task)
	var gh *ghclient.Client
	if sidecar == nil {
		gh = s.resolveGHClient(ctx, orgID, owner, repo)
	}

	// The blueprint_run is live on this step → place the task in_progress before
	// any (possibly slow) workspace setup, so the board reflects the work
	// immediately. The aggregate bounces in_progress ↔ in_review as steps
	// park/resume.
	s.recomputeTaskBoardColumn(orgID, task.ID)

	// Build (step 0, first claim) or rehydrate (later steps / crash re-claim) the
	// shared workspace. A transient setup failure requeues; a persistent one
	// fails the blueprint.
	cfg, err := s.buildStepConfig(stepCtx, orgID, br, *task, *conv, gh, sidecar, localGit)
	if err != nil {
		if stoppedDuringBringUp() {
			return
		}
		s.failEngagement(conv.ID, err)
		s.handlePreAgentFailure(orgID, br, *conv, err)
		return
	}
	cfg.orgID = orgID
	cfg.teamID = conv.TeamID
	cfg.claimID = conv.ClaimID
	cfg.isBlueprintStep = true
	cfg.blueprintRunID = br.ID
	cfg.blueprintStep = stepIdx
	cfg.sidecar = sidecar
	cfg.localGit = localGit

	// Increment the step prompt's usage, routed per trigger type.
	if conv.TriggerType == "manual" {
		if e := s.tx.SyntheticClaimsWithTx(ctx, orgID, conv.CreatorUserID, func(ts db.TxStores) error {
			return ts.Prompts.IncrementUsage(ctx, orgID, stepPrompt.ID)
		}); e != nil {
			dispatchLog.Warn("increment usage for step prompt failed", "prompt", stepPrompt.ID, "error", e)
		}
	} else if e := s.prompts.IncrementUsageSystem(ctx, orgID, stepPrompt.ID); e != nil {
		dispatchLog.Warn("increment usage for step prompt failed", "prompt", stepPrompt.ID, "error", e)
	}

	// Materialize this step's skill so step N only ever sees its own SKILL.md.
	// Two shapes, chosen by whether this host jails the agent:
	//
	//   - sandboxed (multi + Linux): stage it OUTSIDE the workspace, in an
	//     orchestrator-owned dir the launch bind-mounts read-only. The run tree
	//     belongs to step 0's sandbox uid from its first launch onward, and the
	//     orchestrator holds no capabilities after its exec-time drop, so writing
	//     a skill into the tree at step 1's setup is EACCES — the deterministic
	//     failure every multi-step blueprint hit. Isolation is structural here:
	//     the staging dir is keyed by this step's own conversation id and holds
	//     exactly one skill.
	//   - local: byte-identical to released behavior — write `.claude/skills`
	//     inside the worktree, wiping any prior step's first. No jail, and the
	//     orchestrator owns the tree, so nothing to change.
	slug := skills.SlugForBlueprintStep(stepIdx, stepPrompt.Name)
	_, skillSpan := tracer.Start(ctx, "engagement.stage_skill")
	skillErr := s.materializeStepSkill(&cfg, conv.ID, slug, stepPrompt, step.Brief)
	recordSpanError(skillSpan, skillErr)
	skillSpan.End()
	if skillErr != nil {
		s.failEngagement(conv.ID, skillErr)
		s.terminateBlueprint(orgID, br.ID, task.ID, conv.TriggerType, conv.CreatorUserID, startTime, cfg,
			domain.BlueprintRunStatusFailed, fmt.Sprintf("materialize step %d skill: %s", stepIdx, skillErr.Error()), &stepIdx, false)
		return
	}
	// The staging dirs outlive this function only while the step can still be
	// resumed: a parked (`open`) step's next claim re-mounts them. Every other
	// disposition — completed, failed, cancelled — is the end of the step, so
	// reclaim them. A leftover from a crash is swept at startup.
	//
	// The memory dir is re-derived rather than read off cfg: runAgent materializes
	// it (it needs the entity and the priors, which setup does not carry), and cfg
	// travels there by value, so the path this scope holds would always be empty.
	stepParked := false
	defer func() {
		if stepParked {
			return
		}
		if err := skills.RemoveStagedSkills(cfg.skillsSourcePath); err != nil {
			dispatchLog.Warn("remove staged step skill failed", "conversation", conv.ID, "step", stepIdx, "error", err)
		}
		if agentproc.WillSandbox() {
			removeStagedMemory(sandbox.TrustedMemorySourcePath(conv.ID))
		}
	}()

	// Position bit + tool extensions, exactly as the old loop set them.
	cfg.appendSysPrompt = nonterminalStepSysPrompt(stepIdx, len(plan))
	cfg.extraAllowedTools = s.collectExtraTools(stepPrompt.AllowedTools)

	// Next-step name comes from the frozen plan too — no live prompt read; an
	// empty name (impossible from a well-formed plan) falls back to "step N".
	var nextStepName string
	if stepIdx+1 < len(plan) {
		nextStepName = plan[stepIdx+1].PromptName
	}
	mission := buildBlueprintStepWrapperPrompt(*task, step, stepPrompt, slug, len(plan), nextStepName)

	toast.Info(s.wsHub, orgID, fmt.Sprintf("Blueprint step %d/%d: %s (%s)",
		stepIdx+1, len(plan), truncateToastMsg(stepPrompt.Name, 60), shortConversationID(conv.ID)))

	// conv.SessionID is empty on a first claim and non-empty when this run was
	// re-claimed mid-flight by a crash — runAgent resumes it when the warm
	// session survived, else starts fresh.
	// Fleet telemetry: the span from claim to agent start is this run's
	// bring-up cost (clone + worktree + sandbox launch) — the sampler's
	// spawn_p50_ms (TFAC-589). Recorded only on the path that actually reaches
	// agent start, so resume/cancel claims don't skew it.
	s.recordSpawnMS(time.Since(startTime))

	// The runtime ratchet decides which engine drives this claim. It is
	// stamped on the conversation at mint and never changes, so a transcript
	// that ran under one engine is never continued by the other — the native
	// rows do not reconstruct the SDK's session-file state, and the SDK's
	// session does not reconstruct the rows. Both engines report fenced the
	// same way: their engagement writes go through the claim fence, and a
	// refusal means a successor owns the conversation.
	var disp engagementDisposition
	if conv.Runtime == domain.ConversationRuntimeNative {
		disp = s.runNativeAgent(stepCtx, conv.ID, *task, mission, cfg, time.Now(), conv.Model, conv.TriggerType, conv.CreatorUserID)
	} else {
		disp = engagementDisposition{fenced: s.runAgent(stepCtx, conv.ID, *task, mission, cfg, time.Now(), conv.Model, conv.TriggerType, conv.CreatorUserID, conv.SessionID)}
	}

	// A stop during bring-up produces neither of the dispositions below — both
	// runtimes read a cancelled context first and answer with a park — so
	// without this the engagement would end as the unexplained not_started,
	// and a user cancel would be indistinguishable in the trace from a
	// shutdown. A no-op once the agent went live, which is the ordinary case.
	//
	// It must stay ABOVE stepCancel(): past that line stepCtx always reads
	// cancelled and the user-stop-vs-shutdown discriminator is gone.
	s.endEngagementIfStopped(conv.ID, ctx, stepCtx)

	s.mu.Lock()
	delete(s.cancels, conv.ID)
	s.mu.Unlock()
	stepCancel()

	// Fenced out: this executor's claim was released mid-run and a successor
	// is driving the conversation. The reactor below would read the row's
	// CURRENT state — the successor's, not this engagement's — and advance,
	// terminate, or close a task on the strength of it. Nothing was written,
	// nothing is reacted to, and the staged skill + memory dirs stay for
	// whoever holds the claim now.
	if disp.fenced {
		// No-op once the agent went live, which is the usual shape of a fence;
		// it only lands as the engagement's outcome when the claim was taken
		// before the runtime ever came up.
		s.endEngagement(conv.ID, engagementFenced)
		stepParked = true
		return
	}
	// The runtime never came up, so the agent never spoke. Nothing was
	// recorded and nothing is terminal — this goes back through the same
	// hand-back the workspace-setup failures use, and the staged dirs stay
	// for the retry that is seconds away (or for the next claim of a parked
	// step; only the poison pill ends the step and reclaims them).
	if disp.launchErr != nil {
		s.failEngagement(conv.ID, disp.launchErr)
		stepParked = s.handlePreAgentFailure(orgID, br, *conv, disp.launchErr)
		return
	}

	// Re-read the step run for its terminal status, then react. Detached ctx on
	// purpose: the agent has run, so we must read its terminal and advance/finalize
	// the blueprint even if the dispatcher is shutting down — skipping the reactor
	// here would strand the blueprint 'running' with no queued next step.
	// WithoutCancel rather than Background: it drops cancellation exactly as
	// before while keeping the engagement's span context, so the terminal
	// bookkeeping still lands in the run's own trace.
	stepConversation, err := s.conversations.GetSystem(context.WithoutCancel(ctx), orgID, conv.ID)
	if err != nil || stepConversation == nil {
		s.terminateBlueprint(orgID, br.ID, task.ID, conv.TriggerType, conv.CreatorUserID, startTime, cfg,
			domain.BlueprintRunStatusFailed, fmt.Sprintf("read step %d run after agent: %v", stepIdx, err), &stepIdx, false)
		return
	}
	// stepConversation loses OrgID/Attempts through Get; carry the claim's identity fields
	// the reactor needs (trigger type, creator, org) and the authoritative model
	// (stable across the blueprint) so the next enqueued step inherits it.
	stepConversation.OrgID = orgID
	stepConversation.TriggerType = conv.TriggerType
	stepConversation.CreatorUserID = conv.CreatorUserID
	stepConversation.Model = conv.Model
	// Same predicate reactToStepTerminal uses to leave the blueprint running: an
	// `open` step is dormant, not done, so its staged skill stays for the resume.
	stepParked = stepConversation.Status == "open"
	s.reactToStepTerminal(ctx, orgID, br, *stepConversation, cfg, startTime)
}

// dispatchResumeClaim delivers a resume-by-enqueue claim's durably-recorded
// message (TFAC-585): the delivery half of what the retired in-process resume
// goroutine used to do end-to-end. No blueprint-step machinery runs here —
// a resumed run isn't advancing to a new step, it's continuing its
// current one, so this bypasses the mission/skill/plan logic entirely and
// goes straight to ResumeWithMessage, exactly like the retired goroutine
// did. Any executor may run this — ensureWorkspace warm-reuses the
// worktree if this IS the executor that parked it, else cold-rehydrates
// from the durable S3 snapshot.
func (s *Spawner) dispatchResumeClaim(ctx context.Context, conv *domain.Conversation, task *domain.Task, agentMessage, userID string) {
	orgID := conv.OrgID
	blueprintRunID := conv.BlueprintRunID

	// disposed is set once processCompletion + the inline finalize/re-park
	// below have run. It stays false on an early failure/cancel exit,
	// which is the only case the defer's blueprint re-finalize has to
	// cover — mirrors the retired in-process goroutine's own
	// disposed/defer pair exactly (see git history), just relocated to
	// the claim path.
	//
	// The failure exits below assign it from failConversation's fenced result, which
	// is the second reason to skip the re-finalize and a stronger one: a
	// refused terminal means a successor engagement owns this conversation,
	// so finalizing its blueprint would terminate work that is still running.
	//
	// The pre-agent exits set it outright, for the opposite reason: they
	// dispose of nothing at all. The claim goes back on the queue, so the
	// blueprint has to stay exactly as it is for the retry to resume into.
	var disposed bool
	defer func() {
		// An early exit (missing fields / workspace failure / cancel
		// before processCompletion) leaves the step terminal but the
		// blueprint un-finalized. Without this, the blueprint strands
		// 'running' and its snapshot is orphaned (the reaper skips it
		// once the run is no longer resumable). ResumeBlueprintAfterResume
		// is the safe single authority: a cancelled/failed step maps to a
		// terminal blueprint (terminateBlueprint discards the snapshot); a
		// still-parked (open) step or an already-terminal blueprint
		// early-returns. The success path sets disposed=true, so this
		// never doubles up.
		if blueprintRunID != "" && !disposed {
			s.ResumeBlueprintAfterResume(orgID, conv.ID, userID)
		}
	}()

	if conv.SessionID == "" || conv.WorktreePath == "" || conv.Model == "" {
		s.failEngagement(conv.ID, errors.New("resume: claimed conversation missing session/worktree/model"))
		disposed = s.failConversation(orgID, conv.ID, task.ID, conv.ClaimID, "manual", userID, "resume: claimed conversation missing session/worktree/model", domain.ConversationFailureUnclassified)
		return
	}

	owner, repo := ownerRepoForTask(*task)
	var extraTools string
	if conv.PromptID != "" {
		if p, perr := s.prompts.GetSystem(ctx, orgID, conv.PromptID); perr == nil && p != nil {
			extraTools = s.collectExtraTools(p.AllowedTools)
		}
	}
	namespace := memoryNamespace(blueprintRunID)

	// Per-run cancel handle, mirroring dispatchClaimedConversation's own — a
	// Cancel() arriving in the narrow window before this registers falls
	// to the DB-only path (the same pre-existing accepted race a fresh
	// step claim has).
	stepCtx, stepCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancels[conv.ID] = stepCancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.cancels, conv.ID)
		s.mu.Unlock()
		stepCancel()
	}()
	if stepCtx.Err() != nil {
		// No workspace rehydrated yet, so markConversationOpen (the no-snapshot park)
		// rather than parkConversationOpen: there is nothing on disk to capture.
		s.endEngagementIfStopped(conv.ID, ctx, stepCtx)
		disposed = s.markConversationOpen(ctx, resumeParkContext(orgID, conv, task, userID))
		return
	}

	// Flush the queued input: routing only peeked it (dispatchClaimedConversation), so
	// the rows are still there and the delivery has to claim them.
	//
	// Deliver what the flush claimed, not what routing peeked: the queue
	// appends, so a message sent DURING the rehydrate is a row the peek never
	// saw and this flush just marked delivered. Keeping the peeked text would
	// swallow it — the one silent loss this whole path exists to prevent. A
	// failure here logs and proceeds on the peeked text.
	//
	// Called only where this claim is committed to disposing of the
	// conversation itself. Every exit above a call to it hands the claim back
	// with the rows untouched, so the retry re-delivers rather than resuming
	// into silence — which is also why a crash anywhere before delivery costs
	// nothing.
	flushPendingInput := func() {
		if s.pendingInput == nil {
			return
		}
		if msg, uid, ok, cErr := s.pendingInput.Consume(ctx, orgID, conv.ID); cErr != nil {
			dispatchLog.Warn("consume pending input before delivery failed", "conversation", conv.ID, "error", cErr)
		} else if ok && msg != "" {
			agentMessage = msg
			if uid != "" {
				userID = uid
			}
		}
	}

	// handBack is this path's pre-agent exit, the twin of the native runtime's
	// launchFailed: the resume's runtime did not come up, nothing was
	// delivered, and another attempt costs nothing. A cancelled ctx is read
	// first — a user who stopped the run mid-bring-up asked for a park, and
	// retrying it would restart exactly the work they stopped. No snapshot on
	// that park: no agent has touched the tree under this claim, so there is
	// nothing new to capture and the workspace it would resume onto is the one
	// already in the store.
	handBack := func(cause error) {
		disposed = true
		if stepCtx.Err() != nil {
			// A stop, not a failure: cause is whatever the bring-up was doing
			// when the cancel landed, which is not why this engagement ended.
			s.endEngagementIfStopped(conv.ID, ctx, stepCtx)
			disposed = s.markConversationOpen(ctx, resumeParkContext(orgID, conv, task, userID))
			return
		}
		s.failEngagement(conv.ID, cause)
		s.handlePreAgentFailure(orgID, nil, *conv, cause)
	}

	// TF_ROLE=executor: bring up the run network + credential sidecar + proxies
	// for the resumed agent turn. nil on all/local. Torn down after the resume
	// turn returns.
	//
	// It comes up BEFORE the rehydrate below, and that ordering is the whole
	// point: a resume of a finished blueprint cold-rehydrates by design (the
	// concluded run's worktree was torn down), and rebuilding a private repo's
	// worktree needs the network — the shared bare is a blobless partial clone,
	// so the rebuild's checkout triggers a lazy promisor fetch. The sidecar's
	// git proxy is the only credential an executor has for it.
	sidecar, esErr := s.bringUpRunSidecar(stepCtx, orgID, conv, *task)
	if esErr != nil {
		// Still pre-agent: the sidecar is part of the runtime coming up, so
		// the answer is another attempt, not a dead conversation.
		handBack(fmt.Errorf("bring up credential sidecar for resume failed: %w", esErr))
		return
	}
	defer sidecar.Close()
	localGit, lgErr := s.startLocalGitChannel(stepCtx, orgID, *task, agenthost.ConversationInfo{
		OrgID:            orgID,
		UserID:           userID,
		ConversationID:   conv.ID,
		TeamID:           conv.TeamID,
		IsEventTriggered: false,
	})
	if lgErr != nil {
		handBack(fmt.Errorf("bring up managed local Git channel for resume failed: %w", lgErr))
		return
	}
	defer func() { _ = localGit.Close() }()

	// The SDK path's resume carries its context in the session file rather than
	// in rows, so it has no claim-time notice to make honest — the provenance
	// is dropped here rather than threaded on to nothing. No fresh-workspace
	// builder either, for the same reason: that session file lived in the
	// snapshot, so a tree built without one has nothing to reconnect to.
	resumeCwd, _, werr := s.ensureWorkspace(stepCtx, orgID, conv, s.gitSeedFor(stepCtx, orgID, owner, repo, sidecar, localGit), nil)
	if werr != nil {
		// A rehydrate that failed is the resume runtime failing to come up,
		// and it fails for the same passing reasons the native path's jail
		// does. Hand the claim back rather than ending a conversation that
		// has been running perfectly well until now.
		handBack(fmt.Errorf("ensure workspace before resume failed: %w", werr))
		return
	}

	// A resume can only continue the conversation if the Claude session
	// transcript survived into the rehydrated workspace. When it didn't — the
	// parking executor snapshotted without it, or nothing restored it (a wiped
	// ephemeral run-root after a rebuild) — passing --resume anyway makes the
	// SDK fail with an opaque "No conversation found", 8 seconds in, with no
	// surfaced reason. Mirror the crash-reclaim guard (runAgent) and stop here
	// with an actionable failure instead. A silent fresh session is deliberately
	// NOT the fallback on this path: a message-resume means "keep going from
	// where we were", and an amnesiac session that only sees the new steering
	// line would confuse the agent (or re-do already-done work) worse than a
	// clear "start over" would.
	//
	// The one exit here that is a real terminal rather than a hand-back: a
	// lost session file is a diagnosis, not a hiccup, and retrying it four
	// more times would only spend the budget arriving at the same answer. So
	// the queued message is flushed on the way out — it stays in the
	// transcript, and there is no successor claim left to deliver it to.
	if !sessionTranscriptExists(resumeCwd, conv.SessionID) {
		s.failEngagement(conv.ID, errors.New("resume: session transcript did not survive"))
		flushPendingInput()
		disposed = s.failConversation(orgID, conv.ID, task.ID, conv.ClaimID, "manual", userID,
			"This run's chat session could not be restored (its transcript did not survive — most often the executor was restarted or rebuilt), so the conversation can't be resumed. Start a new request to continue this work.",
			domain.ConversationFailureSessionLost)
		return
	}

	// Past every hand-back: this claim now owns the conversation's outcome, so
	// the queued rows become this turn's message.
	flushPendingInput()

	repoEnv := ""
	if owner != "" && repo != "" {
		repoEnv = owner + "/" + repo
	}
	// Prepend the out-of-band <system-note> blocks that accumulated while
	// the run wasn't running — deferred to here (claim time), not the
	// enqueue step, so injections staged AFTER the enqueue are still
	// picked up (the flush is destructive, so composing it early would
	// risk losing anything staged in the gap).
	message := s.resumeSystemPrepends(stepCtx, orgID, conv) + agentMessage

	// The resume path's agent-live. It runs no phase ladder — a resume has no
	// fetch or clone to report — so this is where its setup ends and its turn
	// begins: every hand-back is behind us and the runtime is about to come up.
	s.endEngagement(conv.ID, engagementLive)

	outcome, rerr := s.ResumeWithMessage(stepCtx, orgID, conv.ID, conv.SessionID, resumeCwd, message, ResumeOptions{
		Model:             conv.Model,
		RepoEnv:           repoEnv,
		ExtraAllowedTools: extraTools,
		Namespace:         namespace,
		TeamID:            conv.TeamID,
		sidecar:           sidecar,
		localGit:          localGit,
		claimID:           conv.ClaimID,
	}, "manual", userID)
	if stepCtx.Err() != nil {
		// The agent worked in the rehydrated tree before the kill, so this
		// park snapshots it — the whole point of a stop being a park is that
		// the work survives the gesture.
		park := resumeParkContext(orgID, conv, task, userID)
		park.namespace, park.claudeCwd = namespace, resumeCwd
		disposed = s.parkConversationOpen(ctx, park, conv.SessionID)
		return
	}
	if rerr != nil {
		disposed = s.failConversation(orgID, conv.ID, task.ID, conv.ClaimID, "manual", userID, "resume failed: "+rerr.Error(), classifyFailureKind(rerr))
		return
	}
	if outcome.Completion == nil {
		disposed = s.failConversation(orgID, conv.ID, task.ID, conv.ClaimID, "manual", userID, "resume produced no completion", domain.ConversationFailureNoResult)
		return
	}

	// No inherited-memory fingerprint: a resume continues this run's own
	// conversation in its own tree, so the file at the fixed path is its work.
	parked, fenced := s.processCompletion(stepCtx, orgID, conv.ID, blueprintRunID, conv.ClaimID, *task, outcome.Completion, resumeCwd, nil, conv.SessionID, "manual", userID)
	if fenced {
		// A successor owns the conversation. Finalizing the blueprint off
		// this turn's result would terminate a run someone else is driving,
		// so the defer's re-finalize is suppressed along with everything
		// else.
		disposed = true
		return
	}
	// The resumed step reached a terminal state (it didn't go open again)
	// → hand back to the blueprint orchestrator to finalize.
	if !parked {
		s.ResumeBlueprintAfterResume(orgID, conv.ID, userID)
	}
	// The body owns the disposition now (re-parked, or finalized above),
	// so the defer's re-finalize must not fire on top of it.
	disposed = true
}

// resumeParkContext is the park a cancelled resume writes. A resume is always
// user-initiated whatever the run's original trigger, so it routes as manual
// under the resuming user; conv.ClaimID puts the write through the claim fence,
// because a resume whose executor was reaped mid-turn must not park the
// conversation its successor has picked up.
//
// The caller fills namespace/claudeCwd when there is a workspace worth
// snapshotting — see the two call sites, which differ on exactly that.
func resumeParkContext(orgID string, conv *domain.Conversation, task *domain.Task, userID string) liveParkContext {
	return liveParkContext{
		orgID:          orgID,
		conversationID: conv.ID,
		taskID:         task.ID,
		triggerType:    "manual",
		creatorUserID:  userID,
		claimID:        conv.ClaimID,
		reason:         db.ParkStopped(domain.ParkReasonUserCancelled, ""),
		runtime:        conv.Runtime,
	}
}

// reactToStepTerminal is the blueprint state-machine reactor: given a step run
// that has reached a terminal (or parked) state, advance the blueprint_run.
// The post-step switch:
// continue→enqueue-next, finish→complete+close, abort→leave-open,
// open→leave parked — now driven by the DB rather than a
// goroutine stack. It calls recomputeTaskBoardColumn on every transition so the
// board stays live under the queue model.
func (s *Spawner) reactToStepTerminal(ctx context.Context, orgID string, br *domain.BlueprintRun, stepConversation domain.Conversation, cfg runConfig, startTime time.Time) {
	// The reactor's writes are detached on purpose (see dispatchClaimedConversation's
	// context split): the agent has run, so the blueprint MUST be advanced or
	// finalized even if the dispatcher is shutting down. WithoutCancel is that
	// same detachment with the caller's values kept, so the advance lands in
	// the engagement's trace rather than as an unattributed orphan.
	ctx = context.WithoutCancel(ctx)
	triggerType := stepConversation.TriggerType
	creatorUserID := stepConversation.CreatorUserID
	stepIdx := 0
	if stepConversation.BlueprintStepIndex != nil {
		stepIdx = *stepConversation.BlueprintStepIndex
	}

	// Sequence-level cancel: a cancel requested while the step was running ends
	// the blueprint without enqueuing the next step. Re-read the signal — it may
	// have been raised after the claim. On a read error we proceed with the
	// pre-agent br: a cancel raised in this narrow window could then be missed and
	// enqueue one extra step (the documented claim-window trade-off), so log it.
	//
	// This check comes BEFORE the parked arm below, and that ordering is the
	// whole cancel path: a cancelled step now parks `open` rather than writing
	// a terminal of its own, so cancel_requested is the only thing left that
	// distinguishes "stopped because someone killed it" from "stopped between
	// turns". Read the park first and every cancel would strand its blueprint
	// 'running' forever.
	if fresh, err := s.blueprints.GetRunSystem(ctx, orgID, br.ID); err != nil {
		dispatchLog.Warn("reactor: refresh blueprint_run for cancel check failed; proceeding with pre-agent state (a cancel in this window may enqueue one extra step)", "blueprint_run", br.ID, "error", err)
	} else if fresh != nil {
		br = fresh
	}
	if br.Status != domain.BlueprintRunStatusRunning {
		return // already finalized by a racing cancel/terminate
	}
	// A terminal from a step the blueprint has moved past. Every transition
	// below reads the sequence's position off THIS conversation, so a stale one
	// advances backwards over the live step, re-enqueues it, or terminates the
	// blueprint out from under it — none of it recoverable by a later pass. The
	// claim gate refuses such a step; this is the far-side re-check, which
	// catches an engagement already in flight when it did.
	if !isCurrentBlueprintStep(br, stepConversation.BlueprintStepIndex) {
		dispatchLog.Error("reactor: terminal from a step the blueprint has moved past; ignoring it (no advance, no enqueue, no transition)",
			"blueprint_run", br.ID, "conversation", stepConversation.ID, "step", stepConversation.BlueprintStepIndex, "current_step", br.CurrentStepIndex, "status", stepConversation.Status)
		return
	}
	if br.CancelRequested {
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusCancelled, "cancelled", &stepIdx, false)
		return
	}

	// Parked mid-step with no cancel behind it: leave the blueprint running, the
	// worktree on disk, and the snapshot in the blob store for the resume path.
	// The aggregate column lands the task in_review. Only `open` parks now: a
	// step that queued a draft PR / pending review completes normally and the
	// orchestrator advances — the artifact is a sidecar, surfaced via the
	// derived approval column below.
	if stepConversation.Status == "open" {
		dispatchLog.Info("blueprint_run step paused; blueprint remains running", "blueprint_run", br.ID, "step", stepIdx, "status", stepConversation.Status)
		s.recomputeTaskBoardColumn(orgID, br.TaskID)
		return
	}

	switch stepConversation.Status {
	case "failed":
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusFailed, "step "+stepConversation.Status, &stepIdx, false)
		return
	case "completed":
		// fall through to the outcome decision below
	default:
		// Neither a terminal nor the park above, on a row this engagement just
		// wrote a terminal to: something re-queued or re-claimed the
		// conversation in between, so this status is a SUCCESSOR's, not a
		// wedged step. Same answer as a claim fence, for the same reason —
		// every transition below would be decided on someone else's state.
		// Failing the blueprint here is how a successor's liveness got read as
		// this engagement's corruption, unrecoverably.
		dispatchLog.Error("reactor: the step's re-read shows a successor's state, not this engagement's terminal; writing no blueprint transition",
			"blueprint_run", br.ID, "conversation", stepConversation.ID, "step", stepIdx, "status", stepConversation.Status)
		return
	}

	// Advance off the frozen plan (br was just refreshed via GetRunSystem, so its
	// StepPlan is the snapshot taken at mint), never the live blueprint_steps —
	// editing the blueprint mid-flight must not redirect a running execution.
	plan := br.StepPlan
	if len(plan) == 0 {
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusFailed, "blueprint run has empty step plan", &stepIdx, false)
		return
	}
	isFinal := stepIdx >= len(plan)-1
	decision, abortReason := blueprintDecisionForStepConversation(stepConversation.Outcome, isFinal)
	switch decision {
	case blueprintStepAdvance:
		next := stepIdx + 1
		task, err := s.tasks.GetSystem(ctx, orgID, br.TaskID)
		if err != nil || task == nil {
			s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
				domain.BlueprintRunStatusFailed, "load task for advance", &stepIdx, false)
			return
		}
		// Bump the durable sequencing pointer, then enqueue the next step. Order
		// matters: the pointer is set first so a crash between here and the
		// enqueue leaves current_step_index naming the step the boot reconcile
		// would re-drive.
		if _, err := s.blueprints.SetRunCurrentStepSystem(ctx, orgID, br.ID, next); err != nil {
			dispatchLog.Warn("set current_step_index for blueprint_run failed", "blueprint_run", br.ID, "error", err)
		}
		// The next step's pin is held to the team's set as it stands NOW, not as
		// it stood when this blueprint fired: a set narrowed mid-flight is the
		// case a check at the firing point cannot see. The step this advance
		// would mint has not run, so refusing it costs no work.
		teamModels, err := s.resolveModel(ctx, orgID, stepConversation.TeamID)
		if err != nil {
			s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
				domain.BlueprintRunStatusFailed, fmt.Sprintf("step %d: %v", next, err), &stepIdx, false)
			return
		}
		nextModel, err := stepModelOrInherit(plan[next].Model, stepConversation.Model, teamModels.Enabled())
		if err != nil {
			s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
				domain.BlueprintRunStatusFailed, fmt.Sprintf("step %d: %v", next, err), &stepIdx, false)
			return
		}
		if err := s.enqueueBlueprintStep(ctx, orgID, br.ID, *task, plan[next].Step(br.BlueprintID), nextModel, triggerType, br.TriggerID, creatorUserID, br.ActorAgentID); err != nil {
			s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
				domain.BlueprintRunStatusFailed, fmt.Sprintf("enqueue step %d: %v", next, err), &stepIdx, false)
			return
		}
		// The shared worktree stays on disk (cfg.isBlueprintStep kept runAgent
		// from cleaning it), so the next claim warm-reuses it. Nudge the
		// dispatcher to pick the step up now.
		s.recomputeTaskBoardColumn(orgID, br.TaskID)
		s.wakeDispatcher()
	case blueprintStepFinish:
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusCompleted, "", &stepIdx, false)
	case blueprintStepAbort:
		reason := abortReason
		if reason == "" {
			reason = stepConversation.OutcomeReason
		}
		s.terminateBlueprint(orgID, br.ID, br.TaskID, triggerType, creatorUserID, startTime, cfg,
			domain.BlueprintRunStatusAborted, reason, &stepIdx, false)
	}
}

// modelForClaim resolves the model one claim runs on.
//
// A claim under a running blueprint is a step of a plan, and it runs on the
// model frozen onto that step — the anti-drift rule the whole run path holds
// to, so a config change mid-blueprint never switches models underneath work
// already in progress.
//
// A claim under a blueprint that has already finished is something else: a
// follow-up on concluded work, whose model is a fresh decision rather than an
// inherited one. It gets the team's current default. The alternative is worse
// than it sounds — a blueprint's last step is often its cheapest, a mechanical
// assembly step someone pinned down on purpose, so inheriting would answer a
// person's first-ever follow-up with that step's model, at exactly the moment
// they are deciding whether follow-ups work at all.
//
// Nothing is written: the conversation row keeps the model its step ran on, and
// no blueprint step's model moves. Only this turn is re-modelled.
//
// A fresh decision is a decision that can be refused. When the team's default is
// one its enable-set no longer includes, this claim fails by name rather than
// answering with the step's old model — the org disabled that model, and running
// the follow-up on something the team can no longer pick would be the
// substitution R6 forbids wearing an inheritance costume. A step of a RUNNING
// blueprint never asks: its model was frozen at the firing and mid-blueprint
// work is not re-decided.
func (s *Spawner) modelForClaim(ctx context.Context, orgID string, br *domain.BlueprintRun, conv domain.Conversation) (string, error) {
	if br == nil || br.Status == domain.BlueprintRunStatusRunning {
		return conv.Model, nil
	}
	models, err := s.resolveModel(ctx, orgID, conv.TeamID)
	if err != nil {
		return "", err
	}
	model, err := models.RequireDefault()
	if errors.Is(err, domain.ErrModelNotEnabled) {
		return "", err
	}
	if err != nil {
		// The team names no default at all. The settings save refuses to clear
		// one, so this is a row written before it did, or a fixture with no
		// resolver wired. The step's own model is a worse answer than a fresh
		// decision, but it is a better one than no model — held to the same set
		// either way, because arriving by inheritance is not a licence to run a
		// model nobody may pick.
		if err := models.RequireModel(conv.Model); err != nil {
			return "", err
		}
		return conv.Model, nil
	}
	return model, nil
}

// stepModelOrInherit resolves the model a blueprint step runs on: its own pin
// (Prompt.Model, frozen into the plan as BlueprintPlanStep.Model) when it has
// one, otherwise the run's inherited model — the team default for step 0, the
// prior step's model on an advance.
//
// Two states, and there is no third. A step is unset and inherits, or it is
// pinned and runs on what it names. A pin is honored whatever it costs: nothing
// here ranks models or overrules a pin for being the expensive one, because
// ranking them would need a defensible basis for calling one better than
// another and TF asserts none.
//
// What a pin IS held to is the team's enable-set, and a pin outside it fails the
// step by name. That check belongs here rather than where the prompt was saved,
// because sets drift after a save — an org narrowing its set does not rewrite
// the pins already stored under the old one. Failing is the only honest answer:
// ignoring the pin would run the step on the inherited model, which is the
// substitution R6 forbids.
//
// enabled is the team's effective set, resolved with the default the step
// inherits so both come from one read of one moment.
func stepModelOrInherit(stepModel, inherited string, enabled domain.ModelSet) (string, error) {
	if stepModel == "" {
		return inherited, nil
	}
	if !enabled.Has(stepModel) {
		return "", fmt.Errorf(
			"%w: the blueprint step pins %s, which this team's enabled models do not include (%s) — re-pin the step or enable the model in Settings",
			domain.ErrModelNotEnabled, stepModel, enabled)
	}
	return stepModel, nil
}

// enqueueBlueprintStep mints a queued conversations row for step stepIndex
// of a blueprint_run. Shared by Delegate (step 0) and the reactor (every
// advance). actorAgentID is the executing bot, frozen on the blueprint_run
// at mint and passed through here so every step inherits the same
// conversations.actor_agent_id — resolved once at the delegation entry
// point, never re-derived from the task claim (which is empty at step 0 on
// the event path and cleared by a takeover). triggerID is likewise the
// blueprint_run's frozen firing event_handler, denormalized onto every
// step's conversations.trigger_id (empty for manual → NULL): the JOIN-free
// llm_spend view reads autonomous spend attribution off the conversations
// row alone (the usage by-rule breakdown, TFAC-478), so a step run without
// it would show as autonomous cost attributable to no rule.
// TriggeringEventID is deliberately NOT inherited — the replay fence
// relocated to blueprint_runs, and stamping it per step would collide a
// multi-step chain on the leftover conversations_event_trigger_fence index.
func (s *Spawner) enqueueBlueprintStep(ctx context.Context, orgID, blueprintRunID string, task domain.Task, step domain.BlueprintStep, model, triggerType, triggerID, creatorUserID, actorAgentID string) error {
	stepIdx := step.StepIndex
	conversationID := uuid.New().String()
	// Placement stamp (TFAC-587): the rendezvous winner for this run's
	// (org, repo) key, computed over live registry members here on the
	// enqueuing pod. Recomputed on every step (step 0 from Delegate, each
	// advance from the reactor) so it re-stamps against current membership
	// and never outlives one queue dwell. Empty = no affinity (placement off,
	// non-repo task, or a failed read) → the claim treats it as unowned.
	preferred := s.preferredExecutorFor(ctx, orgID, task, conversationID)
	_, err := s.conversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID:                  conversationID,
		TaskID:              task.ID,
		PromptID:            step.StepPromptID,
		Model:               model,
		TriggerType:         triggerType,
		TriggerID:           triggerID,
		CreatorUserID:       creatorUserID,
		ActorAgentID:        actorAgentID,
		BlueprintRunID:      blueprintRunID,
		BlueprintStepIndex:  &stepIdx,
		PreferredExecutorID: preferred,
	})
	return err
}

// freshStepWorkspace rebuilds a step's run tree from nothing by running the
// same source setup its blueprint's FIRST claim ran — the ensureWorkspace
// ladder's last resort, when the warm tree is gone and no snapshot is coming.
//
// Re-running the setup, rather than assembling a checkout here, keeps "fresh"
// one thing: each shape's construction is specific (a GitHub PR run fetches
// its pull request and lands on its head ref; a Jira or Slack run lands a bare
// run root the agent populates itself), and a second implementation would be a
// second definition of what a first launch produces. The setups key by br.ID,
// so the tree lands at the path the blueprint's steps share.
//
// Only the path is taken from the result — the caller has already
// reconstructed the rest of the step's config from the task.
func (s *Spawner) freshStepWorkspace(ctx context.Context, orgID string, br *domain.BlueprintRun, task domain.Task, conv domain.Conversation, gh *ghclient.Client, sidecar *runSidecar, localGit *localGitChannel) (string, error) {
	var (
		cfg runConfig
		err error
	)
	switch task.EntitySource {
	case "github":
		cfg, err = s.setupGitHub(ctx, orgID, conv.ID, conv.ClaimID, br.ID, conv.CreatorUserID, task, gh, sidecar, localGit)
	case "jira":
		cfg, err = s.setupJira(ctx, orgID, conv.ID, conv.ClaimID, br.ID, conv.CreatorUserID, task, gh)
	case "slack":
		cfg, err = s.setupSlack(ctx, orgID, conv.ID, conv.ClaimID, br.ID, conv.CreatorUserID, task, gh)
	default:
		return "", fmt.Errorf("unsupported task source: %s", task.EntitySource)
	}
	if err != nil {
		return "", err
	}
	return cfg.wtPath, nil
}

// buildStepConfig produces the runConfig for a claimed step. On the first claim
// of a blueprint (br.WorktreePath empty) it builds the shared worktree via the
// source-specific setup and stamps the resolved path onto the blueprint_run. On
// every later claim it reconstructs the lightweight config from the task and
// guarantees the shared worktree is on disk (warm reuse, or cold rehydrate from
// the durable snapshot via ensureWorkspace).
func (s *Spawner) buildStepConfig(ctx context.Context, orgID string, br *domain.BlueprintRun, task domain.Task, conv domain.Conversation, gh *ghclient.Client, sidecar *runSidecar, localChannels ...*localGitChannel) (runConfig, error) {
	var localGit *localGitChannel
	if len(localChannels) > 0 {
		localGit = localChannels[0]
	}
	if br.WorktreePath == "" {
		var (
			cfg runConfig
			err error
		)
		// The run-root is blueprint-scoped (shared across steps, rebuilt under the
		// same key on rehydrate), so setup keys it by br.ID; conv.ID stays the
		// per-run identity for the worktree_path / conversation_worktrees records.
		switch task.EntitySource {
		case "github":
			cfg, err = s.setupGitHub(ctx, orgID, conv.ID, conv.ClaimID, br.ID, conv.CreatorUserID, task, gh, sidecar, localGit)
		case "jira":
			cfg, err = s.setupJira(ctx, orgID, conv.ID, conv.ClaimID, br.ID, conv.CreatorUserID, task, gh)
		case "slack":
			cfg, err = s.setupSlack(ctx, orgID, conv.ID, conv.ClaimID, br.ID, conv.CreatorUserID, task, gh)
		default:
			err = fmt.Errorf("unsupported task source: %s", task.EntitySource)
		}
		if err != nil {
			return runConfig{}, err
		}
		// Whatever the source setup built, it built from nothing: this arm runs
		// only when the blueprint has no worktree recorded yet.
		cfg.workspace = domain.WorkspaceProvenanceFresh
		// Stamp the shared worktree path onto the blueprint_run so later steps
		// (and the resume/cancel cleanup) can reconstruct it.
		if _, e := s.blueprints.SetRunWorktreePathSystem(context.WithoutCancel(ctx), orgID, br.ID, cfg.wtPath); e != nil {
			dispatchLog.Warn("set worktree_path for blueprint_run failed", "blueprint_run", br.ID, "error", e)
		}
		return cfg, nil
	}

	// Later step (or crash re-claim): reconstruct config + ensure the shared
	// worktree exists. ensureWorkspace warm-returns the on-disk path or cold-
	// rebuilds it from the snapshot keyed by the blueprint_run id.
	// ClaimID travels with it: the rehydrate inside ensureWorkspace re-stamps
	// worktree_path, and that stamp is a fenced engagement write.
	convForWS := &domain.Conversation{ID: conv.ID, ClaimID: conv.ClaimID, WorktreePath: br.WorktreePath, BlueprintRunID: br.ID}
	cfg := runConfig{orgID: orgID}
	switch task.EntitySource {
	case "github":
		owner, repo, prNumber := parseGitHubTask(task)
		cfg.owner, cfg.repo, cfg.prNumber = owner, repo, prNumber
		cfg.scope = fmt.Sprintf("Repository: %s/%s\nPR: #%d", owner, repo, prNumber)
		cfg.toolsRef = s.toolsReferenceFor(ctx, orgID, conv.CreatorUserID, conv.ID, eventsource.KindGitHub)
		cfg.hasWT = true
		// Re-fetched rather than inherited from the first step: by now the
		// PR's history includes whatever the earlier steps pushed, which is
		// exactly what this step needs to see.
		cfg.prSkeleton = renderPRSkeleton(ctx, prReadClient(gh, sidecar), owner, repo, prNumber)
		// The rehydrate's git runs through this claim's own sidecar proxy — the
		// sandbox is already up (dispatchClaimedConversation brings it up before calling
		// here), so the proxy is live by the time the rebuild fetches.
		wt, prov, err := s.ensureWorkspace(ctx, orgID, convForWS, s.gitSeedFor(ctx, orgID, owner, repo, sidecar, localGit),
			func(ctx context.Context) (string, error) {
				return s.freshStepWorkspace(ctx, orgID, br, task, conv, gh, sidecar, localGit)
			})
		if err != nil {
			return runConfig{}, err
		}
		cfg.wtPath, cfg.runRoot, cfg.workspace = wt, wt, prov
	case "jira":
		cfg.scope = fmt.Sprintf("Jira issue: %s", task.EntitySourceID)
		cfg.toolsRef = s.toolsReferenceFor(ctx, orgID, conv.CreatorUserID, conv.ID, eventsource.KindJira)
		cfg.hasWT = false
		wt, prov, err := s.ensureWorkspace(ctx, orgID, convForWS, gitSeed{},
			func(ctx context.Context) (string, error) {
				return s.freshStepWorkspace(ctx, orgID, br, task, conv, gh, sidecar, localGit)
			})
		if err != nil {
			return runConfig{}, err
		}
		cfg.wtPath, cfg.runRoot, cfg.workspace = wt, wt, prov
	case "slack":
		cfg.scope = fmt.Sprintf("Slack thread: %s", task.EntitySourceID)
		cfg.toolsRef = s.toolsReferenceFor(ctx, orgID, conv.CreatorUserID, conv.ID, "slack")
		cfg.hasWT = false
		wt, prov, err := s.ensureWorkspace(ctx, orgID, convForWS, gitSeed{},
			func(ctx context.Context) (string, error) {
				return s.freshStepWorkspace(ctx, orgID, br, task, conv, gh, sidecar, localGit)
			})
		if err != nil {
			return runConfig{}, err
		}
		cfg.wtPath, cfg.runRoot, cfg.workspace = wt, wt, prov
	default:
		return runConfig{}, fmt.Errorf("unsupported task source: %s", task.EntitySource)
	}
	// The resolved path goes onto the STEP's own conversation row, not just the
	// blueprint's. That row is where the SDK resume reads the cwd to re-invoke
	// the session in — `claude --resume` keys its session storage by cwd — so a
	// step whose row carries no path is refused a follow-up for a reason that
	// has nothing to do with its state. The first-claim arm above gets this from
	// the source setups; every arm here produces cfg.wtPath, so one write after
	// the switch covers all three.
	//
	// Idempotent, and correct on both workspace paths: a re-claim resolves the
	// same warm path, and a cold rehydrate onto a fresh one writes the tree the
	// resumed session will actually run in.
	// A fence refusal is not this warning's case: "a follow-up will be refused"
	// describes a row left without a path, and a fenced-out engagement's row
	// has whatever the successor put there. setWorktreePath logs the ownership
	// loss itself; the consequence stated here would be a guess about a
	// conversation this executor no longer has any standing to describe.
	if err := s.setWorktreePath(context.WithoutCancel(ctx), orgID, conv.ID, conv.ClaimID, cfg.wtPath); err != nil && !errors.Is(err, db.ErrClaimReleased) {
		dispatchLog.Warn("set worktree_path for blueprint step failed; a follow-up to this conversation will be refused",
			"conversation", conv.ID, "blueprint_run", br.ID, "error", err)
	}
	return cfg, nil
}

// parseGitHubTask splits a GitHub PR task's "owner/repo#N" entity source id into
// its parts. prNumber is 0 when absent/unparseable; callers surface that as a
// setup failure.
func parseGitHubTask(task domain.Task) (owner, repo string, prNumber int) {
	return splitGitHubEntitySourceID(task.EntitySourceID)
}

// materializeStepSkill places one blueprint step's SKILL.md where this host's
// agent will find it, and records the staging path on cfg when there is one.
//
// The branch is the sandbox gate, not the run mode directly: a jailed agent
// reads its skill from a read-only bind mount of an orchestrator-owned staging
// dir (nothing TF writes ever touches the sandbox-owned run tree again after its
// first launch), while an un-jailed one keeps the released behavior of a
// `.claude/skills` directory inside the worktree.
//
// conversationID is the step's own conversation id — the staging key, which is what makes
// each launch's mount hold exactly that step's skill.
func (s *Spawner) materializeStepSkill(cfg *runConfig, conversationID, slug string, stepPrompt *domain.Prompt, brief string) error {
	if !agentproc.WillSandbox() {
		// Wipe first so step N+1 doesn't inherit step N's SKILL.md from the shared
		// worktree. Non-fatal, exactly as before: a stale sibling skill is a
		// discovery nuisance, a failed blueprint is not.
		if err := skills.WipeBlueprintSkills(cfg.wtPath); err != nil {
			dispatchLog.Warn("wipe skills failed", "conversation", conversationID, "error", err)
		}
		return skills.MaterializeStepSkill(cfg.wtPath, slug, stepPrompt, brief)
	}
	dir := sandbox.TrustedSkillsSourcePath(conversationID)
	if err := skills.StageStepSkill(dir, slug, stepPrompt, brief); err != nil {
		return err
	}
	cfg.skillsSourcePath = dir
	return nil
}

// stagedStepSkillsSource returns conversationID's step-skill staging dir when one is
// still on disk, else "". A resume re-invokes the agent in the same conversation
// and deliberately runs none of the blueprint-step machinery, so it re-mounts
// whatever the step's original claim staged rather than re-deriving the step
// from the frozen plan. Absent — a cold resume on an executor that never staged
// it, or after a startup sweep — the resumed agent continues from its transcript
// without the skill file; the mount is a discovery convenience, not the
// conversation's state.
func stagedStepSkillsSource(conversationID string) string {
	if conversationID == "" || !agentproc.WillSandbox() {
		return ""
	}
	dir := sandbox.TrustedSkillsSourcePath(conversationID)
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// engagementDisposition is what one claim's engagement leaves behind, as far
// as the dispatcher is concerned. The zero value is the ordinary case: the
// agent ran, its terminal is on the row, and the reactor should read it.
type engagementDisposition struct {
	// fenced means this engagement's claim was released mid-run and a
	// successor owns the conversation. Nothing was written and nothing may be
	// reacted to — the row now describes somebody else's work.
	fenced bool
	// launchErr means the engagement never reached the agent's first turn:
	// workspace setup, the jail, the tool host, the opening turn. Nothing was
	// recorded, so there is nothing to react to and nothing lost by trying
	// again — see handlePreAgentFailure.
	launchErr error
}

// handlePreAgentFailure disposes of a claimed run whose engagement never got
// as far as the agent's first turn — a workspace that would not build or
// rehydrate, a jail that would not start, a tool host that would not answer.
//
// Every one of those is infrastructure, and infrastructure fails
// transiently: a broker restart, an exhausted subnet, stale runsc state, a
// host briefly out of memory. So the answer is another attempt, through the
// claim itself — RequeueConversation releases the claim 'requeued', the conversation
// stays claimable, and the next scan re-drives it a couple of seconds later.
// Everything the engagement would have needed is untouched by construction,
// because nothing tore it down: the workspace snapshot, the worktree, the
// blueprint and the transcript are all exactly as the last engagement left
// them.
//
// The budget is what stops a deterministic failure from spinning the queue,
// and it is spent per queue episode, not per lifetime (see Conversation.
// Attempts) — a healthy engagement resets it, so a conversation resumed four
// times still meets its next hiccup with a full budget.
//
// Returns whether the conversation survived. A requeue and an exhausted park
// both leave the step live, so whatever this claim staged for it on disk is
// the next claim's to re-mount; false is the poison pill, and the step is over.
func (s *Spawner) handlePreAgentFailure(orgID string, br *domain.BlueprintRun, conv domain.Conversation, cause error) (survived bool) {
	if conv.Attempts >= maxClaimAttempts {
		return s.disposeOfExhaustedConversation(orgID, br, conv, cause)
	}
	dispatchLog.Warn("engagement failed before the agent ran, requeuing", "conversation", conv.ID, "attempt", conv.Attempts, "error", cause)
	if _, err := s.conversationQueue.RequeueConversation(context.Background(), orgID, conv.ID, cause.Error()); err != nil {
		dispatchLog.Warn("requeue conversation after a pre-agent failure failed", "conversation", conv.ID, "error", err)
	}
	return true
}

// disposeOfExhaustedConversation answers for a conversation that failed the
// same way maxClaimAttempts times over. What that means depends entirely on whether
// anything has been said yet, and the two answers are opposites.
//
// A conversation with a transcript is work in progress — a resumed thread, a
// woken park, a step re-claimed after a crash. Failing it would discard a
// workspace, a worktree and a blueprint over a runtime that never started, so
// it parks `open` instead, with a note on the transcript saying what happened
// and what to do about it. The user's next message re-arms a whole fresh
// budget; the note is also what the resumed model reads, so it knows why there
// is a gap.
//
// A first engagement has nothing to protect. Its blueprint step has never run,
// there is no workspace worth keeping, and a loud failure is the honest answer
// — it surfaces on the task and the auto-delegation breaker keeps its signal.
// That is today's poison pill, unchanged.
//
// A nil br is the resume path, which has no blueprint in scope and needs
// none: a resume continues a conversation that has already been driven, so
// it is the first case by construction.
func (s *Spawner) disposeOfExhaustedConversation(orgID string, br *domain.BlueprintRun, conv domain.Conversation, cause error) (survived bool) {
	if br == nil || s.conversationHasTranscript(orgID, conv.ID) {
		dispatchLog.Error("the runtime failed to start on every attempt; parking the conversation instead of failing it",
			"conversation", conv.ID, "attempts", conv.Attempts, "error", cause)
		s.parkAfterLaunchExhaustion(orgID, conv, cause)
		return true
	}
	dispatchLog.Error("workspace setup failed after attempts; failing blueprint", "conversation", conv.ID, "attempts", conv.Attempts, "error", cause)
	s.terminateBlueprint(orgID, br.ID, conv.TaskID, conv.TriggerType, conv.CreatorUserID, time.Now(),
		runConfig{orgID: orgID, teamID: conv.TeamID, wtPath: br.WorktreePath, hasWT: br.WorktreePath != ""},
		domain.BlueprintRunStatusFailed, cause.Error(), conv.BlueprintStepIndex, false)
	return false
}

// conversationHasTranscript reports whether anything has been said in this
// conversation yet — the same signal mintOpeningTurn and prepareInheritedMemory
// read to answer the same question.
//
// A read failure answers "yes". The two ways of being wrong are not
// symmetric: treating a live conversation as fresh destroys its workspace and
// fails its blueprint, while treating a fresh one as live costs a park nobody
// resumes and a task that has to be re-fired by hand.
func (s *Spawner) conversationHasTranscript(orgID, conversationID string) bool {
	if s.conversations == nil {
		return false
	}
	rows, err := s.conversations.ListForAssemblySystem(context.Background(), orgID, conversationID)
	if err != nil {
		dispatchLog.Warn("read transcript to decide an exhausted conversation's disposition failed; treating it as work in progress",
			"conversation", conversationID, "error", err)
		return true
	}
	return len(rows) > 0
}

// parkAfterLaunchExhaustion puts a conversation whose runtime would not start
// back to rest, with the reason on its transcript.
//
// The note is the whole point. Nothing else in the system explains a resume
// that silently never came up — the run station shows a parked conversation
// and the user is left to guess — so it says how many attempts were made, what
// the last one failed with, and that a message starts a fresh one. It goes in
// before the park, and through the claim fence like every other write this
// engagement makes: a successor that took the conversation while this one was
// failing to launch owns it, and neither the note nor the park is ours to
// write then.
//
// Whatever input was waiting is marked delivered on the way down. It is not
// discarded — it stays in the transcript, in order, where the next engagement
// reads it — but it must stop counting as a wake, or `open` plus an
// undelivered message would re-claim the conversation immediately and the
// budget would buy nothing at all.
func (s *Spawner) parkAfterLaunchExhaustion(orgID string, conv domain.Conversation, cause error) {
	s.parkWithStopNote(orgID, conv, domain.ParkReasonLaunchFailed,
		fmt.Sprintf("The runtime failed to start after %d attempts: %s. Send a message to retry.", conv.Attempts, cause),
		fmt.Sprintf("Run %s could not start: %s", shortConversationID(conv.ID), truncateToastMsg(cause.Error(), 160)))
}

// disposeOfModelRefusal answers for a claim whose model its team may no longer
// pick. It is disposeOfExhaustedConversation's split — park what has a
// transcript, fail a step that never ran — reached without the retries, since
// re-reading the same two settings rows cannot produce a different answer.
//
// The note is the refusal itself, which already names the model and the set
// that excludes it, so the person reading the transcript is told the one thing
// that fixes it. "Send a message to retry" is deliberately absent: a message
// would wake the conversation into this same refusal until somebody picks.
func (s *Spawner) disposeOfModelRefusal(orgID string, br *domain.BlueprintRun, conv domain.Conversation, cause error) {
	if br == nil || s.conversationHasTranscript(orgID, conv.ID) {
		dispatchLog.Warn("claim refused: the model this conversation would run on is not enabled for its team; parking",
			"conversation", conv.ID, "team", conv.TeamID, "error", cause)
		s.parkWithStopNote(orgID, conv, domain.ParkReasonModelNotEnabled,
			fmt.Sprintf("This conversation cannot continue: %s", cause),
			fmt.Sprintf("Run %s is paused: %s", shortConversationID(conv.ID), truncateToastMsg(cause.Error(), 160)))
		return
	}
	dispatchLog.Error("blueprint step refused: the model it would run on is not enabled for its team",
		"conversation", conv.ID, "blueprint_run", br.ID, "team", conv.TeamID, "error", cause)
	s.terminateBlueprint(orgID, br.ID, conv.TaskID, conv.TriggerType, conv.CreatorUserID, time.Now(),
		runConfig{orgID: orgID, teamID: conv.TeamID, wtPath: br.WorktreePath, hasWT: br.WorktreePath != ""},
		domain.BlueprintRunStatusFailed, cause.Error(), conv.BlueprintStepIndex, false)
}

// parkWithStopNote is the park every pre-agent stop lands on: a stop note on the
// transcript saying what happened, the waiting input settled so `open` does not
// immediately re-claim, the park itself fenced on this claim, and the two
// surfaces a person watching either the conversation or the board reads from.
//
// The note and the reason are the caller's because they are the only parts that
// differ, and they are what a person is actually told — a park that describes
// the wrong cause sends them to fix the wrong thing.
func (s *Spawner) parkWithStopNote(orgID string, conv domain.Conversation, reason domain.ParkReason, note, toastMsg string) {
	bgCtx := context.Background()
	if _, err := s.conversations.InsertMessageForClaimSystem(bgCtx, orgID, conv.ClaimID, &domain.Message{
		ConversationID: conv.ID,
		UserID:         conv.CreatorUserID,
		Role:           "user",
		Subtype:        domain.MessageSubtypeStopNote,
		Content:        note,
	}); err != nil {
		if errors.Is(err, db.ErrClaimReleased) {
			dispatchLog.Error("claim fence refused the stop note — a successor owns this conversation; recording nothing",
				"conversation", conv.ID, "claim_id", conv.ClaimID, "org_id", orgID)
			return
		}
		dispatchLog.Warn("record stop note failed; the park still lands", "conversation", conv.ID, "error", err)
	}
	if s.pendingInput != nil {
		if _, _, _, err := s.pendingInput.Consume(bgCtx, orgID, conv.ID); err != nil {
			dispatchLog.Warn("settle pending input before parking a conversation that could not start failed", "conversation", conv.ID, "error", err)
		}
	}
	if _, err := s.conversations.ParkOpenForClaimSystem(bgCtx, orgID, conv.ID, conv.ClaimID, db.ParkStopped(reason, "")); err != nil {
		if errors.Is(err, db.ErrClaimReleased) {
			dispatchLog.Error("claim fence refused the park — a successor owns this conversation",
				"conversation", conv.ID, "claim_id", conv.ClaimID, "org_id", orgID)
			return
		}
		dispatchLog.Warn("park conversation that could not start failed", "conversation", conv.ID, "error", err)
		return
	}
	s.broadcastConversationUpdate(orgID, conv.ID, "open")
	s.recomputeTaskBoardColumn(orgID, conv.TaskID)
	toast.Error(s.wsHub, orgID, toastMsg)
}

// failClaimedConversation marks an orphaned claimed conversation failed (its
// blueprint_run vanished, so there is nothing to drive). Best-effort, and
// fenced on this claim like every other terminal an engagement writes: if the
// claim is gone, a successor holds the conversation and reaches this same
// branch itself.
func (s *Spawner) failClaimedConversation(orgID string, conv *domain.Conversation, reason string) {
	dispatchLog.Error("marking conversation failed", "conversation", conv.ID, "reason", reason)
	_, err := s.conversations.MarkFailedIfActiveForClaimSystem(context.Background(), orgID, conv.ID, conv.ClaimID, "")
	if errors.Is(err, db.ErrClaimReleased) {
		dispatchLog.Error("claim fence refused the orphaned-conversation terminal — a successor owns this conversation; recording nothing",
			"conversation", conv.ID, "claim_id", conv.ClaimID, "org_id", orgID, "error", err)
		return
	}
	if err != nil {
		dispatchLog.Warn("mark orphaned conversation failed", "conversation", conv.ID, "error", err)
	}
}
