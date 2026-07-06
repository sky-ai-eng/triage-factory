package pg

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// inTx runs fn against a db.Execer guaranteed to be a transaction, mirroring
// internal/db/postgres's helper of the same name: if q is already a
// *sql.Tx (the app pool inside Stores.Tx.WithTx, or either pool when the
// caller composed us inside NewForTx), fn runs against it directly so the
// outer commit/rollback wins; if q is a *sql.DB (the non-tx aggregate
// Stores, or the raw admin pool), inTx opens its own transaction and
// commits/rolls back based on fn's return.
//
// Needed here (unlike the rest of ee/slack/store/pg, which is single-
// statement) because ReplaceForTeam's insert-then-prune and
// SetPrimarySystem's check-then-clear-then-set each require their
// statements to commit atomically.
func inTx(ctx context.Context, q db.Execer, fn func(db.Execer) error) error {
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
		return fmt.Errorf("slack store: unexpected execer type %T", q)
	}
}
