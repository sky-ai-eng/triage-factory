package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TeamAgentStoreFactory returns the wired store + orgID + teamID +
// agentID, all pre-seeded so FKs into orgs/teams/agents are satisfied,
// plus a disabler for the (team, agent) row those ids name. SQLite ignores
// orgID (asserts LocalDefaultOrgID) and accepts arbitrary strings for
// teamID/agentID; Postgres requires real UUIDs that already exist in the
// respective tables.
type TeamAgentStoreFactory func(t *testing.T) (store db.TeamAgentStore, orgID, teamID, agentID string, disable TeamAgentDisabler)

// TeamAgentDisabler flips the factory's (team, agent) row to enabled=false
// through the backend's own raw SQL. No production door writes that column,
// so the harness is handed the shape rather than a method: the backend test
// owns the statement against its own connection, the same way ScoreSeeder
// owns task rows (SQLite backends wrap SetTeamAgentEnabledDirect).
type TeamAgentDisabler func(t *testing.T)

// RunTeamAgentStoreConformance runs the shared assertion suite. What it
// covers:
//
//   - AddForTeam inserts a default-enabled row.
//   - AddForTeam is idempotent — a second call doesn't reset Enabled
//     to TRUE on a row that has been disabled. Load-bearing for
//     migration runs that hit BootstrapTeamAgent on every boot.
//   - GetForTeam returns (nil, nil) when no row exists; round-trips
//     the row's fields once it does.
//   - Invalid-UUID keys: GetForTeam answers (nil, nil) rather than
//     surfacing a 22P02 parse error (Postgres) or an error of any kind
//     (SQLite).
func RunTeamAgentStoreConformance(t *testing.T, factory TeamAgentStoreFactory) {
	t.Helper()

	t.Run("AddForTeam_FirstCallInsertsEnabled", func(t *testing.T) {
		store, orgID, teamID, agentID, _ := factory(t)
		ctx := context.Background()
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam: %v", err)
		}
		got, err := store.GetForTeam(ctx, orgID, teamID, agentID)
		if err != nil {
			t.Fatalf("GetForTeam: %v", err)
		}
		if got == nil {
			t.Fatal("GetForTeam returned nil after AddForTeam")
		}
		if !got.Enabled {
			t.Error("bot disabled on default insert; team_agents.enabled DEFAULT TRUE expected")
		}
		if got.TeamID != teamID || got.AgentID != agentID {
			t.Errorf("GetForTeam returned (%s, %s); want (%s, %s)", got.TeamID, got.AgentID, teamID, agentID)
		}
		if got.PerTeamModel != "" || got.PerTeamAutonomySuitability != nil {
			t.Errorf("fresh row carries overrides %+v; want none", got)
		}
	})

	t.Run("AddForTeam_IdempotentPreservesDisableState", func(t *testing.T) {
		// The load-bearing invariant: a team's bot is off, every boot calls
		// BootstrapTeamAgent, the team must STAY off. Without this,
		// "disabled" leaks across boots.
		store, orgID, teamID, agentID, disable := factory(t)
		ctx := context.Background()
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam first: %v", err)
		}
		disable(t)
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam re-run: %v", err)
		}
		got, _ := store.GetForTeam(ctx, orgID, teamID, agentID)
		if got == nil {
			t.Fatal("row missing after re-run AddForTeam")
		}
		if got.Enabled {
			t.Fatal("AddForTeam re-ran flipped Enabled back to TRUE; the disable would be lost across boots")
		}
	})

	t.Run("GetForTeam_ReturnsNilWhenAbsent", func(t *testing.T) {
		store, orgID, teamID, agentID, _ := factory(t)
		got, err := store.GetForTeam(context.Background(), orgID, teamID, agentID)
		if err != nil {
			t.Fatalf("GetForTeam: %v", err)
		}
		if got != nil {
			t.Fatalf("GetForTeam on fresh store returned %+v; want nil", got)
		}
	})

	t.Run("GetForTeam_OnInvalidUUID_ReturnsNil", func(t *testing.T) {
		// A malformed team id answers to no row, which is the same (nil, nil)
		// an absent row gets — never a parse error leaking out of Postgres.
		store, orgID, _, agentID, _ := factory(t)
		got, err := store.GetForTeam(context.Background(), orgID, "not-a-uuid", agentID)
		if err != nil {
			t.Errorf("GetForTeam invalid teamID: want nil err, got %v", err)
		}
		if got != nil {
			t.Errorf("GetForTeam invalid teamID returned %+v; want nil", got)
		}
	})
}
