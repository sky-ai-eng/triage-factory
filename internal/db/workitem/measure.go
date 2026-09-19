package workitem

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Measure reads the queue's depths and ages. orgID "" measures every org.
//
// It is a read: counts and ages only, with no emission and no side effect. A
// metrics surface composes it; this package stays out of the metrics pipeline
// so that adopting a work kind does not also mean adopting a collector.
func Measure(ctx context.Context, q DBTX, k Kind, orgID string) (Depths, error) {
	var d Depths
	if err := k.Validate(); err != nil {
		return d, err
	}

	a := newArgs(k.Dialect)
	now := k.nowExpr()
	// Ripe and deferred partition status='ready' on the retry time alone, which
	// is narrower than Claim's eligibility: Claim also takes a ready row whose
	// cancellation was requested, whatever its retry time. So a deferred row
	// with a pending request counts as Deferred here for the seconds before the
	// next pass settles it. Depth is about work waiting, and that row is waiting
	// to be cancelled rather than run — neither bucket describes it, and a third
	// is not in the contract.
	ripe := "status = 'ready' AND (next_attempt_at IS NULL OR next_attempt_at <= " + now + ")"
	deferred := "status = 'ready' AND next_attempt_at > " + now

	stmt := "SELECT " +
		countIf(ripe) + ", " +
		countIf("status = 'leased'") + ", " +
		countIf("status = 'parked'") + ", " +
		countIf(deferred) + ", " +
		k.ageSeconds(ripe) + ", " +
		k.ageSeconds(deferred) +
		" FROM " + k.Table
	if orgID != "" {
		stmt += " WHERE org_id = " + a.bind(orgID)
	}

	var readyAge, deferredAge sql.NullFloat64
	err := q.QueryRowContext(ctx, stmt, a.vals...).
		Scan(&d.Ready, &d.Leased, &d.Parked, &d.Deferred, &readyAge, &deferredAge)
	if err != nil {
		return Depths{}, fmt.Errorf("workitem: measure %s: %w", k.Table, err)
	}
	d.OldestReadyAge = secondsToDuration(readyAge)
	d.OldestDeferredAge = secondsToDuration(deferredAge)
	return d, nil
}

// countIf is a conditional count written as a SUM so the expression is the
// same on both dialects; FILTER would be Postgres idiom the SQLite arm would
// have to spell differently.
func countIf(pred string) string {
	return "COALESCE(SUM(CASE WHEN " + pred + " THEN 1 ELSE 0 END), 0)"
}

// ageSeconds is how long the oldest row matching pred has been waiting,
// measured from first_enqueued_at — the original enqueue, which a redrive
// preserves — so the age reports the obligation, not the retry. NULL when no
// row matches.
func (k Kind) ageSeconds(pred string) string {
	oldest := "MIN(CASE WHEN " + pred + " THEN first_enqueued_at END)"
	if k.Dialect == Postgres {
		return "EXTRACT(EPOCH FROM (clock_timestamp() - " + oldest + "))"
	}
	return "(julianday('now') - julianday(" + oldest + ")) * 86400.0"
}

// secondsToDuration floors at zero: a row stamped ahead of the reading clock
// has not been waiting for a negative time, and a gauge that can go negative
// is worse than one that reads zero.
func secondsToDuration(v sql.NullFloat64) time.Duration {
	if !v.Valid || v.Float64 <= 0 {
		return 0
	}
	return time.Duration(v.Float64 * float64(time.Second))
}
