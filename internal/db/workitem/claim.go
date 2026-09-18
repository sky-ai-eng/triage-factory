package workitem

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// claimRounds bounds how many times one Claim call re-picks. A round that
// settles every row it picked has leased nothing, so without a bound a queue
// full of cancelled work would keep a claimer in this call indefinitely.
const claimRounds = 10

// Claim leases up to n rows, oldest first. orgID "" claims across every org.
//
// A picked row is settled rather than leased when it carries a cancellation
// request or has already spent its budget; settled rows yield no receipt and
// do not count toward n, so a caller asking for n units of work gets n units
// of work rather than a batch diluted by bookkeeping. That is why this loops:
// it re-picks until it holds n receipts or a pick comes back empty.
//
// A non-nil error still returns the receipts from rounds that COMMITTED before
// it. They are real leases on real rows, and dropping them on the floor would
// leave that work leased until expiry with nobody holding authority to dispose
// of it. The failing round itself contributes nothing, receipts and counters
// alike: it rolled back.
func Claim(ctx context.Context, conn *sql.DB, k Kind, owner Owner, orgID string, n int) (ClaimResult, error) {
	var out ClaimResult
	if err := k.Validate(); err != nil {
		return out, err
	}
	if n < 1 {
		return out, fmt.Errorf("workitem: %s claim asked for %d rows", k.Table, n)
	}
	if owner.ID == "" {
		return out, fmt.Errorf("workitem: %s claim has no owner", k.Table)
	}

	for round := 0; round < claimRounds && len(out.Claimed) < n; round++ {
		picked, committed, err := k.claimRound(ctx, conn, owner, orgID, n-len(out.Claimed))
		if err != nil {
			return out, err
		}
		out.Claimed = append(out.Claimed, committed.Claimed...)
		out.Cancelled += committed.Cancelled
		out.Parked += committed.Parked
		if picked == 0 {
			break
		}
	}
	return out, nil
}

// picked is one row the claim statement selected, with everything the
// disposition choice needs. The budget is read from the row rather than from
// the policy: max_attempts was copied at admission, and that copy is the
// budget this row was admitted under.
type picked struct {
	id        int64
	orgID     string
	attempt   int
	maxTries  int
	cancelled bool
}

// claimRound picks and disposes of up to limit rows in one transaction,
// returning how many it picked and what it committed. Postgres takes row locks
// with SKIP LOCKED so concurrent claimers pick disjoint sets.
//
// SQLite takes neither, and its mutual exclusion comes from outside this
// package: db.OpenAt caps the pool at one connection, so every claimer in the
// process queues behind the same handle and no two transactions interleave.
//
// That is a property of the handle, not of the file, and db.InTx begins
// DEFERRED — so the pick and the per-row writes take their locks in two steps.
// A second PROCESS on the same file that holds the write lock at that upgrade,
// or that commits anything at all between the two, fails this transaction
// immediately: busy_timeout does not cover a lock upgrade, because waiting
// there can deadlock. Nothing is double-leased — the round rolls back whole,
// receipts and all — so the cost is a lost cycle, not a lost fence.
//
// This is local mode's shape rather than this package's: db.InTx is shared, the
// read-then-write stores beside it are exposed identically, and the fix belongs
// at the handle where one begin mode covers all of them.
//
// TODO(TFAC-1027): local mode's DSN gains _txlock=immediate, which makes the
// busy handler apply here. Until it lands, a SQLite consumer claiming while an
// unsandboxed agent's exec verbs write must treat a "database is locked" from
// Claim as retryable rather than as a fault.
//
// The round's work accumulates locally and is merged into the caller's result
// only after the commit. A round is one transaction, so a row that fails partway
// through takes its predecessors' leases down with it — and a receipt for a
// rolled-back lease is worse than no receipt at all, since its holder would act
// on authority the database never granted.
func (k Kind) claimRound(ctx context.Context, conn *sql.DB, owner Owner, orgID string, limit int) (int, ClaimResult, error) {
	var (
		n     int
		round ClaimResult
	)
	err := db.InTx(ctx, conn, func(tx *sql.Tx) error {
		// Reset per attempt: database/sql may retry the begin, and a partially
		// filled result from an abandoned run must not survive into this one.
		n, round = 0, ClaimResult{}
		rows, err := k.pick(ctx, tx, orgID, limit)
		if err != nil {
			return err
		}
		n = len(rows)
		for _, row := range rows {
			switch {
			// Cancellation outranks the budget: an item nobody wants any more
			// must not be parked for an operator to look at.
			case row.cancelled:
				if err := k.settleAtClaim(ctx, tx, row); err != nil {
					return err
				}
				round.Cancelled++
			case row.attempt >= row.maxTries:
				if err := k.parkAtClaim(ctx, tx, row); err != nil {
					return err
				}
				round.Parked++
			default:
				r, err := k.lease(ctx, tx, row, owner)
				if err != nil {
					return err
				}
				round.Claimed = append(round.Claimed, r)
			}
		}
		return nil
	})
	if err != nil {
		return 0, ClaimResult{}, err
	}
	return n, round, nil
}

