package sqlite_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// No login identities in local mode, so no seeder.
func TestOperatorStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunOperatorStoreConformance(t, func(t *testing.T) (db.OperatorStore, *dbtest.LoginIdentitySeeder) {
		conn := newSQLiteForArtifactTest(t)
		return sqlitestore.NewOperatorStore(conn), nil
	})
}

// TestOperatorStore_SQLite_BoundGrantMatchesItsPrincipal: nothing in local mode
// binds a grant, but a bound row reads the way it does in Postgres, matching
// its principal by id and nobody else.
func TestOperatorStore_SQLite_BoundGrantMatchesItsPrincipal(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	store := sqlitestore.NewOperatorStore(conn)
	ctx := context.Background()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO operators (email, user_id) VALUES ('ops@example.com', ?)`, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed bound grant: %v", err)
	}
	ok, err := store.IsOperatorUserSystem(ctx, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("IsOperatorUserSystem: %v", err)
	}
	if !ok {
		t.Error("the principal a grant is bound to must be an operator")
	}
	ok, err = store.IsOperatorUserSystem(ctx, "44444444-4444-4444-4444-444444444444")
	if err != nil {
		t.Fatalf("IsOperatorUserSystem: %v", err)
	}
	if ok {
		t.Error("a principal no grant is bound to must not be an operator")
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].UserID != runmode.LocalDefaultUserID {
		t.Fatalf("list = %+v, want the one grant bound to %s", list, runmode.LocalDefaultUserID)
	}
}
