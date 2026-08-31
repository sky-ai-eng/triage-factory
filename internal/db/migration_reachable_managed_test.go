package db

import (
	"testing"

	_ "modernc.org/sqlite"
)

// The reachable-repo mirror has to hold a workspace riding the deployment's
// shared App, and three constraints per dialect decide whether it can. Two of
// them refuse loudly if they are wrong; the third does not, which is why it is
// pinned here beside them —
//
//   - The class CHECK on each table, which names the accepted values.
//   - The scope CHECK, which pairs each class with the scope column its rows
//     carry. Both App classes hang off an installation and carry no host; PAT is
//     the tier that carries a host and no installation.
//   - The App tier's uniqueness index, which is PARTIAL. A class outside its
//     predicate would carry no uniqueness constraint at all, and since the
//     writer inserts ON CONFLICT DO NOTHING — which needs an index to have a
//     conflict to detect — GitHub repeating a repository across a paginated walk
//     would mint a second row and a wrong total_count, quietly.
func TestMigrate_ReachableRepoManagedClass(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
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

	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo, external_id)
		VALUES (?, 'managed_app', '456', 'Acme', 'API', '10')`, org,
	); err != nil {
		t.Fatalf("insert managed entry: %v", err)
	}

	// The silent one: a class outside the App predicate would let this land.
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo, external_id)
		VALUES (?, 'managed_app', '456', 'acme', 'api', '10')`, org,
	); err == nil {
		t.Error("a second casing of one repository landed on the managed class; the folded key must refuse it")
	}

	// The scope alternative holds for managed exactly as it does for a workspace
	// on its own App: an installation and no host, never the reverse and never
	// both.
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, host, owner, repo)
		VALUES (?, 'managed_app', 'https://github.com', 'acme', 'hosted')`, org,
	); err == nil {
		t.Error("a managed entry scoped by host landed; its reach is enumerated per installation, so the CHECK must refuse it")
	}
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, installation_id, host, owner, repo)
		VALUES (?, 'managed_app', '456', 'https://github.com', 'acme', 'both')`, org,
	); err == nil {
		t.Error("a managed entry carrying both an installation and a host landed; the CHECK must refuse it")
	}

	// And the foreign key still reaches the installation row, which is what makes
	// an uninstall take the reach with it rather than orphaning rows that keep
	// answering "the App can reach this".
	if _, err := database.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo)
		VALUES (?, 'managed_app', '999', 'acme', 'ghost')`, org,
	); err == nil {
		t.Error("a managed entry landed for an unknown installation; the FK must refuse it")
	}
	if _, err := database.Exec(
		`DELETE FROM org_github_app_installations WHERE org_id = ? AND installation_id = '456'`, org,
	); err != nil {
		t.Fatalf("delete installation: %v", err)
	}
	var remaining int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM reachable_repositories WHERE org_id = ? AND credential_class = 'managed_app'`, org,
	).Scan(&remaining); err != nil {
		t.Fatalf("count managed entries: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d managed entries survived the installation's deletion; want 0", remaining)
	}

	// The refresh marker: a managed org records its installation id here exactly
	// as a workspace on its own App does, and the class is part of the key.
	if _, err := database.Exec(`
		INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'managed_app', '456')`, org,
	); err != nil {
		t.Fatalf("insert managed scope marker: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'managed_app', '456')`, org,
	); err == nil {
		t.Error("a second marker for one managed scope landed; the primary key must refuse it")
	}
	if _, err := database.Exec(`
		INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'shared_app', '456')`, org,
	); err == nil {
		t.Error("a marker in an unknown credential class landed; widening the CHECK must not have opened it")
	}
}
