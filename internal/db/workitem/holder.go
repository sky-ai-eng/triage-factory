package workitem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Complete is the SingleTx completion: it opens a transaction, locks and
// verifies the item row, runs fn, and flips the row done — all together, so
// the domain mutation and the completion both exist or neither does.
//
// The lock is taken on the item row FIRST, before anything fn touches. That
// fixed order is what makes the cancellation race decidable: a request
// committed before the lock is observed here and fn never runs, and one
// arriving afterwards waits behind this transaction and is observed by the
// holder's next operation.
func Complete(ctx context.Context, conn *sql.DB, k Kind, r Receipt, fn func(tx *sql.Tx) error) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if k.Strategy != SingleTx {
		return fmt.Errorf("workitem: %s is %s, so it completes with MarkDone", k.Table, k.Strategy)
	}
	if fn == nil {
		return fmt.Errorf("workitem: %s complete has no closure", k.Table)
	}

	// The settlement is a commit, not a failure, so it cannot travel out of the
	// transaction as an error — that would roll the settlement back.
	var settled error
	err := inTx(ctx, conn, func(tx *sql.Tx) error {
		cancelled, err := k.lockAndVerify(ctx, tx, r)
		if err != nil {
			return err
		}
		if cancelled {
			if err := k.settleUnderGuard(ctx, tx, r); err != nil {
				return err
			}
			settled = ErrCancelled
			return nil
		}
		if err := fn(tx); err != nil {
			return fmt.Errorf("workitem: %s complete closure: %w", k.Table, err)
		}
		return k.flipDone(ctx, tx, r)
	})
	if err != nil {
		return err
	}
	return settled
}

// lockAndVerify takes the item row's lock and evaluates the guard against
// fresh database time under it. It reports whether a cancellation request is
// pending; a guard miss is ErrLeaseLost.
func (k Kind) lockAndVerify(ctx context.Context, tx *sql.Tx, r Receipt) (bool, error) {
	a := newArgs(k.Dialect)
	stmt := "SELECT (" + k.guardBody(a, r) + "), (cancel_requested_at IS NOT NULL) FROM " + k.Table +
		" WHERE id = " + a.bind(r.ItemID) + " AND org_id = " + a.bind(r.OrgID)
	if k.Dialect == Postgres {
		stmt += " FOR UPDATE"
	}

	var held, cancelled dbBool
	switch err := tx.QueryRowContext(ctx, stmt, a.vals...).Scan(&held, &cancelled); {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrLeaseLost
	case err != nil:
		return false, fmt.Errorf("workitem: lock %s row %d: %w", k.Table, r.ItemID, err)
	}
	if !held.V {
		return false, ErrLeaseLost
	}
	return cancelled.V, nil
}

// guardBody is the guard's ownership terms without the row address, for use as
// a selected boolean beside the lock.
func (k Kind) guardBody(a *args, r Receipt) string {
	return "status = " + quoteLiteral(StatusLeased) +
		" AND lease_generation = " + a.bind(r.LeaseGeneration) +
		" AND lease_expires_at IS NOT NULL" +
		" AND lease_expires_at > " + k.nowExpr()
}

// flipDone is the terminal write, under the guard re-evaluated against fresh
// time. A miss here rolls the whole transaction back, fn's writes included:
// authority that lapsed mid-unit never produced a completion.
func (k Kind) flipDone(ctx context.Context, tx *sql.Tx, r Receipt) error {
	return k.plainGuardedWrite(ctx, tx, r, k.terminalDone())
}

// settleUnderGuard writes the cancellation settlement for a holder that
// observed the request under its own lock.
func (k Kind) settleUnderGuard(ctx context.Context, tx *sql.Tx, r Receipt) error {
	return k.plainGuardedWrite(ctx, tx, r, k.cancelSettlement())
}

// plainGuardedWrite is a guarded UPDATE with no cancellation branch, for the
// callers that already resolved the question under a row lock.
func (k Kind) plainGuardedWrite(ctx context.Context, q DBTX, r Receipt, sets []assign) error {
	a := newArgs(k.Dialect)
	stmt := "UPDATE " + k.Table + " SET " + joinComma(renderAssigns(a, sets)) + " WHERE " + k.guardSQL(a, r)
	res, err := q.ExecContext(ctx, stmt, a.vals...)
	if err != nil {
		return fmt.Errorf("workitem: guarded write on %s: %w", k.Table, err)
	}
	return rowsAffectedOne(res)
}

