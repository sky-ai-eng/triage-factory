package workitem

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DBTX is the minimal executor the package needs. *sql.DB and *sql.Tx satisfy
// it, as do both dialect packages' unexported queryer types, so an adopting
// store can compose an admission or a disposition into a transaction it
// already owns.
type DBTX interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// sqliteTimeLayout is the shape every block timestamp carries on SQLite. It is
// what strftime('%Y-%m-%d %H:%M:%f') renders, in UTC, so a value bound from Go
// and a value the engine stamped compare as plain text without a second
// convention entering the column.
const sqliteTimeLayout = "2006-01-02 15:04:05.000"

// sqliteNowExpr is fresh database time on SQLite. SQLite evaluates 'now' once
// per statement and advances it across statements — including inside a
// transaction — which is exactly what an expiry guard needs and what Postgres
// now() would not give.
const sqliteNowExpr = `strftime('%Y-%m-%d %H:%M:%f','now')`

// args accumulates bind values and renders the dialect's placeholder token, so
// a builder can append a parameter without tracking its own index.
type args struct {
	dialect Dialect
	vals    []any
}

func newArgs(d Dialect) *args { return &args{dialect: d} }

func (a *args) bind(v any) string {
	a.vals = append(a.vals, v)
	if a.dialect == Postgres {
		return "$" + strconv.Itoa(len(a.vals))
	}
	return "?"
}

// nowExpr is fresh database time at the statement: clock_timestamp() rather
// than now(), which is transaction start and would let a long transaction's
// guard read an expiry that has already passed as live.
func (k Kind) nowExpr() string {
	if k.Dialect == Postgres {
		return "clock_timestamp()"
	}
	return sqliteNowExpr
}

// ripePredicate and deferredPredicate are the two halves of status='ready',
// split on whether the row may be claimed now: ripe rows have reached their
// retry time and pass the kind's claim filter, deferred rows are every other
// ready row. The list filter and the depth gauges both read the partition
// through these, so what an operator lists as deferred is exactly what the
// deferred gauge counts. Both are written over the alias t, which every
// statement that renders them gives its table, because the claim filter is
// declared over that alias.
//
// The split is narrower than Claim's eligibility, which also takes a ready
// row whose cancellation was requested, whatever its retry time. Such a row
// counts as deferred here for the seconds before the next pass settles it:
// it is waiting to be cancelled rather than run, neither half describes it,
// and a third is not in the contract.
func (k Kind) ripePredicate() string {
	return "t.status = " + quoteLiteral(StatusReady) + " AND " + k.ripeCondition()
}

func (k Kind) deferredPredicate() string {
	return "t.status = " + quoteLiteral(StatusReady) + " AND NOT (" + k.ripeCondition() + ")"
}

// ripeCondition is what makes a ready row claimable now: its retry time has
// come, and the kind's claim filter, if it declares one, admits it. Shared by
// the two predicates above and by the claim's first arm so the three cannot
// disagree about which ready rows are ripe.
func (k Kind) ripeCondition() string {
	cond := "(t.next_attempt_at IS NULL OR t.next_attempt_at <= " + k.nowExpr() + ")"
	if k.ClaimFilter != "" {
		cond += " AND (" + k.ClaimFilter + ")"
	}
	return cond
}

// nowPlusExpr is database time offset by a Go-computed duration. The duration
// is a parameter — a policy lease, a computed backoff — while the instant it
// is measured from stays the database's.
func (k Kind) nowPlusExpr(a *args, d time.Duration) string {
	if k.Dialect == Postgres {
		return "clock_timestamp() + make_interval(secs => " + a.bind(d.Seconds()) + ")"
	}
	return `strftime('%Y-%m-%d %H:%M:%f','now',` + a.bind(sqliteModifier(d)) + `)`
}

// sqliteModifier renders a duration as SQLite's signed "NNN.NNN seconds"
// date-function modifier. Millisecond resolution matches what %f stores.
func sqliteModifier(d time.Duration) string {
	return fmt.Sprintf("%+.3f seconds", d.Seconds())
}

