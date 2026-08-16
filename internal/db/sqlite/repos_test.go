package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// anyRepository is a minimal fixture for orgID-guard tests where the
// row's fields don't matter — assertLocalOrg fires before the INSERT.
func anyRepository() domain.Repository {
	id := uuid.New().String()
	return domain.Repository{
		Owner: id, Repo: id,
	}
}

// TestRepositoryStore_SQLite runs the shared conformance suite against the
// SQLite RepositoryStore impl. Each subtest gets a fresh in-memory DB.
func TestRepositoryStore_SQLite(t *testing.T) {
	dbtest.RunRepositoryStoreConformance(t, func(t *testing.T) (db.RepositoryStore, string) {
		t.Helper()
		conn := newSQLiteForRepoTest(t)
		stores := sqlitestore.New(conn)
		return stores.Repos, runmode.LocalDefaultOrgID
	})
}

// TestRepositoryStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// guard — every method must refuse a non-local orgID.
func TestRepositoryStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForRepoTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	if err := stores.Repos.Upsert(t.Context(), bogusOrg, anyRepository()); err == nil {
		t.Errorf("Upsert with non-local orgID should error")
	}
	if _, err := stores.Repos.GetByRef(t.Context(), bogusOrg, domain.RepoRefFromSlug("any/repo")); err == nil {
		t.Errorf("Get with non-local orgID should error")
	}
	if _, err := stores.Repos.List(t.Context(), bogusOrg); err == nil {
		t.Errorf("List with non-local orgID should error")
	}
}

// TestRepositoryStore_SQLite_ListTeamScoped_MirrorsList pins the local-mode
// asymmetry (TFAC-559): N=1 has no other team to scope away, so
// ListTeamScoped returns the identical set List does — unlike the
// Postgres impl, which semi-joins through team_github_repos under RLS.
func TestRepositoryStore_SQLite_ListTeamScoped_MirrorsList(t *testing.T) {
	conn := newSQLiteForRepoTest(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	if err := stores.Repos.SetConfigured(ctx, runmode.LocalDefaultOrgID, []string{"a/one", "b/two"}); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	all, err := stores.Repos.List(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	scoped, err := stores.Repos.ListTeamScoped(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListTeamScoped: %v", err)
	}
	if len(scoped) != len(all) {
		t.Fatalf("ListTeamScoped returned %d rows, want %d (unscoped local mode)", len(scoped), len(all))
	}
	for i := range all {
		if all[i].ID != scoped[i].ID {
			t.Errorf("row %d: List=%s ListTeamScoped=%s", i, all[i].ID, scoped[i].ID)
		}
	}
}

func newSQLiteForRepoTest(t *testing.T) *sql.DB {
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
