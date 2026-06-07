// User-driven cancellation and the failure-finalization helpers a
// cancelled or errored run uses to reach a terminal DB state +
// surface a toast.

package delegate

import (
	"context"
	"fmt"
	"log"
	"time"

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
		run          *domain.AgentRun
		preflightErr error
	)
	if userID != "" {
		preflightErr = s.tx.SyntheticClaimsWithTx(context.Background(), orgID, userID, func(ts db.TxStores) error {
			r, e := ts.AgentRuns.Get(context.Background(), orgID, runID)
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
	if s.controller.Cancel(runID) {
		return nil
	}

	// No active goroutine — the run may be parked in awaiting_input
	// with no subprocess to kill (SKY-139). Mark it cancelled directly
	// via DB. MarkAgentRunCancelledIfActive's status-NOT-IN filter
	// handles every non-terminal state, so this is also a defensive
	// catch for any other "no goroutine but row not terminal"
	// edge case.
	//
	// We also have to drain the per-entity firing queue ourselves on
	// terminal exit. The active-goroutine cancel paths drain via
	// their goroutine defer (Delegate's defer / ResumeAfterYield's
	// defer); a Cancel() that hits this DB-only path has no defer to
	// piggy-back on, so an auto-fired run cancelled while parked in
	// awaiting_input would leave the entity's firing queue stuck
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
	var (
		flipped bool
		err     error
	)
	bgCtx := context.Background()
	if userID != "" {
		err = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, userID, func(ts db.TxStores) error {
			f, mErr := ts.AgentRuns.MarkCancelledIfActive(bgCtx, orgID, runID, "user_cancelled", "Run cancelled by user")
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
	// the step had parked (yield / pending_approval), so the orchestrator already
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
func (s *Spawner) handleCancelled(orgID, runID string, startTime time.Time, wtPath, triggerType, creatorUserID string) {
	if s.wasTakenOver(runID) {
		// Takeover owns the DB row, the worktree, and the broadcast from
		// here on — it needs the temp worktree to stay on disk until its
		// copy completes, then will explicitly remove it. The cancel
		// that woke us up was just the mechanism for stopping the
		// headless process; everything else is Takeover's job.
		return
	}
	elapsed := int(time.Since(startTime).Milliseconds())
	bgCtx := context.Background()
	var completeErr error
	if triggerType == "manual" {
		completeErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			return ts.AgentRuns.Complete(bgCtx, orgID, runID, "cancelled", 0, elapsed, 0, "cancelled", "Cancelled by user", "", "")
		})
	} else {
		completeErr = s.agentRuns.CompleteSystem(bgCtx, orgID, runID, "cancelled", 0, elapsed, 0, "cancelled", "Cancelled by user", "", "")
	}
	if completeErr != nil {
		log.Printf("[delegate] warning: failed to record cancellation for run %s: %v", runID, completeErr)
	}
	s.broadcastRunUpdate(orgID, runID, "cancelled")
	if wtPath != "" {
		// Best-effort cleanup; same rationale as the defer in runAgent.
		_ = worktree.RemoveAt(wtPath, runID)
	}
}

func (s *Spawner) failRun(orgID, runID, taskID, triggerType, creatorUserID, errMsg string) {
	if s.wasTakenOver(runID) {
		// Takeover finalized this run; whatever error the goroutine
		// observed is downstream of the SIGKILL we sent it. Don't
		// overwrite taken_over with failed.
		return
	}
	log.Printf("[delegate] run %s failed: %s", runID, errMsg)

	bgCtx := context.Background()

	// Guarded — if a terminal racing path (takeover, cancel, natural
	// completion) reached the row first, leave its status in place
	// rather than clobbering. The wasTakenOver check above only
	// covers takeover; cancel and completion can still race here.
	var markErr error
	if triggerType == "manual" {
		markErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			_, mErr := ts.AgentRuns.MarkFailedIfActive(bgCtx, orgID, runID)
			return mErr
		})
	} else {
		_, markErr = s.agentRuns.MarkFailedIfActiveSystem(bgCtx, orgID, runID)
	}
	if markErr != nil {
		log.Printf("[delegate] warning: failed to mark run %s as failed: %v", runID, markErr)
	}

	failMsg := &domain.AgentMessage{
		RunID:   runID,
		Role:    "assistant",
		Subtype: "text",
		Content: "Error: " + errMsg,
		IsError: true,
	}
	var insertErr error
	if triggerType == "manual" {
		insertErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			_, ierr := ts.AgentRuns.InsertMessage(bgCtx, orgID, failMsg)
			return ierr
		})
	} else {
		_, insertErr = s.agentRuns.InsertMessageSystem(bgCtx, orgID, failMsg)
	}
	if insertErr != nil {
		log.Printf("[delegate] warning: failed to record failure message for run %s: %v", runID, insertErr)
	}

	s.updateBreakerCounter(taskID, triggerType, "failed")
	s.broadcastRunUpdate(orgID, runID, "failed")

	// A failed run won't resume, so drop the workspace snapshot it may have
	// written when it parked (e.g. a yield that then failed mid-resume, or a
	// persistYield that couldn't record). Keyed by the run's own id: for a
	// blueprint step (whose snapshot is keyed by blueprint_run_id) this is a
	// harmless no-op and terminateBlueprint owns that blob; for a run that never
	// snapshotted it's also a no-op. The single failure chokepoint covers every
	// failRun caller (the resume goroutine's three exits among them).
	s.discardWorkspaceSnapshot(bgCtx, orgID, runID)

	// Surface as a sticky error toast so the user sees the failure even when
	// they're not watching the runs page. Truncate the message — full stderr
	// dumps don't fit in a toast card.
	toast.Error(s.wsHub, orgID, fmt.Sprintf("Run %s failed: %s", shortRunID(runID), truncateToastMsg(errMsg, 160)))
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
