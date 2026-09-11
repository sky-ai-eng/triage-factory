package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// WaitTxDone blocks until database/sql has finished rolling a transaction back
// after its ctx was canceled, then returns nil. Use it as the last thing a
// transaction-helper test's body does, so the Commit that follows is
// guaranteed to land on a finished transaction.
//
// It exists because the failure these tests are about is a race the caller
// cannot see. database/sql cancels a Tx from a background goroutine, so a
// Commit issued afterwards reports either ctx.Err() (it checked first) or
// sql.ErrTxDone (the goroutine won). Only the second reaches the caller with
// nothing left of the cancellation, which is the case db.TxCause exists to
// repair — and a test that just cancels and returns takes whichever branch the
// scheduler hands it, so it passes with the repair removed. Waiting the race
// out picks the branch deliberately.
//
// probe must run a statement on the transaction under the ctx it is handed —
// a live one, so the only thing that can fail it is the transaction already
// being done, which is the state being waited for. A non-nil return means the
// transaction never finished, and the body should return it so the test fails
// as a broken premise rather than a missing wrap.
func WaitTxDone(t *testing.T, probe func(context.Context) error) error {
	t.Helper()
	const deadline = 2 * time.Second
	for waited := time.Duration(0); waited < deadline; waited += time.Millisecond {
		if err := probe(context.Background()); errors.Is(err, sql.ErrTxDone) {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("dbtest: transaction was still live 2s after its ctx was canceled; the rollback this test waits for never happened")
}
