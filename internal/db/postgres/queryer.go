package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// queryer is the minimum interface a Postgres store impl needs from
// its underlying handle. Both *sql.DB and *sql.Tx satisfy it via the
// pgx stdlib driver, so the same store body runs inside or outside a
// transaction. database/sql doesn't ship a common interface that both
// types satisfy, so we declare our own.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// inTx runs fn against a queryer that's guaranteed to be a transaction:
//   - if q is already a *sql.Tx (caller composed us inside Stores.Tx.WithTx),
//     fn runs against that tx directly so the outer commit/rollback wins.
//   - if q is a *sql.DB, inTx opens a fresh tx, runs fn against it, and
//     commits or rolls back based on fn's return.
//
// Mirrors the SQLite-side helper of the same name. Used by store
// methods that must apply a multi-statement operation atomically —
// e.g. RepositoryStore.SetConfigured deletes dropped repos and upserts
// skeleton rows for new ones inside one tx so the table can't
// observe a partial mid-sync state.
func inTx(ctx context.Context, q queryer, fn func(queryer) error) error {
	return inTxRaw(ctx, q, func(tx *sql.Tx) error { return fn(tx) })
}

// inTxRaw is inTx for bodies that need the *sql.Tx itself rather than the
// queryer subset — a helper they hand it to, a statement whose result they
// read off the tx. Same composition rule: an outer *sql.Tx wins, a *sql.DB
// gets a fresh transaction. db.InTx owns the begin/commit half so the
// cancellation attribution every transaction needs lives in one place.
func inTxRaw(ctx context.Context, q queryer, fn func(*sql.Tx) error) error {
	switch v := q.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		return db.InTx(ctx, v, fn)
	default:
		return fmt.Errorf("postgres store: unexpected queryer type %T", q)
	}
}
