// User-driven cancellation and the failure-finalization helpers a
// cancelled or errored run uses to reach a terminal DB state +
// surface a toast.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Cancel aborts a run at any phase — clone, fetch, worktree setup, or agent execution.
// The goroutine handles cleanup (worktree removal, status update).
//
// userID identifies the actor for audit. User-initiated cancels
// (handler-driven) pass the requesting user's ID and the row-mark
// write routes under that user's synthetic claims. System-initiated
// cancels (router cleanup, pending-firing sweeps) pass "" and the
// write routes through the admin pool. Local mode handlers pass
// runmode.LocalDefaultUserID; multi-mode handlers extract from JWT
// claims.
func (s *Spawner) Cancel(orgID, runID, userID string) error {
	// Preflight: load the run under the caller's identity so a
	// cross-org runID surfaces as "not found" BEFORE we tear anything
	// down. The cancels map below is keyed only by runID, so without
	// this gate any caller who learns an active runID could fire its
	// goroutine cancel() regardless of which org owns the run — the
	// goroutine then writes the terminal row under its own captured
	// cfg.orgID and the cross-org actor is invisible to the audit
	// trail. User-initiated cancels gate via the app pool under the
	// caller's claims (RLS does the visibility check); system-
	// initiated cancels (router cleanup, drain sweeps) still scope
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
		return fmt.Errorf("no active run %s", runID)
	}

	// Route the hard-kill through the control seam: at N=1 it resolves the
	// registered ctx cancel from s.cancels; horizontal scaling swaps it for
	// a DB-signal to the executor that owns the run. A found handle SIGKILLs
	// the live process (the goroutine then writes the terminal cancelled
	// status when it observes ctx.Err()).
	if s.getController().Cancel(runID) {
		return nil
	}

	// No local handle. Per the reply-leg contract, cancel is fire-and-
	// forget cross-pod: the DB-only write below is already the source of
	// truth and already works cross-pod, so a best-effort signal to a live
	// remote owner only HASTENS the kill — never waited on, never affects
	// this call's outcome.
	s.signalCancelBestEffort(orgID, runID, run.ExecutorID)

	// No active goroutine — the run may be parked `open` with no subprocess to
	// kill. Mark it cancelled directly via DB.
	// MarkCancelledIfActive's status-NOT-IN filter handles every non-terminal
	// state, so this is also a defensive catch for any other "no goroutine but
	// row not terminal" edge case.
	//
	// We also have to drain the per-entity firing queue ourselves on
	// terminal exit. The active-goroutine cancel paths drain via
	// their goroutine defer (Delegate's defer / ResumeOpenRun's
	// defer); a Cancel() that hits this DB-only path has no defer to
	// piggy-back on, so an auto-fired run cancelled while parked
	// `open` would leave the entity's firing queue stuck
	// until some other run on that entity terminated. The preflight
	// above already loaded the run, so trigger_type is in hand; we
	// only need a separate task read to resolve entity_id for the
	// drain notify. Errors on that task read are swallowed: the flip
	// below decides whether to surface as "no active run" or proceed;
	// drain just won't fire if entityID stays empty.
	triggerType := run.TriggerType
	var entityID string
	if task, _ := s.tasks.GetSystem(context.Background(), orgID, run.TaskID); task != nil {
		entityID = task.EntityID
	}

	// User-initiated cancel: write under the cancelling user's
	// synthetic claims so RLS sees a legitimate user-attributed
	// transition. System-initiated cancel (router cleanup, drain
	// sweeps): admin pool, no user attribution. Detached context —
	// the request that triggered Cancel can be gone but the
	// terminal write still needs to land.
	//
	// Unfenced, deliberately: this is an outside actor ending a run, not an
	// engagement ending itself. The whole point of a cancel is to override
	// whichever executor holds the run — it even signals the remote owner
	// best-effort above — so gating it on claim ownership would break the
	// feature. The claim-fenced variants exist for the executor's own
	// self-cancel (handleCancelled, markCancelledAfterResume); do not route
	// this path through them.
	var (
		flipped bool
		err     error
	)
	bgCtx := context.Background()
	if userID != "" {
		err = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, userID, func(ts db.TxStores) error {
			f, mErr := ts.Conversations.MarkCancelledIfActive(bgCtx, orgID, runID, "user_cancelled", "Run cancelled by user")
			flipped = f
			return mErr
		})
	} else {
		flipped, err = s.agentRuns.MarkCancelledIfActiveSystem(bgCtx, orgID, runID, "system_cancelled", "Run cancelled by system")
	}
	if err != nil {
		return fmt.Errorf("mark cancelled: %w", err)
	}
	if !flipped {
		return fmt.Errorf("no active run %s", runID)
	}
	s.broadcastRunUpdate(orgID, runID, "cancelled")
	// This DB-only cancel path runs only with no live orchestrator goroutine —
	// the step had parked (open), so the orchestrator already
	// returned and the owning blueprint_run is stuck in 'running'. Finalize it
	// (cancel the blueprint_run, clean the shared worktree, discard the
	// blueprint_run-keyed snapshot) so neither the row nor the blob is orphaned.
	// The per-entity drain stays below, keyed off the run's trigger type so the
	// manual short-circuit holds.
	s.finalizeParkedBlueprintOnCancel(bgCtx, orgID, run, userID)
	if entityID != "" {
		s.notifyDrainer(orgID, triggerType, entityID)
	}
	return nil
}

