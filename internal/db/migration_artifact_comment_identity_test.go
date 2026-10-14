package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that adds node_id and posted_by_tf to artifacts
// (202610130001). Among the rows written before it, posted_by_tf starts on for
// the comment rows a posting verb wrote: every Jira and Linear comment, and a
// GitHub comment the add verb stamped a target on. A GitHub comment with no
// target was minted by an edit or a delete of someone else's comment, and
// every other kind is no comment at all; both stay off.
func TestMigrate_ArtifactCommentIdentityBackfillsPostedByTF(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, 202610110001)
	seedGitHubHostOrg(t, database)

	rows := []struct {
		id, provider, kind, target string
		want                       bool
	}{
		{"gh-posted", "github", "comment", "octo/api#3", true},
		{"gh-edited", "github", "comment", "", false},
		{"jira-posted", "jira", "comment", "ENG-1", true},
		{"linear-posted", "linear", "comment", "ENG-2", true},
		{"pr", "github", "pull_request", "octo/api#3", false},
	}
	for _, r := range rows {
		if _, err := database.Exec(`
			INSERT INTO artifacts (id, team_id, provider, scope, kind, target, state, dedup_key)
			VALUES (?, 'team-1', ?, 'https://github.com', ?, ?, 'posted', ?)
		`, r.id, r.provider, r.kind, r.target, "key-"+r.id); err != nil {
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
		var posted bool
		var nodeID *string
		if err := database.QueryRow(`SELECT posted_by_tf, node_id FROM artifacts WHERE id = ?`, r.id).Scan(&posted, &nodeID); err != nil {
			t.Fatalf("read artifact %s: %v", r.id, err)
		}
		if posted != r.want {
			t.Errorf("%s posted_by_tf = %v, want %v", r.id, posted, r.want)
		}
		if nodeID != nil {
			t.Errorf("%s node_id = %q, want NULL", r.id, *nodeID)
		}
	}
}