// MarkDone is the FencedReplay completion: the guarded terminal flip alone.
// The domain writes committed in their own transactions, each carrying the
// kind's own replay fence, and a crash between them and this call leaves the
// row leased for the next claimer to re-execute.
func MarkDone(ctx context.Context, q DBTX, k Kind, r Receipt) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if k.Strategy != FencedReplay {
		return fmt.Errorf("workitem: %s is %s, so it completes with Complete", k.Table, k.Strategy)
	}
	return k.holderWrite(ctx, q, r, k.terminalDone())
}

// Requeue returns a failed attempt to the queue, or parks it, and reports
// which: parked is true when the row landed parked, false when it returned to
// ready with a retry time. A worker logs and disposes on that answer, and a
// metrics reader counts parks from it.
//
// A permanent outcome parks immediately whatever the budget: retrying a
// rejection only spends attempts. Otherwise the stored budget decides, in SQL,
// against the max_attempts the row was admitted under.
func Requeue(ctx context.Context, q DBTX, k Kind, r Receipt, outcome Outcome, cause error) (parked bool, err error) {
	if err := k.Validate(); err != nil {
		return false, err
	}
	if !outcome.valid() {
		return false, fmt.Errorf("workitem: %s requeue with unknown outcome %q", k.Table, outcome)
	}

	var causeText any
	if cause != nil {
		causeText = cause.Error()
	}
	now := k.nowExpr()

	var status, doneAt, nextAttempt valueExpr
	if outcome == OutcomePermanent {
		status = lit(quoteLiteral(StatusParked))
		doneAt = lit(now)
		nextAttempt = keep("next_attempt_at")
	} else {
		// The budget lives on the row, copied there at admission, so the arm is
		// chosen in SQL against the budget this row was actually admitted under
		// rather than against whatever the policy says today.
		exhausted := "attempt >= max_attempts"
		status = lit("CASE WHEN " + exhausted + " THEN " + quoteLiteral(StatusParked) + " ELSE " + quoteLiteral(StatusReady) + " END")
		doneAt = lit("CASE WHEN " + exhausted + " THEN " + now + " ELSE done_at END")
		// The delay is computed in Go from the attempt this receipt charged, so
		// the value is pinnable; only the instant it is measured from is the
		// database's.
		delay := Backoff(k.Policy.Backoff, r.Attempt, nil)
		nextAttempt = bound(func(a *args) string {
			return "CASE WHEN " + exhausted + " THEN next_attempt_at ELSE " + k.nowPlusExpr(a, delay) + " END"
		})
	}

	sets := append([]assign{
		{"status", status},
		{"done_at", doneAt},
		{"next_attempt_at", nextAttempt},
		{"last_outcome", bound(func(a *args) string { return a.bind(string(outcome)) })},
		{"last_error", bound(func(a *args) string { return a.bind(causeText) })},
	}, clearLease...)
	// The landed status is read back from the same statement rather than
	// predicted from the receipt's attempt, so the answer is the row's.
	landed, err := k.holderWriteReturningStatus(ctx, q, r, sets)
	if err != nil {
		return false, err
	}
	return landed == StatusParked, nil
}

// Park takes the item out of circulation for an operator to redrive or
// supersede. reason is required: a parked row whose last_outcome says nothing
// is a row nobody can triage.
func Park(ctx context.Context, q DBTX, k Kind, r Receipt, reason string) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if reason == "" {
		return fmt.Errorf("workitem: %s park has no reason", k.Table)
	}
	sets := append([]assign{
		{"status", lit(quoteLiteral(StatusParked))},
		{"done_at", lit(k.nowExpr())},
		{"last_outcome", bound(func(a *args) string { return a.bind(reason) })},
	}, clearLease...)
	return k.holderWrite(ctx, q, r, sets)
}

