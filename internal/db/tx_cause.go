package db

import (
	"context"
	"errors"
	"fmt"
)

// TxCause attributes a transaction failure to the context when the context is
// done. database/sql binds a Tx to its ctx and rolls it back from a background
// goroutine the moment the ctx is canceled, so a statement or Commit that
// lands after that rollback reports sql.ErrTxDone rather than ctx.Err() —
// which of the two surfaces is a race the caller cannot see. A handler that
// classifies a client disconnect by errors.Is(err, context.Canceled) would
// then treat half of them as internal faults. Wrapping both keeps errors.Is
// true for the original error and for the context's, so a caller reading
// either stays correct. A nil err, a live ctx, or an err that already carries
// the context's error passes through unchanged.
func TxCause(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	cerr := ctx.Err()
	if cerr == nil || errors.Is(err, cerr) {
		return err
	}
	return fmt.Errorf("%w (%w)", err, cerr)
}
