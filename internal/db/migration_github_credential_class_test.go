package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that states an org's GitHub credential class instead of
// leaving it to be inferred (202608150003). SQLite is forward-only and ships
// against existing laptop installs, so the backfill has to reconstruct the
// class from the fact the code was inferring it from — the presence of an
// org_github_apps row.
//
// The staged case is the one that would be easy to get wrong. An App
// registered mid PAT→App switch is active = false with a live PAT beside it,
// and a backfill keyed on "which credential is live" would call that org PAT.
// It isn't: the class names WHICH CREDENTIAL SYSTEM the org is in, and that org
// is in the App system from the moment its key was stored. Keying the backfill
// on row presence rather than on active is what keeps the class and the Active
// bit answering their two different questions.
func TestMigrate_BackfillsGitHubCredentialClass(t *testing.T) {
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
	// Stop one version short of the credential-class migration.
	upToErr := goose.UpTo(database, dir, 202608150002)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	seedOrg := func(id, name string) {
		t.Helper()
		if _, err := database.Exec(
			`INSERT INTO orgs (id, name, slug) VALUES (?, ?, ?)`, id, name, name,
		); err != nil {
			t.Fatalf("seed org %s: %v", id, err)
		}
		if _, err := database.Exec(`INSERT INTO org_settings (org_id) VALUES (?)`, id); err != nil {
			t.Fatalf("seed org_settings %s: %v", id, err)
		}
	}
	seedApp := func(orgID, appID string, active int) {
		t.Helper()
		if _, err := database.Exec(`
			INSERT INTO org_github_apps
				(org_id, app_id, slug, client_id, client_secret_ref, pem_ref, webhook_secret_ref, active)
			VALUES (?, ?, 'tf', 'Iv1.x', 'cs', 'pem', 'wh', ?)
		`, orgID, appID, active); err != nil {
			t.Fatalf("seed org_github_apps %s: %v", orgID, err)
		}
	}

	// A PAT org: no registration row at all.
	seedOrg("org-pat", "pat-org")
	// A live BYO-App org.
	seedOrg("org-live-app", "live-app-org")
	seedApp("org-live-app", "1", 1)
	// A STAGED App: registered but not yet cut over, so the PAT beside it is
	// still the live credential. Still a BYO-App org.
	seedOrg("org-staged-app", "staged-app-org")
	seedApp("org-staged-app", "2", 0)
	// An org with no org_settings row at all — the column's DEFAULT is what
	// answers for it once provisioning (or the store's partial upsert) creates
	// one, and the backfill must not choke on its absence.
	if _, err := database.Exec(`INSERT INTO orgs (id, name, slug) VALUES ('org-no-settings', 'n', 'n')`); err != nil {
		t.Fatalf("seed settings-less org: %v", err)
	}

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.Up(database, dir)
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.Up: %v", upErr)
	}

	classOf := func(orgID string) string {
		t.Helper()
		var class string
		if err := database.QueryRow(
			`SELECT github_credential_class FROM org_settings WHERE org_id = ?`, orgID,
		).Scan(&class); err != nil {
			t.Fatalf("read class for %s: %v", orgID, err)
		}
		return class
	}

	for _, c := range []struct{ org, want string }{
		{"org-pat", "pat"},
		{"org-live-app", "byo_app"},
		// The assertion this test exists for: staged is byo_app, NOT pat.
		{"org-staged-app", "byo_app"},
	} {
		if got := classOf(c.org); got != c.want {
			t.Errorf("%s: class = %q, want %q", c.org, got, c.want)
		}
	}

	// The column is NOT NULL with a default, so a row created after the
	// migration lands on 'pat' without anyone writing it.
	if _, err := database.Exec(`INSERT INTO org_settings (org_id) VALUES ('org-no-settings')`); err != nil {
		t.Fatalf("insert post-migration settings row: %v", err)
	}
	if got := classOf("org-no-settings"); got != "pat" {
		t.Errorf("post-migration row default class = %q, want pat", got)
	}
}
