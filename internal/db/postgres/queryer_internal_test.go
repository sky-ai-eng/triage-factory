package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// TestInTx_CanceledCtxSurfacesAsCanceled pins the disconnect contract on the
// store-internal transaction helper: a ctx that dies inside fn surfaces as
// context.Canceled whichever of the stdlib's rollback goroutine or the commit
// lands first.
func TestInTx_CanceledCtxSurfacesAsCanceled(t *testing.T) {
	h := pgtest.Shared(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := inTx(ctx, h.AdminDB, func(q queryer) error {
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
