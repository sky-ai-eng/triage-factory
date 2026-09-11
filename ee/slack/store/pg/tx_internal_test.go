package pg

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// TestInTx_CanceledCtxSurfacesAsCanceled pins the disconnect contract on the
// Slack store's transaction helper: a ctx that dies inside fn surfaces as
// context.Canceled whichever of the stdlib's rollback goroutine or the commit
// lands first, so the Slack handlers' client-gone classification holds.
func TestInTx_CanceledCtxSurfacesAsCanceled(t *testing.T) {
	h := pgtest.Shared(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := inTx(ctx, h.AdminDB, func(q db.Execer) error {
		if _, err := q.ExecContext(ctx, `SELECT 1`); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("inTx committed under a canceled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false; got %v", err)
	}
}
