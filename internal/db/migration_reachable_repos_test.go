package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The reachable-repo cache migration lands on an existing laptop install: it
// replaces the App-only grant mirror with a table both credential tiers write,
// beside a column on a table that already has rows. Five things it must get
// right, none of which announce themselves if they go wrong —
//
//   - The existing installation rows survive and read as "grant width not
//     established" rather than picking up a value nobody reported. NULL there is
//     load-bearing: it is what the org page renders as unknown instead of as an
//     all-clear.
//   - The folded unique index actually refuses a second casing, PER TIER. A
//     case-sensitive index behind a case-insensitive guard admits duplicates
//     whenever two writers race, and a duplicated entry double-counts reach.
//   - The two tiers do not collide with each other. An org mid-cutover has both
//     answers on file, and one tier's entry for a slug must not block the
//     other's.
//   - The scope CHECK holds: exactly one of installation_id / host per row,
//     never both and never neither, and never an unknown credential class.
//   - The FK reaches the installation row, so a hard delete of an App
//     registration takes the App tier's entries with it rather than orphaning
//     rows that keep answering "the App can reach this" — while leaving the PAT
//     tier's, which hang off the host instead.
func TestMigrate_ReachableRepoCache(t *testing.T) {
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

	// The App tier's entries: scoped by installation, keyed on the folded slug.
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo, external_id)
		VALUES (?, 'byo_app', '456', 'Acme', 'API', '10')`, org,
	); err != nil {
		t.Fatalf("insert app-tier entry: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo, external_id)
		VALUES (?, 'byo_app', '456', 'acme', 'api', '10')`, org,
	); err == nil {
		t.Error("a second casing of one repository landed; the folded key must refuse it")
	}

	var source string
	if err := database.QueryRow(
		`SELECT source FROM reachable_repositories WHERE org_id = ?`, org,
	).Scan(&source); err != nil {
		t.Fatalf("read reachable entry: %v", err)
	}
	if source != "github" {
		t.Errorf("source = %q; want the default %q — the registry join is keyed on it", source, "github")
	}

	// The PAT tier scopes on the host instead, and its identity index is a
	// separate partial one — so the same slug can be reachable on both tiers at
	// once (an org mid-cutover) without either colliding with the other.
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, host, owner, repo)
		VALUES (?, 'pat', 'https://github.com', 'acme', 'api')`, org,
	); err != nil {
		t.Fatalf("insert pat-tier entry alongside the app-tier one: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, host, owner, repo)
		VALUES (?, 'pat', 'https://github.com', 'Acme', 'API')`, org,
	); err == nil {
		t.Error("a second casing landed on the pat tier; its folded key must refuse it too")
	}

	// The scope columns are alternatives, and it is the database that says so:
	// a row with both has two answers to "what does one refresh replace", and a
	// row with neither can never be replaced at all.
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, installation_id, host, owner, repo)
		VALUES (?, 'byo_app', '456', 'https://github.com', 'acme', 'both')`, org,
	); err == nil {
		t.Error("an entry carrying both an installation and a host landed; the CHECK must refuse it")
	}
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, owner, repo)
		VALUES (?, 'pat', 'acme', 'neither')`, org,
	); err == nil {
		t.Error("an entry carrying no scope landed; the CHECK must refuse it")
	}
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, host, owner, repo)
		VALUES (?, 'something-else', 'https://github.com', 'acme', 'unknown')`, org,
	); err == nil {
		t.Error("an entry in an unknown credential class landed; the CHECK must refuse it")
	}

	// An App-tier entry for an installation that does not exist has nothing to
	// hang off, and would answer "the App can reach this" on behalf of no App.
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo)
		VALUES (?, 'byo_app', '999', 'acme', 'ghost')`, org,
	); err == nil {
		t.Error("an entry landed for an unknown installation; the FK must refuse it")
	}

	// Hard-deleting the installation takes the App tier's entries with it. A
	// soft removal (removed_at) is the store's job; this is the cascade behind
	// it. The PAT tier's entry survives — it hangs off the host, not the
	// installation, which is exactly why it carries one.
	if _, err := database.Exec(
		`DELETE FROM org_github_app_installations WHERE org_id = ? AND installation_id = '456'`, org,
	); err != nil {
		t.Fatalf("delete installation: %v", err)
	}
	var remaining int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM reachable_repositories WHERE org_id = ? AND credential_class = 'byo_app'`, org,
	).Scan(&remaining); err != nil {
		t.Fatalf("count app-tier entries: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d app-tier entries survived the installation's deletion; want 0", remaining)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM reachable_repositories WHERE org_id = ? AND credential_class = 'pat'`, org,
	).Scan(&remaining); err != nil {
		t.Fatalf("count pat-tier entries: %v", err)
	}
	if remaining != 1 {
		t.Errorf("%d pat-tier entries after the installation's deletion; want 1 — they hang off the host, not the installation", remaining)
	}

	// The refresh markers: one row per scope, keyed the same way, and carrying
	// the one thing repository rows cannot express — that a refresh RAN, even
	// when it found nothing.
	if _, err := database.Exec(`
		INSERT INTO reachable_scopes (org_id, credential_class, scope)
		VALUES (?, 'pat', 'https://github.com')`, org,
	); err != nil {
		t.Fatalf("insert scope marker: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO reachable_scopes (org_id, credential_class, scope)
		VALUES (?, 'pat', 'https://github.com')`, org,
	); err == nil {
		t.Error("a second marker for one scope landed; the primary key must refuse it")
	}
	if _, err := database.Exec(`
		INSERT INTO reachable_scopes (org_id, credential_class, scope)
		VALUES (?, 'something-else', 'https://github.com')`, org,
	); err == nil {
		t.Error("a marker in an unknown credential class landed; the CHECK must refuse it")
	}
}
