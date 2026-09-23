package dbtest

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The conversation status writes a test stages go through the holder's
// fenced doors, because those are the only doors there are: a terminal or a
// park is written by the engagement holding the conversation, in one
// transaction with its claim release. These helpers stand in for that
// engagement — they use the conversation's live claim, or mint one the way a
// go-live confirmation does when there is none — so a fixture reaches exactly
// the state production would.

// holderTestExecutor is the executor id a minted stand-in claim carries.
const holderTestExecutor = "exec-holder-test"

// holderClaim returns the conversation's live claim, minting one when there is
// none. minted reports which, so a write that turns out not to release the
// claim can hand a minted one back rather than leave a stand-in engagement
// live on the row.
func holderClaim(ctx context.Context, store db.ConversationStore, orgID, conversationID string) (claimID string, minted bool, err error) {
	c, err := store.SetActiveClaimPhaseSystem(ctx, orgID, conversationID, "")
	if err != nil {
		return "", false, err
	}
	if c != nil {
		return c.ID, false, nil
	}
	c, err = store.SetExecutorSystem(ctx, orgID, conversationID, holderTestExecutor, 1)
	if err != nil {
		return "", false, err
	}
	if c == nil {
		return "", false, errors.New("holder claim: mint returned no claim")
	}
	return c.ID, true, nil
}

// releaseMinted hands back a stand-in claim a write left live.
func releaseMinted(ctx context.Context, store db.ConversationStore, orgID, conversationID string, minted bool) {
	if minted {
		_, _ = store.SetExecutorSystem(ctx, orgID, conversationID, "", 0)
	}
}

// HolderComplete is CompleteForClaimSystem driven by a stand-in holder.
func HolderComplete(store db.ConversationStore, ctx context.Context, orgID, conversationID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) (*domain.Conversation, error) {
	claimID, _, err := holderClaim(ctx, store, orgID, conversationID)
	if err != nil {
		return nil, err
	}
	return store.CompleteForClaimSystem(ctx, orgID, conversationID, claimID, status, costUSD, durationMs, numTurns, resultSummary, outcome, outcomeReason, failureKind)
}

// HolderPark is ParkOpenForClaimSystem driven by a stand-in holder.
func HolderPark(store db.ConversationStore, ctx context.Context, orgID, conversationID string, park db.Park) (bool, error) {
	claimID, minted, err := holderClaim(ctx, store, orgID, conversationID)
	if err != nil {
		return false, err
	}
	ok, err := store.ParkOpenForClaimSystem(ctx, orgID, conversationID, claimID, park)
	if err == nil && !ok {
		releaseMinted(ctx, store, orgID, conversationID, minted)
	}
	return ok, err
}

// HolderMarkFailed is MarkFailedIfActiveForClaimSystem driven by a stand-in
// holder.
func HolderMarkFailed(store db.ConversationStore, ctx context.Context, orgID, conversationID, failureKind string) (bool, error) {
	claimID, minted, err := holderClaim(ctx, store, orgID, conversationID)
	if err != nil {
		return false, err
	}
	ok, err := store.MarkFailedIfActiveForClaimSystem(ctx, orgID, conversationID, claimID, failureKind)
	if err == nil && !ok {
		releaseMinted(ctx, store, orgID, conversationID, minted)
	}
	return ok, err
}
