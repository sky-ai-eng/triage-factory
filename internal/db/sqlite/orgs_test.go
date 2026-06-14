package sqlite_test

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestOrgsStore_SQLite_ListActiveSystem_ReturnsSentinel pins the
// local-mode behavior: the only active org is the
// runmode.LocalDefaultOrgID sentinel seeded by the baseline
// migration. Background-service callers (poller, tracker,
// projectclassify, repoprofile) collapse their per-org loop to N=1
// against this row.
func TestOrgsStore_SQLite_ListActiveSystem_ReturnsSentinel(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	got, err := stores.Orgs.ListActiveSystem(context.Background())
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(got) != 1 || got[0] != runmode.LocalDefaultOrgID {
		t.Errorf("ListActiveSystem = %v; want [%s]", got, runmode.LocalDefaultOrgID)
	}
}

// TestOrgsStore_SQLite_ListActiveSystem_IteratesAllRows pins that the
// implementation makes no assumption about local mode being N=1 — a
// hypothetical fixture with multiple rows iterates them in stable id
// order. Local mode collapses to one row in practice, but the SQL
// doesn't know that.
func TestOrgsStore_SQLite_ListActiveSystem_IteratesAllRows(t *testing.T) {
	conn := openSQLiteForTest(t)
	if _, err := conn.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		"00000000-0000-0000-0000-000000000200", "extra-a", "Extra A",
	); err != nil {
		t.Fatalf("seed extra org: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		"00000000-0000-0000-0000-000000000300", "extra-b", "Extra B",
	); err != nil {
		t.Fatalf("seed extra org: %v", err)
	}

	stores := sqlitestore.New(conn)
	got, err := stores.Orgs.ListActiveSystem(context.Background())
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	want := []string{
		runmode.LocalDefaultOrgID,
		"00000000-0000-0000-0000-000000000200",
		"00000000-0000-0000-0000-000000000300",
	}
	if len(got) != len(want) {
		t.Fatalf("ListActiveSystem returned %d rows; want %d (%v)", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("ListActiveSystem[%d] = %s; want %s (ordering should be id ASC)", i, got[i], id)
		}
	}
}
