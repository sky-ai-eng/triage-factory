package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TeamAgentStoreFactory returns the wired store + orgID + teamID +
// agentID, all pre-seeded so FKs into orgs/teams/agents are satisfied.
// SQLite ignores orgID (asserts LocalDefaultOrgID) and accepts arbitrary
// strings for teamID/agentID; Postgres requires real UUIDs that already
// exist in the respective tables.
type TeamAgentStoreFactory func(t *testing.T) (store db.TeamAgentStore, orgID, teamID, agentID string)

// RunTeamAgentStoreConformance runs the shared assertion suite. What it
// covers:
//
//   - AddForTeam inserts a default-enabled row.
//   - AddForTeam is idempotent — a second call doesn't reset Enabled
//     to TRUE if the user has disabled the bot. Load-bearing for
//     migration runs that hit BootstrapTeamAgent on every boot.
//   - GetForTeam returns (nil, nil) when no row exists; round-trips
//     the row's fields once it does.
//   - SetEnabled toggles in both directions.
//   - SetOverrides writes nil → cleared, value → persisted.
//   - Remove deletes the row.
//   - ListForOrg returns every team_agents row for the agent.
//   - Postgres invalid-UUID guards: mutating methods no-op rather than
//     surfacing 22P02 parse errors.
func RunTeamAgentStoreConformance(t *testing.T, factory TeamAgentStoreFactory) {
	t.Helper()

	t.Run("AddForTeam_FirstCallInsertsEnabled", func(t *testing.T) {
		store, orgID, teamID, agentID := factory(t)
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
	})

	t.Run("AddForTeam_IdempotentPreservesDisableState", func(t *testing.T) {
		// The load-bearing invariant: a team disables the bot, every
		// boot calls BootstrapTeamAgent, the team must STAY disabled.
		// Without this, "disabled" leaks across boots.
		store, orgID, teamID, agentID := factory(t)
		ctx := context.Background()
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam first: %v", err)
		}
		if err := store.SetEnabled(ctx, orgID, teamID, agentID, false); err != nil {
			t.Fatalf("SetEnabled false: %v", err)
		}
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam re-run: %v", err)
		}
		got, _ := store.GetForTeam(ctx, orgID, teamID, agentID)
		if got == nil {
			t.Fatal("row missing after re-run AddForTeam")
		}
		if got.Enabled {
			t.Fatal("AddForTeam re-ran flipped Enabled back to TRUE; user's disable would be lost across boots")
		}
	})

	t.Run("GetForTeam_ReturnsNilWhenAbsent", func(t *testing.T) {
		store, orgID, teamID, agentID := factory(t)
		got, err := store.GetForTeam(context.Background(), orgID, teamID, agentID)
		if err != nil {
			t.Fatalf("GetForTeam: %v", err)
		}
		if got != nil {
			t.Fatalf("GetForTeam on fresh store returned %+v; want nil", got)
		}
	})

	t.Run("SetEnabled_TogglesBothDirections", func(t *testing.T) {
		store, orgID, teamID, agentID := factory(t)
		ctx := context.Background()
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam: %v", err)
		}
		if err := store.SetEnabled(ctx, orgID, teamID, agentID, false); err != nil {
			t.Fatalf("SetEnabled false: %v", err)
		}
		got, _ := store.GetForTeam(ctx, orgID, teamID, agentID)
		if got == nil || got.Enabled {
			t.Errorf("after false: got=%+v want enabled=false", got)
		}
		if err := store.SetEnabled(ctx, orgID, teamID, agentID, true); err != nil {
			t.Fatalf("SetEnabled true: %v", err)
		}
		got, _ = store.GetForTeam(ctx, orgID, teamID, agentID)
		if got == nil || !got.Enabled {
			t.Errorf("after true: got=%+v want enabled=true", got)
		}
	})

	t.Run("SetOverrides_ValuesPersist", func(t *testing.T) {
		store, orgID, teamID, agentID := factory(t)
		ctx := context.Background()
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam: %v", err)
		}
		model := "claude-haiku-4-5"
		autonomy := 0.4
		if err := store.SetOverrides(ctx, orgID, teamID, agentID, &model, &autonomy); err != nil {
			t.Fatalf("SetOverrides: %v", err)
		}
		got, _ := store.GetForTeam(ctx, orgID, teamID, agentID)
		if got == nil {
			t.Fatal("row missing after SetOverrides")
		}
		if got.PerTeamModel != "claude-haiku-4-5" {
			t.Errorf("PerTeamModel=%q want claude-haiku-4-5", got.PerTeamModel)
		}
		if got.PerTeamAutonomySuitability == nil || !nearlyEqual(*got.PerTeamAutonomySuitability, 0.4) {
			t.Errorf("PerTeamAutonomySuitability=%v want 0.4", got.PerTeamAutonomySuitability)
		}
	})

	t.Run("SetOverrides_NilClears", func(t *testing.T) {
		store, orgID, teamID, agentID := factory(t)
		ctx := context.Background()
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam: %v", err)
		}
		model := "claude-haiku-4-5"
		autonomy := 0.4
		if err := store.SetOverrides(ctx, orgID, teamID, agentID, &model, &autonomy); err != nil {
			t.Fatalf("SetOverrides first: %v", err)
		}
		// Now clear both.
		if err := store.SetOverrides(ctx, orgID, teamID, agentID, nil, nil); err != nil {
			t.Fatalf("SetOverrides clear: %v", err)
		}
		got, _ := store.GetForTeam(ctx, orgID, teamID, agentID)
		if got == nil {
			t.Fatal("row missing after clear")
		}
		if got.PerTeamModel != "" {
			t.Errorf("PerTeamModel=%q after nil-clear; want empty", got.PerTeamModel)
		}
		if got.PerTeamAutonomySuitability != nil {
			t.Errorf("PerTeamAutonomySuitability=%v after nil-clear; want nil", got.PerTeamAutonomySuitability)
		}
	})

	t.Run("Remove_DeletesRow", func(t *testing.T) {
		store, orgID, teamID, agentID := factory(t)
		ctx := context.Background()
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam: %v", err)
		}
		if err := store.Remove(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		got, err := store.GetForTeam(ctx, orgID, teamID, agentID)
		if err != nil {
			t.Fatalf("GetForTeam: %v", err)
		}
		if got != nil {
			t.Errorf("row still present after Remove: %+v", got)
		}
	})

	t.Run("ListForOrg_IncludesAddedRow", func(t *testing.T) {
		store, orgID, teamID, agentID := factory(t)
		ctx := context.Background()
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam: %v", err)
		}
		rows, err := store.ListForOrg(ctx, orgID, agentID)
		if err != nil {
			t.Fatalf("ListForOrg: %v", err)
		}
		var found bool
		for _, r := range rows {
			if r.TeamID == teamID && r.AgentID == agentID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ListForOrg did not return the row we just added; got %d rows", len(rows))
		}
	})

	t.Run("Mutations_OnInvalidUUID_AreNoops", func(t *testing.T) {
		store, orgID, _, agentID := factory(t)
		ctx := context.Background()
		if err := store.SetEnabled(ctx, orgID, "not-a-uuid", agentID, false); err != nil {
			t.Errorf("SetEnabled invalid teamID: want nil, got %v", err)
		}
		if err := store.SetOverrides(ctx, orgID, "not-a-uuid", agentID, nil, nil); err != nil {
			t.Errorf("SetOverrides invalid teamID: want nil, got %v", err)
		}
		if err := store.Remove(ctx, orgID, "not-a-uuid", agentID); err != nil {
			t.Errorf("Remove invalid teamID: want nil, got %v", err)
		}
		got, err := store.GetForTeam(ctx, orgID, "not-a-uuid", agentID)
		if err != nil {
			t.Errorf("GetForTeam invalid teamID: want nil err, got %v", err)
		}
		if got != nil {
			t.Errorf("GetForTeam invalid teamID returned %+v; want nil", got)
		}
	})
}
