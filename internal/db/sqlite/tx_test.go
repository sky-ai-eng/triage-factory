package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSyntheticClaimsWithTx_SQLite_AcceptsLocalOrg pins the SKY-296
// SQLite contract: SyntheticClaimsWithTx delegates to the same body as
// WithTx in local mode (no auth concept), so passing
// runmode.LocalDefaultOrgID + any userID runs fn inside a tx and commits
// on nil-error.
func TestSyntheticClaimsWithTx_SQLite_AcceptsLocalOrg(t *testing.T) {
	conn := newSQLiteForTxTest(t)
	stores := sqlitestore.New(conn)

	called := false
	if err := stores.Tx.SyntheticClaimsWithTx(
		context.Background(),
		runmode.LocalDefaultOrgID,
		runmode.LocalDefaultUserID,
		func(tx db.TxStores) error {
			called = true
			// Sanity: the bound stores are usable inside the closure.
			if _, err := tx.Repos.List(context.Background(), runmode.LocalDefaultOrgID); err != nil {
				return err
			}
			return nil
		},
	); err != nil {
		t.Fatalf("SyntheticClaimsWithTx: %v", err)
	}
	if !called {
		t.Fatal("fn was not invoked")
	}
}

// TestSyntheticClaimsWithTx_SQLite_RejectsNonLocalOrg verifies that a
// confused caller passing a real UUID-shape orgID (rather than the
// local sentinel) fails loudly. SQLite has no org_id filtering, so the
// assertion is the only line of defense against a multi-mode caller
// hitting the local store by mistake.
func TestSyntheticClaimsWithTx_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForTxTest(t)
	stores := sqlitestore.New(conn)

	err := stores.Tx.SyntheticClaimsWithTx(
		context.Background(),
		"11111111-1111-1111-1111-111111111111", // any non-sentinel UUID
		runmode.LocalDefaultUserID,
		func(tx db.TxStores) error { return nil },
	)
	if err == nil {
		t.Fatal("SyntheticClaimsWithTx with non-local org returned nil; want assertion error")
	}
	if !strings.Contains(err.Error(), runmode.LocalDefaultOrgID) {
		t.Errorf("error %q does not mention the local-org assertion", err.Error())
	}
}

// TestSyntheticClaimsWithTx_SQLite_RollsBackOnError mirrors WithTx's
// commit-on-success / rollback-on-error semantics. fn returning an
// error must leave the database unchanged.
func TestSyntheticClaimsWithTx_SQLite_RollsBackOnError(t *testing.T) {
	conn := newSQLiteForTxTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	// Seed a repo via the non-tx path so we have a baseline row count.
	if err := stores.Repos.SetConfigured(ctx, runmode.LocalDefaultOrgID, []string{"baseline/repo"}); err != nil {
		t.Fatalf("seed baseline: %v", err)
	}

	sentinel := errors.New("forced rollback")
	err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID,
		func(tx db.TxStores) error {
			// Insert a row that should roll back.
			if err := tx.Repos.SetConfigured(ctx, runmode.LocalDefaultOrgID, []string{"baseline/repo", "rolled/back"}); err != nil {
				return err
			}
			return sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}

	got, err := stores.Repos.ListConfiguredNames(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListConfiguredNames: %v", err)
	}
	for _, name := range got {
		if name == "rolled/back" {
			t.Errorf("rolled/back row visible after rollback; row count=%d", len(got))
		}
	}
}

func newSQLiteForTxTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}
