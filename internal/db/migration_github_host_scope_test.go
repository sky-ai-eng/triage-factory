package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration that scopes repositories and GitHub team mappings by host
// (202610100001) computes the host in SQL from the org's stored base URL, and
// every later read and write computes it through EffectiveGitHubHost. If the two
// disagreed, every pre-existing repository and mapping would sit under a host no
// read asks for: the tracked set would read empty and the poller would stop. So
// each case stages a row, migrates, and requires the stored host to equal what
// EffectiveGitHubHost answers for the same base URL.
func TestMigrate_GitHubHostBackfillMatchesGo(t *testing.T) {
	cases := []struct {
		name string
		base *string // the org's GitHub base URL; nil = no override recorded
	}{
		{name: "default", base: nil},
		{name: "empty", base: ptr("")},
		{name: "host", base: ptr("https://ghe.example.com")},
		{name: "trailing slash", base: ptr("https://ghe.example.com/")},
		{name: "trailing slashes", base: ptr("https://ghe.example.com//")},
		{name: "context path", base: ptr("https://example.com/github/")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := openMigrationsTestDBAt(t, TestDSNMemory, 202610080001)
			seedGitHubHostOrg(t, database)
			base := ""
			if tc.base != nil {
				base = *tc.base
				if _, err := database.Exec(`
					INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', ?)
					ON CONFLICT (org_id, kind) DO UPDATE SET base_url = excluded.base_url
				`, runmode.LocalDefaultOrgID, base); err != nil {
					t.Fatalf("seed base url: %v", err)
				}
			}
			if _, err := database.Exec(
				`INSERT INTO repositories (id, owner, repo, source, external_id) VALUES ('r1', 'octo', 'api', 'github', '7')`,
			); err != nil {
				t.Fatalf("seed repository: %v", err)
			}
			if _, err := database.Exec(
				`INSERT INTO team_github_groups (team_id, github_org_login, github_team_slug) VALUES ('team-1', 'octo', 'core')`,
			); err != nil {
				t.Fatalf("seed group: %v", err)
			}

			migrateGitHubHostScope(t, database)

			want := EffectiveGitHubHost(base)
			var repoHost, groupHost string
			if err := database.QueryRow(`SELECT host FROM repositories WHERE id = 'r1'`).Scan(&repoHost); err != nil {
				t.Fatalf("read repository host: %v", err)
			}
			if repoHost != want {
				t.Errorf("migrated repository host = %q, EffectiveGitHubHost = %q", repoHost, want)
			}
			if err := database.QueryRow(`SELECT host FROM team_github_groups WHERE team_id = 'team-1'`).Scan(&groupHost); err != nil {
				t.Fatalf("read group host: %v", err)
			}
			if groupHost != want {
				t.Errorf("migrated group host = %q, EffectiveGitHubHost = %q", groupHost, want)
			}
		})
	}
}

// TestMigrate_GitHubHostKeepsReferencesAndSettlesDuplicateIDs migrates a
// registry that two rows share one provider id in — nothing enforced the id's
// uniqueness before — under the rows that reference it. The id stays on the
// most recently updated row, every reference survives, and the new indexes
// separate hosts.
func TestMigrate_GitHubHostKeepsReferencesAndSettlesDuplicateIDs(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, 202610080001)
	seedGitHubHostOrg(t, database)
	if _, err := database.Exec(`
		INSERT INTO repositories (id, owner, repo, source, external_id, updated_at)
		VALUES ('r-old', 'octo', 'api', 'github', '7', '2026-01-01 00:00:00'),
		       ('r-new', 'octo', 'platform', 'github', '7', '2026-02-01 00:00:00'),
		       ('r-other', 'octo', 'web', 'github', '8', '2026-01-01 00:00:00')
	`); err != nil {
		t.Fatalf("seed repositories: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO team_github_repos (team_id, repository_id, org_id)
		VALUES ('team-1', 'r-old', ?), ('team-1', 'r-other', ?)
	`, runmode.LocalDefaultOrgID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed tracking: %v", err)
	}

	migrateGitHubHostScope(t, database)

	ids := map[string]sql.NullString{}
	rows, err := database.Query(`SELECT id, external_id FROM repositories`)
	if err != nil {
		t.Fatalf("read repositories: %v", err)
	}
	for rows.Next() {
		var id string
		var ext sql.NullString
		if err := rows.Scan(&id, &ext); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids[id] = ext
	}
	_ = rows.Close()
	if ids["r-new"].String != "7" || ids["r-old"].Valid || ids["r-other"].String != "8" {
		t.Errorf("external ids after migrate = %v, want the shared id only on the most recently updated row", ids)
	}

	var tracked int
	if err := database.QueryRow(`SELECT COUNT(*) FROM team_github_repos`).Scan(&tracked); err != nil {
		t.Fatalf("count tracking: %v", err)
	}
	if tracked != 2 {
		t.Errorf("tracking rows = %d after migrate, want 2", tracked)
	}
	fk, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer fk.Close()
	if fk.Next() {
		t.Error("foreign_key_check reports a dangling reference after the migration")
	}

	// One name per host, and one id per host.
	if _, err := database.Exec(`
		INSERT INTO repositories (id, owner, repo, source, host, external_id)
		VALUES ('r-ghe', 'OCTO', 'API', 'github', 'https://ghe.example.com', '7')
	`); err != nil {
		t.Errorf("the same name and id on another host was refused: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO repositories (id, owner, repo, source, host)
		VALUES ('r-dup', 'Octo', 'Api', 'github', 'https://github.com')
	`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("a second row under one name on one host: err=%v, want a UNIQUE violation", err)
	}
	if _, err := database.Exec(`
		INSERT INTO repositories (id, owner, repo, source, host, external_id)
		VALUES ('r-dup-id', 'octo', 'elsewhere', 'github', 'https://github.com', '8')
	`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("a second row under one id on one host: err=%v, want a UNIQUE violation", err)
	}
}

// seedGitHubHostOrg stages the local org and one team; a migrated database
// carries neither until boot seeds them.
func seedGitHubHostOrg(t *testing.T, database *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + runmode.LocalDefaultOrgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('team-1', '` + runmode.LocalDefaultOrgID + `', 'core', 'Core')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
}

func migrateGitHubHostScope(t *testing.T, database *sql.DB) {
	t.Helper()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202610100001)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo github host scope: %v", upErr)
	}
}
