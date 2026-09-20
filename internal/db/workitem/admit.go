package workitem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Admit inserts a ready row: the shared block filled by the package, the
// kind's own columns filled from cols.
//
// deduplicated is true when the kind's uniqueness mode suppressed the insert,
// and id is then the existing row's — the row that actually blocked this
// insert, not whichever row holds the key by the time the answer is read.
// Admission dedup is not execution safety — it stops a second row for the same
// obligation, not a second run of the one that exists.
//
// On Postgres a deduplicated admission writes the row it conflicted with, so
// under RLS the table's UPDATE policy governs it. That is the policy every
// guarded write in this package already needs — a claim or a completion with
// none behind it matches no rows and reports nothing — so admission adds no
// requirement to an adopting table. It is only the operation that says out loud
// when one is missing.
func Admit(ctx context.Context, q DBTX, k Kind, orgID, uniqueKey string, cols map[string]any) (int64, bool, error) {
	if err := k.Validate(); err != nil {
		return 0, false, err
	}
	if orgID == "" {
		return 0, false, fmt.Errorf("workitem: %s admit has no org", k.Table)
	}
	if uniqueKey != "" && k.Unique == UniqueNone {
		return 0, false, fmt.Errorf("workitem: %s declares no uniqueness, so unique key %q would dedup nothing", k.Table, uniqueKey)
	}

	// SQLite decides insert-versus-dedup across two statements, which only
	// agree with each other while the insert's write lock is held — that is,
	// inside a transaction. A caller handing over a pool is not asking for a
	// weaker answer, so take the boundary here rather than return an id another
	// process may already have settled out from under.
	if conn, ok := q.(*sql.DB); ok && k.Dialect == SQLite && k.Unique != UniqueNone {
		var (
			id      int64
			deduped bool
		)
		if err := inTx(ctx, conn, func(tx *sql.Tx) error {
			var err error
			id, deduped, err = k.admit(ctx, tx, orgID, uniqueKey, cols)
			return err
		}); err != nil {
			return 0, false, err
		}
		return id, deduped, nil
	}
	return k.admit(ctx, q, orgID, uniqueKey, cols)
}

func (k Kind) admit(ctx context.Context, q DBTX, orgID, uniqueKey string, cols map[string]any) (int64, bool, error) {
	// Sorted so the statement text is a function of the column set rather than
	// of a map's iteration order — which is what lets a driver cache it.
	names := make([]string, 0, len(cols))
	for name := range cols {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !k.declares(name) {
			return 0, false, fmt.Errorf("workitem: %s does not declare column %q", k.Table, name)
		}
	}

	p := k.Policy.resolved()
	a := newArgs(k.Dialect)
	now := k.nowExpr()

	insertCols := []string{"org_id", "status", "attempt", "max_attempts", "unique_key", "first_enqueued_at", "created_at"}
	values := []string{
		a.bind(orgID),
		quoteLiteral(StatusReady),
		"0",
		a.bind(p.MaxAttempts),
		a.bind(nullableText(uniqueKey)),
		now,
		now,
	}
	for _, name := range names {
		insertCols = append(insertCols, name)
		values = append(values, a.bind(cols[name]))
	}

	stmt := "INSERT INTO " + k.Table + " (" + strings.Join(insertCols, ", ") + ")" +
		" VALUES (" + strings.Join(values, ", ") + ")" +
		k.onConflictClause()

	if k.Dialect == Postgres {
		var (
			id       int64
			inserted bool
		)
		// xmax is zero on a tuple this statement inserted and carries the
		// locking transaction on one it updated, which is what lets the write
		// itself report which arm ran.
		err := q.QueryRowContext(ctx, stmt+" RETURNING id, xmax = 0", a.vals...).Scan(&id, &inserted)
		if err != nil {
			return 0, false, fmt.Errorf("workitem: admit %s: %w", k.Table, err)
		}
		return id, !inserted, nil
	}

	var id int64
	err := q.QueryRowContext(ctx, stmt+" RETURNING id", a.vals...).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("workitem: admit %s: %w", k.Table, err)
	}

	// ON CONFLICT DO NOTHING ... RETURNING yields zero rows on conflict, so the
	// existing row's id is a second read rather than something the write can
	// hand back.
	id, err = k.existingByKey(ctx, q, orgID, uniqueKey)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (k Kind) declares(name string) bool {
	for _, c := range k.Columns {
		if c == name {
			return true
		}
	}
	return false
}

// onConflictClause targets the mode's partial unique index by restating its
// predicate, which is how both dialects infer a partial index.
//
// The arms differ because only one dialect can name the conflicting row.
// Postgres takes DO UPDATE with a no-op SET, which returns that row from the
// write and so needs no follow-up read. It pays a row lock for that: an
// admission waits out a concurrent claim or completion of the row it
// deduplicates against, and then holds that row itself until its own caller
// commits, during which a claim skips it. SQLite cannot report
// insert-versus-update from an upsert, so it takes DO NOTHING and reads the row
// back, which is safe there because the insert holds the write lock until
// commit.
func (k Kind) onConflictClause() string {
	var pred string
	switch k.Unique {
	case UniqueForever:
		pred = " ON CONFLICT (org_id, unique_key) WHERE unique_key IS NOT NULL"
	case UniqueWhileUnsettled:
		pred = " ON CONFLICT (org_id, unique_key) WHERE unique_key IS NOT NULL AND status IN (" + unsettledStatusList + ")"
	default:
		return ""
	}
	if k.Dialect == Postgres {
		return pred + " DO UPDATE SET unique_key = " + k.Table + ".unique_key"
	}
	return pred + " DO NOTHING"
}

// existingByKey reads the row a SQLite admission conflicted with. Under
// UniqueWhileUnsettled it repeats the index's status predicate, since settled
// rows with the same key are not what the insert lost to.
//
// It identifies that row by its key rather than by identity, because
// ON CONFLICT DO NOTHING reports no conflicting row. That is only the same row
// while nothing else can settle this key in between, which is why Admit runs
// this on a transaction whether or not the caller supplied one: the conflicting
// insert holds the file's write lock, so no other connection can settle the
// blocking row before this read.
func (k Kind) existingByKey(ctx context.Context, q DBTX, orgID, uniqueKey string) (int64, error) {
	a := newArgs(k.Dialect)
	stmt := "SELECT id FROM " + k.Table +
		" WHERE org_id = " + a.bind(orgID) +
		" AND unique_key = " + a.bind(uniqueKey)
	if k.Unique == UniqueWhileUnsettled {
		stmt += " AND status IN (" + unsettledStatusList + ")"
	}
	stmt += " ORDER BY id LIMIT 1"

	var id int64
	switch err := q.QueryRowContext(ctx, stmt, a.vals...).Scan(&id); {
	case err == nil:
		return id, nil
	case errors.Is(err, sql.ErrNoRows):
		// Reachable only on a DBTX that is neither a pool nor a transaction,
		// where the write lock's boundary is the caller's to hold. Refusing
		// beats guessing: the caller retries.
		return 0, fmt.Errorf("workitem: admit %s key %q: %w", k.Table, uniqueKey, ErrAdmissionRaced)
	default:
		return 0, fmt.Errorf("workitem: admit %s read existing: %w", k.Table, err)
	}
}

// nullableText binds an empty string as NULL, so an unkeyed admission sits
// outside every partial unique index rather than colliding with the next one.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
