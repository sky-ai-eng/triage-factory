package pg

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// TestInTx_CanceledCtxSurfacesAsCanceled pins the disconnect contract on the
// Slack store's transaction helper: a ctx that dies inside fn surfaces as
// context.Canceled on the branch where the stdlib's rollback goroutine beat
// the commit, so the Slack handlers' client-gone classification holds. See
// dbtest.WaitTxDone for why the body waits that race out instead of running
// it.
func TestInTx_CanceledCtxSurfacesAsCanceled(t *testing.T) {
	h := pgtest.Shared(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := inTx(ctx, h.AdminDB, func(q db.Execer) error {
		if _, err := q.ExecContext(ctx, `SELECT 1`); err != nil {
			return err
		}
		cancel()
		return dbtest.WaitTxDone(t, func(live context.Context) error {
			_, err := q.ExecContext(live, `SELECT 1`)
			return err
		})
	})
	if err == nil {
		t.Fatal("inTx committed under a canceled ctx")
	}
	if !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("test did not reach the ErrTxDone branch it exists to pin; got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false; got %v", err)
	}
}