// pick selects claimable rows. The three arms are: a ripe ready row, a ready
// row carrying a cancellation request whatever its retry time, and a leased row
// whose lease has expired. The third is the whole of recovery — no sweeper
// resets anything, an expired lease is simply claimable.
func (k Kind) pick(ctx context.Context, tx *sql.Tx, orgID string, limit int) ([]picked, error) {
	a := newArgs(k.Dialect)
	now := k.nowExpr()

	where := "((t.status = 'ready' AND (t.next_attempt_at IS NULL OR t.next_attempt_at <= " + now + "))" +
		" OR (t.status = 'ready' AND t.cancel_requested_at IS NOT NULL)" +
		" OR (t.status = 'leased' AND t.lease_expires_at <= " + now + "))"
	if orgID != "" {
		where += " AND t.org_id = " + a.bind(orgID)
	}

	var prefix, join, order string
	if k.Policy.Fairness {
		// Fewest currently leased rows first, so one org's backlog cannot hold
		// the head of the queue against every other tenant. The count is a
		// single pass over this table, taken once per pick.
		prefix = "WITH org_leased AS (SELECT org_id, count(*) AS leased FROM " + k.Table +
			" WHERE status = 'leased' GROUP BY org_id) "
		join = " LEFT JOIN org_leased ol ON ol.org_id = t.org_id"
		order = " ORDER BY COALESCE(ol.leased, 0), t.id"
	} else {
		order = " ORDER BY t.id"
	}

	stmt := prefix + "SELECT t.id, t.org_id, t.attempt, t.max_attempts, (t.cancel_requested_at IS NOT NULL)" +
		" FROM " + k.Table + " t" + join +
		" WHERE " + where + order +
		" LIMIT " + a.bind(limit)
	if k.Dialect == Postgres {
		// OF t names the non-nullable side: the fairness join's other side is
		// an aggregate, which cannot be locked.
		stmt += " FOR UPDATE OF t SKIP LOCKED"
	}

	rows, err := tx.QueryContext(ctx, stmt, a.vals...)
	if err != nil {
		return nil, fmt.Errorf("workitem: pick %s: %w", k.Table, err)
	}
	defer rows.Close()

	var out []picked
	for rows.Next() {
		var p picked
		var cancelled dbBool
		if err := rows.Scan(&p.id, &p.orgID, &p.attempt, &p.maxTries, &cancelled); err != nil {
			return nil, fmt.Errorf("workitem: pick %s scan: %w", k.Table, err)
		}
		p.cancelled = cancelled.V
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workitem: pick %s: %w", k.Table, err)
	}
	return out, nil
}

