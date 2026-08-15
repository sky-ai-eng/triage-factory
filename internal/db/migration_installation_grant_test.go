package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The grant-mirror migration lands on an existing laptop install: a new table
// plus a new column on a table that already has rows. Three things it must get
// right, none of which announce themselves if they go wrong —
//
//   - The existing installation rows survive and read as "grant width not
//     established" rather than picking up a value nobody reported. NULL there is
//     load-bearing: it is what the org page renders as unknown instead of as an
//     all-clear.
//   - The folded unique index actually refuses a second casing. A case-sensitive
//     index behind a case-insensitive guard admits duplicates whenever two
//     writers race, and a duplicated grant entry double-counts reach.
//   - The FK reaches the installation row, so a hard delete of an App
//     registration takes its mirror with it rather than orphaning rows that keep
//     answering "the App can reach this".
func TestMigrate_InstallationGrantMirror(t *testing.T) {
	database := openMigrationsTestDB(t)

	gooseMu.Lock()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		gooseMu.Unlock()
		t.Fatalf("migrationsFor: %v", err)
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		gooseMu.Unlock()
		t.Fatalf("SetDialect: %v", err)
	}
	// One version short of the mirror, so the rows below are staged exactly as
	// builds before it wrote them.
	upToErr := goose.UpTo(database, dir, 202608150006)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	const org = "00000000-0000-0000-0000-000000000001"
	if _, err := database.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES (?, 'local', 'Local')`, org,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO org_github_app_installations
			(installation_id, org_id, account_type, account_login)
		VALUES ('456', ?, 'Organization', 'acme')`, org,
	); err != nil {
		t.Fatalf("seed installation: %v", err)
	}

	gooseMu.Lock()
	upErr := goose.Up(database, dir)
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.Up: %v", upErr)
	}

	var (
		login     string
		selection *string
	)
	if err := database.QueryRow(`
		SELECT account_login, repository_selection
		  FROM org_github_app_installations
		 WHERE org_id = ? AND installation_id = '456'`, org,
	).Scan(&login, &selection); err != nil {
		t.Fatalf("read migrated installation: %v", err)
	}
	if login != "acme" {
		t.Errorf("account_login = %q after the migration; want %q", login, "acme")
	}
	if selection != nil {
		t.Errorf("repository_selection = %q on a pre-existing row; want NULL — nothing has reported a width", *selection)
	}

	if _, err := database.Exec(`
		UPDATE org_github_app_installations SET repository_selection = 'some-third-thing'
		 WHERE org_id = ? AND installation_id = '456'`, org,
	); err == nil {
		t.Error("the CHECK accepted a value outside GitHub's two; a third state would read as neither")
	}

	if _, err := database.Exec(`
		INSERT INTO installation_repositories (org_id, installation_id, owner, repo, external_id)
		VALUES (?, '456', 'Acme', 'API', '10')`, org,
	); err != nil {
		t.Fatalf("insert grant entry: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO installation_repositories (org_id, installation_id, owner, repo, external_id)
		VALUES (?, '456', 'acme', 'api', '10')`, org,
	); err == nil {
		t.Error("a second casing of one repository landed; the folded key must refuse it")
	}

	var source string
	if err := database.QueryRow(
		`SELECT source FROM installation_repositories WHERE org_id = ?`, org,
	).Scan(&source); err != nil {
		t.Fatalf("read grant entry: %v", err)
	}
	if source != "github" {
		t.Errorf("source = %q; want the default %q — the registry join is keyed on it", source, "github")
	}

	// A grant entry for an installation that does not exist has nothing to
	// hang off, and would answer "the App can reach this" on behalf of no App.
	if _, err := database.Exec(`
		INSERT INTO installation_repositories (org_id, installation_id, owner, repo)
		VALUES (?, '999', 'acme', 'ghost')`, org,
	); err == nil {
		t.Error("a grant entry landed for an unknown installation; the FK must refuse it")
	}

	// Hard-deleting the installation takes the mirror with it. A soft removal
	// (removed_at) is the store's job; this is the cascade behind it.
	if _, err := database.Exec(
		`DELETE FROM org_github_app_installations WHERE org_id = ? AND installation_id = '456'`, org,
	); err != nil {
		t.Fatalf("delete installation: %v", err)
	}
	var remaining int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM installation_repositories WHERE org_id = ?`, org,
	).Scan(&remaining); err != nil {
		t.Fatalf("count grant entries: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d grant entries survived the installation's deletion; want 0", remaining)
	}
}
