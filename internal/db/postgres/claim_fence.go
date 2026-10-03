package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// assertClaimActive is the fence every claim-fenced engagement write opens
// with. q must be a transaction, and the write it guards must run in that
// same transaction — the lock is what closes the race, and a lock taken in a
// different transaction is released before the write it was supposed to
// protect ever lands.
//
// Authority is a lease on database time: a claim is live while it is
// unreleased AND its lease has not lapsed. The holder renews the lease on a
// timer and fences its own engagement when it cannot, and every write it makes
// presents the lease here. So the guarantee is per-claim and two-sided —
// ownership ends at a timestamp whether or not a successor exists, and the
// holder has stopped by its own local deadline well before that timestamp
// arrives.
//
// FOR SHARE, specifically:
//
//   - A locking read observes the CURRENT committed version of the row, not
//     the version the statement snapshot froze. Without it a repeatable-read
//     writer could validate a claim released before it even started.
//   - It conflicts with the row lock the release UPDATE takes, so the two
//     serialize: a release arriving mid-write blocks until this transaction
//     commits, and one that got there first is visible here.
//   - FOR KEY SHARE would not do: released_at is not a key column, so a
//     release would take a lock that does not conflict with it and both
//     would proceed. lease_expires_at is not a key column either, so the same
//     reasoning covers it unchanged.
//
// A plain EXISTS check narrows the window to microseconds and leaves it open,
// which is the same thing as not having a fence.
//
// The claim must be live AND own conversationID. Liveness alone would answer
// a weaker question than the one being asked — "does this caller hold some
// claim somewhere in the org" rather than "does this caller own THIS
// conversation" — and every fenced write names its target separately from its
// claim, so a mis-threaded pair is a wiring mistake the fence is exactly the
// right place to catch. The org binds too, as defense in depth alongside RLS
// like every other statement in these stores.
//
// Every way the row can fail to resolve — released, expired, wrong org, wrong
// conversation, never existed — is one answer: this caller is not the owner.
// The one refinement is that an unreleased claim whose lease lapsed says so,
// as db.ErrClaimLeaseExpired, which is still db.ErrClaimReleased to every
// caller that asks only that. The row is read whatever its state rather than
// matched by the predicate, so it is locked whenever it exists, which adds no
// window: a row the old predicate skipped was one no write could land on
// anyway.
//
// statement_timestamp() rather than now(): the expiry has to be read against
// fresh database time, not the instant the caller's transaction began, or a
// long transaction's guard would pass on a lease that lapsed while it was
// open.
func assertClaimActive(ctx context.Context, q queryer, orgID, conversationID, claimID string) error {
	if claimID == "" {
		return fmt.Errorf("%w: no claim id supplied", db.ErrClaimReleased)
	}
	// id and conversation_id are uuid columns; a malformed value would fail
	// Postgres parsing (22P02) rather than the ownership test it is standing
	// in for. Same answer either way — this caller owns nothing.
	if !isValidUUID(claimID) {
		return fmt.Errorf("%w: claim %q is not a valid id", db.ErrClaimReleased, claimID)
	}
	if !isValidUUID(conversationID) {
		return fmt.Errorf("%w: conversation %q is not a valid id", db.ErrClaimReleased, conversationID)
	}
	return claimRefusal(ctx, q, orgID, conversationID, claimID, "FOR SHARE")
}

// claimRefusal reads the named claim's state and answers whether a holder
// write against it would be refused: nil while it is live, db.ErrClaimReleased
// when there is no such claim on the conversation or it is released, and
// db.ErrClaimLeaseExpired when it is unreleased with a lapsed lease. lock is
// the locking clause to read under, "" for none.
//
// "live" is exactly the guard every holder write and the renewal test —
// unreleased with a lease in the future — so the classification never passes
// a claim the guard would refuse. A live claim with no lease is impossible
// here (claims_live_has_lease), and would read as released, not expired.
func claimRefusal(ctx context.Context, q queryer, orgID, conversationID, claimID, lock string) error {
	var state string
	err := q.QueryRowContext(ctx, `
		SELECT CASE
		         WHEN released_at IS NULL AND lease_expires_at > statement_timestamp() THEN 'live'
		         WHEN released_at IS NULL AND lease_expires_at IS NOT NULL THEN 'expired'
		         ELSE 'released'
		       END
		FROM claims
		WHERE id = $1 AND org_id = $2 AND conversation_id = $3
		`+lock, claimID, orgID, conversationID).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && state == "released"):
		return fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimReleased, claimID, conversationID)
	case err != nil:
		return err
	case state == "expired":
		return fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimLeaseExpired, claimID, conversationID)
	}
	return nil
}
