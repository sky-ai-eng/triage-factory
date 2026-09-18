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
// by is validated but not stored. The shared block has no column for who
// redrove a row — cancel_requested_by belongs to the cancellation path — and
// inventing one here would put a second meaning into a column the contract
// already assigns.
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
