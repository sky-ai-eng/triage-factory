package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// assertClaimActive is the claim fence's test on this dialect: the named
// claim must still be live and must be the one holding this conversation, or
// the engagement write behind it is refused with db.ErrClaimReleased.
//
// The rival owner it guards against here is not a successor executor — local
// mode has one — but the stop verb. A person stopping a conversation parks the
// row and releases its claim without asking the engagement, deliberately (see
// ConversationStore.ParkOpenForClaimSystem), and an engagement that was still
// bringing its runtime up when that happened arrives at its next write holding
// a claim that is settled history. Refusing that write is what keeps the agent
// from spawning into a conversation the user has already stopped.
//
// No row lock, and none is needed: the store runs on a single connection, so
// the read and the write it guards sit in one serialized transaction and no
// release can land between them. The check must still be issued INSIDE the
// caller's transaction (through the queryer handed to inTx), not against the
// store's own handle beforehand — a separate statement would be a separate
// turn on the connection, and the release could take the turn in between.
func assertClaimActive(ctx context.Context, q queryer, conversationID, claimID string) error {
	if claimID == "" {
		return fmt.Errorf("%w: no claim id supplied", db.ErrClaimReleased)
	}
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM claims
		WHERE id = ? AND conversation_id = ? AND released_at IS NULL
	`, claimID, conversationID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimReleased, claimID, conversationID)
	}
	return err
}
