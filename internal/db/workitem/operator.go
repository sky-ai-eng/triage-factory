package workitem

import (
	"context"
	"fmt"
)

// RequestCancel records an intent to cancel. It does not change status: a
// request is not a disposition, and nothing but a claimer or the live holder
// may settle one. That separation is what lets a request land safely against a
// row somebody else is mid-way through executing.
//
// It touches no column but its own three, so it can never disturb a lease.
// Zero rows means there was nothing cancellable: the row is terminal, or a
// request is already recorded.
func RequestCancel(ctx context.Context, q DBTX, k Kind, orgID string, itemID int64, by, reason string) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if by == "" || reason == "" {
		return fmt.Errorf("workitem: %s cancel request needs both a requester and a reason", k.Table)
	}
	a := newArgs(k.Dialect)
	stmt := "UPDATE " + k.Table +
		" SET cancel_requested_at = " + k.nowExpr() +
		", cancel_requested_by = " + a.bind(by) +
		", cancel_reason = " + a.bind(reason) +
		" WHERE org_id = " + a.bind(orgID) +
		" AND id = " + a.bind(itemID) +
		" AND status IN ('ready','leased')" +
		" AND cancel_requested_at IS NULL"

	res, err := q.ExecContext(ctx, stmt, a.vals...)
	if err != nil {
		return fmt.Errorf("workitem: request cancel on %s: %w", k.Table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotCancellable
	}
	return nil
}

// Redrive returns a parked row to ready with a fresh budget, preserving its
// history: first_enqueued_at is untouched, so the obligation keeps reporting
// its real age, and the generation moves so any receipt from before the park
// is dead.
//
// A parked row carrying a cancellation request is deliberately not redrivable.
// Somebody asked for the work to stop; the answer to that is Supersede.
//
// by is validated but not stored, and the asymmetry with Supersede below is
// the point rather than an oversight. A supersede produces a cancelled row, so
// cancel_requested_by names the actor behind a cancellation that did happen. A
// redrive produces a ready row, where that column would name a cancellation
// nobody asked for. The block has no other column for an actor, and minting a
// second meaning for this one is worse than not recording the name.
func Redrive(ctx context.Context, q DBTX, k Kind, orgID string, itemID int64, by string) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if by == "" {
		return fmt.Errorf("workitem: %s redrive has no requester", k.Table)
	}
	a := newArgs(k.Dialect)
	stmt := "UPDATE " + k.Table +
		" SET status = 'ready'" +
		", attempt = 0" +
		", next_attempt_at = NULL" +
		", done_at = NULL" +
		", lease_generation = lease_generation + 1" +
		", last_outcome = " + quoteLiteral(outcomeRedriven) +
		", last_error = NULL" +
		" WHERE org_id = " + a.bind(orgID) +
		" AND id = " + a.bind(itemID) +
		" AND status = 'parked'" +
		" AND cancel_requested_at IS NULL"
	return k.operatorWrite(ctx, q, stmt, a)
}

// Supersede settles a parked row as cancelled and records the row that
// replaces it, which is how a UniqueWhileUnsettled key is released for
// replacement work without destroying the parked row's history.
//
// It stamps cancel_requested_by and deliberately leaves cancel_requested_at and
// cancel_reason alone, so the resulting row carries an actor without a request.
// That reads odd beside §1.1's "when, by whom and why", and it is what the
// contract specifies: a supersede IS the cancellation rather than a request for
// one, so there is no request time to record, and last_outcome already says
// why. Nothing reads the partial triple — every predicate in this package keys
// on cancel_requested_at, which stays NULL — so it is provenance, not state.
func Supersede(ctx context.Context, q DBTX, k Kind, orgID string, itemID int64, by string, supersededBy int64) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if by == "" {
		return fmt.Errorf("workitem: %s supersede has no requester", k.Table)
	}
	if supersededBy <= 0 {
		return fmt.Errorf("workitem: %s supersede has no replacement row", k.Table)
	}
	if supersededBy == itemID {
		return fmt.Errorf("workitem: %s row %d cannot supersede itself", k.Table, itemID)
	}
	a := newArgs(k.Dialect)
	stmt := "UPDATE " + k.Table +
		" SET status = 'cancelled'" +
		", done_at = " + k.nowExpr() +
		", superseded_by = " + a.bind(supersededBy) +
		", last_outcome = " + quoteLiteral(outcomeSuperseded) +
		", cancel_requested_by = " + a.bind(by) +
		" WHERE org_id = " + a.bind(orgID) +
		" AND id = " + a.bind(itemID) +
		" AND status = 'parked'"
	return k.operatorWrite(ctx, q, stmt, a)
}

// operatorWrite runs a parked-row control, mapping a miss onto ErrNotParked.
func (k Kind) operatorWrite(ctx context.Context, q DBTX, stmt string, a *args) error {
	res, err := q.ExecContext(ctx, stmt, a.vals...)
	if err != nil {
		return fmt.Errorf("workitem: operator write on %s: %w", k.Table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotParked
	}
	return nil
}
