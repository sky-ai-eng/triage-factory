package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// TestInTx_CanceledCtxSurfacesAsCanceled pins the disconnect contract on the
// store-internal transaction helper, and on inTxRaw below it: a ctx that dies
// inside fn surfaces as context.Canceled on the branch where the stdlib's
// rollback goroutine beat the commit. See dbtest.WaitTxDone for why the body
// waits that race out instead of running it.
func TestInTx_CanceledCtxSurfacesAsCanceled(t *testing.T) {
	h := pgtest.Shared(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := inTx(ctx, h.AdminDB, func(q queryer) error {
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

// TestInTxRaw_ComposesWithOuterTx pins the other arm: handed a *sql.Tx it runs
// fn against that one and neither commits nor rolls back, so the outer caller
// keeps ownership of the boundary.
func TestInTxRaw_ComposesWithOuterTx(t *testing.T) {
	h := pgtest.Shared(t)

	outer, err := h.AdminDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin outer: %v", err)
	}
	defer func() { _ = outer.Rollback() }()

	var got *sql.Tx
	if err := inTxRaw(context.Background(), outer, func(tx *sql.Tx) error {
		got = tx
		return nil
	}); err != nil {
		t.Fatalf("inTxRaw on an outer tx: %v", err)
	}
	if got != outer {
		t.Error("fn did not run against the caller's own tx")
	}
	// Still usable: inTxRaw must not have ended the outer transaction.
	if _, err := outer.ExecContext(context.Background(), `SELECT 1`); err != nil {
		t.Errorf("outer tx unusable after inTxRaw: %v", err)
	}
}
