package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEntityStore_SQLite runs the shared conformance suite against the
// SQLite EntityStore impl. Each subtest opens a fresh in-memory DB so
// lifecycle assertions don't bleed across.
func TestEntityStore_SQLite(t *testing.T) {
	dbtest.RunEntityStoreConformance(t, func(t *testing.T) (db.EntityStore, string, dbtest.EntitySeeder) {
		t.Helper()
		conn := newSQLiteForEntityTest(t)
		seed := newSQLiteEntitySeeder(conn)
		stores := sqlitestore.New(conn)
		return stores.Entities, runmode.LocalDefaultOrgID, seed
	})
}

// TestEntityStore_SQLite_RejectsNonLocalOrg pins the SQLite-side
// assertLocalOrg guard: a multi-mode caller that drifts through with a
// real-looking org UUID must fail loudly rather than silently reading
// the lone local row.
func TestEntityStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForEntityTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	if _, _, err := stores.Entities.FindOrCreate(t.Context(), bogusOrg, "github", "owner/repo#1", "pr", "T", ""); err == nil {
		t.Errorf("expected error for non-local orgID, got nil")
	}
	if _, err := stores.Entities.Get(t.Context(), bogusOrg, "any-id"); err == nil {
		t.Errorf("expected error for non-local orgID on Get, got nil")
	}
}

// TestEntityStore_SQLite_ListActiveJiraTeamScoped pins the local-mode
// contract for the discovery-read variant: SQLite applies NO team
// scoping (N=1) and returns the full active Jira set, identical to
// ListActive(orgID, "jira"). The Postgres impl scopes to the viewer's
// team-attached Jira projects; local mode deliberately does not.
func TestEntityStore_SQLite_ListActiveJiraTeamScoped(t *testing.T) {
	conn := newSQLiteForEntityTest(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	// Two Jira entities (no jira_project_status_rules rows configured at
	// all) plus a GitHub entity that must never appear in a Jira read.
	jira1, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "AAA-1", "issue", "One", "")
	if err != nil {
		t.Fatalf("seed jira1: %v", err)
	}
	jira2, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "BBB-2", "issue", "Two", "")
	if err != nil {
		t.Fatalf("seed jira2: %v", err)
	}
	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "owner/repo#1", "pr", "PR", ""); err != nil {
		t.Fatalf("seed github: %v", err)
	}

	got, err := stores.Entities.ListActiveJiraTeamScoped(ctx, org, "")
	if err != nil {
		t.Fatalf("ListActiveJiraTeamScoped: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range got {
		if e.Source != "jira" {
			t.Errorf("non-jira entity %s (%s) leaked into jira deck", e.ID, e.Source)
		}
		ids[e.ID] = true
	}
	// Both Jira entities surface despite no rules configured — local mode
	// is unscoped. If this started excluding untasked/unconfigured Jira
	// tickets, the deck would hide its own contents.
	if !ids[jira1.ID] || !ids[jira2.ID] {
		t.Errorf("local ListActiveJiraTeamScoped should return all active jira; got %v, want %s and %s", ids, jira1.ID, jira2.ID)
	}
}

func newSQLiteForEntityTest(t *testing.T) *sql.DB {
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

func newSQLiteEntitySeeder(conn *sql.DB) dbtest.EntitySeeder {
	return dbtest.EntitySeeder{
		Team: func(t *testing.T, name string) string {
			t.Helper()
			// Raw insert rather than the Teams store: local mode is N=1 and
			// its team creation is not a supported operation, but the column
			// under test is an FK to this table in both dialects, so the
			// conformance suite still needs two real rows to write against.
			id := uuid.New().String()
			if _, err := conn.Exec(
				`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
				id, runmode.LocalDefaultOrgID, "seed-"+id[:8], name,
			); err != nil {
				t.Fatalf("seed team %s: %v", name, err)
			}
			return id
		},
		User: func(t *testing.T, name string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(
				`INSERT INTO users (id, display_name) VALUES (?, ?)`, id, name,
			); err != nil {
				t.Fatalf("seed user %s: %v", name, err)
			}
			return id
		},
		CommissionedBy: func(t *testing.T, entityID string) string {
			t.Helper()
			var got sql.NullString
			if err := conn.QueryRow(
				`SELECT commissioned_by_user_id FROM entities WHERE id = ?`, entityID,
			).Scan(&got); err != nil {
				t.Fatalf("read commissioned_by_user_id for %s: %v", entityID, err)
			}
			return got.String
		},
	}
}
