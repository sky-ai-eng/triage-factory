package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
//   - Invalid-UUID keys: SetEnabled / SetOverrides refuse with
//     db.ErrNoSuchTeamAgent rather than surfacing a 22P02 parse error
//     (Postgres) or silently affecting zero rows (SQLite); Remove stays a
//     silent no-op because it is a delete.
//   - Every single-row write returns the row it persisted.
func RunTeamAgentStoreConformance(t *testing.T, factory TeamAgentStoreFactory) {
	t.Helper()

	t.Run("every_single_row_write_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard applied to each converted method in turn:
		// what the write handed back is what a point read finds.
		store, orgID, teamID, agentID := factory(t)
		ctx := context.Background()
		if err := store.AddForTeam(ctx, orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam: %v", err)
		}
		read := func() (*domain.TeamAgent, error) { return store.GetForTeam(ctx, orgID, teamID, agentID) }

		disabled, err := store.SetEnabled(ctx, orgID, teamID, agentID, false)
		if err != nil {
			t.Fatalf("SetEnabled: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetEnabled", disabled, read)
		if disabled.Enabled {
			t.Error("SetEnabled returned enabled=true after disabling")
		}

		model := "claude-opus-4-7"
		autonomy := 0.8
		overridden, err := store.SetOverrides(ctx, orgID, teamID, agentID, &model, &autonomy)
		if err != nil {
			t.Fatalf("SetOverrides: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetOverrides", overridden, read)
		// The row carries the enabled bit the earlier write set, which the
		// override arguments say nothing about.
		if overridden.Enabled || overridden.PerTeamModel != model {
			t.Errorf("SetOverrides returned %+v, want the stored enabled bit and the new model", overridden)
		}

		// The clear arm: an empty model stores NULL, and only the row says so.
		empty := ""
		cleared, err := store.SetOverrides(ctx, orgID, teamID, agentID, &empty, nil)
		if err != nil {
			t.Fatalf("SetOverrides (clear): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetOverrides (clear)", cleared, read)
		if cleared.PerTeamModel != "" || cleared.PerTeamAutonomySuitability != nil {
			t.Errorf("SetOverrides (clear) returned %+v, want both overrides cleared", cleared)
		}

		missing := uuid.New().String()
		if _, err := store.SetEnabled(ctx, orgID, missing, agentID, true); !errors.Is(err, db.ErrNoSuchTeamAgent) {
			t.Errorf("SetEnabled on a missing team: got %v, want db.ErrNoSuchTeamAgent", err)
		}
		if _, err := store.SetOverrides(ctx, orgID, missing, agentID, nil, nil); !errors.Is(err, db.ErrNoSuchTeamAgent) {
			t.Errorf("SetOverrides on a missing team: got %v, want db.ErrNoSuchTeamAgent", err)
		}
	})

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
		if _, err := store.SetEnabled(ctx, orgID, teamID, agentID, false); err != nil {
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
		if _, err := store.SetEnabled(ctx, orgID, teamID, agentID, false); err != nil {
			t.Fatalf("SetEnabled false: %v", err)
		}
		got, _ := store.GetForTeam(ctx, orgID, teamID, agentID)
		if got == nil || got.Enabled {
			t.Errorf("after false: got=%+v want enabled=false", got)
		}
		if _, err := store.SetEnabled(ctx, orgID, teamID, agentID, true); err != nil {
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
		if _, err := store.SetOverrides(ctx, orgID, teamID, agentID, &model, &autonomy); err != nil {
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
		if _, err := store.SetOverrides(ctx, orgID, teamID, agentID, &model, &autonomy); err != nil {
			t.Fatalf("SetOverrides first: %v", err)
		}
		// Now clear both.
		if _, err := store.SetOverrides(ctx, orgID, teamID, agentID, nil, nil); err != nil {
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

	t.Run("Mutations_OnInvalidUUID_RefuseTheKey", func(t *testing.T) {
		store, orgID, _, agentID := factory(t)
		ctx := context.Background()
		// A malformed team id answers to no row, which both backends now
		// report as db.ErrNoSuchTeamAgent rather than a silent success. Remove
		// stays a silent no-op: it is a delete, and deleting nothing is done.
		if _, err := store.SetEnabled(ctx, orgID, "not-a-uuid", agentID, false); !errors.Is(err, db.ErrNoSuchTeamAgent) {
			t.Errorf("SetEnabled invalid teamID: want db.ErrNoSuchTeamAgent, got %v", err)
		}
		if _, err := store.SetOverrides(ctx, orgID, "not-a-uuid", agentID, nil, nil); !errors.Is(err, db.ErrNoSuchTeamAgent) {
			t.Errorf("SetOverrides invalid teamID: want db.ErrNoSuchTeamAgent, got %v", err)
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