// bindTime binds an absolute instant in the dialect's storage shape. Callers
// pass instants they chose themselves (a deferral's retry time); guards never
// take one.
func (k Kind) bindTime(a *args, t time.Time) string {
	if k.Dialect == Postgres {
		return a.bind(t.UTC())
	}
	return a.bind(t.UTC().Format(sqliteTimeLayout))
}

// guardSQL is the ownership check on every holder operation. Expiry alone ends
// authority, so the lease window is part of the predicate rather than a
// courtesy check the caller might skip.
//
// The IS NOT NULL term is not redundant: a cleared lease leaves the comparison
// NULL, and a NULL predicate would scan as neither true nor false in the
// boolean this is also selected as.
func (k Kind) guardSQL(a *args, r Receipt) string {
	return "id = " + a.bind(r.ItemID) +
		" AND org_id = " + a.bind(r.OrgID) +
		" AND status = " + quoteLiteral(StatusLeased) +
		" AND lease_generation = " + a.bind(r.LeaseGeneration) +
		" AND lease_expires_at IS NOT NULL" +
		" AND lease_expires_at > " + k.nowExpr()
}

// valueExpr is one column's value on one branch of a write.
//
// It renders lazily because it binds its own parameters, and SQLite's
// placeholders are positional: a value bound before the clause that uses it is
// emitted lands against the wrong `?`. Rendering in emission order makes bind
// order follow statement order by construction rather than by each builder
// remembering to.
//
// The constant/bound split is what lets holderSQL recognise two branches that
// are the same constant and collapse them, which is not cosmetic: Postgres
// cannot resolve a type for CASE WHEN … THEN NULL ELSE NULL END, and there is
// no branch to choose there anyway.
type valueExpr struct {
	text  string
	build func(a *args) string
}

// lit is a value that binds nothing — a literal, a keyword, or an expression
// over the row's own columns.
func lit(text string) valueExpr { return valueExpr{text: text} }

// bound is a value that binds one or more parameters when it renders.
func bound(build func(a *args) string) valueExpr { return valueExpr{build: build} }

// keep leaves a column at its current value on the branch it appears on.
func keep(col string) valueExpr { return lit(col) }

func (v valueExpr) render(a *args) string {
	if v.build != nil {
		return v.build(a)
	}
	return v.text
}

// sameConstant reports whether both values are the identical constant, so the
// branch they appear on cannot matter.
func (v valueExpr) sameConstant(o valueExpr) bool {
	return v.build == nil && o.build == nil && v.text == o.text
}

// assign is one column's value on an operation's own branch.
type assign struct {
	col  string
	expr valueExpr
}

// cancelSettlement is the write that settles a requested cancellation,
// wherever it is observed. It is one shape on purpose: claim-time settlement
// and holder-observed settlement leave byte-identical rows, so an operator
// reading a cancelled row never has to ask which path produced it.
//
// Columns absent from this list keep their value, cancel_requested_* included
// — the terminal record retains who asked and why.
func (k Kind) cancelSettlement() []assign {
	return []assign{
		{"status", lit(quoteLiteral(StatusCancelled))},
		{"done_at", lit(k.nowExpr())},
		{"last_outcome", lit(quoteLiteral(outcomeCancelled))},
		{"lease_owner", lit("NULL")},
		{"lease_epoch", lit("NULL")},
		{"leased_at", lit("NULL")},
		{"lease_expires_at", lit("NULL")},
	}
}

// clearLease is the lease columns every disposition releases.
var clearLease = []assign{
	{"lease_owner", lit("NULL")},
	{"lease_epoch", lit("NULL")},
	{"leased_at", lit("NULL")},
	{"lease_expires_at", lit("NULL")},
}

