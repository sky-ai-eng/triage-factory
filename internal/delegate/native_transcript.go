package delegate

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// nativeTranscript adapts the conversations store to the native loop's
// Transcript surface. It is the conversationSink of this runtime: every row the loop
// writes lands through here and fans out to the websocket, so a native
// conversation streams to the UI in the same shapes an SDK one does.
//
// Every write is a claim-fenced engagement write, for both trigger types —
// the fence's ownership check stands in for the RLS pass the former
// synthetic-claims route provided, exactly as the SDK sink's writes do. The
// loop is a long-lived goroutine, so each write is its own short transaction
// rather than one that spans the engagement; a write refused with
// db.ErrClaimReleased surfaces through the engine as the engagement's
// failure, and recordNativeResult recognizes it as a fence-out.
type nativeTranscript struct {
	spawner      *Spawner
	orgID        string
	conversation string
	claimID      string
}

var _ agentloop.Transcript = (*nativeTranscript)(nil)

func newNativeTranscript(s *Spawner, orgID, conversationID, claimID string) *nativeTranscript {
	return &nativeTranscript{
		spawner:      s,
		orgID:        orgID,
		conversation: conversationID,
		claimID:      claimID,
	}
}

// ListForAssembly reads the assembly window. The admin pool is the right
// door for both trigger types: it is a system read on a detached goroutine,
// and org_id + conversation_id are bound as defense in depth regardless.
// Reads are deliberately unfenced — a stale read corrupts nothing, and the
// write fence is what protects the transcript.
func (t *nativeTranscript) ListForAssembly(ctx context.Context, orgID, conversationID string) ([]domain.Message, error) {
	return t.spawner.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
}

func (t *nativeTranscript) MarkDelivered(ctx context.Context, orgID, conversationID string, ids []int, subtype string) error {
	return t.spawner.conversations.MarkDeliveredForClaimSystem(ctx, orgID, conversationID, t.claimID, ids, subtype)
}

// Insert appends a row and broadcasts it. The row is attributed to this
// engagement's claim by the same call that fences the write.
func (t *nativeTranscript) Insert(ctx context.Context, orgID string, msg *domain.Message) (int, error) {
	id, err := t.spawner.conversations.InsertMessageForClaimSystem(ctx, orgID, t.claimID, msg)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	msg.ID = int(id)
	// An undelivered row written by the loop is machine-minted input — the
	// opening turn, a repair notice — and rendering it would show the user a
	// message nobody typed and the model has not been shown yet.
	//
	// Not a rule about undelivered rows in general: a user's follow-up is the
	// same shape and IS broadcast, by the control-plane path that wrote it
	// (queueFollowUp), because a person is waiting to see their own words. The
	// distinction is who authored the row, not whether it has been delivered.
	if msg.Delivered == nil || *msg.Delivered {
		t.spawner.broadcastMessage(orgID, t.conversation, msg)
	}
	return int(id), nil
}

// Compact maps to the fenced store commit and broadcasts what it inserted —
// the result row always, the reconstructed reply row when the forced-shape
// path produced one (delivered + inactive is compacted history, which the
// display keeps).
func (t *nativeTranscript) Compact(ctx context.Context, orgID, conversationID string, replyRow, resultRow *domain.Message, inactiveIDs []int) error {
	if err := t.spawner.conversations.CompactForClaimSystem(ctx, orgID, conversationID, t.claimID, replyRow, resultRow, inactiveIDs); err != nil {
		return err
	}
	if replyRow != nil {
		t.spawner.broadcastMessage(orgID, t.conversation, replyRow)
	}
	t.spawner.broadcastMessage(orgID, t.conversation, resultRow)
	return nil
}

func (t *nativeTranscript) SettleCompactionRequest(ctx context.Context, orgID, conversationID string, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, costUSD *float64, reason string) error {
	return t.spawner.conversations.SettleCompactionRequestForClaimSystem(ctx, orgID, conversationID, t.claimID,
		requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens, costUSD, reason)
}

// spendGuard is the native loop's pre-call spend arm. It reuses the
// admission gate's own checks — the same org and team daily caps, read off
// the same messages-backed ledger, with the same fail-open behavior and the
// same user-visible wording — so a run that would have been refused at
// delegation is stopped mid-engagement by the identical rule rather than a
// parallel one that could drift from it.
//
// The difference is what a breach does. At admission it refuses the spawn;
// here it parks the conversation `open` with a notice, because there is
// already work in progress and a conversation is never failed for spending
// money it was allowed to spend.
type spendGuard struct {
	spawner *Spawner
	orgID   string
	teamID  string
}

var _ agentloop.Guard = (*spendGuard)(nil)

func (g *spendGuard) Check(ctx context.Context, _ int) (string, error) {
	if err := g.spawner.checkDailyCostCap(ctx, g.orgID); err != nil {
		return spendParkNotice(err), nil
	}
	if g.teamID != "" {
		if err := g.spawner.checkTeamDailyCostCap(ctx, g.orgID, g.teamID); err != nil {
			return spendParkNotice(err), nil
		}
	}
	return "", nil
}

// spendParkNotice renders the cap error as the transcript notice. The cap
// error already carries the scope and the today-vs-cap figures, which is
// exactly what a reader needs; this adds only what the native mapping
// contributes — that the work is paused, not lost.
func spendParkNotice(err error) string {
	return "<system-note>\nThis run is paused: " + err.Error() + ". " +
		"No work has been lost — it resumes on the next message, or once the cap resets or is raised.\n</system-note>"
}
