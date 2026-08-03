// The conversation-level stop verb, plus the failure-finalization helpers a
// stopped or errored run uses to reach its final DB state + surface a toast.
//
// Stopping means one thing, and it means it for every conversation — an
// intermediate blueprint step, a final step, the only step, or a conversation
// with no blueprint at all. The agent stops, the conversation parks `open`,
// and everything outside the conversation freezes. Nothing continues until
// someone resumes it or, for task-driven work, dispositions the task.
//
// Cancellation is a real concept, but it belongs to the layers that own a
// lifecycle: the blueprint has cancel_requested / BlueprintRunStatusCancelled
// and the task has return-to-queue / mark-done. A stop that reached into the
// blueprint would make `open` mean two different things depending on a column
// the user cannot see — parked-and-resumable under a running blueprint,
// parked-and-dead under a terminal one, because the claim gate only ever
// drives steps of a running blueprint. So the stop verb leaves the blueprint
// alone and the callers that own a lifecycle one layer up spell their own
// cancellation (StopAndCancelBlueprint, below).
//
// The park keeps the workspace: the snapshot is written before the flip and
// the retention TTL is what eventually collects it, because the moment a user
// kills a wedged run is exactly the moment they are most likely to want the
// work back.
//
// A frozen blueprint is the deliberate cost of that. A stopped step leaves its
// blueprint 'running' with no queued step and no live claim, holding its
// worktree, until the conversation is resumed or the task is dispositioned —
// so every surface that counts live blueprints counts it.
//
// That cost is bounded to the stopped conversation's own task, and it has to
// stay that way. Auto-delegation gates on the task, so a frozen blueprint
// holds up only its own situation; the entity's other tasks keep firing. When
// the gate keyed on the entity instead, one stopped run silently halted every
// future automated firing on that pull request — and since the firing queue
// drains off a run reaching a terminal, nothing was left to reopen it.

package delegate

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
)

// ErrNoActiveRun is the answer when there is nothing to stop: the conversation
// is not visible to the caller, it already concluded, or a racing terminal
// reached the row first. It is deliberately one error for all three — telling
// an unauthorized caller which of them applies would confirm the id exists.
//
// It exists so callers can tell "nothing to stop" apart from "the stop
// failed": everything else this returns wraps an internal fault (a failed
// read, a failed park) and must not be reported as a missing run, nor echoed
// to a client.
var ErrNoActiveRun = errors.New("no active run")

// Stop ends a conversation's work at any phase — clone, fetch, worktree setup,
// or agent execution — and parks it `open`, resumable. Nothing outside the
// conversation moves: its blueprint keeps its status and its task keeps its
// disposition. The goroutine handles cleanup (worktree removal, status update).
//
// userID identifies the actor for audit. User-initiated stops
// (handler-driven) pass the requesting user's ID and the row-mark
// write routes under that user's synthetic claims. System-initiated
// stops (router cleanup, pending-firing sweeps) pass "" and the
// write routes through the admin pool. Local mode handlers pass
// runmode.LocalDefaultUserID; multi-mode handlers extract from JWT
// claims.
func (s *Spawner) Stop(orgID, runID, userID string) error {
	return s.stop(orgID, runID, userID, false)
}

// StopAndCancelBlueprint stops the conversation and finalizes its blueprint
// 'cancelled' alongside it. That second half belongs to callers that own a
// lifecycle one layer up and have already decided it is over — a task closed
// by the router, a task swiped by a user, a team archived. Nothing will resume
// those conversations, so freezing their blueprints 'running' would hold a
// worktree and inflate every live-blueprint count for work that is finished.
//
// A conversation-level stop must never route here: the terminal blueprint is
// exactly what makes a parked conversation unresumable.
func (s *Spawner) StopAndCancelBlueprint(orgID, runID, userID string) error {
	return s.stop(orgID, runID, userID, true)
}

