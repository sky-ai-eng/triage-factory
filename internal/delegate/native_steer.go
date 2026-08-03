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
	// The one gate: a conversation takes a message when something is driving it
	// (running, or queued and about to be), or when it is resting somewhere a
	// wake can reach (resumableState). `failed` is neither — its workspace did
	// not survive, so there is nothing to say a message to.
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
		// An aborted blueprint reopens atomically with the flip. Not for
		// claimability — an abort leaves current_step_index on the step that
		// aborted, so the finished-blueprint arm of the claim gate would take
		// it either way — but for disposition: an abort is work that paused
		// mid-plan, so its blueprint goes back to running and re-finalizes off
		// the resumed step's new terminal. A blueprint that FINISHED is the
		// opposite case and stays finished; the follow-up rides it as-is.
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
	s.recordResumeTaskEvent(ctx, orgID, userID, run)
	s.recomputeTaskBoardColumn(orgID, run.TaskID)
	s.wakeDispatcher()
	return nil
}
