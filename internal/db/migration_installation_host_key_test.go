package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration that puts github_host into the active-account key of
// org_github_app_installations (202610100003). Before it, one org could hold a
// login once across every GitHub; after it, once per GitHub. The fixture starts
// on the old key — the second host's install of the same login is refused —
// so the case proves the migration is what admits it, and that one host still
// refuses the login twice.
func TestMigrate_InstallationAccountKeyIncludesHost(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, 202610100001)
	seedGitHubHostOrg(t, database)

	install := func(installationID, host string) error {
		_, err := database.Exec(`
			INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login, github_host)
			VALUES (?, ?, 'Organization', 'acme', ?)
		`, installationID, runmode.LocalDefaultOrgID, host)
		return err
	}
	if err := install("1", "https://github.com"); err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	if err := install("2", "https://ghe.example.com"); err == nil {
		t.Fatal("the same login on a second host was admitted before the migration; the fixture must start on the old key")
	}

	migrateInstallationHostKey(t, database)

	if err := install("2", "https://ghe.example.com"); err != nil {
		t.Errorf("the same login on a second host after the migration: %v", err)
	}
	if err := install("3", "https://ghe.example.com"); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("the same login twice on one host after the migration: err=%v, want a UNIQUE violation", err)
	}
	var live int
	if err := database.QueryRow(`SELECT COUNT(*) FROM org_github_app_installations WHERE removed_at IS NULL`).Scan(&live); err != nil {
		t.Fatalf("count installations: %v", err)
	}
	if live != 2 {
		t.Errorf("live installations = %d, want 2", live)
	}
}

func migrateInstallationHostKey(t *testing.T, database *sql.DB) {
	t.Helper()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202610100003)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo installation host key: %v", upErr)
	}
}
