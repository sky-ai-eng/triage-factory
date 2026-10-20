package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// entityRepositoryVersion is 202610200001_entity_repository.sql.
const entityRepositoryVersion int64 = 202610200001

// The migration gives every GitHub entity the registry row of its repository,
// minting the rows that do not exist yet, and then refuses a GitHub row
// without one. Its failure modes are quiet: a key resolved to the wrong host's
// row, two rows minted for one repository spelled two ways, or a row left NULL
// that the update trigger then makes unwritable for the poller. So this stages
// each case at the version before, migrates, and reads every row back.
func TestMigrate_EntityRepositoryBackfillsAndGuards(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, providerScopesVersion)
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}

	const (
		github = "https://github.com"
		ghe    = "https://ghe.example.com"
	)
	for _, stmt := range []string{
		// A repository a team tracks, already in the registry.
		`INSERT INTO repositories (id, owner, repo, source, host) VALUES ('repo-tracked', 'octo', 'tracked', 'github', '` + github + `')`,

		// A pull request in it, its key spelled in another casing: it takes
		// the existing row rather than minting a second.
		`INSERT INTO entities (id, source, scope, source_id, kind, created_at)
		 VALUES ('e-tracked', 'github', '` + github + `', 'Octo/Tracked#1', 'pr', '2026-01-01 00:00:00')`,

		// Two pull requests in a repository with no row, spelled two ways:
		// one row is minted, carrying the newer key's casing.
		`INSERT INTO entities (id, source, scope, source_id, kind, created_at)
		 VALUES ('e-new-old', 'github', '` + github + `', 'acme/widgets#2', 'pr', '2026-01-01 00:00:00')`,
		`INSERT INTO entities (id, source, scope, source_id, kind, created_at, state, closed_at)
		 VALUES ('e-new-new', 'github', '` + github + `', 'Acme/Widgets#3', 'pr', '2026-02-01 00:00:00', 'closed', '2026-02-02 00:00:00')`,

		// The tracked repository's name on another host is another
		// repository: it gets a row of its own there.
		`INSERT INTO entities (id, source, scope, source_id, kind)
		 VALUES ('e-ghe', 'github', '` + ghe + `', 'octo/tracked#4', 'pr')`,

		// Other sources reference no repository.
		`INSERT INTO entities (id, source, scope, source_id, external_id, kind)
		 VALUES ('e-jira', 'jira', 'https://jira.example.com', 'OCTO-1', '10001', 'issue')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.UpTo(database, dir, entityRepositoryVersion)
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo %d: %v", entityRepositoryVersion, upErr)
	}

	repositoryOf := func(entityID string) string {
		t.Helper()
		var id sql.NullString
		if err := database.QueryRow(`SELECT repository_id FROM entities WHERE id = ?`, entityID).Scan(&id); err != nil {
			t.Fatalf("read repository_id of %s: %v", entityID, err)
		}
		return id.String
	}
	type registryRow struct{ id, owner, repo, host string }
	rowFor := func(owner, repo, host string) registryRow {
		t.Helper()
		var r registryRow
		err := database.QueryRow(`
			SELECT id, owner, repo, host FROM repositories
			 WHERE LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?) AND host = ?
		`, owner, repo, host).Scan(&r.id, &r.owner, &r.repo, &r.host)
		if err != nil {
			t.Fatalf("registry row for %s/%s on %s: %v", owner, repo, host, err)
		}
		return r
	}

	if got := repositoryOf("e-tracked"); got != "repo-tracked" {
		t.Errorf("e-tracked references %q, want the existing row repo-tracked", got)
	}

	widgets := rowFor("acme", "widgets", github)
	if widgets.owner != "Acme" || widgets.repo != "Widgets" {
		t.Errorf("minted row is %s/%s, want the newer key's casing Acme/Widgets", widgets.owner, widgets.repo)
	}
	for _, e := range []string{"e-new-old", "e-new-new"} {
		if got := repositoryOf(e); got != widgets.id {
			t.Errorf("%s references %q, want the one minted row %s", e, got, widgets.id)
		}
	}

	gheRow := rowFor("octo", "tracked", ghe)
	if gheRow.id == "repo-tracked" {
		t.Error("the other host's pull request took github.com's row; a name on another host is another repository")
	}
	if got := repositoryOf("e-ghe"); got != gheRow.id {
		t.Errorf("e-ghe references %q, want its host's row %s", got, gheRow.id)
	}

	if got := repositoryOf("e-jira"); got != "" {
		t.Errorf("e-jira references %q, want no repository", got)
	}

	var registryRows, tracking int
	if err := database.QueryRow(`SELECT COUNT(*) FROM repositories`).Scan(&registryRows); err != nil {
		t.Fatal(err)
	}
	if registryRows != 3 {
		t.Errorf("registry holds %d rows, want 3: the tracked row and one minted per repository on each host", registryRows)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM team_github_repos`).Scan(&tracking); err != nil {
		t.Fatal(err)
	}
	if tracking != 0 {
		t.Errorf("team_github_repos holds %d rows, want 0: minting a registry row tracks nothing", tracking)
	}

	refused := func(what, stmt string, args ...any) {
		t.Helper()
		_, err := database.Exec(stmt, args...)
		if err == nil || !strings.Contains(err.Error(), "entities_repository_required") {
			t.Errorf("%s = %v, want the entities_repository_required trigger to refuse it", what, err)
		}
	}
	refused("a github insert with no repository",
		`INSERT INTO entities (id, source, scope, source_id, kind) VALUES ('e-bare', 'github', ?, 'octo/tracked#5', 'pr')`, github)
	refused("clearing a github row's repository",
		`UPDATE entities SET repository_id = NULL WHERE id = 'e-tracked'`)
	refused("turning a row into a github row with no repository",
		`UPDATE entities SET source = 'github' WHERE id = 'e-jira'`)

	if _, err := database.Exec(`INSERT INTO entities (id, source, scope, source_id, kind, repository_id)
		VALUES ('e-ok', 'github', ?, 'octo/tracked#6', 'pr', 'repo-tracked')`, github); err != nil {
		t.Errorf("a github insert naming its repository: %v", err)
	}
	if _, err := database.Exec(`UPDATE entities SET snapshot_json = '{}' WHERE id = 'e-tracked'`); err != nil {
		t.Errorf("an update leaving the repository in place: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO entities (id, source, scope, source_id, external_id, kind)
		VALUES ('e-jira-2', 'jira', 'https://jira.example.com', 'OCTO-2', '10002', 'issue')`); err != nil {
		t.Errorf("a jira insert with no repository: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO entities (id, source, scope, source_id, kind, repository_id)
		VALUES ('e-ghost', 'github', ?, 'ghost/repo#1', 'pr', 'no-such-repository')`, github); err == nil {
		t.Error("a github insert naming a repository row that does not exist succeeded; the foreign key should refuse it")
	}
}
