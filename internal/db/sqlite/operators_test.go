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
		conn := newSQLiteForDeliverableTest(t)
		return sqlitestore.New(conn).Operators, nil
	})
}

// Nothing in local mode
// binds a grant, but a bound row reads the way it does in Postgres, matching
// its principal by id and nobody else.
func TestOperatorStore_SQLite_BoundGrantMatchesItsPrincipal(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	store := sqlitestore.New(conn).Operators
	ctx := context.Background()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO operators (email, user_id) VALUES ('ops@example.com', ?)`, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed bound grant: %v", err)
	}
	for userID, want := range map[string]bool{runmode.LocalDefaultUserID: true, "44444444-4444-4444-4444-444444444444": false} {
		if ok, err := store.IsOperatorUserSystem(ctx, userID); err != nil || ok != want {
			t.Errorf("IsOperatorUserSystem(%s) = %v, %v; want %v", userID, ok, err, want)
		}
	}
	if list, err := store.List(ctx); err != nil || len(list) != 1 || list[0].UserID != runmode.LocalDefaultUserID {
		t.Fatalf("list = %+v, %v; want the one grant bound to %s", list, err, runmode.LocalDefaultUserID)
	}
}
