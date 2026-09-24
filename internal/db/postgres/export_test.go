package postgres

import (
	"context"
	"database/sql"
)

// AssertClaimActiveInTx exposes the claim fence to the external test package,
// so a test can run the fence itself inside a transaction it controls rather
// than a copy of its predicate.
func AssertClaimActiveInTx(ctx context.Context, tx *sql.Tx, orgID, conversationID, claimID string) error {
	return assertClaimActive(ctx, tx, orgID, conversationID, claimID)
}
