package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that converts every repository reference to the registry row's
// id rebuilds four tables and rewrites the references between them. Each of the
// three conversions can go wrong silently and in the same direction — a row
// that does not come across — and the symptom is not an error but a tracked set
// that quietly shrank, a worktree ledger with a hole in it, or a project whose
// pins vanished.
//
// So this stages the pre-migration shape, migrates, and asserts that every
// reference survived and now resolves to the same repository through its id.
func TestMigrate_RepoRowIDsPreservesReferences(t *testing.T) {
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
	// Stop one version short of the conversion, so the rows below are staged
	// exactly as builds before it wrote them.
	upToErr := goose.UpTo(database, dir, 202608160001)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	const (
		orgID  = "00000000-0000-0000-0000-000000000001"
		teamID = "00000000-0000-0000-0000-000000000010"
		convID = "00000000-0000-0000-0000-000000000020"
		projID = "00000000-0000-0000-0000-000000000030"
		userID = "00000000-0000-0000-0000-000000000100"
	)
	seed := []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('` + teamID + `', '` + orgID + `', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('` + userID + `', 'Local')`,
		`INSERT INTO conversations (id, org_id, team_id, origin, status)
		 VALUES ('` + convID + `', '` + orgID + `', '` + teamID + `', 'interactive', 'running')`,

		// A tracked repository with a profile the conversion must not drop.
		`INSERT INTO repositories (id, owner, repo, profile_text, base_branch)
		 VALUES ('octo/tracked', 'octo', 'tracked', 'profile body', 'develop')`,
		`INSERT INTO team_github_repos (team_id, owner, repo) VALUES ('` + teamID + `', 'octo', 'tracked')`,

		// The same repository tracked a second time under different casing.
		// The folded identity index says those are one repository, so the two
		// tracking rows have to collapse onto one rather than raising.
		`INSERT INTO team_github_repos (team_id, owner, repo) VALUES ('` + teamID + `', 'Octo', 'Tracked')`,

		// A repository nothing tracks anymore but a run's worktree ledger and a
		// project's pins still name — the state the old GC produced, and the
		// one where the conversion has to mint the registry row rather than
		// drop the references.
		`INSERT INTO conversation_worktrees (conversation_id, repo_id, path, ref)
		 VALUES ('` + convID + `', 'octo/untracked', '/tmp/wt/octo/untracked/pr-7', 'pr-7')`,
		`INSERT INTO projects (id, org_id, team_id, name, pinned_repos, creator_user_id)
		 VALUES ('` + projID + `', '` + orgID + `', '` + teamID + `', 'Pins',
		         '["octo/untracked","octo/tracked"]', '` + userID + `')`,

		// A ledger row whose repo_id was never a slug. It names no repository,
		// so it cannot be converted and is dropped.
		`INSERT INTO conversation_worktrees (conversation_id, repo_id, path, ref)
		 VALUES ('` + convID + `', 'not-a-slug', '/tmp/wt/junk', '@default')`,
	}
	for _, stmt := range seed {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// Stop exactly at the conversion under test — not goose.Up to head — so
	// this test stays pinned to what 202608160002 itself did and is immune to
	// later migrations dropping tables it reads (e.g. projects/project_pinned_repos,
	// removed by 202608260005).
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.UpTo(database, dir, 202608160002)
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo conversion version: %v", upErr)
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

	// The tracked repository kept its profile and its user-set branch — the
	// rebuild copied the row rather than re-creating it bare.
	if got := scalar(`SELECT profile_text FROM repositories WHERE owner = 'octo' AND repo = 'tracked'`); got != "profile body" {
		t.Errorf("profile_text = %q, want the seeded body", got)
	}
	if got := scalar(`SELECT base_branch FROM repositories WHERE owner = 'octo' AND repo = 'tracked'`); got != "develop" {
		t.Errorf("base_branch = %q, want develop — a user setting a rebuild must not drop", got)
	}
	// The untracked-but-referenced repository got a row so its references have
	// something to point at.
	if n := count(`SELECT COUNT(*) FROM repositories WHERE owner = 'octo' AND repo = 'untracked'`); n != 1 {
		t.Errorf("rows for the untracked-but-referenced repo = %d, want 1", n)
	}
	// Every id is a surrogate, not a slug.
	if n := count(`SELECT COUNT(*) FROM repositories WHERE id LIKE '%/%'`); n != 0 {
		t.Errorf("%d repository ids still look like slugs; the id is a surrogate now", n)
	}

	// The two case-differing tracking rows collapsed onto the one repository.
	if n := count(`SELECT COUNT(*) FROM team_github_repos`); n != 1 {
		t.Errorf("tracking rows = %d, want 1 — two casings of one repository are one tracking row", n)
	}
	if got := scalar(`
		SELECT r.owner || '/' || r.repo FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
	`); got != "octo/tracked" {
		t.Errorf("tracked repo = %q, want octo/tracked", got)
	}

	// The worktree ledger entry survived and resolves to the repository it
	// named, while the row that named no repository at all is gone.
	if n := count(`SELECT COUNT(*) FROM conversation_worktrees`); n != 1 {
		t.Errorf("worktree rows = %d, want 1 (the sluggish one kept, the malformed one dropped)", n)
	}
	if got := scalar(`
		SELECT r.owner || '/' || r.repo FROM conversation_worktrees w
		JOIN repositories r ON r.id = w.repository_id
	`); got != "octo/untracked" {
		t.Errorf("worktree repo = %q, want octo/untracked — a ledger entry outlives its tracking", got)
	}

	// The composite foreign keys refuse a tracking row that straddles two
	// tenants. Asserted here rather than through the store because the store
	// checks first and would mask the constraint — and the constraint is the
	// half that holds when a writer forgets.
	if _, err := database.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES ('00000000-0000-0000-0000-000000000002', 'other', 'Other')`,
	); err != nil {
		t.Fatalf("seed second org: %v", err)
	}
	var trackedRepoID string
	if err := database.QueryRow(
		`SELECT id FROM repositories WHERE owner = 'octo' AND repo = 'tracked'`,
	).Scan(&trackedRepoID); err != nil {
		t.Fatalf("read tracked repository id: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO team_github_repos (team_id, repository_id, org_id)
		VALUES (?, ?, '00000000-0000-0000-0000-000000000002')
	`, teamID, trackedRepoID); err == nil {
		t.Error("a tracking row naming one org's team and another org's repository was accepted")
	}

	// The pins came across in order.
	rows, err := database.Query(`
		SELECT r.owner || '/' || r.repo FROM project_pinned_repos pr
		JOIN repositories r ON r.id = pr.repository_id
		WHERE pr.project_id = ? ORDER BY pr.position
	`, projID)
	if err != nil {
		t.Fatalf("read pins: %v", err)
	}
	defer rows.Close()
	var pins []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			t.Fatalf("scan pin: %v", err)
		}
		pins = append(pins, slug)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read pins: %v", err)
	}
	if len(pins) != 2 || pins[0] != "octo/untracked" || pins[1] != "octo/tracked" {
		t.Errorf("pins = %v, want [octo/untracked octo/tracked] in the saved order", pins)
	}
}