// holderSQL renders a holder operation as ONE guarded statement in which every
// assignment carries both branches: the operation's own disposition, and the
// cancellation settlement when a request is pending.
//
// One statement rather than a read followed by a write, because the holder
// operations take a DBTX that may be a bare *sql.DB. Split across two
// statements, a request landing between them would be written over by the
// disposition and lost.
//
// The RETURNING clause leads with whether the cancel branch fired, so the
// caller learns which disposition it actually got.
func (k Kind) holderSQL(a *args, r Receipt, sets []assign, returning []string) string {
	const cond = "cancel_requested_at IS NOT NULL"

	cancel := k.cancelSettlement()
	order := make([]string, 0, len(cancel)+len(sets))
	cancelExpr := make(map[string]valueExpr, len(cancel))
	for _, s := range cancel {
		cancelExpr[s.col] = s.expr
		order = append(order, s.col)
	}
	normalExpr := make(map[string]valueExpr, len(sets))
	for _, s := range sets {
		if _, ok := normalExpr[s.col]; ok {
			panic("workitem: duplicate assignment for column " + s.col)
		}
		normalExpr[s.col] = s.expr
		if _, ok := cancelExpr[s.col]; !ok {
			order = append(order, s.col)
		}
	}

	clauses := make([]string, 0, len(order))
	for _, col := range order {
		// An unassigned column keeps its value on that branch.
		ce, ok := cancelExpr[col]
		if !ok {
			ce = keep(col)
		}
		ne, ok := normalExpr[col]
		if !ok {
			ne = keep(col)
		}
		if ce.sameConstant(ne) {
			clauses = append(clauses, col+" = "+ce.render(a))
			continue
		}
		// Both branches render here, in this order, so their binds land in the
		// order the CASE reads them.
		clauses = append(clauses, col+" = CASE WHEN "+cond+" THEN "+ce.render(a)+" ELSE "+ne.render(a)+" END")
	}

	ret := append([]string{"(" + cond + ")"}, returning...)
	return "UPDATE " + k.Table +
		" SET " + strings.Join(clauses, ", ") +
		" WHERE " + k.guardSQL(a, r) +
		" RETURNING " + strings.Join(ret, ", ")
}

// dbTime scans a block timestamp from either dialect. Postgres hands back a
// time.Time; SQLite's TEXT column arrives as the string the package wrote.
type dbTime struct {
	Valid bool
	Time  time.Time
}

// sqliteTimeLayouts are the shapes a block timestamp can arrive in. The first
// is what this package writes; the second and third cover a value bound by the
// driver's own time format, which an adopting table's other writers may leave
// in a column this package later reads.
var sqliteTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05.999999999-07:00",
	time.RFC3339Nano,
}

func (d *dbTime) Scan(src any) error {
	d.Valid, d.Time = false, time.Time{}
	switch v := src.(type) {
	case nil:
		return nil
	case time.Time:
		d.Valid, d.Time = true, v.UTC()
		return nil
	case []byte:
		return d.parse(string(v))
	case string:
		return d.parse(v)
	default:
		return fmt.Errorf("workitem: cannot scan %T as a timestamp", src)
	}
}

func (d *dbTime) parse(s string) error {
	if s == "" {
		return nil
	}
	for _, layout := range sqliteTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			d.Valid, d.Time = true, t.UTC()
			return nil
		}
	}
	return fmt.Errorf("workitem: cannot parse %q as a timestamp", s)
}

// dbBool scans a boolean expression from either dialect: Postgres returns a
// bool, SQLite an integer.
type dbBool struct{ V bool }

func (b *dbBool) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		b.V = false
	case bool:
		b.V = v
	case int64:
		b.V = v != 0
	case float64:
		b.V = v != 0
	case []byte:
		b.V = len(v) == 1 && v[0] == '1'
	case string:
		b.V = v == "1" || v == "t" || v == "true"
	default:
		return fmt.Errorf("workitem: cannot scan %T as a boolean", src)
	}
	return nil
}

// rowsAffectedOne reports whether a guarded write matched its row, mapping a
// miss onto ErrLeaseLost.
func rowsAffectedOne(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// joinComma is strings.Join with the one separator every SET list uses.
func joinComma(parts []string) string { return strings.Join(parts, ", ") }

// renderAssigns renders a SET list in order, so each assignment's binds land
// in the position the statement reads them from.
func renderAssigns(a *args, sets []assign) []string {
	clauses := make([]string, 0, len(sets))
	for _, s := range sets {
		clauses = append(clauses, s.col+" = "+s.expr.render(a))
	}
	return clauses
}
