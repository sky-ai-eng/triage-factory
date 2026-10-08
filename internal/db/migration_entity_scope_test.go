package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration that gives every entity a scope (202610080001) computes it in
// SQL from data already stored, and the Go side computes the same value through
// domain.EntityScope on every later read and write. If the two disagreed, every
// pre-existing row would sit under a scope no lookup asks for, and the poller
// would mint a second entity beside each one. So each case here stages a row
// the way the tracker wrote it, migrates, and requires the stored scope to equal
// what domain.EntityScope answers for the same base URL.
func TestMigrate_EntityScopeBackfillMatchesGo(t *testing.T) {
	cases := []struct {
		name   string
		source string
		base   *string // the org's base URL for the source; nil = no override recorded
		url    func(base string) string
	}{
		{name: "github default", source: "github", base: nil},
		{name: "github empty", source: "github", base: ptr("")},
		{name: "github host", source: "github", base: ptr("https://ghe.example.com")},
		{name: "github trailing slash", source: "github", base: ptr("https://ghe.example.com/")},
		{name: "github trailing slashes", source: "github", base: ptr("https://ghe.example.com//")},
		{name: "github context path", source: "github", base: ptr("https://example.com/github/")},
		{name: "jira host", source: "jira", base: ptr("https://acme.atlassian.net"), url: jiraBrowseURL},
		{name: "jira trailing slash", source: "jira", base: ptr("https://jira.example.com/"), url: jiraBrowseURL},
		{name: "jira context path", source: "jira", base: ptr("https://jira.example.com/jira/"), url: jiraBrowseURL},
		{name: "jira stub falls back to the base url", source: "jira", base: ptr("https://jira.example.com/jira/")},
		{name: "jira stub with a schemeless base url", source: "jira", base: ptr("jira.example.com")},
		{name: "jira stub with no base url", source: "jira", base: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := openMigrationsTestDBAt(t, TestDSNMemory, 202610060003)
			seedEntityScopeOrg(t, database)
			base := ""
			if tc.base != nil {
				base = *tc.base
				if _, err := database.Exec(`
					INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, ?, ?)
					ON CONFLICT (org_id, kind) DO UPDATE SET base_url = excluded.base_url
				`, runmode.LocalDefaultOrgID, tc.source, base); err != nil {
					t.Fatalf("seed base url: %v", err)
				}
			}
			url := ""
			if tc.url != nil {
				url = tc.url(base)
			}
			if _, err := database.Exec(
				`INSERT INTO entities (id, source, source_id, kind, title, url) VALUES ('e1', ?, 'KEY-1', 'issue', 't', ?)`,
				tc.source, url,
			); err != nil {
				t.Fatalf("seed entity: %v", err)
			}

			migrateEntityScope(t, database)

			var got string
			if err := database.QueryRow(`SELECT scope FROM entities WHERE id = 'e1'`).Scan(&got); err != nil {
				t.Fatalf("read scope: %v", err)
			}
			settings := domain.OrgSettings{GitHubBaseURL: base, JiraBaseURL: base}
			if want := domain.EntityScope(tc.source, settings); got != want {
				t.Errorf("migrated scope = %q, domain.EntityScope = %q", got, want)
			}
			if tc.source == "github" {
				if want := EffectiveGitHubHost(base); got != want {
					t.Errorf("migrated scope = %q, EffectiveGitHubHost = %q", got, want)
				}
			}
		})
	}
}

// TestMigrate_EntityScopeKeepsEveryReference rebuilds the entities table under
// rows that point at it, with foreign keys on. A rebuild that let the drop
// cascade would delete every task, event and memory row with the old table.
func TestMigrate_EntityScopeKeepsEveryReference(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, 202610060003)
	seedEntityScopeOrg(t, database)
	if _, err := database.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, state, closed_at)
		VALUES ('e1', 'github', 'octo/api#1', 'pr', 't', 'active', NULL),
		       ('e2', 'github', 'octo/api#2', 'pr', 't', 'closed', CURRENT_TIMESTAMP)
	`); err != nil {
		t.Fatalf("seed entities: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev1', 'e1', 'github:pr:opened', '')`,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES ('t1', 'e1', 'github:pr:opened', 'ev1', 'queued')
	`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	migrateEntityScope(t, database)

	for table, want := range map[string]int{"entities": 2, "events": 1, "tasks": 1} {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s rows = %d after the rebuild, want %d", table, n, want)
		}
	}
	var state, extID sql.NullString
	if err := database.QueryRow(`SELECT state, external_id FROM entities WHERE id = 'e2'`).Scan(&state, &extID); err != nil {
		t.Fatalf("read e2: %v", err)
	}
	if state.String != "closed" || extID.Valid {
		t.Errorf("e2 = state %q external_id %v, want closed with no id learned", state.String, extID)
	}
	rows, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("foreign_key_check reports a dangling reference after the rebuild")
	}
	// The key is unique among active rows only: a closed row's key is free.
	if _, err := database.Exec(`
		INSERT INTO entities (id, source, scope, source_id, kind, state)
		VALUES ('e3', 'github', 'https://github.com', 'octo/api#2', 'pr', 'active')
	`); err != nil {
		t.Errorf("a new active row under a closed row's key was refused: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO entities (id, source, scope, source_id, kind, state)
		VALUES ('e4', 'github', 'https://github.com', 'octo/api#1', 'pr', 'active')
	`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("a second active row under one key: err=%v, want a UNIQUE violation", err)
	}
}

// seedEntityScopeOrg stages the local org and the catalog row the fixtures
// foreign keys need; a migrated database carries neither until boot seeds them.
func seedEntityScopeOrg(t *testing.T, database *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + runmode.LocalDefaultOrgID + `', 'local', 'Local')`,
		`INSERT OR IGNORE INTO events_catalog (id, source, category, label, description) VALUES ('github:pr:opened', 'github', 'pr', 'Opened', '')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
}

func migrateEntityScope(t *testing.T, database *sql.DB) {
	t.Helper()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202610080001)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo entity scope: %v", upErr)
	}
}

func jiraBrowseURL(base string) string {
	return strings.TrimRight(base, "/") + "/browse/KEY-1"
}

func ptr(s string) *string { return &s }
