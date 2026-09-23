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
// Live means unreleased AND holding an unexpired lease. Authority is a lease
// on database time, renewed by the holder on a timer; a holder that cannot
// renew fences its own engagement before the lease lapses, and every write it
// makes presents the lease here. SQLite advances 'now' across statements
// inside a transaction, so the reading is taken at the guard rather than at
// BEGIN — the fresh-time property the Postgres twin gets from
// statement_timestamp().
//
// The rival it guards against here is not a successor executor — local mode
// has one — but the engagement's own past: an engagement that stalled past its
// lease holds a claim that is settled history, and its next write must be
// refused rather than land on a row it no longer owns.
//
// The predicate is the Postgres fence's, org included. There the claim's
// org_id is bound and a composite FK ties (conversation_id, org_id) to the
// conversation, so binding the claim's org also asserts the conversation's.
// This schema has no such FK — claims.org_id and the conversation it points at
// can disagree — so the conversation is joined on org explicitly, and a claim
// whose org does not match its conversation's, or the caller's, is refused
// rather than trusted. assertLocalOrg has already pinned the caller's org by
// the time this runs; the binding here is the same defense in depth every
// other org-bound SQLite read carries, so the fence answers identically on
// both dialects instead of relying on a single-tenant gate one layer up.
//
// No row lock, and none is needed: the store runs on a single connection, so
// the read and the write it guards sit in one serialized transaction and no
// release can land between them. The check must still be issued INSIDE the
// caller's transaction (through the queryer handed to inTx), not against the
// store's own handle beforehand — a separate statement would be a separate
// turn on the connection, and the release could take the turn in between.
func assertClaimActive(ctx context.Context, q queryer, orgID, conversationID, claimID string) error {
	if claimID == "" {
		return fmt.Errorf("%w: no claim id supplied", db.ErrClaimReleased)
	}
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM claims cl
		JOIN conversations c ON c.id = cl.conversation_id AND c.org_id = cl.org_id
		WHERE cl.id = ? AND cl.org_id = ? AND cl.conversation_id = ?
		  AND cl.released_at IS NULL AND cl.lease_expires_at > `+sqliteNowExpr+`
	`, claimID, orgID, conversationID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimReleased, claimID, conversationID)
	}
	return err
}