// Defer returns the row to ready for expected waiting, refunding this
// acquisition's attempt charge. Repeated healthy waiting therefore cannot
// exhaust the failure budget.
//
// The refund is bounded to one per acquisition by the guard itself: the
// generation matched here belongs to this lease, and the write that spends it
// also drops the row out of 'leased', so a second call with the same receipt
// matches nothing.
//
// predicate is the kind's declared deferral check, run under the item's lock
// and before any refund. False means the reason to defer did not hold, which
// is not a failure and not a refund: ErrDeferRefused leaves the row leased and
// the holder must Requeue or let the lease lapse.
func Defer(ctx context.Context, conn *sql.DB, k Kind, r Receipt, reason string, nextAttemptAt time.Time, predicate func(tx *sql.Tx) (bool, error)) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if reason == "" {
		return fmt.Errorf("workitem: %s defer has no reason", k.Table)
	}
	if predicate == nil {
		return fmt.Errorf("workitem: %s defer has no predicate", k.Table)
	}
	// A zero instant is a caller that forgot the argument, not a request to
	// retry in year one. Refusing keeps that mistake out of the column.
	if nextAttemptAt.IsZero() {
		return fmt.Errorf("workitem: %s defer has no retry time", k.Table)
	}

	var settled error
	err := inTx(ctx, conn, func(tx *sql.Tx) error {
		cancelled, err := k.lockAndVerify(ctx, tx, r)
		if err != nil {
			return err
		}
		if cancelled {
			if err := k.settleUnderGuard(ctx, tx, r); err != nil {
				return err
			}
			settled = ErrCancelled
			return nil
		}
		ok, err := predicate(tx)
		if err != nil {
			return fmt.Errorf("workitem: %s defer predicate: %w", k.Table, err)
		}
		if !ok {
			return ErrDeferRefused
		}
		sets := append([]assign{
			{"status", lit(quoteLiteral(StatusReady))},
			{"attempt", lit("attempt - 1")},
			{"next_attempt_at", bound(func(a *args) string { return k.bindTime(a, nextAttemptAt) })},
			{"last_outcome", lit(quoteLiteral(outcomeDeferred))},
			{"last_error", bound(func(a *args) string { return a.bind(reason) })},
		}, clearLease...)
		return k.plainGuardedWrite(ctx, tx, r, sets)
	})
	if err != nil {
		return err
	}
	return settled
}

// RenewLease pushes the lease out to fresh database time plus the policy
// lease. It never extends the old timestamp, so a renewal that arrives late
// cannot resurrect authority that already lapsed.
//
// Renewal is a holder operation like any other, so it also observes a pending
// cancellation and settles it — which is the point of §1.7's "next fenced
// write or renewal": a long unit that only ever renews still learns.
func RenewLease(ctx context.Context, q DBTX, k Kind, r Receipt) (Receipt, error) {
	if err := k.Validate(); err != nil {
		return Receipt{}, err
	}
	a := newArgs(k.Dialect)
	p := k.Policy.resolved()
	sets := []assign{{"lease_expires_at", bound(func(a *args) string { return k.nowPlusExpr(a, p.Lease) })}}

	stmt := k.holderSQL(a, r, sets, []string{"lease_expires_at"})
	var cancelled dbBool
	var expires dbTime
	switch err := q.QueryRowContext(ctx, stmt, a.vals...).Scan(&cancelled, &expires); {
	case errors.Is(err, sql.ErrNoRows):
		return Receipt{}, ErrLeaseLost
	case err != nil:
		return Receipt{}, fmt.Errorf("workitem: renew %s row %d: %w", k.Table, r.ItemID, err)
	}
	if cancelled.V {
		return Receipt{}, ErrCancelled
	}
	renewed := r
	renewed.LeaseExpiresAt = expires.Time
	return renewed, nil
}

// terminalDone is the completion write both strategies share.
func (k Kind) terminalDone() []assign {
	return append([]assign{
		{"status", lit(quoteLiteral(StatusDone))},
		{"done_at", lit(k.nowExpr())},
		{"last_outcome", lit(quoteLiteral(outcomeDone))},
	}, clearLease...)
}

// holderWrite runs a holder disposition as one cancel-aware guarded statement.
func (k Kind) holderWrite(ctx context.Context, q DBTX, r Receipt, sets []assign) error {
	_, err := k.holderWriteReturningStatus(ctx, q, r, sets)
	return err
}

// holderWriteReturningStatus is holderWrite reporting the status the row
// landed in, for the dispositions whose arm is chosen in SQL.
func (k Kind) holderWriteReturningStatus(ctx context.Context, q DBTX, r Receipt, sets []assign) (string, error) {
	a := newArgs(k.Dialect)
	stmt := k.holderSQL(a, r, sets, []string{"status"})
	var cancelled dbBool
	var status string
	switch err := q.QueryRowContext(ctx, stmt, a.vals...).Scan(&cancelled, &status); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrLeaseLost
	case err != nil:
		return "", fmt.Errorf("workitem: holder write on %s: %w", k.Table, err)
	}
	if cancelled.V {
		return status, ErrCancelled
	}
	return status, nil
}