// claimDisposition applies one non-leasing disposition to a picked row. The
// predicate re-states the claimable statuses rather than trusting the pick,
// so a logic error here misses its row instead of overwriting a terminal one.
func (k Kind) claimDisposition(ctx context.Context, tx *sql.Tx, p picked, sets []assign) error {
	a := newArgs(k.Dialect)
	clauses := renderAssigns(a, sets)
	stmt := "UPDATE " + k.Table + " SET " + strings.Join(clauses, ", ") +
		" WHERE id = " + a.bind(p.id) +
		" AND org_id = " + a.bind(p.orgID) +
		" AND status IN ('ready','leased')"
	res, err := tx.ExecContext(ctx, stmt, a.vals...)
	if err != nil {
		return fmt.Errorf("workitem: claim disposition on %s: %w", k.Table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("workitem: %s row %d moved under a locked claim", k.Table, p.id)
	}
	return nil
}

// settleAtClaim settles a cancellation request found on a claimable row.
//
// The generation is deliberately unchanged: settling is not an acquisition. An
// expired holder's later write still matches nothing, because the guard also
// requires status = 'leased'.
func (k Kind) settleAtClaim(ctx context.Context, tx *sql.Tx, p picked) error {
	return k.claimDisposition(ctx, tx, p, k.cancelSettlement())
}

// parkAtClaim parks a row whose budget is already spent. last_outcome is kept
// when it has one: the failure that spent the budget is more useful to whoever
// reads the parked row than the fact that it ran out.
func (k Kind) parkAtClaim(ctx context.Context, tx *sql.Tx, p picked) error {
	sets := append([]assign{
		{"status", lit(quoteLiteral("parked"))},
		{"done_at", lit(k.nowExpr())},
		{"last_outcome", lit("COALESCE(last_outcome, " + quoteLiteral(outcomeBudgetExhausted) + ")")},
	}, clearLease...)
	return k.claimDisposition(ctx, tx, p, sets)
}

// lease takes ownership and returns the receipt, reading the frozen columns in
// the same statement so their values belong to this acquisition rather than to
// whatever the row holds by the time the holder looks.
func (k Kind) lease(ctx context.Context, tx *sql.Tx, p picked, owner Owner) (Receipt, error) {
	a := newArgs(k.Dialect)
	p2 := k.Policy.resolved()
	now := k.nowExpr()

	sets := []assign{
		{"status", lit(quoteLiteral("leased"))},
		{"attempt", lit("attempt + 1")},
		{"lease_generation", lit("lease_generation + 1")},
		{"lease_owner", bound(func(a *args) string { return a.bind(owner.ID) })},
		{"lease_epoch", bound(func(a *args) string { return a.bind(owner.Epoch) })},
		{"leased_at", lit(now)},
		{"lease_expires_at", bound(func(a *args) string { return k.nowPlusExpr(a, p2.Lease) })},
	}
	clauses := renderAssigns(a, sets)

	returning := []string{"lease_generation", "lease_expires_at", "attempt", "COALESCE(unique_key, '')"}
	returning = append(returning, k.Frozen...)

	stmt := "UPDATE " + k.Table + " SET " + strings.Join(clauses, ", ") +
		" WHERE id = " + a.bind(p.id) +
		" AND org_id = " + a.bind(p.orgID) +
		" AND status IN ('ready','leased')" +
		" RETURNING " + strings.Join(returning, ", ")

	r := Receipt{ItemID: p.id, OrgID: p.orgID}
	var expires dbTime
	dest := []any{&r.LeaseGeneration, &expires, &r.Attempt, &r.UniqueKey}
	frozen := make([]any, len(k.Frozen))
	for i := range frozen {
		dest = append(dest, &frozen[i])
	}
	if err := tx.QueryRowContext(ctx, stmt, a.vals...).Scan(dest...); err != nil {
		return Receipt{}, fmt.Errorf("workitem: lease %s row %d: %w", k.Table, p.id, err)
	}
	r.LeaseExpiresAt = expires.Time
	if len(k.Frozen) > 0 {
		r.Frozen = make(map[string]any, len(k.Frozen))
		for i, name := range k.Frozen {
			r.Frozen[name] = normalizeScanned(frozen[i])
		}
	}
	return r, nil
}

// normalizeScanned collapses the drivers' two spellings of a text column, so a
// frozen value compares the same on both dialects.
func normalizeScanned(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}
