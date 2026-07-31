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
// The org binds as defense in depth alongside the claim id, matching every
// other statement in these stores. A claim id that does not resolve — wrong
// org, released, or never existed — is one answer to the only question being
// asked: is this caller still the owner. It is not.
func assertClaimActive(ctx context.Context, q queryer, orgID, claimID string) error {
	if claimID == "" {
		return fmt.Errorf("%w: no claim id supplied", db.ErrClaimReleased)
	}
	if !isValidUUID(claimID) {
		// claim_id is a uuid column; a malformed id would fail Postgres
		// parsing (22P02) rather than the ownership test it is standing in
		// for. Same answer either way — this caller owns nothing.
		return fmt.Errorf("%w: claim %q is not a valid id", db.ErrClaimReleased, claimID)
	}
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM claims
		WHERE id = $1 AND org_id = $2 AND released_at IS NULL
		FOR SHARE
	`, claimID, orgID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: claim %s", db.ErrClaimReleased, claimID)
	}
	return err
}
