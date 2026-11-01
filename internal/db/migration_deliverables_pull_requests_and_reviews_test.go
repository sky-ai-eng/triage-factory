package db

import (
	"slices"
	"testing"
)

// The version just before the migration that narrows deliverables to pull
// requests and reviews, at which the test seeds one row of every kind.
const deliverablesPullRequestsAndReviewsPrior = 202610310007

// TestMigrate_DeliverablesPullRequestsAndReviews pins the narrowing: the
// branch, issue, comment and message deliverables are deleted, and the pull
// requests and reviews survive.
func TestMigrate_DeliverablesPullRequestsAndReviews(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, deliverablesPullRequestsAndReviewsPrior)
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	const (
		orgID  = "00000000-0000-0000-0000-000000000001"
		teamID = "00000000-0000-0000-0000-000000000010"
		userID = "00000000-0000-0000-0000-000000000100"
	)
	row := func(id, provider, kind, state string) string {
		return `INSERT INTO deliverables (id, conversation_id, org_id, team_id, provider, kind, scope, target, state, dedup_key)
			VALUES ('` + id + `', 'c-1', '` + orgID + `', '` + teamID + `', '` + provider + `', '` + kind + `', 'https://github.com', 'octo/api#1', '` + state + `', '` + id + `')`
	}
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('` + teamID + `', '` + orgID + `', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('` + userID + `', 'Local')`,
		`INSERT INTO conversations (id, org_id, team_id, origin, status) VALUES ('c-1', '` + orgID + `', '` + teamID + `', 'interactive', 'completed')`,
		row("pr", "github", "pull_request", "draft"),
		row("review", "github", "review", "pending"),
		row("branch", "git", "branch", "pushed"),
		row("issue", "jira", "issue", "created"),
		row("comment", "github", "comment", "posted"),
		row("message", "slack", "message", "posted"),
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	migrateThrough(t, database, 202610310008)

	rows, err := database.Query(`SELECT id FROM deliverables ORDER BY id`)
	if err != nil {
		t.Fatalf("read deliverables: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read deliverables: %v", err)
	}
	if want := []string{"pr", "review"}; !slices.Equal(got, want) {
		t.Errorf("deliverables after the migration = %v, want %v", got, want)
	}
}