// stop is the shared body of both verbs. cancelBlueprint gates the two places
// the blueprint layer is touched, and nothing else differs — one path, so the
// stop verb and the lifecycle teardown cannot drift apart in the parts they
// share.
func (s *Spawner) stop(orgID, runID, userID string, cancelBlueprint bool) error {
	// Preflight: load the run under the caller's identity so a
	// cross-org runID surfaces as "not found" BEFORE we tear anything
	// down. The cancels map below is keyed only by runID, so without
	// this gate any caller who learns an active runID could fire its
	// goroutine cancel() regardless of which org owns the run — the
	// goroutine then writes the terminal row under its own captured
	// cfg.orgID and the cross-org actor is invisible to the audit
	// trail. User-initiated stops gate via the app pool under the
	// caller's claims (RLS does the visibility check); system-
	// initiated stops (router cleanup, drain sweeps) still scope
	// the read by orgID but go through the admin pool because there
	// is no user identity to project.
	var (
		run          *domain.Conversation
		preflightErr error
	)
	if userID != "" {
		preflightErr = s.tx.SyntheticClaimsWithTx(context.Background(), orgID, userID, func(ts db.TxStores) error {
			r, e := ts.Conversations.Get(context.Background(), orgID, runID)
			run = r
			return e
		})
	} else {
		run, preflightErr = s.agentRuns.GetSystem(context.Background(), orgID, runID)
	}
	if preflightErr != nil {
		return fmt.Errorf("load run: %w", preflightErr)
	}
	if run == nil {
		return fmt.Errorf("%w %s", ErrNoActiveRun, runID)
	}
	// A run that already concluded has nothing to stop, and saying so here —
	// rather than letting the park write below discover it — is what keeps a
	// stale stop a pure no-op. It has to come before the blueprint signal:
	// a completed step whose blueprint is still advancing would otherwise have
	// its NEXT step cancelled by a click aimed at work that had already
	// finished.
	if domain.IsTerminalRunStatus(run.Status) {
		return fmt.Errorf("%w %s", ErrNoActiveRun, runID)
	}

	if cancelBlueprint {
		// The lifecycle caller's half. Raised FIRST — before anything is
		// killed and before this call's own write. Two things follow from the
		// ordering: whichever path disposes of the run (the reactor in the
		// run's own goroutine, or the DB-only write below) sees
		// cancel_requested and finalizes the blueprint 'cancelled', and the
		// claim gate stops handing this blueprint's steps out, so nothing
		// re-claims the run in the window between the kill and the finalize.
		s.requestBlueprintCancel(context.Background(), orgID, run.BlueprintRunID)
	}

	// Route the hard-kill through the control seam: at N=1 it resolves the
	// registered ctx cancel from s.cancels; horizontal scaling swaps it for
	// a DB-signal to the executor that owns the run. A found handle SIGKILLs
	// the live process; the goroutine then parks the run when it observes
	// ctx.Err(), and its reactor either freezes the blueprint (the stop verb)
	// or finalizes it off the signal raised above (the lifecycle teardown).
	if s.getController().Cancel(runID) {
		return nil
	}

	// No local handle. Per the reply-leg contract, the kill is fire-and-
	// forget cross-pod: the DB-only write below is already the source of
	// truth and already works cross-pod, so a best-effort signal to a live
	// remote owner only HASTENS the kill — never waited on, never affects
	// this call's outcome.
	s.signalCancelBestEffort(orgID, runID, run.ExecutorID)

	// No active goroutine — the run may be parked `open` with no subprocess to
	// kill. Park it directly via DB.
	// ParkOpen's status-NOT-IN filter handles every non-terminal state, so this
	// is also a defensive catch for any other "no goroutine but row not
	// terminal" edge case.
	//
	// We also have to drain the task's firing queue ourselves on
	// terminal exit. The active-goroutine kill paths drain via
	// their goroutine defer (Delegate's defer / the resume claim's
	// defer); a stop that hits this DB-only path has no defer to
	// piggy-back on, so an auto-fired run stopped while parked
	// `open` would leave the task's firing queue stuck
	// until some other run on that task terminated. The preflight
	// above already loaded the run, so both the trigger type and the
	// task the drain is keyed on are already in hand.
	triggerType := run.TriggerType

	// User-initiated stop: write under the stopping user's
	// synthetic claims so RLS sees a legitimate user-attributed
	// transition. System-initiated stop (router cleanup, drain
	// sweeps): admin pool, no user attribution. Detached context —
	// the request that triggered the stop can be gone but the
	// park still needs to land.
	//
	// Unfenced, deliberately: this is an outside actor ending a run, not an
	// engagement ending itself. The whole point of a stop is to override
	// whichever executor holds the run — it even signals the remote owner
	// best-effort above — so gating it on claim ownership would break the
	// feature. The claim-fenced variants exist for the executor's own
	// self-park (parkRunOpen with a claim in scope, parkCancelledAfterResume);
	// do not route this path through them.
	//
	// No snapshot is taken here: this path runs precisely when no live
	// engagement exists, so either the run already parked (and snapshotted on
	// its way down) or it never got far enough to have a workspace worth
	// capturing.
	var (
		flipped bool
		err     error
	)
	bgCtx := context.Background()
	park := db.ParkStopped("user_cancelled", "Run cancelled by user")
	if userID == "" {
		park = db.ParkStopped("system_cancelled", "Run cancelled by system")
	}
	if userID != "" {
		err = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, userID, func(ts db.TxStores) error {
			f, mErr := ts.Conversations.ParkOpen(bgCtx, orgID, runID, park)
			flipped = f
			return mErr
		})
	} else {
		flipped, err = s.agentRuns.ParkOpenSystem(bgCtx, orgID, runID, park)
	}
	if err != nil {
		return fmt.Errorf("park stopped run: %w", err)
	}
	if !flipped {
		return fmt.Errorf("%w %s", ErrNoActiveRun, runID)
	}
	s.broadcastRunUpdate(orgID, runID, "open")
	if cancelBlueprint {
		// This DB-only path runs only with no live orchestrator goroutine — the
		// step had parked (open), so the orchestrator already returned and no
		// reactor will see the signal raised above. Finalize the blueprint_run
		// here instead (cancel it, clean the warm worktree) so the row isn't
		// left 'running' with nothing coming. The snapshot is deliberately NOT
		// dropped: it is the parked workspace this stop just retained.
		//
		// The plain stop verb skips this entirely — a frozen blueprint is the
		// state it wants, and finalizing here is exactly what used to make a
		// stopped conversation permanently unresumable.
		s.finalizeParkedBlueprintOnCancel(bgCtx, orgID, run, userID)
	}
	// The task's drain is keyed off the run's trigger type so the manual
	// short-circuit holds.
	s.notifyDrainer(orgID, triggerType, run.TaskID)
	return nil
}

