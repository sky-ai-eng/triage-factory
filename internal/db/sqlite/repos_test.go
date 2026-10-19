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
		Host:  dbtest.TestGitHubHost,
		Owner: id, Repo: id,
	}
}

// TestRepositoryStore_SQLite runs the shared conformance suite against the
// SQLite RepositoryStore impl. Each subtest gets a fresh in-memory DB.
func TestRepositoryStore_SQLite(t *testing.T) {
	dbtest.RunRepositoryStoreConformance(t, func(t *testing.T) (db.RepositoryStore, string, dbtest.RepositorySeeder) {
		t.Helper()
		conn := newSQLiteForRepoTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.RepositorySeeder{
			Tracking: stores.TeamGitHubRepos,
			TeamID:   runmode.LocalDefaultTeamID,
			Team: func(t *testing.T, slug string) string {
				t.Helper()
				id := uuid.New().String()
				seedExtraTeam(t, conn, id, slug)
				return id
			},
		}
		return stores.Repos, runmode.LocalDefaultOrgID, seed
	})
}

// TestRepositoryStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// guard — every method must refuse a non-local orgID.
// TestRepositoryStore_SQLite_UpsertReturnedRowConformance runs the
// host-carrying upsert's returned-row suite against the SQLite impl, the same
// cases Postgres runs under RLS.
func TestRepositoryStore_SQLite_UpsertReturnedRowConformance(t *testing.T) {
	dbtest.RunRepositoryUpsertReturnedRowConformance(t, func(t *testing.T) (db.RepositoryStore, string) {
		t.Helper()
		return sqlitestore.New(openSQLiteForTest(t)).Repos, runmode.LocalDefaultOrgID
	})
}

func TestRepositoryStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForRepoTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	if _, err := stores.Repos.Upsert(t.Context(), bogusOrg, anyRepository()); err == nil {
		t.Errorf("Upsert with non-local orgID should error")
	}
	if _, err := stores.Repos.GetByRef(t.Context(), bogusOrg, domain.RepoRefFromSlug(dbtest.TestGitHubHost, "any/repo")); err == nil {
		t.Errorf("Get with non-local orgID should error")
	}
	if _, _, err := stores.Repos.List(t.Context(), bogusOrg, dbtest.TestGitHubHost, db.ListOpts{}); err == nil {
		t.Errorf("List with non-local orgID should error")
	}
	if _, _, err := stores.Repos.ListTrackedSystem(t.Context(), bogusOrg, dbtest.TestGitHubHost, db.ListOpts{}); err == nil {
		t.Errorf("ListTrackedSystem with non-local orgID should error")
	}
	if _, err := stores.Repos.ListTrackedByTeamSystem(t.Context(), bogusOrg, runmode.LocalDefaultTeamID, dbtest.TestGitHubHost); err == nil {
		t.Errorf("ListTrackedByTeamSystem with non-local orgID should error")
	}
}

// TestRepositoryStore_SQLite_ListTeamScoped_MirrorsTrackedSet pins the
// local-mode asymmetry: N=1 has no other team to scope away, so
// ListTeamScoped returns the identical set ListTrackedSystem does — unlike the
// Postgres impl, which semi-joins through team_github_repos under RLS. A
// registry row no team tracks is in neither.
func TestRepositoryStore_SQLite_ListTeamScoped_MirrorsTrackedSet(t *testing.T) {
	conn := newSQLiteForRepoTest(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	track := func(repos ...domain.TeamGitHubRepo) {
		t.Helper()
		if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, dbtest.TestGitHubHost, repos); err != nil {
			t.Fatalf("track repos: %v", err)
		}
	}
	track(domain.TeamGitHubRepo{Owner: "a", Repo: "one"}, domain.TeamGitHubRepo{Owner: "b", Repo: "two"}, domain.TeamGitHubRepo{Owner: "c", Repo: "gone"})
	track(domain.TeamGitHubRepo{Owner: "a", Repo: "one"}, domain.TeamGitHubRepo{Owner: "b", Repo: "two"})

	tracked, _, err := stores.Repos.ListTrackedSystem(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, db.ListOpts{Limit: 50})
	if err != nil {
		t.Fatalf("ListTrackedSystem: %v", err)
	}
	scoped, total, err := stores.Repos.ListTeamScoped(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, db.ListOpts{Limit: 50})
	if err != nil {
		t.Fatalf("ListTeamScoped: %v", err)
	}
	if len(scoped) != 2 || total != 2 || len(tracked) != len(scoped) {
		t.Fatalf("ListTeamScoped returned %d rows (total %d), ListTrackedSystem %d; want 2 on both — c/gone is untracked", len(scoped), total, len(tracked))
	}
	for i := range tracked {
		if tracked[i].ID != scoped[i].ID {
			t.Errorf("row %d: ListTrackedSystem=%s ListTeamScoped=%s", i, tracked[i].ID, scoped[i].ID)
		}
	}
}

func newSQLiteForRepoTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
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
