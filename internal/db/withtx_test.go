package db_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// TestWithTx_SetsClaims confirms that the JSON payload reaches
// request.jwt.claims and is observable via tf.current_user_id() /
// tf.current_org_id() inside the transaction.
//
// Uses the admin connection — claim visibility doesn't depend on the
// role, only on the GUC being set. RLS-gated behavior is tested
// separately under the harness's WithUser helper.
func TestWithTx_SetsClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	wantSub := "11111111-1111-1111-1111-111111111111"
	wantOrg := "22222222-2222-2222-2222-222222222222"

	err := db.WithTx(context.Background(), h.AdminDB,
		db.Claims{Sub: wantSub, OrgID: wantOrg},
		func(tx *sql.Tx) error {
			var sub, org string
			// tf.current_user_id() returns uuid; cast to text for the assertion
			if err := tx.QueryRow(
				`SELECT tf.current_user_id()::text, tf.current_org_id()::text`,
			).Scan(&sub, &org); err != nil {
				return err
			}
			if sub != wantSub {
				t.Errorf("sub claim: got %q want %q", sub, wantSub)
			}
			if org != wantOrg {
				t.Errorf("org claim: got %q want %q", org, wantOrg)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

// TestWithTx_RollsBackOnError confirms that an error from fn doesn't
// commit. We INSERT a row inside fn, then return an error; the row
// should not be visible outside the transaction.
func TestWithTx_RollsBackOnError(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	var probeID string
	if err := h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&probeID); err != nil {
		t.Fatalf("gen uuid: %v", err)
	}
	h.SeedAuthUser(t, probeID, probeID+"@test")

	sentinel := "rollback-marker"
	err := db.WithTx(context.Background(), h.AdminDB,
		db.Claims{Sub: probeID, OrgID: probeID},
		func(tx *sql.Tx) error {
			if _, err := tx.Exec(
				`INSERT INTO users (id, display_name) VALUES ($1, $2)`, probeID, sentinel,
			); err != nil {
				return err
			}
			return errForcedRollback
		})
	if err != errForcedRollback {
		t.Fatalf("expected errForcedRollback, got %v", err)
	}

	var count int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE display_name = $1`, sentinel,
	).Scan(&count); err != nil {
		t.Fatalf("post-rollback count: %v", err)
	}
	if count != 0 {
		t.Errorf("row visible after rollback: count=%d", count)
	}
}

// TestWithTx_ScopedSetConfig confirms the SET LOCAL doesn't leak — a
// second WithTx call with empty claims sees the SQL helpers return
// their nil-shaped fallback rather than the previous call's values.
func TestWithTx_ScopedSetConfig(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	first := "33333333-3333-3333-3333-333333333333"
	if err := db.WithTx(context.Background(), h.AdminDB,
		db.Claims{Sub: first, OrgID: first},
		func(tx *sql.Tx) error { return nil },
	); err != nil {
		t.Fatalf("WithTx #1: %v", err)
	}

	// Second call with no claims; the GUC must be empty inside.
	if err := db.WithTx(context.Background(), h.AdminDB, db.Claims{},
		func(tx *sql.Tx) error {
			var got string
			if err := tx.QueryRow(
				`SELECT current_setting('request.jwt.claims', true)`,
			).Scan(&got); err != nil {
				return err
			}
			// We marshalled `db.Claims{}` so the payload is `{"sub":""}`,
			// not the previous call's payload — that's the assertion.
			if got == `{"sub":"`+first+`","org_id":"`+first+`"}` {
				t.Fatalf("GUC leaked across transactions: %q", got)
			}
			return nil
		}); err != nil {
		t.Fatalf("WithTx #2: %v", err)
	}
}

var errForcedRollback = forcedErr("forced rollback for test")

type forcedErr string

func (e forcedErr) Error() string { return string(e) }

// TestWithTx_CanceledCtxSurfacesAsCanceled pins the disconnect contract on
// the claims-setting helper: a ctx that dies inside fn surfaces as
// context.Canceled whichever of the stdlib's rollback goroutine or the
// commit lands first, so a handler's client-gone classification holds.
func TestWithTx_CanceledCtxSurfacesAsCanceled(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := db.WithTx(ctx, h.AdminDB, db.Claims{Sub: "11111111-1111-1111-1111-111111111111"},
		func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `SELECT 1`); err != nil {
				return err
			}
			cancel()
			return nil
		})
	if err == nil {
		t.Fatal("WithTx committed under a canceled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false; got %v", err)
	}
}
