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
//     would proceed.
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
// Every way the row can fail to resolve — released, wrong org, wrong
// conversation, never existed — is one answer: this caller is not the owner.
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
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM claims
		WHERE id = $1 AND org_id = $2 AND conversation_id = $3 AND released_at IS NULL
		FOR SHARE
	`, claimID, orgID, conversationID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimReleased, claimID, conversationID)
	}
	return err
}