// handleCancelled finalizes a run that exited via context cancel. wtPath
// is the worktree directory the run was using (empty for no-cwd Jira
// runs); we clean it up explicitly here in addition to runAgent's
// deferred cleanup so the bare-repo registration is pruned even if the
// goroutine returns through one of the early paths that doesn't reach
// the defer (e.g., setupErr before the defer is installed).
// claimID names the engagement writing this terminal. It matters more here
// than anywhere else in the fenced set: killing every live sandbox is exactly
// what a partition self-fence trip does, and each killed run's goroutine then
// arrives here to record its own cancellation. If the self-fence fired late —
// the failure this whole layer exists to backstop — that write would land on
// a conversation the reaper has already handed to a successor. Empty claimID
// keeps the unfenced write for paths with no claimed run in scope.
//
// Returns fenced: true when the terminal was refused. Nothing was recorded,
// nothing was broadcast, and the worktree was left alone.
func (s *Spawner) handleCancelled(orgID, runID string, startTime time.Time, wtPath, claimID, triggerType, creatorUserID string) (fenced bool) {
	elapsed := int(time.Since(startTime).Milliseconds())
	bgCtx := context.Background()
	var completeErr error
	switch {
	case claimID != "":
		completeErr = s.agentRuns.CompleteForClaimSystem(bgCtx, orgID, runID, claimID, "cancelled", 0, elapsed, 0, "cancelled", "Cancelled by user", "", "", "")
	case triggerType == "manual":
		completeErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			return ts.Conversations.Complete(bgCtx, orgID, runID, "cancelled", 0, elapsed, 0, "cancelled", "Cancelled by user", "", "", "")
		})
	default:
		completeErr = s.agentRuns.CompleteSystem(bgCtx, orgID, runID, "cancelled", 0, elapsed, 0, "cancelled", "Cancelled by user", "", "", "")
	}
	if errors.Is(completeErr, db.ErrClaimReleased) {
		// A successor owns the conversation, so this run's cancellation is
		// not its state to report. The worktree stays too — it may be the
		// workspace that successor is running in.
		delegateLog.Error("claim fence refused the cancellation terminal — a successor owns this conversation; recording nothing",
			"run_id", runID, "claim_id", claimID, "org_id", orgID, "error", completeErr)
		return true
	}
	if completeErr != nil {
		delegateLog.Warn("failed to record cancellation", "run_id", runID, "error", completeErr)
	}
	s.broadcastRunUpdate(orgID, runID, "cancelled")
	if wtPath != "" {
		// Best-effort cleanup; same rationale as the defer in runAgent.
		_ = worktree.RemoveAt(wtPath, runID)
	}
	return false
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
