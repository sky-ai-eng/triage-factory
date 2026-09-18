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
// and id is then the existing row's. Admission dedup is not execution safety —
// it stops a second row for the same obligation, not a second run of the one
// that exists.
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
		quoteLiteral("ready"),
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
		k.onConflictClause() +
		" RETURNING id"

	var id int64
	err := q.QueryRowContext(ctx, stmt, a.vals...).Scan(&id)
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
func (k Kind) onConflictClause() string {
	switch k.Unique {
	case UniqueForever:
		return " ON CONFLICT (org_id, unique_key) WHERE unique_key IS NOT NULL DO NOTHING"
	case UniqueWhileUnsettled:
		return " ON CONFLICT (org_id, unique_key) WHERE unique_key IS NOT NULL AND status IN (" + unsettledStatusList + ") DO NOTHING"
	default:
		return ""
	}
}

// existingByKey reads the row the admission conflicted with. Under
// UniqueWhileUnsettled it repeats the index's status predicate, since settled
// rows with the same key are not what the insert lost to.
//
// It identifies that row by its key rather than by identity, because
// ON CONFLICT DO NOTHING reports no conflicting row. Under
// UniqueWhileUnsettled that leaves a window: if the blocking row settles and a
// different admission takes the freed key before this read runs, the caller is
// told it deduplicated against a row it never raced. The window is real and
// widens when q is a bare *sql.DB rather than the caller's transaction — pass
// a transaction where the answer matters.
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
		// The conflicting row settled between the insert and this read, so the
		// key is free again. Refusing beats guessing: the caller retries.
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
