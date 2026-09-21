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
//
// Only unsettled rows are scanned. Terminal rows contribute to no depth, and
// on a retained table they are most of it, so the restriction is what keeps a
// periodic measure off the bulk of the table.
func Measure(ctx context.Context, q DBTX, k Kind, orgID string) (Depths, error) {
	if err := k.Validate(); err != nil {
		return Depths{}, err
	}
	a := newArgs(k.Dialect)
	stmt := "SELECT " + k.depthColumns() + " FROM " + k.Table + " t" +
		" WHERE t.status IN (" + unsettledStatusList + ")"
	if orgID != "" {
		stmt += " AND t.org_id = " + a.bind(orgID)
	}
	var d Depths
	if err := scanDepths(q.QueryRowContext(ctx, stmt, a.vals...), &d); err != nil {
		return Depths{}, fmt.Errorf("workitem: measure %s: %w", k.Table, err)
	}
	return d, nil
}

// MeasureByOrg is Measure grouped by org, in one statement over the unsettled
// rows. An org with no unsettled row is absent from the map rather than
// present at zero, which is what lets a reader retire a series for an org that
// no longer has one.
func MeasureByOrg(ctx context.Context, q DBTX, k Kind) (map[string]Depths, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	stmt := "SELECT t.org_id, " + k.depthColumns() + " FROM " + k.Table + " t" +
		" WHERE t.status IN (" + unsettledStatusList + ")" +
		" GROUP BY t.org_id"
	rows, err := q.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("workitem: measure %s by org: %w", k.Table, err)
	}
	defer rows.Close()
	out := map[string]Depths{}
	for rows.Next() {
		var (
			org string
			d   Depths
		)
		if err := scanDepths(rows, &d, &org); err != nil {
			return nil, fmt.Errorf("workitem: measure %s by org: %w", k.Table, err)
		}
		out[org] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workitem: measure %s by org: %w", k.Table, err)
	}
	return out, nil
}

// depthColumns is the six aggregate columns both measures select, in the
// order scanDepths reads them. Rendered over the alias t, which both measures
// give their table.
func (k Kind) depthColumns() string {
	ripe := k.ripePredicate()
	deferred := k.deferredPredicate()
	return countIf(ripe) + ", " +
		countIf("t.status = "+quoteLiteral(StatusLeased)) + ", " +
		countIf("t.status = "+quoteLiteral(StatusParked)) + ", " +
		countIf(deferred) + ", " +
		k.ageSeconds(ripe) + ", " +
		k.ageSeconds(deferred)
}

// scanDepths reads one depthColumns row, after any leading columns the
// caller selected ahead of them.
func scanDepths(row interface{ Scan(...any) error }, d *Depths, leading ...any) error {
	var readyAge, deferredAge sql.NullFloat64
	dest := append(leading, &d.Ready, &d.Leased, &d.Parked, &d.Deferred, &readyAge, &deferredAge)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	d.OldestReadyAge = secondsToDuration(readyAge)
	d.OldestDeferredAge = secondsToDuration(deferredAge)
	return nil
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
	oldest := "MIN(CASE WHEN " + pred + " THEN t.first_enqueued_at END)"
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