// classifyFailureKind maps a runtime error from the agent process to
// its machine-readable failure kind, via errors.Is on the chain —
// never message text. Anything that isn't the recognized memory-limit
// kill is a generic runtime crash.
func classifyFailureKind(err error) domain.RunFailureKind {
	if errors.Is(err, agentproc.ErrRunMemoryLimit) {
		return domain.RunFailureMemoryLimit
	}
	return domain.RunFailureCrash
}

// failRun records the infra-failure terminal for a run: guarded status flip,
// a failure row on the transcript, breaker + broadcast + snapshot cleanup.
//
// claimID names the engagement doing the failing, when there is one in scope
// — every path that reached the agent has it. The terminal then goes through
// the claim fence, so an executor that was reaped mid-run cannot bury a
// successor's live conversation under its own failure. Empty claimID keeps
// the unfenced behavior for the paths that have no engagement to speak for
// (cleanup and orchestration entries that never claimed the row).
//
// Returns fenced: true when the terminal was refused because the claim is
// released. Nothing was written, and the caller must not go on to react to
// the run's state either — the row it would read belongs to the successor.
func (s *Spawner) failRun(orgID, runID, taskID, claimID, triggerType, creatorUserID, errMsg string, kind domain.RunFailureKind) (fenced bool) {
	delegateLog.Error("run failed", "run_id", runID, "error", errMsg, "failure_kind", string(kind))

	bgCtx := context.Background()

	failMsg := &domain.Message{
		ConversationID: runID,
		Role:           "assistant",
		Subtype:        "text",
		Content:        "Error: " + errMsg,
		IsError:        true,
	}
	// The failure row goes in BEFORE the status flip, because the flip
	// releases the claim: on the fenced path an insert afterwards would name
	// a claim this call had just retired and be refused as if by a zombie.
	// Ordering it first also makes the fence's answer arrive before anything
	// irreversible happens — the flip below only runs for an engagement that
	// still owns the row.
	var insertErr error
	switch {
	case claimID != "":
		_, insertErr = s.agentRuns.InsertMessageForClaimSystem(bgCtx, orgID, claimID, failMsg)
	case triggerType == "manual":
		insertErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			_, ierr := ts.Conversations.InsertMessage(bgCtx, orgID, failMsg)
			return ierr
		})
	default:
		_, insertErr = s.agentRuns.InsertMessageSystem(bgCtx, orgID, failMsg)
	}
	if errors.Is(insertErr, db.ErrClaimReleased) {
		// Not this engagement's run to fail anymore. Everything below writes
		// or broadcasts about a conversation a successor is driving, so the
		// whole tail is skipped — including the breaker tick and the snapshot
		// discard, which would delete the workspace that successor resumes
		// from.
		delegateLog.Error("claim fence refused the failure terminal — a successor owns this conversation; recording nothing",
			"run_id", runID, "claim_id", claimID, "org_id", orgID, "error", insertErr)
		return true
	}
	if insertErr != nil {
		delegateLog.Warn("failed to record failure message", "run_id", runID, "error", insertErr)
	}

	// Guarded — if a terminal racing path (cancel, natural completion)
	// reached the row first, leave its status in place rather than
	// clobbering.
	var markErr error
	switch {
	case claimID != "":
		_, markErr = s.agentRuns.MarkFailedIfActiveForClaimSystem(bgCtx, orgID, runID, claimID, string(kind))
	case triggerType == "manual":
		markErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			_, mErr := ts.Conversations.MarkFailedIfActive(bgCtx, orgID, runID, string(kind))
			return mErr
		})
	default:
		_, markErr = s.agentRuns.MarkFailedIfActiveSystem(bgCtx, orgID, runID, string(kind))
	}
	if errors.Is(markErr, db.ErrClaimReleased) {
		// The release landed between the two writes. Same answer, same tail
		// to skip; the failure row already on the transcript is the one
		// artifact of this engagement that stands, and it is attributed to
		// this claim rather than the successor's.
		delegateLog.Error("claim fence refused the failure terminal — a successor owns this conversation; recording nothing further",
			"run_id", runID, "claim_id", claimID, "org_id", orgID, "error", markErr)
		return true
	}
	if markErr != nil {
		delegateLog.Warn("failed to mark run as failed", "run_id", runID, "error", markErr)
	}

	s.updateBreakerCounter(taskID, triggerType, "failed")
	s.broadcastRunFailed(orgID, runID, kind)

	// A failed run won't resume, so drop the workspace snapshot it may have
	// written when it parked (e.g. an idle hibernation that later failed
	// mid-resume). Keyed by the run's own id: for a blueprint step (whose
	// snapshot is keyed by blueprint_run_id) this is a harmless no-op and
	// terminateBlueprint owns that blob; for a run that never snapshotted it's
	// also a no-op. The single failure chokepoint covers every failRun caller
	// (the resume goroutine's three exits among them).
	s.discardWorkspaceSnapshot(bgCtx, orgID, runID)

	// Surface as a sticky error toast so the user sees the failure even when
	// they're not watching the runs page. A memory-limit kill gets copy that
	// says what happened and which knob to turn instead of echoing the raw
	// error prefix; everything else truncates the message — full stderr dumps
	// don't fit in a toast card.
	if kind == domain.RunFailureMemoryLimit {
		toast.Error(s.wsHub, orgID, fmt.Sprintf(
			"Run %s was stopped: it exceeded its memory limit. Raise TF_RUN_MEMORY_LIMIT_MB if it legitimately needs more.",
			shortRunID(runID)))
	} else {
		toast.Error(s.wsHub, orgID, fmt.Sprintf("Run %s failed: %s", shortRunID(runID), truncateToastMsg(errMsg, 160)))
	}
	return false
}

// truncateToastMsg caps an error message at maxLen runes with an ellipsis.
// Toasts show a short body; full errors belong in the runs log.
func truncateToastMsg(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}
