package delegate

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// queueNativeMessage delivers a user's message to a native conversation by
// appending it as an undelivered row, and — when the conversation is parked
// — re-queuing the row so an executor picks it back up.
//
// The two halves are deliberately separate. Queueing is delivery: the loop
// drains every undelivered row before each call, so a running conversation
// needs nothing else, and there is no process to find, no executor to signal,
// and no path that can 409 because the work happens to live on another pod.
// Re-queueing is only about getting a driver, which a parked conversation has
// none of.
//
// Ordering matters: the message row is written BEFORE the status flip. A
// crash between the two leaves an undelivered row that the next claim drains
// (the message is not lost); the reverse order would leave a claimable
// conversation with nothing to say.
func (s *Spawner) queueNativeMessage(ctx context.Context, orgID string, run domain.Conversation, userID, text string) error {
	if text == "" {
		return fmt.Errorf("send: empty message")
	}
	if isTerminalForSteer(run) {
		return ErrRunNotSteerable
	}
	parked := resumableState(run.Status, run.Outcome)
	if !parked && !domain.IsActiveRunStatus(run.Status) && run.Status != "queued" {
		return ErrRunNotSteerable
	}
	// The same two structural refusals the SDK resume path applies, in the same
	// most-permanent-first order, so both surfaces answer a given row
	// identically. Both refuse BEFORE the message row is written: a queued
	// message nothing will ever deliver is worse than a refusal.
	if parked && !s.workspaceRecoverable(ctx, orgID, &run) {
		return ErrWorkspaceExpired
	}
	if parked && !s.blueprintDrivableForResume(ctx, orgID, &run) {
		return ErrConversationConcluded
	}

	pending := false
	msg := &domain.Message{
		ConversationID: run.ID,
		UserID:         userID,
		Role:           "user",
		Content:        text,
		Delivered:      &pending,
	}
	if err := s.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		_, iErr := ts.Conversations.InsertMessage(ctx, orgID, msg)
		return iErr
	}); err != nil {
		return fmt.Errorf("queue message: %w", err)
	}
	s.broadcastMessage(orgID, run.ID, msg)

	if !parked {
		// Running or queued: a driver already exists (or is about to claim),
		// and it drains before its next call.
		return nil
	}
	return s.requeueParkedNative(ctx, orgID, run, userID)
}

// isTerminalForSteer reports whether a conversation has reached a state no
// message can reopen. A `completed` conversation whose outcome was an abort
// is deliberately NOT terminal — the agent chose to stop, and a follow-up
// picks the work back up. A cancelled conversation isn't terminal either: it
// parks `open`, and whether it can actually be woken is the blueprint's
// answer to give (blueprintDrivableForResume), not this switch's.
func isTerminalForSteer(run domain.Conversation) bool {
	switch run.Status {
	case "failed":
		return true
	case "completed":
		return domain.RunOutcome(run.Outcome) != domain.RunOutcomeAbort
	default:
		return false
	}
}

// requeueParkedNative flips a parked native conversation back to `queued` so
// the dispatcher claims it again, re-opening an aborted blueprint in the same
// transaction.
//
// This is the resume flip alone — no pending-input row. The message is
// already queued by the caller, and the native loop's drain is what delivers
// it; the SDK path's separate resume-input store exists only because its
// runtime has no such queue.
func (s *Spawner) requeueParkedNative(ctx context.Context, orgID string, run domain.Conversation, userID string) error {
	reopenAborted := run.Status == "completed" && domain.RunOutcome(run.Outcome) == domain.RunOutcomeAbort

	var flipped bool
	err := s.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		f, fErr := ts.Conversations.MarkQueuedForResume(ctx, orgID, run.ID)
		if fErr != nil {
			return fErr
		}
		flipped = f
		if !f {
			return nil // lost the wake CAS — another wake already re-queued it
		}
		// ClaimNextRun only claims rows whose blueprint_run is 'running', so
		// an aborted blueprint must reopen atomically with the flip or the
		// row becomes permanently unclaimable.
		if reopenAborted && run.BlueprintRunID != "" {
			if _, rErr := ts.Blueprints.ReopenRunForResume(ctx, orgID, run.BlueprintRunID); rErr != nil {
				return rErr
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("requeue parked conversation: %w", err)
	}
	if !flipped {
		// A concurrent wake won. The message is already queued and the
		// winner's claim will drain it, so this is a success, not a conflict.
		return nil
	}
	s.broadcastRunUpdate(orgID, run.ID, "queued")
	s.recomputeTaskBoardColumn(orgID, run.TaskID)
	s.wakeDispatcher()
	return nil
}
