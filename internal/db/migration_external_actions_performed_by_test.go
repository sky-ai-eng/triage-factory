package db

import (
	"testing"
)

// The version just before the migration that adds
// external_actions.performed_by, at which the test seeds ledger rows.
const externalActionsPerformedByPrior = 202610310001

// TestMigrate_ExternalActionsPerformedBy pins the backfill: a row with no
// conversation and no actor is TF's own (system), a row with no conversation
// and an actor is a person's (person), and every row with a conversation stays
// the agent's, with an actor or without one. The column's CHECK refuses any
// other value.
func TestMigrate_ExternalActionsPerformedBy(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, externalActionsPerformedByPrior)

	const (
		orgID  = "00000000-0000-0000-0000-000000000001"
		teamID = "00000000-0000-0000-0000-000000000010"
		userID = "00000000-0000-0000-0000-000000000100"
	)
	row := func(id, conversationID, actorUserID string) string {
		nullable := func(v string) string {
			if v == "" {
				return "NULL"
			}
			return "'" + v + "'"
		}
		return `INSERT INTO external_actions (id, provider, scope, action, target, credential, dedup_key, conversation_id, actor_user_id)
			VALUES ('` + id + `', 'github', 'https://github.com', 'pr_marked_ready', 'octo/api#1', 'github_app', '` + id + `', ` +
			nullable(conversationID) + `, ` + nullable(actorUserID) + `)`
	}
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('` + teamID + `', '` + orgID + `', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('` + userID + `', 'Local')`,
		`INSERT INTO conversations (id, org_id, team_id, origin, status) VALUES ('c-1', '` + orgID + `', '` + teamID + `', 'interactive', 'completed')`,
		row("mirror", "", ""),
		row("board-drag", "", userID),
		row("event-run", "c-1", ""),
		row("manual-run", "c-1", userID),
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	want := map[string]string{
		"mirror":     "system",
		"board-drag": "person",
		"event-run":  "agent",
		"manual-run": "agent",
	}
	rows, err := database.Query(`SELECT id, performed_by FROM external_actions`)
	if err != nil {
		t.Fatalf("read performed_by: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, performedBy string
		if err := rows.Scan(&id, &performedBy); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = performedBy
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read performed_by: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ledger holds %v after the migration, want %v", got, want)
	}
	for id, performedBy := range want {
		if got[id] != performedBy {
			t.Errorf("row %s performed_by = %q, want %q", id, got[id], performedBy)
		}
	}

	if _, err := database.Exec(`UPDATE external_actions SET performed_by = 'bot' WHERE id = 'mirror'`); err == nil {
		t.Error("performed_by accepted 'bot'; want the CHECK to refuse it")
	}
}
