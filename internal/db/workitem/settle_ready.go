package workitem

import (
	"context"
	"fmt"
)

// SettleReady flips a ready row done with no lease: the work the row stands
// for was done another way, by a party that never claimed it. It is the
// admitter's counterpart to a holder's completion, for a kind whose claim
// filter keeps a row unclaimable for exactly as long as that other way is
// open.
//
// Only a ready row with no cancellation request matches. A leased row has a
// holder whose disposition stands, a parked row is an operator's, and a
// requested cancellation is the claimer's to settle as cancelled. The
// generation moves, so a receipt from before a requeue cannot write over the
// settlement. ErrNotReady means nothing matched, and the caller's own writes
// in the transaction should roll back with it.
//
// q is the caller's transaction when the kind's own columns are written with
// the flip.
func SettleReady(ctx context.Context, q DBTX, k Kind, orgID string, itemID int64) error {
	if err := k.Validate(); err != nil {
		return err
	}
	a := newArgs(k.Dialect)
	sets := append(k.terminalDone(), assign{"lease_generation", lit("lease_generation + 1")})
	stmt := "UPDATE " + k.Table + " SET " + joinComma(renderAssigns(a, sets)) +
		" WHERE org_id = " + a.bind(orgID) +
		" AND id = " + a.bind(itemID) +
		" AND status = " + quoteLiteral(StatusReady) +
		" AND cancel_requested_at IS NULL"
	res, err := q.ExecContext(ctx, stmt, a.vals...)
	if err != nil {
		return fmt.Errorf("workitem: settle ready on %s: %w", k.Table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotReady
	}
	k.observe().Completed(orgID)
	return nil
}
