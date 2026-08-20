package postgres

import (
	"context"
	"database/sql"
	"fmt"
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
//
// BlueprintStore inlines its own tx wrapper because it needs
// savepoint-on-claim-race semantics. Stores without that requirement
// should use this helper.
func inTx(ctx context.Context, q queryer, fn func(queryer) error) error {
	switch v := q.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return fmt.Errorf("postgres store: unexpected queryer type %T", q)
	}
}
