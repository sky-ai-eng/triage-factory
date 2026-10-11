package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration that keys installations by (org_id, github_host,
// installation_id) and puts the host on every reach row and scope marker
// (202610100005). The fixture is written under the old key: one installation
// per id in the org, App reach rows and markers that name no host, and an App
// marker whose installation no longer exists. Afterwards each row carries its
// installation's host, the orphan marker is gone, the same id on a second
// host is admitted, and the foreign key holds a reach row to the installation
// on its own host.
func TestMigrate_InstallationHostPrimaryKey(t *testing.T) {
	const (
		org  = runmode.LocalDefaultOrgID
		ghes = "https://ghe.example.com"
		dot  = "https://github.com"
	)
	database := openMigrationsTestDBAt(t, TestDSNMemory, 202610100004)
	seedGitHubHostOrg(t, database)
	exec := func(t *testing.T, query string, args ...any) {
		t.Helper()
		if _, err := database.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(t, `INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login, github_host, repository_selection)
	         VALUES ('3', ?, 'Organization', 'acme', ?, 'selected')`, org, ghes)
	exec(t, `INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo)
	         VALUES (?, 'byo_app', '3', 'acme', 'api')`, org)
	exec(t, `INSERT INTO reachable_repositories (org_id, credential_class, host, owner, repo)
	         VALUES (?, 'pat', ?, 'acme', 'personal')`, org, dot)
	exec(t, `INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'byo_app', '3')`, org)
	exec(t, `INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'byo_app', '9')`, org)
	exec(t, `INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'pat', ?)`, org, dot)

	migrateInstallationHostPK(t, database)

	var selection string
	if err := database.QueryRow(`SELECT repository_selection FROM org_github_app_installations
	                              WHERE org_id = ? AND github_host = ? AND installation_id = '3'`, org, ghes).Scan(&selection); err != nil {
		t.Fatalf("read the copied installation: %v", err)
	}
	if selection != "selected" {
		t.Errorf("repository_selection = %q after the copy; want selected", selection)
	}

	hosts := func(t *testing.T, query string) map[string]string {
		t.Helper()
		rows, err := database.Query(query, org)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var key, host string
			if err := rows.Scan(&key, &host); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[key] = host
		}
		return out
	}
	reach := hosts(t, `SELECT credential_class || ':' || repo, host FROM reachable_repositories WHERE org_id = ?`)
	if len(reach) != 2 || reach["byo_app:api"] != ghes || reach["pat:personal"] != dot {
		t.Errorf("reach rows = %v; want the App row on its installation's %s and the PAT row on %s", reach, ghes, dot)
	}
	scopes := hosts(t, `SELECT credential_class || ':' || scope, host FROM reachable_scopes WHERE org_id = ?`)
	if len(scopes) != 2 || scopes["byo_app:3"] != ghes || scopes["pat:"+dot] != dot {
		t.Errorf("scope markers = %v; want 3 on %s and the PAT marker on %s, and none for the missing installation 9", scopes, ghes, dot)
	}

	// The same id on another host is another installation, with reach of its
	// own on that host.
	exec(t, `INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login, github_host)
	         VALUES ('3', ?, 'Organization', 'globex', ?)`, org, dot)
	exec(t, `INSERT INTO reachable_repositories (org_id, credential_class, installation_id, host, owner, repo)
	         VALUES (?, 'byo_app', '3', ?, 'globex', 'web')`, org, dot)
	if _, err := database.Exec(`INSERT INTO reachable_repositories (org_id, credential_class, installation_id, host, owner, repo)
	                            VALUES (?, 'byo_app', '3', 'https://elsewhere.example.com', 'acme', 'ghost')`, org); err == nil {
		t.Error("an App reach row landed on a host where its installation does not exist; the foreign key must refuse it")
	}
	if _, err := database.Exec(`INSERT INTO org_github_app_installations (installation_id, org_id, account_type, account_login)
	                            VALUES ('4', ?, 'Organization', 'initech')`, org); err == nil {
		t.Error("an installation with no host landed; the column has no default to stand in for one")
	}

	// Deleting one host's installation takes its reach and nothing else.
	exec(t, `DELETE FROM org_github_app_installations WHERE org_id = ? AND github_host = ? AND installation_id = '3'`, org, ghes)
	reach = hosts(t, `SELECT credential_class || ':' || repo, host FROM reachable_repositories WHERE org_id = ?`)
	if len(reach) != 2 || reach["byo_app:web"] != dot || reach["pat:personal"] != dot {
		t.Errorf("reach rows after deleting the GHES installation = %v; want the github.com App row and the PAT row", reach)
	}
}

func migrateInstallationHostPK(t *testing.T, database *sql.DB) {
	t.Helper()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202610100005)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo installation host primary key: %v", upErr)
	}
}
