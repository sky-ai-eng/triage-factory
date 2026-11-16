package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration under test, and the one before it, whose schema the test
// seeds.
const (
	sdkSessionCost       int64 = 202611160002
	beforeSDKSessionCost int64 = 202611160001
)

// TestMigrate_SDKSessionCostBackfill: an un-ended conversation with a session
// starts from its newest cost row, whatever rows with no cost follow it, and
// an ended conversation, one with no cost row, and one with no session start
// from zero.
func TestMigrate_SDKSessionCostBackfill(t *testing.T) {
	database := openMigrationsTestDBAt(t, beforeSDKSessionCost)
	org, team, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID
	execAll(t, database,
		`INSERT INTO orgs (id, slug, name) VALUES ('`+org+`', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+team+`', '`+org+`', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('`+user+`', 'Local')`,
		`INSERT INTO conversations (id, org_id, team_id, type, origin, status, sdk_session_id)
			VALUES ('c-live', '`+org+`', '`+team+`', 'interactive', 'interactive', 'open', 's-live')`,
		`INSERT INTO conversations (id, org_id, team_id, type, origin, status, sdk_session_id, ended_at, ended_reason)
			VALUES ('c-ended', '`+org+`', '`+team+`', 'interactive', 'interactive', 'open', 's-ended', '2026-11-01 00:00:00', 'requeued')`,
		`INSERT INTO conversations (id, org_id, team_id, type, origin, status, sdk_session_id)
			VALUES ('c-no-cost', '`+org+`', '`+team+`', 'interactive', 'interactive', 'open', 's-no-cost')`,
		`INSERT INTO conversations (id, org_id, team_id, type, origin, status)
			VALUES ('c-no-session', '`+org+`', '`+team+`', 'interactive', 'interactive', 'open')`,
		// The live conversation's newest cost row is followed by a person's
		// next message, which carries none.
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-live', 'assistant', 0.00697188)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-live', 'user', NULL)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-live', 'assistant', 0.00825322)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-live', 'user', NULL)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-ended', 'assistant', 0.5)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-no-cost', 'assistant', NULL)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-no-session', 'assistant', 0.25)`,
	)

	migrateThrough(t, database, sdkSessionCost)

	for _, tc := range []struct {
		conversation string
		want         float64
	}{
		{"c-live", 0.00825322},
		{"c-ended", 0},
		{"c-no-cost", 0},
		{"c-no-session", 0},
	} {
		var got float64
		if err := database.QueryRow(`SELECT sdk_session_cost_usd FROM conversations WHERE id = ?`, tc.conversation).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.conversation, err)
		}
		if got != tc.want {
			t.Errorf("%s sdk_session_cost_usd = %v, want %v", tc.conversation, got, tc.want)
		}
	}
}
