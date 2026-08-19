package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The default-checkout slug rename rewrites conversation_worktrees.ref in
// place, and ref is the value `workspace add` looks a reservation up by — so
// the two silent failure shapes are a "@default" row the rewrite missed (the
// idempotent re-add re-clones a repo the conversation already holds) and a row
// it touched that it shouldn't have (a PR or branch-slug reservation, or the
// path column, which must keep naming the directory that exists on disk).
//
// This stages the pre-migration rows, migrates, and asserts exactly the
// no-selector ref moved — and nothing else did.
func TestMigrate_DefaultCheckoutSlugRename(t *testing.T) {
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
	// Stop one version short of the rename, so the rows below are staged
	// exactly as builds before it wrote them.
	upToErr := goose.UpTo(database, dir, 202608180001)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	const (
		orgID  = "00000000-0000-0000-0000-000000000001"
		teamID = "00000000-0000-0000-0000-000000000010"
		convID = "00000000-0000-0000-0000-000000000020"
		userID = "00000000-0000-0000-0000-000000000100"
		repoID = "00000000-0000-0000-0000-0000000000aa"
	)
	seed := []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('` + teamID + `', '` + orgID + `', 'default', 'Default')`,
		// conversations.creator_user_id defaults to this sentinel and carries an
		// FK, so the row it names has to exist before the insert below.
		`INSERT INTO users (id, display_name) VALUES ('` + userID + `', 'Local')`,
		`INSERT INTO conversations (id, org_id, team_id, origin, status)
		 VALUES ('` + convID + `', '` + orgID + `', '` + teamID + `', 'interactive', 'running')`,
		`INSERT INTO repositories (id, owner, repo) VALUES ('` + repoID + `', 'octo', 'api')`,

		// The no-selector reservation an older build wrote: ref moves, path
		// stays pointed at the on-disk directory that still carries the old name.
		`INSERT INTO conversation_worktrees (conversation_id, repository_id, path, ref)
		 VALUES ('` + convID + `', '` + repoID + `', '/tmp/wt/octo/api/@default', '@default')`,
		// A PR reservation and a branch reservation — including a branch
		// literally named "default", whose slug carries the ref- prefix — must
		// come through untouched.
		`INSERT INTO conversation_worktrees (conversation_id, repository_id, path, ref)
		 VALUES ('` + convID + `', '` + repoID + `', '/tmp/wt/octo/api/pr-7', 'pr-7')`,
		`INSERT INTO conversation_worktrees (conversation_id, repository_id, path, ref)
		 VALUES ('` + convID + `', '` + repoID + `', '/tmp/wt/octo/api/ref-default', 'ref-default')`,
	}
	for _, stmt := range seed {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.Up(database, dir)
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.Up: %v", upErr)
	}

	scalar := func(query string, args ...any) string {
		t.Helper()
		var got sql.NullString
		if err := database.QueryRow(query, args...).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return got.String
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := database.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}

	if n := count(`SELECT COUNT(*) FROM conversation_worktrees WHERE ref = '@default'`); n != 0 {
		t.Errorf("rows still on the old '@default' ref = %d, want 0", n)
	}
	// The renamed row is addressable by the new ref and kept its on-disk path.
	if got := scalar(`SELECT path FROM conversation_worktrees WHERE ref = 'default'`); got != "/tmp/wt/octo/api/@default" {
		t.Errorf("renamed row's path = %q, want the on-disk /tmp/wt/octo/api/@default untouched", got)
	}
	// The PR and branch reservations were not the rename's to touch.
	for _, ref := range []string{"pr-7", "ref-default"} {
		if n := count(`SELECT COUNT(*) FROM conversation_worktrees WHERE ref = ?`, ref); n != 1 {
			t.Errorf("rows with ref %q = %d, want 1 untouched", ref, n)
		}
	}
}
