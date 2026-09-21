package workitem

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
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
		picked, committed, byOrg, err := k.claimRound(ctx, conn, owner, orgID, n-len(out.Claimed))
		if err != nil {
			return out, err
		}
		out.Claimed = append(out.Claimed, committed.Claimed...)
		out.Cancelled += committed.Cancelled
		out.Parked += committed.Parked
		out.Reclaimed += committed.Reclaimed
		k.reportRound(byOrg)
		if picked == 0 {
			break
		}
	}
	return out, nil
}

// orgRound is one committed round's shape for one org, as the observer is
// told it. A cross-org claim's round mixes tenants, and every observer call
// names one org, so the round is tallied per org as it is applied.
type orgRound struct {
	leased, reclaimed, cancelled, parked int
}

// reportRound tells the observer about one committed round, one org at a
// time in a fixed order so a recording observer sees a deterministic
// sequence. Each budget park and each settled cancellation is reported on its
// own beside the round's shape, because those are dispositions the counters
// key on and Claimed is the round's summary rather than a second count.
func (k Kind) reportRound(byOrg map[string]*orgRound) {
	if len(byOrg) == 0 {
		return
	}
	obs := k.observe()
	orgs := make([]string, 0, len(byOrg))
	for org := range byOrg {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)
	for _, org := range orgs {
		r := byOrg[org]
		obs.Claimed(org, r.leased, r.reclaimed, r.cancelled, r.parked)
		for i := 0; i < r.parked; i++ {
			obs.Parked(org, ReasonBudgetExhausted)
		}
		for i := 0; i < r.cancelled; i++ {
			obs.Cancelled(org)
		}
	}
}

// picked is one row the claim statement selected, with everything the
// disposition choice needs and what a reclaim reports. The budget is read
// from the row rather than from the policy: max_attempts was copied at
// admission, and that copy is the budget this row was admitted under.
type picked struct {
	id        int64
	orgID     string
	attempt   int
	maxTries  int
	cancelled bool
	// wasLeased marks a row the third arm selected: its previous holder's
	// lease expired without a terminal write. prevOwner is that holder, read
	// in the same pick so a reclaim can name whom it took the row from.
	wasLeased bool
	prevOwner string
}

