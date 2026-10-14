package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that adds node_id to artifacts (202610130001). Among the rows
// written before it, a GitHub comment row with no target was left by an edit or
// a delete of a comment TF had not posted, and is deleted, so a comment row
// existing means TF posted the comment. The rows the posting verbs wrote stay,
// and so does every targetless row of another provider or kind.
func TestMigrate_ArtifactCommentIdentityKeepsOnlyCommentsTFPosted(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, 202610110001)
	seedGitHubHostOrg(t, database)

	rows := []struct {
		id, provider, kind, target, state string
		kept                              bool
	}{
		{"gh-posted", "github", "comment", "octo/api#3", "posted", true},
		{"gh-posted-then-deleted", "github", "comment", "octo/api#3", "deleted", true},
		{"gh-edited", "github", "comment", "", "posted", false},
		{"gh-deleted", "github", "comment", "", "deleted", false},
		{"jira-posted", "jira", "comment", "ENG-1", "posted", true},
		{"linear-posted", "linear", "comment", "ENG-2", "posted", true},
		{"pr", "github", "pull_request", "octo/api#3", "open", true},
		{"jira-targetless", "jira", "comment", "", "posted", true},
		{"gh-targetless-branch", "github", "branch", "", "pushed", true},
	}
	for _, r := range rows {
		if _, err := database.Exec(`
			INSERT INTO artifacts (id, team_id, provider, scope, kind, target, state, dedup_key)
			VALUES (?, 'team-1', ?, 'https://github.com', ?, ?, ?, ?)
		`, r.id, r.provider, r.kind, r.target, r.state, "key-"+r.id); err != nil {
			t.Fatalf("seed artifact %s: %v", r.id, err)
		}
	}

	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202610130001)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo artifact comment identity: %v", upErr)
	}

	for _, r := range rows {
		var nodeID *string
		err := database.QueryRow(`SELECT node_id FROM artifacts WHERE id = ?`, r.id).Scan(&nodeID)
		switch {
		case !r.kept && err == nil:
			t.Errorf("%s survived the migration, want it deleted", r.id)
		case r.kept && err != nil:
			t.Errorf("read %s: %v, want it kept", r.id, err)
		case r.kept && nodeID != nil:
			t.Errorf("%s node_id = %q, want NULL", r.id, *nodeID)
		}
	}
}
