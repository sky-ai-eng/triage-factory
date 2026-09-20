package workitem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// inTx runs fn inside a transaction on conn, committing when fn returns nil
// and rolling back otherwise. It is this package's own transaction boundary
// rather than internal/db's because internal/db names this package's types
// in its store interfaces, so nothing here may import it back.
//
// Both the body's error and Commit's are attributed to the context when the
// context is done: database/sql binds a Tx to its ctx and rolls it back from a
// background goroutine the moment that ctx is canceled, so whatever lands
// after the rollback reports sql.ErrTxDone instead of the cancellation that
// caused it. Wrapping both keeps errors.Is true for the original error and for
// the context's, so a caller reading either stays correct.
func inTx(ctx context.Context, conn *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return txCause(ctx, err)
	}
	return txCause(ctx, tx.Commit())
}

// txCause attributes a transaction failure to the context when the context is
// done. A nil err, a live ctx, or an err that already carries the context's
// error passes through unchanged.
func txCause(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	cerr := ctx.Err()
	if cerr == nil || errors.Is(err, cerr) {
		return err
	}
	return fmt.Errorf("%w (%w)", err, cerr)
}