// claimRound picks and disposes of up to limit rows in one transaction,
// returning how many it picked and what it committed. Postgres takes row locks
// with SKIP LOCKED so concurrent claimers pick disjoint sets.
//
// SQLite takes neither, and its mutual exclusion comes from outside this
// package: the local handle caps its pool at one connection, so every claimer
// in the process queues behind the same handle and no two transactions
// interleave. Across processes the handle's IMMEDIATE begin does the work: the
// write lock is taken at BEGIN, before the pick, so a second process on the
// same file — an unsandboxed agent's exec verbs, say — waits out busy_timeout
// for this round rather than failing it at a lock upgrade, and this round
// waits the same way behind that process's write.
//
// The round's work accumulates locally and is merged into the caller's result
// only after the commit. A round is one transaction, so a row that fails partway
// through takes its predecessors' leases down with it — and a receipt for a
// rolled-back lease is worse than no receipt at all, since its holder would act
// on authority the database never granted.
func (k Kind) claimRound(ctx context.Context, conn *sql.DB, owner Owner, orgID string, limit int) (int, ClaimResult, map[string]*orgRound, error) {
	var (
		n     int
		round ClaimResult
		byOrg map[string]*orgRound
	)
	tally := func(org string) *orgRound {
		t, ok := byOrg[org]
		if !ok {
			t = &orgRound{}
			byOrg[org] = t
		}
		return t
	}
	err := inTx(ctx, conn, func(tx *sql.Tx) error {
		// Reset per attempt: database/sql may retry the begin, and a partially
		// filled result from an abandoned run must not survive into this one.
		n, round, byOrg = 0, ClaimResult{}, map[string]*orgRound{}
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
				tally(row.orgID).cancelled++
			case row.attempt >= row.maxTries:
				if err := k.parkAtClaim(ctx, tx, row); err != nil {
					return err
				}
				round.Parked++
				tally(row.orgID).parked++
			default:
				r, err := k.lease(ctx, tx, row, owner)
				if err != nil {
					return err
				}
				round.Claimed = append(round.Claimed, r)
				tally(row.orgID).leased++
				if r.Reclaimed {
					round.Reclaimed++
					tally(row.orgID).reclaimed++
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, ClaimResult{}, nil, err
	}
	return n, round, byOrg, nil
}

// pick selects claimable rows. The three arms are: a ripe ready row, a ready
// row carrying a cancellation request whatever its retry time, and a leased row
// whose lease has expired. The third is the whole of recovery — no sweeper
// resets anything, an expired lease is simply claimable.
func (k Kind) pick(ctx context.Context, tx *sql.Tx, orgID string, limit int) ([]picked, error) {
	a := newArgs(k.Dialect)
	now := k.nowExpr()

	ready, leased := quoteLiteral(StatusReady), quoteLiteral(StatusLeased)
	where := "((t.status = " + ready + " AND (t.next_attempt_at IS NULL OR t.next_attempt_at <= " + now + "))" +
		" OR (t.status = " + ready + " AND t.cancel_requested_at IS NOT NULL)" +
		" OR (t.status = " + leased + " AND t.lease_expires_at <= " + now + "))"
	if orgID != "" {
		where += " AND t.org_id = " + a.bind(orgID)
	}

	var prefix, join, order string
	if k.Policy.Fairness {
		// Fewest currently leased rows first, so one org's backlog cannot hold
		// the head of the queue against every other tenant. The count is a
		// single pass over this table, taken once per pick.
		prefix = "WITH org_leased AS (SELECT org_id, count(*) AS leased FROM " + k.Table +
			" WHERE status = " + leased + " GROUP BY org_id) "
		join = " LEFT JOIN org_leased ol ON ol.org_id = t.org_id"
		order = " ORDER BY COALESCE(ol.leased, 0), t.id"
	} else {
		order = " ORDER BY t.id"
	}

	stmt := prefix + "SELECT t.id, t.org_id, t.attempt, t.max_attempts, (t.cancel_requested_at IS NOT NULL)," +
		" (t.status = " + quoteLiteral(StatusLeased) + "), COALESCE(t.lease_owner, '')" +
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
		var cancelled, wasLeased dbBool
		if err := rows.Scan(&p.id, &p.orgID, &p.attempt, &p.maxTries, &cancelled, &wasLeased, &p.prevOwner); err != nil {
			return nil, fmt.Errorf("workitem: pick %s scan: %w", k.Table, err)
		}
		p.cancelled = cancelled.V
		p.wasLeased = wasLeased.V
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
		" AND status IN (" + claimableStatusList + ")"
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
		{"status", lit(quoteLiteral(StatusParked))},
		{"done_at", lit(k.nowExpr())},
		{"last_outcome", lit("COALESCE(last_outcome, " + quoteLiteral(outcomeBudgetExhausted) + ")")},
	}, clearLease...)
	return k.claimDisposition(ctx, tx, p, sets)
}

// lease takes ownership and returns the receipt, reading the frozen columns in
// the same statement so their values belong to this acquisition rather than to
// whatever the row holds by the time the holder looks. A row the third arm
// selected is stamped Reclaimed: the receipt is the first place a worker can
// learn that the unit it is about to run was interrupted somewhere.
func (k Kind) lease(ctx context.Context, tx *sql.Tx, p picked, owner Owner) (Receipt, error) {
	a := newArgs(k.Dialect)
	p2 := k.Policy.resolved()
	now := k.nowExpr()

	sets := []assign{
		{"status", lit(quoteLiteral(StatusLeased))},
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
		" AND status IN (" + claimableStatusList + ")" +
		" RETURNING " + strings.Join(returning, ", ")

	r := Receipt{ItemID: p.id, OrgID: p.orgID, Reclaimed: p.wasLeased, PreviousOwner: p.prevOwner}
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
