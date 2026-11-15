package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration under test, and the migration before it, whose schema the
// test seeds.
const (
	renameIdleParkReason       int64 = 202611150006
	beforeRenameIdleParkReason int64 = 202611150005
)

// TestMigrate_RenameIdleParkReason: every stored `idle` park reads
// `turn_incomplete`, and no other reason, or a row with none, changes.
func TestMigrate_RenameIdleParkReason(t *testing.T) {
	database := openMigrationsTestDBAt(t, beforeRenameIdleParkReason)
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	execAll(t, database,
		`INSERT INTO orgs (id, slug, name) VALUES ('`+org+`', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+team+`', '`+org+`', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('`+runmode.LocalDefaultUserID+`', 'Local')`,
	)

	rows := []struct{ id, reason, want string }{
		{id: "idle", reason: "idle", want: "turn_incomplete"},
		{id: "stopped", reason: "user_cancelled", want: "user_cancelled"},
		{id: "stalled", reason: "stalled", want: "stalled"},
		{id: "no-reason", reason: "", want: ""},
	}
	for _, r := range rows {
		if _, err := database.Exec(`
			INSERT INTO conversations (id, org_id, team_id, origin, status, park_reason)
			VALUES (?, ?, ?, 'interactive', 'open', ?)`,
			r.id, org, team, nullable(r.reason)); err != nil {
			t.Fatalf("seed conversation %s: %v", r.id, err)
		}
	}

	migrateThrough(t, database, renameIdleParkReason)

	for _, r := range rows {
		var got sql.NullString
		if err := database.QueryRow(`SELECT park_reason FROM conversations WHERE id = ?`, r.id).Scan(&got); err != nil {
			t.Fatalf("read conversation %s: %v", r.id, err)
		}
		if got.String != r.want || got.Valid != (r.want != "") {
			t.Errorf("conversation %s: park_reason = %v, want %q", r.id, got, r.want)
		}
	}
}
