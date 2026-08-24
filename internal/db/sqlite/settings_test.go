package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSettingsStores_SQLite runs the shared settings conformance suite
// against the SQLite impl (Orgs/Teams/Users/JiraStatusRules together).
// Local mode resolves every method to the runmode.LocalDefault* sentinel
// rows seeded by the baseline migration; each subtest opens a
// fresh in-memory DB so row state doesn't leak across cases.
func TestSettingsStores_SQLite(t *testing.T) {
	dbtest.RunSettingsStoresConformance(t, func(t *testing.T) (dbtest.SettingsStores, dbtest.SettingsIDs) {
		t.Helper()
		conn := openSQLiteForTest(t)
		// Conformance subtests assert behavior against an *unseeded*
		// settings row — the baseline migration seeds rows
		// for the local sentinel org/team to keep production paths
		// honest, but those defaults collide with the round-trip
		// + "empty row returns store defaults" assertions. Drop the
		// seeded rows here so the conformance suite sees the same
		// pristine state both backends present to it.
		if _, err := conn.Exec(`DELETE FROM org_settings`); err != nil {
			t.Fatalf("drop seeded org_settings: %v", err)
		}
		if _, err := conn.Exec(`DELETE FROM team_settings`); err != nil {
			t.Fatalf("drop seeded team_settings: %v", err)
		}
		stores := sqlitestore.New(conn)
		return dbtest.SettingsStores{
				MultiMode:        false,
				Orgs:             stores.Orgs,
				Teams:            stores.Teams,
				Users:            stores.Users,
				JiraStatusRules:  stores.JiraStatusRules,
				TeamGitHubGroups: stores.TeamGitHubGroups,
				TeamGitHubRepos:  stores.TeamGitHubRepos,
				OrgEventSources:  stores.OrgEventSources,
			}, dbtest.SettingsIDs{
				OrgID:  runmode.LocalDefaultOrgID,
				TeamID: runmode.LocalDefaultTeamID,
				UserID: runmode.LocalDefaultUserID,
			}
	})
}
