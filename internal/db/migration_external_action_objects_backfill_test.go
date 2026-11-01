package db

import (
	"database/sql"
	"testing"
)

// The version just before the migration that names the object on a pull
// request's and a review's ledger rows recorded before rows named one.
const externalActionObjectsBackfillPrior = 202610310009

// TestMigrate_ExternalActionObjectsBackfill pins the backfill's two rules: a
// pull request action with an entity names the pull request, and a review
// submit or dismissal with a positive integer id names the review. A row with
// neither, an action outside both rules, and a row that already names an
// object are left as they are.
func TestMigrate_ExternalActionObjectsBackfill(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, externalActionObjectsBackfillPrior)

	const (
		orgID  = "00000000-0000-0000-0000-000000000001"
		teamID = "00000000-0000-0000-0000-000000000010"
		userID = "00000000-0000-0000-0000-000000000100"
	)
	row := func(id, action, entityID, externalID, objectKind, objectKey string) string {
		nullable := func(v string) string {
			if v == "" {
				return "NULL"
			}
			return "'" + v + "'"
		}
		return `INSERT INTO external_actions (id, org_id, provider, scope, action, target, credential, dedup_key,
				conversation_id, entity_id, external_id, object_kind, object_key)
			VALUES ('` + id + `', '` + orgID + `', 'github', 'https://github.com', '` + action + `', 'octo/api#1', 'github_app', '` + id + `',
				'c-1', ` + nullable(entityID) + `, ` + nullable(externalID) + `, ` + nullable(objectKind) + `, ` + nullable(objectKey) + `)`
	}
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('` + teamID + `', '` + orgID + `', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('` + userID + `', 'Local')`,
		`INSERT INTO conversations (id, org_id, team_id, origin, status) VALUES ('c-1', '` + orgID + `', '` + teamID + `', 'interactive', 'completed')`,
		`INSERT INTO repositories (id, org_id, owner, repo, source) VALUES ('r1', '` + orgID + `', 'octo', 'api', 'github')`,
		`INSERT INTO entities (id, org_id, source, scope, source_id, kind, repository_id)
			VALUES ('e1', '` + orgID + `', 'github', 'https://github.com', 'octo/api#1', 'pr', 'r1')`,
		row("created", "pr_created", "e1", "", "", ""),
		row("merged", "pr_merged", "e1", "", "", ""),
		row("unlinked", "pr_created", "", "", "", ""),
		row("label", "label_added", "e1", "", "", ""),
		row("submitted", "review_submitted", "e1", "4242", "", ""),
		row("dismissed", "review_dismissed", "", "77", "", ""),
		row("no-id", "review_submitted", "e1", "0", "", ""),
		row("named", "review_submitted", "e1", "4242", "review", "github:review:9"),
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	migrateThrough(t, database, 202610310010)

	type object struct{ kind, key string }
	want := map[string]object{
		"created":   {"pull_request", ""},
		"merged":    {"pull_request", ""},
		"unlinked":  {},
		"label":     {},
		"submitted": {"review", "github:review:4242"},
		"dismissed": {"review", "github:review:77"},
		"no-id":     {},
		"named":     {"review", "github:review:9"},
	}
	for id, w := range want {
		var kind, key sql.NullString
		if err := database.QueryRow(`SELECT object_kind, object_key FROM external_actions WHERE id = ?`, id).Scan(&kind, &key); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got := (object{kind.String, key.String}); got != w {
			t.Errorf("%s names %+v after the migration, want %+v", id, got, w)
		}
	}
}
