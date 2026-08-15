package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that gives repo_profiles its identity columns rebuilds the
// table, because SQLite cannot ALTER a table-level UNIQUE in place. A rebuild
// is a copy, and the two ways a copy goes wrong are both silent: a row that
// does not come across, and a row that comes across twice because the widened
// key admits what the old one refused. Neither announces itself — the product
// keeps running against a table that quietly lost a repository's cached
// profile, or that now has two rows racing to be the one the poller writes.
//
// So this stages rows in the pre-migration shape, migrates, and asserts the
// count is unchanged and every column came with them.
func TestMigrate_RepoIdentityRebuildPreservesRows(t *testing.T) {
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
	// Stop one version short of the rebuild, so the rows below are staged
	// exactly as builds before it wrote them.
	upToErr := goose.UpTo(database, dir, 202608150005)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	seed := []string{
		// A fully profiled repo carrying every column a rebuild could drop:
		// AI profile, user-configured base branch, clone outcome, and the
		// poller's conditional-request state.
		`INSERT INTO repo_profiles
		    (id, owner, repo, description, has_readme, has_claude_md, has_agents_md,
		     profile_text, clone_url, default_branch, base_branch, profiled_at,
		     clone_status, clone_error, clone_error_kind, pulls_etag, pulls_polled_at)
		 VALUES
		    ('octo/widget', 'octo', 'widget', 'Widget service', 1, 1, 0,
		     'Service profile body', 'git@github.com:octo/widget.git', 'main', 'develop',
		     '2026-08-01 00:00:00', 'ok', NULL, NULL, '"etag-v1"', '2026-08-02 00:00:00')`,
		// A bare row: tracked, never profiled. Its NULLs must stay NULL rather
		// than picking up defaults from the new table definition.
		`INSERT INTO repo_profiles (id, owner, repo) VALUES ('octo/bare', 'octo', 'bare')`,
		// A repo whose clone failed, so the failure columns are non-NULL.
		`INSERT INTO repo_profiles (id, owner, repo, clone_status, clone_error, clone_error_kind)
		 VALUES ('octo/broken', 'octo', 'broken', 'failed', 'permission denied', 'ssh')`,
		// Two spellings of the same repository. The old UNIQUE(owner, repo) is
		// case-sensitive, so a build before this could hold both; the widened
		// key is a superset of it, so both must still be here afterwards. This
		// is the row pair that fails if the rebuild's key merged anything.
		`INSERT INTO repo_profiles (id, owner, repo) VALUES ('Acme/Api', 'Acme', 'Api')`,
		`INSERT INTO repo_profiles (id, owner, repo) VALUES ('acme/api', 'acme', 'api')`,
	}
	for _, stmt := range seed {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	var before int
	if err := database.QueryRow(`SELECT COUNT(*) FROM repo_profiles`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 5 {
		t.Fatalf("staged %d rows, want 5 — the fixture itself is wrong", before)
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var after int
	if err := database.QueryRow(`SELECT COUNT(*) FROM repo_profiles`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Errorf("repo_profiles rows = %d, want %d — the rebuild neither drops nor splits a row", after, before)
	}

	scalar := func(query string, args ...any) string {
		t.Helper()
		var got sql.NullString
		if err := database.QueryRow(query, args...).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return got.String
	}
	isNull := func(query string, args ...any) bool {
		t.Helper()
		var got sql.NullString
		if err := database.QueryRow(query, args...).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return !got.Valid
	}

	// Every pre-existing row is a GitHub repository whose id TF has not
	// learned. NULL is the honest answer, not a placeholder to be filled by
	// the migration — nothing here has a payload to read one from.
	var nonGitHub int
	if err := database.QueryRow(`SELECT COUNT(*) FROM repo_profiles WHERE source <> 'github'`).Scan(&nonGitHub); err != nil {
		t.Fatalf("count non-github: %v", err)
	}
	if nonGitHub != 0 {
		t.Errorf("rows with source <> 'github' = %d, want 0", nonGitHub)
	}
	var withExternalID int
	if err := database.QueryRow(`SELECT COUNT(*) FROM repo_profiles WHERE external_id IS NOT NULL`).Scan(&withExternalID); err != nil {
		t.Fatalf("count external ids: %v", err)
	}
	if withExternalID != 0 {
		t.Errorf("rows with a non-NULL external_id = %d, want 0 — the migration invents no ids", withExternalID)
	}

	// The profiled row came across whole. A rebuild that silently dropped a
	// column would show up here as an empty profile or a lost base branch —
	// the repo would re-profile and lose the user's branch setting.
	if got := scalar(`SELECT profile_text FROM repo_profiles WHERE id = 'octo/widget'`); got != "Service profile body" {
		t.Errorf("profile_text = %q, want the seeded body", got)
	}
	if got := scalar(`SELECT base_branch FROM repo_profiles WHERE id = 'octo/widget'`); got != "develop" {
		t.Errorf("base_branch = %q, want develop — a user setting a rebuild must not drop", got)
	}
	if got := scalar(`SELECT clone_url FROM repo_profiles WHERE id = 'octo/widget'`); got != "git@github.com:octo/widget.git" {
		t.Errorf("clone_url = %q, want the seeded URL", got)
	}
	if got := scalar(`SELECT default_branch FROM repo_profiles WHERE id = 'octo/widget'`); got != "main" {
		t.Errorf("default_branch = %q, want main", got)
	}
	if got := scalar(`SELECT profiled_at FROM repo_profiles WHERE id = 'octo/widget'`); got == "" {
		t.Error("profiled_at is NULL — the repo would re-profile immediately, ignoring the TTL")
	}
	if got := scalar(`SELECT has_readme || '/' || has_claude_md || '/' || has_agents_md FROM repo_profiles WHERE id = 'octo/widget'`); got != "1/1/0" {
		t.Errorf("doc flags = %q, want 1/1/0", got)
	}
	if got := scalar(`SELECT pulls_etag FROM repo_profiles WHERE id = 'octo/widget'`); got != `"etag-v1"` {
		t.Errorf("pulls_etag = %q, want the seeded ETag — losing it re-lists every open PR", got)
	}
	if got := scalar(`SELECT pulls_polled_at FROM repo_profiles WHERE id = 'octo/widget'`); got == "" {
		t.Error("pulls_polled_at is NULL — the conditional-request state is half-copied")
	}
	if got := scalar(`SELECT clone_status FROM repo_profiles WHERE id = 'octo/broken'`); got != "failed" {
		t.Errorf("clone_status = %q, want failed", got)
	}
	if got := scalar(`SELECT clone_error_kind FROM repo_profiles WHERE id = 'octo/broken'`); got != "ssh" {
		t.Errorf("clone_error_kind = %q, want ssh", got)
	}

	// The bare row stays bare: NULLs copied as NULLs, and clone_status still
	// on its table default rather than something the copy invented.
	if !isNull(`SELECT profile_text FROM repo_profiles WHERE id = 'octo/bare'`) {
		t.Error("profile_text on the never-profiled row is non-NULL after the rebuild")
	}
	if !isNull(`SELECT profiled_at FROM repo_profiles WHERE id = 'octo/bare'`) {
		t.Error("profiled_at on the never-profiled row is non-NULL after the rebuild")
	}
	if got := scalar(`SELECT clone_status FROM repo_profiles WHERE id = 'octo/bare'`); got != "pending" {
		t.Errorf("clone_status on the bare row = %q, want pending", got)
	}

	// Both case spellings survive, individually addressable.
	for _, id := range []string{"Acme/Api", "acme/api"} {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM repo_profiles WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if n != 1 {
			t.Errorf("rows for %q = %d, want 1 — the widened key must not merge case variants", id, n)
		}
	}

	// The widened key is live: a second row for the same (org, source, owner,
	// repo) is refused. Without this the assertions above would also pass on a
	// migration that dropped the constraint entirely.
	if _, err := database.Exec(
		`INSERT INTO repo_profiles (id, owner, repo) VALUES ('octo/widget-2', 'octo', 'widget')`,
	); err == nil {
		t.Error("a duplicate (org_id, source, owner, repo) was accepted; want the unique to refuse it")
	}
	// And it is genuinely wider than the old one: the same slug under another
	// source is a different repository, which the old UNIQUE(owner, repo)
	// would have refused. (The slug PK still forces a distinct id here — see
	// the migration's note on why that stays local mode's business.)
	if _, err := database.Exec(
		`INSERT INTO repo_profiles (id, owner, repo, source) VALUES ('gitlab:octo/widget', 'octo', 'widget', 'gitlab')`,
	); err != nil {
		t.Errorf("same slug under a different source was refused: %v", err)
	}

	// The slug lookup index the poller reads through is recreated with the
	// table, not lost with the DROP.
	var idx int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_repo_profiles_owner_repo'`,
	).Scan(&idx); err != nil {
		t.Fatalf("probe index: %v", err)
	}
	if idx != 1 {
		t.Error("idx_repo_profiles_owner_repo is missing after the rebuild")
	}

	// Nothing is left behind for the app to trip over.
	var staging int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'repo_profiles_new'`,
	).Scan(&staging); err != nil {
		t.Fatalf("probe staging table: %v", err)
	}
	if staging != 0 {
		t.Error("repo_profiles_new survived the rebuild; it is renamed, not copied")
	}
}
